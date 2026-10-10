# Cloudflare Workers 上的个人中转

`relay.armadra.com` 是唯一的环境（没有 staging，49 服务器不承载任何 Armadra 服务）：wrangler 项目在 [apps/relay-workers](../../apps/relay-workers/)（[wrangler.jsonc](../../apps/relay-workers/wrangler.jsonc)），运行时在 [packages/relay-runtime-workers](../../packages/relay-runtime-workers/)。线上形状与 Node 中继相同（契约 [cloud-api §7–§12、§17](../../docs/contracts/cloud-api.md)），唯一差别是隧道节点地址带 `?src=<sourceId>`。

部署、录 secret、改 DNS 都是对外操作，由维护者在本机执行；仓库里的脚本与测试只在本地 workerd 上跑，不连 Cloudflare 账号。日常运维（初始化、心跳、配额、回滚、排障）见[运维手册](../../docs/runbooks/relay-on-cloudflare.md)；区域规则、人机挑战、预算告警与拉闸见[防护手册](../../docs/runbooks/cloudflare-protection.md)。

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

| 名字                      | 必填 | 说明                                                                                               |
| ------------------------- | ---- | -------------------------------------------------------------------------------------------------- |
| `RELAY_MASTER_KEY`        | 是   | 签名私钥的主密钥，64 位十六进制（`openssl rand -hex 32`）；丢了就要换签名钥                        |
| `RELAY_PERSONAL_ACCOUNT`  | 首次 | 首次初始化的账号名；初始化后删掉                                                                   |
| `RELAY_PERSONAL_PASSWORD` | 首次 | 首次初始化的口令（策略与 Node 中继相同）；初始化后删掉                                             |
| `RELAY_METRICS_TOKEN`     |      | `/metrics` 的 Bearer 令牌；不录就不开 `/metrics`                                                   |
| `RELAY_NODE_TOKEN`        |      | 只有 `RELAY_MODE=saas`：向控制面回报在线（cloud-api §17）的节点令牌                                |
| `RELAY_TURNSTILE_SECRET`  |      | 人机挑战（cloud-api §16）：录了才要求登录带 Turnstile 令牌，先配好 `vars.RELAY_TURNSTILE_SITE_KEY` |

## 部署前要在 Cloudflare 面板做的事

1. **确认 Workers 套餐**（Workers & Pages → Plans）。Free 就用现在的配置 F；第三台主机起或连续几天 `/metrics` 的 `relay_budget_used` 超过额度七成时升 Paid，按上表改成配置 P，同时在账单里确认**预算告警**已开并设阈值（例如 $15 / 月）。
2. **不要手建 `relay` 的 DNS 记录**：Custom Domain 在第一次部署时自动建记录、自动签证书。已有同名记录要先删掉，否则部署失败。
3. **Bot Fight Mode 保持关闭**（Security → Bots）：它会挑战原生 App 与 core 的非浏览器请求，WAF 规则绕不过。
4. 区域规则（自定义规则、Free 托管规则、1 条区域级限速兜底、Bot Fight Mode 关闭、控制面 app / status 的 DNS 记录）用 [terraform/](terraform/) 管，不在面板手点；步骤、import 与回滚见[防护手册](../../docs/runbooks/cloudflare-protection.md)。Worker 里的闸按节点各自计数，区域级限速是全局的。
5. 拉闸用的维护页 Worker 预先部署一次：`pnpm exec wrangler deploy -c wrangler.maintenance.jsonc`（没有路由，平时不收流量）；`node scripts/kill-switch.mjs` 切换，缺省只演练。
6. 可选（CI 自动部署才需要）：建一个只有 Workers Scripts:Edit、Durable Objects:Edit、Workers Routes:Edit 的 API Token，自己放进 GitHub secret `CLOUDFLARE_API_TOKEN`。本机部署用已登录的 `wrangler login` 即可。

## 部署（维护者在本机）

只有生产。首次部署与初始化用一次性脚本 [bootstrap-production.sh](../../apps/relay-workers/scripts/bootstrap-production.sh)，**由用户在本机终端运行**：它读取 Turnstile secret 并校验、生成 `RELAY_MASTER_KEY` 与登录口令、随首次部署上传、初始化、再删掉首次专用 secret；登录账号与口令只在你的终端显示一次，先存进密码管理器再回车继续。本机代理使用假 IP 解析时新域名可能连不上，脚本里的探测走 DoH（`curl --doh-url`），手动检查也要同样处理。命令在 `apps/relay-workers/` 下执行：

```sh
pnpm install && pnpm libs:build
node scripts/build.mjs --image ghcr.io/owlbay/armadra-server:<deploy/compat.json 的 web>   # 页面产物；不给就只有自有页面
pnpm exec wrangler whoami                                      # 确认账号 a27d9dab…
bash scripts/bootstrap-production.sh                           # 首次部署 + 初始化（一次性）
curl -s --doh-url https://cloudflare-dns.com/dns-query https://relay.armadra.com/health
curl -s --doh-url https://cloudflare-dns.com/dns-query https://relay.armadra.com/.well-known/armadra-platform
pnpm exec wrangler tail --format pretty                        # 看日志（不含路径、头、令牌）
```

之后的代码更新只需 `pnpm exec wrangler deploy`。验证：桌面「远程访问」登记一个中转账号（地址 `https://relay.armadra.com`），等隧道在线；手机或第二台桌面经它访问；撤销源后连接即时断开。

**桌面端网络**：本机代理（如 Clash 的 fake-ip 模式）可能把 `relay.armadra.com` 解析成假 IP 而连不上。让 `armadra.com` 走直连并使用正常 DNS（代理规则里加 `DOMAIN-SUFFIX,armadra.com,DIRECT`，并把它从 fake-ip 过滤里排除）。

- 每次 `wrangler deploy` 都会重启全部 DO、断开全部 WS：core 1–60 秒退避重连，客户端会短暂看到 `source_offline`。
- 回滚：`pnpm exec wrangler rollback`（保留 100 个版本）。DO 类只在首版建一次，跨类生命周期的变更不能回滚；schema 只经对象内的迁移账本追加。
- 本地：`pnpm --filter @armadra/relay-workers dev`（`wrangler dev --local`，secret 放 `apps/relay-workers/.dev.vars`，不提交）；`pnpm e2e --mode personal-workers` 起本地 workerd 跑一遍端到端。
