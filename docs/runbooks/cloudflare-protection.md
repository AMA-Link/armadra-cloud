# Cloudflare 上的防护、预算与拉闸

`armadra.com` 是 Free Website 区域，Workers 中继按 Workers Free 配置（[deploy/cloudflare/README.md](../../deploy/cloudflare/README.md) 的配置 F）。本文讲部署之后怎么设预算告警、开人机挑战、管区域规则、回滚、拉闸，以及被打时怎么处置。日常运维（初始化、心跳、日配额、撤销、排障）见 [relay-on-cloudflare.md](relay-on-cloudflare.md)。

所有步骤都是对外操作，由维护者在本机或面板执行；令牌、secret 不进仓库、不进对话、不进日志。

## 防护分层

| 层              | 在哪                                                                                                     | 管什么                                                                                                                                |
| --------------- | -------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| 区域规则        | [deploy/cloudflare/terraform/](../../deploy/cloudflare/terraform/)                                       | 自定义规则（中继只认常用方法、源地址之外的扫描路径直接拦、可选按国家拦）、Free 托管规则、1 条区域级限速、Bot Fight Mode 关闭          |
| Worker 入口的闸 | `wrangler.jsonc` 的 `ratelimits`                                                                         | 按 IP、按路径的 Rate Limiting 绑定（登录、接受链接、登记、隧道、媒体票、无令牌请求、预检），按 Cloudflare 节点各自计数                |
| 对象里的上限    | `SourceDO` / `ControlDO`                                                                                 | 帧与消息大小、每源 256 条流、每账号 256 条 WS、登录锁定、日配额（超了答 429 `limit_reached`）                                         |
| 人机挑战        | `RELAY_TURNSTILE_SITE_KEY` + `RELAY_TURNSTILE_SECRET`（Node 中继：`--challenge turnstile`）              | 登录要 Turnstile 令牌（契约 [cloud-api §16](../contracts/cloud-api.md)），缺失 `400 challenge_required`、无效 `400 challenge_invalid` |
| 拉闸            | [scripts/kill-switch.mjs](../../scripts/kill-switch.mjs) + 维护页 Worker（`wrangler.maintenance.jsonc`） | 一条命令把中继主机切到只答 503 的维护页，或整个摘掉                                                                                   |

自定义规则与区域级限速在 Worker 之前执行：在那里挡掉的请求不计 Workers 请求数，也不吃日配额。

## 预算告警（升 Workers Paid 之后）

Workers Free 没有付费用量，超了额度是请求直接失败（不是账单），靠日配额自限（`RELAY_DAILY_REQUEST_BUDGET = 80000`）避免整天失败。升 Workers Paid 后按下面设：

1. 面板 Manage Account → Billing → **Budget alerts**：确认已开启（PAYG 账号 2026-06-15 起缺省开），加两档阈值，例如 **$15** 与 **$30 / 月**，收件人填维护者邮箱。预算告警只发邮件、**不封顶**，第二天才生效。
2. Notifications → Add：加「Workers 用量」类通知（单产品指标阈值，Paid 才有），阈值取 Workers Paid 内含额度的 70%（1,000 万请求 / 月 → 700 万）。
3. `wrangler.jsonc` 改成配置 P：`RELAY_ALARM_MS = 60000`、`RELAY_DAILY_REQUEST_BUDGET = 2000000`、取消注释 `limits.cpu_ms = 5000`，先 `--env staging` 部署验证再生产。
4. 记一下当月基线：`/metrics` 的 `relay_budget_used{kind="requests"}` 每天截一次，告警来了能对上是哪天开始涨的。

告警邮件到了先看 [被打时的处置](#被打时的处置)；确认是正常增长就调高阈值，不是就拉闸。

## 人机挑战（Turnstile）

只对登录要求（`RELAY_CHALLENGE_SCOPE = auth.login`）；接受分享链接由链接自带的 128 位秘密与按 IP 限流保护，core 接受链接时没有挑战组件，不要把 `links.accept` 加进去，除非所有访客都从浏览器进来。

Workers 中继：

1. 面板里已建两个 Turnstile 组件（Managed 模式，主机名含 `armadra.com`）：staging 对应 `relay-staging.armadra.com`，生产对应 `relay.armadra.com`。站点密钥不是秘密，已写进 `apps/relay-workers/wrangler.jsonc`（顶层 = 生产，`env.staging` = staging）的 `vars.RELAY_TURNSTILE_SITE_KEY`，同处的 `vars.RELAY_TURNSTILE_HOSTNAMES` 是 siteverify 答复里允许的主机名（逗号分隔；空 = 拒绝一切；https 的 issuer 下不得含 localhost / 127.0.0.1）。部署（`pnpm exec wrangler deploy --env staging`）后还不要求挑战。
2. 录 secret（只有用户本人执行，粘贴到提示里，不进命令行参数、聊天或仓库）：
   - staging：`pnpm exec wrangler secret put RELAY_TURNSTILE_SECRET --env staging`
   - 生产：`pnpm exec wrangler secret put RELAY_TURNSTILE_SECRET`

   录了 secret 才要求挑战；**站点密钥为空时录 secret 是配错**，整个 Worker 答 503 `unavailable`（`wrangler tail` 有「配置不对」），先补站点密钥。

3. 客户端：Turnstile 小组件渲染时必须传 `action: "armadra-login"`（`data-action`）；中继在 siteverify 的答复里核对 `action`，对不上即 `challenge_invalid`（`links.accept` 对应 `armadra-accept`）。
4. 验证：`curl https://relay-staging.armadra.com/.well-known/armadra-platform` 里有 `"challenge":{"provider":"turnstile",…}` 与能力 `challenge.turnstile`；不带令牌登录答 `400 challenge_required`；页面与桌面的中转账号登录框能正常登录。
5. 对生产重复第 2、4 步（去掉 `--env staging`）。关掉挑战：`wrangler secret delete RELAY_TURNSTILE_SECRET`。

服务端校验（Cloudflare existing-widget flow）：令牌缺失、非字符串、空或超过 2048 字符直接拒绝；向 `https://challenges.cloudflare.com/turnstile/v0/siteverify` 以表单 POST `secret`、`response`、`remoteip`，10 秒超时；失败一律关闭（网络错、非 2xx、答复解析失败都答 `400 challenge_invalid`）；要求 `success === true`、`action` 等于入口的 action、`hostname` 在 `RELAY_TURNSTILE_HOSTNAMES` 内。令牌一次性由 Turnstile 保证（重放答 `timeout-or-duplicate`）。

Node 中继（49 的 `cn.relay.armadra.com`，缺省关）：secret 写进文件（0600），`personal serve` 加 `--challenge turnstile --turnstile-site-key <站点密钥> --turnstile-secret-file <文件> --turnstile-hostnames <主机名,…>`（或 `RELAY_CHALLENGE` / `RELAY_TURNSTILE_SITE_KEY` / `RELAY_TURNSTILE_SECRET_FILE` / `RELAY_TURNSTILE_HOSTNAMES`，compose 里用 secret 挂载）。Turnstile 小组件的主机名里要加 `cn.relay.armadra.com`。

挑战先于口令校验，挑战失败不计入登录失败次数；同一个令牌用第二次直接 `challenge_invalid`（本地另留 300 秒防重放表省一次往返）；Turnstile 没答（超时、5xx）同一个幂等键重试一次，仍不行答 `challenge_invalid`，客户端换新令牌重来。日志只记「人机挑战没通过」与原因码，不记令牌。

## 区域规则（Terraform）

1. 面板 My Profile → API Tokens 建一个只给 `armadra.com` 的令牌：Zone:Read、Zone WAF:Edit、DNS:Edit、Bot Management:Edit。放进当前 shell 的 `CLOUDFLARE_API_TOKEN`，不写文件。
2. 本机装 Terraform（≥ 1.6），在 `deploy/cloudflare/terraform/` 下：

   ```sh
   terraform init
   terraform plan -var origin_ipv4=<49 的公网 IPv4>        # 不给 origin_ipv4 就不建 cn.relay / app / status 记录
   terraform apply -var origin_ipv4=<49 的公网 IPv4>
   ```

   第一次 `init` 生成的 `.terraform.lock.hcl` 提交进仓库；state（`terraform.tfstate`）留在本机、不提交，备份到自己的私有位置。要多人共用就换成 R2 的 `s3` 后端（`backend "s3"` 指向 R2 的 S3 端点，`skip_credentials_validation` 等按 R2 文档配）。

3. **已有的入口规则集要先 import**：区域里如果已经在面板里建过自定义规则、托管规则或限速规则，`apply` 会报「这个阶段已有入口规则集」。先查 id 再 import：

   ```sh
   curl -s -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
     "https://api.cloudflare.com/client/v4/zones/<zone_id>/rulesets" | jq '.result[] | {id, phase, kind}'
   terraform import cloudflare_ruleset.custom 'zones/<zone_id>/<id>'          # http_request_firewall_custom
   terraform import 'cloudflare_ruleset.managed[0]' 'zones/<zone_id>/<id>'    # http_request_firewall_managed
   terraform import cloudflare_ruleset.ratelimit 'zones/<zone_id>/<id>'       # http_ratelimit
   terraform import cloudflare_bot_management.this '<zone_id>'
   ```

   `terraform output zone_id` 给出区域 id。import 之后再 `plan`，面板里的旧规则会被 Terraform 的版本替换。

4. 变量（都有缺省）：`zone_plan`（`free` / `pro`；`pro` 时限速改 1 分钟窗、封 10 分钟、按主机收窄）、`enable_paid_managed_waf`（Pro 才能开 Cloudflare Managed + OWASP）、`blocked_countries`、`login_requests_per_period`（缺省每 IP 每 10 秒 10 次）、`manage_ddos_override` 与 `ddos_sensitivity`、`status_proxied`、`enable_saas_wildcard`（SaaS 上线、订了 Advanced Certificate Manager 之后）。

Worker 的 Custom Domain（`relay`、`relay-staging`）由 wrangler 建记录与证书，Terraform 不管，不要在面板或 Terraform 里手建同名记录。

## 回滚

| 要回滚的    | 怎么做                                                                                                                                                         |
| ----------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Worker 代码 | `pnpm exec wrangler rollback [--env staging]`（保留 100 个版本，`wrangler versions list` 查）。DO 数据不随代码变，跨 DO 类生命周期的变更不能回滚               |
| `vars` 改错 | 改回 `wrangler.jsonc` 再部署；或 `wrangler rollback` 到改之前的版本                                                                                            |
| 挑战误伤    | `wrangler secret delete RELAY_TURNSTILE_SECRET [--env staging]`，立即不再要求（Node 中继去掉 `--challenge` 重启）                                              |
| 区域规则    | `git checkout <上一个提交> -- deploy/cloudflare/terraform && terraform apply`；紧急时 `terraform apply -var enable_managed_waf=false` 之类的变量开关先关掉一项 |
| 拉闸之后    | `node scripts/kill-switch.mjs restore --apply`                                                                                                                 |

## 拉闸

前提：维护页 Worker 已经部署过一次（没有路由、平时不收流量）：

```sh
cd apps/relay-workers
pnpm exec wrangler deploy -c wrangler.maintenance.jsonc
```

拉闸与恢复（命令在仓库根下执行，缺省演练，加 `--apply` 才真的执行；令牌要 Workers Scripts:Edit、Workers Routes:Edit）：

```sh
node scripts/kill-switch.mjs status                      # 演练：打印要发的请求
CLOUDFLARE_API_TOKEN=… node scripts/kill-switch.mjs status --apply
CLOUDFLARE_API_TOKEN=… node scripts/kill-switch.mjs maintenance --apply             # 生产切维护页
CLOUDFLARE_API_TOKEN=… node scripts/kill-switch.mjs maintenance --env staging --apply
CLOUDFLARE_API_TOKEN=… node scripts/kill-switch.mjs restore --apply                 # 切回中继
CLOUDFLARE_API_TOKEN=… node scripts/kill-switch.mjs disable --apply                 # 最后手段：摘掉 Custom Domain
```

- `maintenance`：所有请求答 503 `unavailable`（带 `Retry-After: 300`），WS 升级也拒；core 按退避重连，客户端看到服务不可用。维护页 Worker 没有绑定，不计 DO 用量，但仍计 Workers 请求数。
- `disable`：删掉 Custom Domain，主机不再解析，Worker 一个请求都收不到。`restore` 会重新挂上，证书重签要几分钟。
- 切换后 1 分钟内全球生效；用 `curl -i https://relay.armadra.com/health` 确认。
- 维护页 Worker 也挂了时，在面板 Workers & Pages → `armadra-relay` → Settings → Domains & Routes 里手动删掉 Custom Domain，效果同 `disable`。

## 被打时的处置

1. **确认**：面板 Security → Events（被规则挡下的）、Workers & Pages → `armadra-relay` → Metrics（请求数、错误、CPU）、`/metrics` 的 `relay_budget_used`。`wrangler tail --format pretty` 看是哪类请求（日志不含路径与令牌，只有结果与关闭码）。
2. **只是登录被刷**：开挑战（上一节），或把 `login_requests_per_period` 调低后 `terraform apply`。登录锁定（5 次失败锁 1 分钟、翻倍、封顶 15 分钟）本来就在。
3. **扫描或某个地区的大流量**：`blocked_countries` 加上来源地区，或按面板事件里的 ASN / IP 段临时加一条自定义规则（Free 共 5 条，Terraform 已用 2–3 条）。
4. **不要开 I'm Under Attack / Bot Fight Mode**：它们给所有请求加 JS 挑战，原生 App、core 的隧道与 `/v1` 全部失败，等于自己拉闸。
5. **日配额先兜住**：Workers Free 上超了额度是请求直接失败；日配额（`RELAY_DAILY_REQUEST_BUDGET`）让源地址与新隧道先答 429，`/v1` 与已建立的东西尽量照常。必要时临时调低它再部署。
6. **账单失控或扛不住**：`kill-switch.mjs maintenance --apply`；仍有大量请求打到 Worker（例如对方直接打 Custom Domain 的 IP）就 `disable`。
7. **恢复**：攻击停了先 `restore` 到 staging 验证（`--env staging`），再恢复生产；回头把临时规则写进 Terraform 或删掉，Issue 里记一笔（时间线、规则、用量）。

## 演练清单

部署之后、每次改防护设置之后各走一遍（staging 上）：

- [ ] `node scripts/kill-switch.mjs maintenance --env staging`（演练）打印的主机与 Worker 名对；`--apply` 后 `curl -i https://relay-staging.armadra.com/health` 答 503 且带 `Retry-After`；`restore --apply` 后答 200。
- [ ] `terraform plan` 没有意外的增删；面板 Security → WAF 里看得到 `armadra custom rules` 与 `armadra rate limits`；同一 IP 10 秒内连续 11 次登录被区域规则挡下。
- [ ] Bot Fight Mode 是关的（Security → Bots）。
- [ ] 开了挑战时：`platform.info` 报 `challenge`；不带令牌登录 `400 challenge_required`；桌面中转账号登录框、页面 `/app/` 登录正常；删 secret 后不再要求。
- [ ] Workers Paid 时：预算告警的两档阈值与收件人在；Notifications 里有 Workers 用量通知。
- [ ] `wrangler rollback --env staging` 能回到上一版，再部署回来。
