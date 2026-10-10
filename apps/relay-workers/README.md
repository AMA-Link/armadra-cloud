# @armadra/relay-workers

Cloudflare Workers 上的 Armadra 个人中转（`relay.armadra.com`，staging `relay-staging.armadra.com`）。这里只有 wrangler 项目：[wrangler.jsonc](wrangler.jsonc)、入口 [src/index.ts](src/index.ts)（re-export [`@armadra/relay-runtime-workers`](../../packages/relay-runtime-workers/)）与页面产物的构建脚本；部署步骤见 [deploy/cloudflare/README.md](../../deploy/cloudflare/README.md)，运维见[运维手册](../../docs/runbooks/relay-on-cloudflare.md)。

```sh
pnpm --filter @armadra/relay-workers build -- --web ../Armadra/apps/web/dist   # 页面产物拷进 assets/（或 --image <服务器壳镜像>）
pnpm --filter @armadra/relay-workers dev      # wrangler dev --local：本地 workerd，不连 Cloudflare 账号
pnpm --filter @armadra/relay-workers test     # 按这份 wrangler.jsonc 起本地 workerd 的冒烟测试
pnpm e2e --mode personal-workers              # 本地 workerd 上的端到端
```

本地 `dev` 的 secret 写在 `apps/relay-workers/.dev.vars`（已在 `.gitignore`，不提交），名字与线上相同：

| 名字                      | 说明                                                                           |
| ------------------------- | ------------------------------------------------------------------------------ |
| `RELAY_MASTER_KEY`        | 必填，64 位十六进制                                                            |
| `RELAY_PERSONAL_ACCOUNT`  | 首次初始化的账号名                                                             |
| `RELAY_PERSONAL_PASSWORD` | 首次初始化的口令                                                               |
| `RELAY_METRICS_TOKEN`     | 可选，`/metrics` 的 Bearer 令牌                                                |
| `RELAY_NODE_TOKEN`        | 只有 `RELAY_MODE=saas`：节点回报（cloud-api §17）的令牌                        |
| `RELAY_TURNSTILE_SECRET`  | 可选，人机挑战（cloud-api §16）；录了才要求，要先配 `RELAY_TURNSTILE_SITE_KEY` |

维护页 Worker（拉闸用，[docs/runbooks/cloudflare-protection.md](../../docs/runbooks/cloudflare-protection.md)）在 [wrangler.maintenance.jsonc](wrangler.maintenance.jsonc)，入口 [src/maintenance.ts](src/maintenance.ts)。

本地起的时候把 `RELAY_ISSUER` 换成本机地址：`pnpm --filter @armadra/relay-workers dev -- --var RELAY_ISSUER:http://127.0.0.1:8787`。
