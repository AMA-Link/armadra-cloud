/**
 * `personal init | status | passwd | rotate-key | links | traffic`：只读写存储的子命令。
 *
 * 与正在运行的 `serve` 并存：状态文件的写入走目录锁、SQLite 的写入走 `BEGIN IMMEDIATE` 事务、
 * PostgreSQL 的写入先锁租户行，`serve` 每秒察觉一次外部改动，改口令、轮换密钥、撤销链接立即生效（撤销的访客 WS 会被断开）。
 * 输出只有账号名、id、时间、指纹这类元数据，不打印口令、令牌或私钥。
 */
import { changePassword } from "@armadra/control-core/accounts";
import { listLinks, revokeLink } from "@armadra/control-core/links";
import { DEFAULT_TENANT, type TrafficRow } from "@armadra/control-core/ports";

import { defaultIssuer, UsageError, type PersonalInitOptions } from "./args.ts";
import { stateOnly } from "./context.ts";
import { createState, obtainPassword, passwordUsageError } from "./init.ts";
import { openPersonalStore } from "./store.ts";
import {
  STORE_LABELS,
  type DatabaseOptions,
  type PersonalStore,
} from "./store-types.ts";
import { existingCaFingerprint, selfSignedMaterial, tlsNames } from "./tls.ts";

export type Print = (line: string) => void;

const time = (ms: number | null) =>
  ms === null ? "—" : new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");

/** 打开存储跑一下就关（SQLite 要关库，PostgreSQL 要关连接池）。 */
export async function withStore<T>(
  dataDir: string,
  fn: (store: PersonalStore) => T | Promise<T>,
  database?: DatabaseOptions,
): Promise<T> {
  const store = await openPersonalStore(dataDir, undefined, database);
  try {
    return await fn(store);
  } finally {
    await store.close();
  }
}

export async function runInit(
  options: PersonalInitOptions,
  print: Print,
): Promise<void> {
  const issuer =
    options.publicOrigin ?? defaultIssuer(options.host, options.port);
  const password = await obtainPassword(options.passwordFile, true);
  const store = await createState({
    dataDir: options.dataDir,
    account: options.account,
    password,
    issuer,
    ...(options.store ? { store: options.store } : {}),
    ...(options.database ? { database: options.database } : {}),
  });
  await store.close();
  print(`已初始化：${options.dataDir}`);
  print(`存储：${STORE_LABELS[store.kind]}`);
  print(`账号：${options.account}`);
  print(`对外地址：${issuer}`);
  if (options.tls === "self-signed") {
    const material = selfSignedMaterial(
      options.dataDir,
      tlsNames(options.host),
    );
    print(`CA 指纹：${material.fingerprint}`);
  }
}

export async function runStatus(
  dataDir: string,
  print: Print,
  nowMs = Date.now(),
  database?: DatabaseOptions,
): Promise<void> {
  const { state, kind } = await withStore(
    dataDir,
    async (store) => ({ state: await store.snapshot(), kind: store.kind }),
    database,
  );
  const fingerprint = existingCaFingerprint(dataDir);
  print(`存储：${STORE_LABELS[kind]}`);
  print(`账号：${state.account.name}`);
  print(`对外地址：${state.issuer}`);
  print(`CA 指纹：${fingerprint ?? "—"}`);
  if (state.account.lockedUntilMs > nowMs)
    print(`锁定到：${time(state.account.lockedUntilMs)}`);
  const keys = state.signingKeys
    .map((key) =>
      key.retiredAtMs === null
        ? key.kid
        : `${key.kid}（退役于 ${time(key.retiredAtMs)}）`,
    )
    .join("、");
  print(`签名钥：${keys}`);
  const sources = state.sources.filter((row) => row.revokedAtMs === null);
  print(`源：${sources.length}`);
  for (const source of sources)
    print(
      `  ${source.sourceId}  ${source.name}  ${source.kind}  ${source.coreVersion}  最近在线 ${time(source.lastSeenAtMs)}`,
    );
  const links = state.links.filter(
    (row) => row.revokedAtMs === null && row.expiresAtMs > nowMs,
  );
  print(`链接：${links.length}`);
  const devices = state.devices.filter(
    (row) => row.revokedAtMs === null && row.expiresAtMs > nowMs,
  );
  print(`设备：${devices.length}`);
}

export async function runPasswd(
  dataDir: string,
  passwordFile: string | undefined,
  print: Print,
  database?: DatabaseOptions,
): Promise<void> {
  // 先确认存储能打开，再问口令。
  await withStore(dataDir, () => undefined, database);
  const password = await obtainPassword(passwordFile, true);
  await withStore(
    dataDir,
    async (store) => {
      try {
        await changePassword(
          stateOnly(store),
          (await store.snapshot()).account.name,
          password,
          Date.now(),
        );
      } catch (error) {
        throw passwordUsageError(error);
      }
    },
    database,
  );
  print("口令已更新，账号的全部设备需要重新登录");
}

export async function runRotateKey(
  dataDir: string,
  print: Print,
  database?: DatabaseOptions,
): Promise<void> {
  const kid = await withStore(
    dataDir,
    (store) => store.rotateKey(Date.now()),
    database,
  );
  print(`已启用新签名钥：${kid}`);
}

export async function runLinksList(
  dataDir: string,
  print: Print,
  nowMs = Date.now(),
  database?: DatabaseOptions,
): Promise<void> {
  const links = await withStore(
    dataDir,
    (store) => listLinks(stateOnly(store)),
    database,
  );
  if (links.length === 0) {
    print("没有链接");
    return;
  }
  for (const link of links) {
    const status =
      link.revokedAtMs !== null
        ? "已撤销"
        : link.expiresAtMs <= nowMs
          ? "已过期"
          : link.maxUses !== null && link.uses >= link.maxUses
            ? "已用完"
            : "有效";
    const uses =
      link.maxUses === null ? `${link.uses}` : `${link.uses}/${link.maxUses}`;
    print(
      `  ${link.linkId}  ${status}  ${link.label || "—"}  ${link.role ?? ""}  ${uses}  到期 ${time(link.expiresAtMs)}  源 ${link.sourceId}`,
    );
  }
}

export async function runLinksRevoke(
  dataDir: string,
  linkId: string,
  print: Print,
  database?: DatabaseOptions,
): Promise<void> {
  const found = await withStore(
    dataDir,
    (store) => revokeLink(stateOnly(store), linkId, Date.now()),
    database,
  );
  if (found === null) throw new UsageError(`没有这条有效链接：${linkId}`);
  print(`已撤销链接：${linkId}`);
}

/** 字节数的可读写法（1024 进位）。 */
export function formatBytes(bytes: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return unit === 0 ? `${bytes} B` : `${value.toFixed(1)} ${units[unit]}`;
}

/** 一张对齐的表（按显示宽度：中文算两格）。 */
function table(rows: string[][]): string[] {
  const width = (text: string) =>
    [...text].reduce(
      (sum, char) => sum + ((char.codePointAt(0) ?? 0) > 0xff ? 2 : 1),
      0,
    );
  const widths =
    rows[0]?.map((_, column) =>
      Math.max(...rows.map((row) => width(row[column] ?? ""))),
    ) ?? [];
  return rows.map((row) =>
    row
      .map((cell, column) =>
        column === row.length - 1
          ? cell
          : cell + " ".repeat((widths[column] ?? 0) - width(cell)),
      )
      .join("  ")
      .trimEnd(),
  );
}

/**
 * `personal traffic [--since 7d] [--source <id>]`：读存储里的流量统计（cloud-api §19），按
 * 源 × 账号 × 设备 × 类别合计成一张表，按总字节从多到少。状态文件存储只在 `serve` 的内存里累计，
 * 这里读不到。
 */
export async function runTraffic(
  options: { dataDir: string; sinceMs: number; sourceId?: string },
  print: Print,
  nowMs = Date.now(),
  database?: DatabaseOptions,
): Promise<void> {
  const rows = await withStore(
    options.dataDir,
    (store) => {
      if (store.kind === "file")
        throw new UsageError(
          "状态文件存储只在 serve 的内存里累计流量：先运行 armadra-relay personal migrate --to sqlite",
        );
      return store.ports.traffic.query(DEFAULT_TENANT, {
        fromMs: nowMs - options.sinceMs - (nowMs % 3_600_000),
        toMs: nowMs + 1,
        granularity: "day",
        limit: 1_000_000,
        ...(options.sourceId ? { sourceId: options.sourceId } : {}),
      });
    },
    database,
  );
  const merged = new Map<string, TrafficRow>();
  for (const row of rows) {
    const key = [row.sourceId, row.subject, row.deviceId, row.kind].join(
      "\u0000",
    );
    const found = merged.get(key);
    if (found) {
      found.connections += row.connections;
      found.bytesToSource += row.bytesToSource;
      found.bytesToClient += row.bytesToClient;
    } else merged.set(key, { ...row });
  }
  if (merged.size === 0) {
    print("没有流量记录");
    return;
  }
  const sorted = [...merged.values()].sort(
    (left, right) =>
      right.bytesToSource +
      right.bytesToClient -
      (left.bytesToSource + left.bytesToClient),
  );
  const total = sorted.reduce(
    (sum, row) => ({
      connections: sum.connections + row.connections,
      up: sum.up + row.bytesToSource,
      down: sum.down + row.bytesToClient,
    }),
    { connections: 0, up: 0, down: 0 },
  );
  for (const line of table([
    ["源", "账号", "设备", "类别", "连接", "上行", "下行"],
    ...sorted.map((row) => [
      row.sourceId,
      row.subject,
      row.deviceId || "—",
      row.kind,
      String(row.connections),
      formatBytes(row.bytesToSource),
      formatBytes(row.bytesToClient),
    ]),
    [
      "合计",
      "",
      "",
      "",
      String(total.connections),
      formatBytes(total.up),
      formatBytes(total.down),
    ],
  ]))
    print(line);
}
