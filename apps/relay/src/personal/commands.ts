/**
 * `personal init | status | passwd | rotate-key | links`：只读写状态文件的子命令。
 *
 * 与正在运行的 `serve` 并存：写入都走 `StateStore.update` 的目录锁，`serve` 每秒看一眼文件，
 * 改口令、轮换密钥、撤销链接立即生效（撤销的访客 WS 会被断开）。输出只有账号名、id、时间、
 * 指纹这类元数据，不打印口令、令牌或私钥。
 */
import { changePassword } from "./accounts.ts";
import { defaultIssuer, UsageError, type PersonalInitOptions } from "./args.ts";
import { createState, obtainPassword, passwordUsageError } from "./init.ts";
import { listLinks, revokeLinkIn } from "./links.ts";
import { rotateSigningKey } from "./signer.ts";
import { StateStore } from "./state.ts";
import { existingCaFingerprint, selfSignedMaterial, tlsNames } from "./tls.ts";

export type Print = (line: string) => void;

const time = (ms: number | null) =>
  ms === null ? "—" : new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");

export async function runInit(
  options: PersonalInitOptions,
  print: Print,
): Promise<void> {
  const issuer =
    options.publicOrigin ?? defaultIssuer(options.host, options.port);
  const password = await obtainPassword(options.passwordFile, true);
  await createState({
    dataDir: options.dataDir,
    account: options.account,
    password,
    issuer,
  });
  print(`已初始化：${options.dataDir}`);
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

export function runStatus(
  dataDir: string,
  print: Print,
  nowMs = Date.now(),
): void {
  const state = StateStore.open(dataDir).current;
  const fingerprint = existingCaFingerprint(dataDir);
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
): Promise<void> {
  const store = StateStore.open(dataDir);
  const password = await obtainPassword(passwordFile, true);
  try {
    await changePassword(
      (fn) => store.update(fn),
      store.current.account.name,
      password,
      Date.now(),
    );
  } catch (error) {
    throw passwordUsageError(error);
  }
  print("口令已更新，账号的全部设备需要重新登录");
}

export function runRotateKey(dataDir: string, print: Print): void {
  const store = StateStore.open(dataDir);
  const kid = store.update((draft) => rotateSigningKey(draft, Date.now()));
  print(`已启用新签名钥：${kid}`);
}

export function runLinksList(
  dataDir: string,
  print: Print,
  nowMs = Date.now(),
): void {
  const state = StateStore.open(dataDir).current;
  const links = listLinks(state);
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

export function runLinksRevoke(
  dataDir: string,
  linkId: string,
  print: Print,
): void {
  const store = StateStore.open(dataDir);
  const found = store.update((draft) =>
    revokeLinkIn(draft, linkId, Date.now()),
  );
  if (!found) throw new UsageError(`没有这条有效链接：${linkId}`);
  print(`已撤销链接：${linkId}`);
}
