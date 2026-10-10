// 拉闸（中继设计 §4.5；运维手册 docs/runbooks/cloudflare-protection.md）：账单失控或被打时，一条命令
// 把 relay.armadra.com的 Worker Custom Domain 切走，止血后再切回。
//
//   node scripts/kill-switch.mjs <动作> [--apply]
//
//   status       看这个主机现在挂在哪个 Worker 上
//   maintenance  切到维护页 Worker（armadra-relay-maintenance，要预先部署），所有请求答 503
//   disable      删掉 Custom Domain：主机不再解析，Worker 一个请求都收不到（恢复要重新挂、重新签证书）
//   restore      切回中继 Worker（armadra-relay）
//
// 缺省是演练：只打印要发的请求，不读令牌、不连网络。加 --apply 才真的调 Cloudflare API，令牌只从
// 环境变量 CLOUDFLARE_API_TOKEN 读（Workers Scripts:Edit、Workers Routes:Edit），不收命令行明文、
// 不打印。账号 id 缺省取 wrangler.jsonc 里的那个，可用 --account 或 CLOUDFLARE_ACCOUNT_ID 换。
import { pathToFileURL } from "node:url";

export const API = "https://api.cloudflare.com/client/v4";
export const DEFAULT_ACCOUNT = "a27d9dab9190150768845b62cfd57cfe";
export const ZONE_NAME = "armadra.com";
export const MAINTENANCE_SERVICE = "armadra-relay-maintenance";

export const TARGET = {
  hostname: "relay.armadra.com",
  service: "armadra-relay",
};

export const ACTIONS = ["status", "maintenance", "disable", "restore"];

export const USAGE = `用法: node scripts/kill-switch.mjs <动作> [选项]

  status       看主机现在挂在哪个 Worker 上
  maintenance  切到维护页 Worker（${MAINTENANCE_SERVICE}），所有请求答 503
  disable      删掉 Custom Domain，主机不再解析（恢复要重新签证书，几分钟）
  restore      切回中继 Worker

选项:
  --account ID   Cloudflare 账号 id（或 CLOUDFLARE_ACCOUNT_ID），缺省 ${DEFAULT_ACCOUNT}
  --apply        真的执行；不加就只演练、打印要发的请求
  --help         打印用法

令牌只从环境变量 CLOUDFLARE_API_TOKEN 读，只在 --apply 时读。`;

export class UsageError extends Error {
  constructor(message) {
    super(message);
    this.name = "UsageError";
  }
}

export function parseArgs(argv, env = {}) {
  const options = {
    action: undefined,
    account: env.CLOUDFLARE_ACCOUNT_ID || DEFAULT_ACCOUNT,
    apply: false,
    help: false,
  };
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === "--apply") options.apply = true;
    else if (arg === "--help" || arg === "-h") options.help = true;
    else if (arg === "--account") {
      const value = argv[index + 1];
      if (value === undefined || value.startsWith("--"))
        throw new UsageError(`${arg} 缺少取值`);
      options.account = value;
      index += 1;
    } else if (arg.startsWith("--")) throw new UsageError(`未知选项：${arg}`);
    else if (options.action === undefined) options.action = arg;
    else throw new UsageError(`多余的参数：${arg}`);
  }
  if (options.help) return options;
  if (!options.action)
    throw new UsageError("缺少动作：status | maintenance | disable | restore");
  if (!ACTIONS.includes(options.action))
    throw new UsageError(
      `动作只能是 ${ACTIONS.join(" | ")}：${options.action}`,
    );
  if (!/^[0-9a-f]{32}$/.test(options.account))
    throw new UsageError(`--account 不是账号 id：${options.account}`);
  return options;
}

/**
 * 要发的请求（与令牌无关、纯函数）。`{id}` 是要先从列表里查出来的 Custom Domain id（只有 disable 用）。
 */
export function planKillSwitch(options) {
  const target = TARGET;
  const base = `/accounts/${options.account}/workers/domains`;
  const list = {
    method: "GET",
    path: `${base}?hostname=${encodeURIComponent(target.hostname)}`,
    note: `查 ${target.hostname} 现在挂在哪个 Worker 上`,
  };
  const attach = (service, note) => ({
    method: "PUT",
    path: base,
    body: {
      hostname: target.hostname,
      service,
      zone_name: ZONE_NAME,
      enabled: true,
    },
    note,
  });
  switch (options.action) {
    case "status":
      return { hostname: target.hostname, steps: [list] };
    case "maintenance":
      return {
        hostname: target.hostname,
        steps: [
          list,
          attach(
            MAINTENANCE_SERVICE,
            `把 ${target.hostname} 切到维护页 Worker ${MAINTENANCE_SERVICE}`,
          ),
        ],
      };
    case "restore":
      return {
        hostname: target.hostname,
        steps: [
          list,
          attach(
            target.service,
            `把 ${target.hostname} 切回 ${target.service}`,
          ),
        ],
      };
    case "disable":
      return {
        hostname: target.hostname,
        steps: [
          list,
          {
            method: "DELETE",
            path: `${base}/{id}`,
            note: `删掉 ${target.hostname} 的 Custom Domain（主机不再解析）`,
          },
        ],
      };
    default:
      throw new UsageError(`动作不认识：${options.action}`);
  }
}

function describeStep(step) {
  const body = step.body ? ` ${JSON.stringify(step.body)}` : "";
  return `  ${step.method} ${API}${step.path}${body}\n    # ${step.note}`;
}

async function call(fetcher, token, step, path) {
  const response = await fetcher(`${API}${path}`, {
    method: step.method,
    headers: {
      authorization: `Bearer ${token}`,
      ...(step.body ? { "content-type": "application/json" } : {}),
    },
    ...(step.body ? { body: JSON.stringify(step.body) } : {}),
  });
  let payload = null;
  try {
    payload = await response.json();
  } catch {
    payload = null;
  }
  if (!response.ok || payload?.success === false) {
    const messages = (payload?.errors ?? [])
      .map((error) => `${error.code ?? ""} ${error.message ?? ""}`.trim())
      .join("；");
    throw new Error(
      `Cloudflare API 答 ${response.status}：${messages || "没有错误说明"}`,
    );
  }
  return payload?.result ?? null;
}

/**
 * 执行（或演练）。`deps.fetch` / `deps.token` / `deps.print` 可注入，测试用；演练时一个都不碰。
 * 答退出码。
 */
export async function runKillSwitch(options, deps = {}) {
  const print = deps.print ?? ((line) => console.log(line));
  const plan = planKillSwitch(options);
  if (!options.apply) {
    print(
      `演练（没加 --apply，不读令牌、不发请求）：${options.action} ${plan.hostname}`,
    );
    for (const step of plan.steps) print(describeStep(step));
    print("确认无误后加 --apply 执行；令牌放在环境变量 CLOUDFLARE_API_TOKEN。");
    return 0;
  }
  const token = deps.token;
  if (!token) {
    print(
      "缺少 CLOUDFLARE_API_TOKEN：--apply 要一个 API Token（Workers Scripts:Edit、Workers Routes:Edit）",
    );
    return 64;
  }
  const fetcher = deps.fetch ?? fetch;
  const [list, ...rest] = plan.steps;
  const domains = (await call(fetcher, token, list, list.path)) ?? [];
  const current = domains.find((domain) => domain.hostname === plan.hostname);
  print(
    current
      ? `现在：${plan.hostname} → ${current.service}`
      : `现在：${plan.hostname} 没有挂 Custom Domain`,
  );
  for (const step of rest) {
    if (step.method === "DELETE") {
      if (!current) {
        print("已经没有 Custom Domain，不用删");
        continue;
      }
      await call(fetcher, token, step, step.path.replace("{id}", current.id));
    } else {
      if (current?.service === step.body.service) {
        print(`已经是 ${step.body.service}，不用切`);
        continue;
      }
      await call(fetcher, token, step, step.path);
    }
    print(`完成：${step.note}`);
  }
  return 0;
}

async function main() {
  let options;
  try {
    options = parseArgs(process.argv.slice(2), process.env);
  } catch (error) {
    if (error instanceof UsageError) {
      console.error(`${error.message}\n\n${USAGE}`);
      process.exit(64);
    }
    throw error;
  }
  if (options.help) {
    console.log(USAGE);
    return;
  }
  try {
    process.exitCode = await runKillSwitch(options, {
      // 只有 --apply 才读令牌。
      token: options.apply ? process.env.CLOUDFLARE_API_TOKEN : undefined,
    });
  } catch (error) {
    console.error(
      `失败：${error instanceof Error ? error.message : String(error)}`,
    );
    process.exitCode = 1;
  }
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) await main();
