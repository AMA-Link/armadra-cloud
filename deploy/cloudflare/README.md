# Cloudflare Workers 上的个人中转

`relay.armadra.com`（生产）与 `relay-staging.armadra.com`（staging）是同一份 Worker 代码的两个环境：wrangler 项目在 [apps/relay-workers](../../apps/relay-workers/)（[wrangler.jsonc](../../apps/relay-workers/wrangler.jsonc)），运行时在 [packages/relay-runtime-workers](../../packages/relay-runtime-workers/)。线上形状与 Node 中继相同（契约 [cloud-api §7–§12、§17](../../docs/contracts/cloud-api.md)），唯一差别是隧道节点地址带 `?src=<sourceId>`。

部署、录 secret、改 DNS 都是对外操作，由维护者在本机执行；仓库里的脚本与测试只在本地 workerd 上跑，不连 Cloudflare 账号。日常运维（初始化、心跳、配额、回滚、排障）见[运维手册](../../docs/runbooks/relay-on-cloudflare.md)。

## 架构

| 对象        | 一个对应                     | 做什么                                                                                             |
| ----------- | ---------------------------- | -------------------------------------------------------------------------------------------------- |
| Worker      | 每个请求                     | 按路径分派；进对象前过 Rate Limiting 绑定的闸；`/health`、`/ready`、`/metrics`；托管页面           |
| `SourceDO`  | 一个源（`src:<sourceId>`）   | 隧道 WS 与经它的客户端 WS（Hibernation）、HTTP / WS 转发、媒体票、撤销缓存                         |
| `ControlDO` | 一个租户（`tenant:default`） | DO SQLite 存账号 / 设备 / 源 / 链接 / 签名钥（与 Node 中继同一份迁移），`/v1/*`、me-stream、日配额 |

## 两套配置（zone 都是 Free Website）

| 项                                | 配置 F：Workers Free（`wrangler.jsonc` 现状） | 配置 P：Workers Paid（$5 / 月）          |
| --------------------------------- | --------------------------------------------- | ---------------------------------------- |
| `RELAY_ALARM_MS`（检查周期）      | `120000`                                      | `60000`                                  |
| `RELAY_DAILY_REQUEST_BUDGET`      | `80000`（Free 每天 10 万请求是硬封顶）        | `2000000`                                |
| `RELAY_DAILY_TUNNEL_BYTES_BUDGET` | `53687091200`（50 GB）                        | 同                                       |
| `limits.cpu_ms`                   | 不支持（保持注释）                            | 取消注释：`5000`                         |
| 能承载的常驻隧道                  | 2–3 台主机                                    | 内含额度约 3 条常驻，之后每条每月约 $1.6 |

心跳 `RELAY_HEARTBEAT_MS` 缺省 `45000`，可改 10–60 秒（改成 `20000` 掉线判定更快，DO 时长约翻倍）。

## secret

只经 `wrangler secret put` 录入，值不进仓库、不进对话、不进日志：

| 名字                      | 必填 | 说明                                                                        |
| ------------------------- | ---- | --------------------------------------------------------------------------- |
| `RELAY_MASTER_KEY`        | 是   | 签名私钥的主密钥，64 位十六进制（`openssl rand -hex 32`）；丢了就要换签名钥 |
| `RELAY_PERSONAL_ACCOUNT`  | 首次 | 首次初始化的账号名；初始化后删掉                                            |
| `RELAY_PERSONAL_PASSWORD` | 首次 | 首次初始化的口令（策略与 Node 中继相同）；初始化后删掉                      |
| `RELAY_METRICS_TOKEN`     |      | `/metrics` 的 Bearer 令牌；不录就不开 `/metrics`                            |
| `RELAY_NODE_TOKEN`        |      | 只有 `RELAY_MODE=saas`：向控制面回报在线（cloud-api §17）的节点令牌         |
| `RELAY_TURNSTILE_SECRET`  |      | 预留给挑战（cloud-api §16）；校验落地前不用录                               |

## 部署前要在 Cloudflare 面板做的事

1. **确认 Workers 套餐**（Workers & Pages → Plans）。Free 就用现在的配置 F；第三台主机起或连续几天 `/metrics` 的 `relay_budget_used` 超过额度七成时升 Paid，按上表改成配置 P，同时在账单里确认**预算告警**已开并设阈值（例如 $15 / 月）。
2. **不要手建 `relay` 与 `relay-staging` 的 DNS 记录**：Custom Domain 在第一次部署时自动建记录、自动签证书。已有同名记录要先删掉，否则部署失败。
3. **Bot Fight Mode 保持关闭**（Security → Bots）：它会挑战原生 App 与 core 的非浏览器请求，WAF 规则绕不过。
4. 可选：Security → WAF → Rate limiting rules 建 1 条区域级规则兜底（按 IP，`/v1/auth/login` 与 `/v1/links/*/accept`，10 秒窗）；Worker 里的闸按节点各自计数，这条是全局的。
5. 可选（CI 自动部署才需要）：建一个只有 Workers Scripts:Edit、Durable Objects:Edit、Workers Routes:Edit 的 API Token，自己放进 GitHub secret `CLOUDFLARE_API_TOKEN`。本机部署用已登录的 `wrangler login` 即可。

## 部署（维护者在本机）

先 staging，验证过再生产。命令都在 `apps/relay-workers/` 下执行：

```sh
pnpm install && pnpm libs:build
node scripts/build.mjs --image ghcr.io/owlbay/armadra-server:<deploy/compat.json 的 web>   # 页面产物；不给就只有自有页面
pnpm exec wrangler whoami                                      # 确认账号 a27d9dab…
pnpm exec wrangler deploy --env staging                        # 首次部署：建 DO 类、Custom Domain、证书
pnpm exec wrangler secret put RELAY_MASTER_KEY --env staging   # 粘贴 openssl rand -hex 32 的输出
pnpm exec wrangler secret put RELAY_PERSONAL_ACCOUNT --env staging
pnpm exec wrangler secret put RELAY_PERSONAL_PASSWORD --env staging
pnpm exec wrangler secret put RELAY_METRICS_TOKEN --env staging   # 可选
curl https://relay-staging.armadra.com/health
curl https://relay-staging.armadra.com/ready                   # 第一次请求触发初始化；{"ok":true}
curl https://relay-staging.armadra.com/.well-known/armadra-platform
pnpm exec wrangler secret delete RELAY_PERSONAL_PASSWORD --env staging   # 初始化成功后删掉
pnpm exec wrangler secret delete RELAY_PERSONAL_ACCOUNT --env staging
pnpm exec wrangler tail --env staging --format pretty          # 看日志（不含路径、头、令牌）
```

验证：桌面「远程访问」登记一个中转账号（地址 `https://relay-staging.armadra.com`），等隧道在线；手机或第二台桌面经它访问；撤销源后连接即时断开。都通过后去掉 `--env staging` 对生产重复一遍。

- 每次 `wrangler deploy` 都会重启全部 DO、断开全部 WS：core 1–60 秒退避重连，客户端会短暂看到 `source_offline`。
- 回滚：`pnpm exec wrangler rollback [--env staging]`（保留 100 个版本）。DO 类只在首版建一次，跨类生命周期的变更不能回滚；schema 只经对象内的迁移账本追加。
- 本地：`pnpm --filter @armadra/relay-workers dev`（`wrangler dev --local`，secret 放 `apps/relay-workers/.dev.vars`，不提交）；`pnpm e2e --mode personal-workers` 起本地 workerd 跑一遍端到端。
