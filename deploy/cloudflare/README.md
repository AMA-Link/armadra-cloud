# armadra.com 的 Cloudflare 区域设置

区域规则（WAF 自定义规则、Free 托管规则、1 条区域级限速兜底、Bot Fight Mode 关闭、控制面 app / status 的 DNS 记录）用 [terraform/](terraform/) 管，不在面板手点。维护者在本机 `terraform init && terraform plan`，核对后再 `terraform apply`；API Token 只走环境变量 `CLOUDFLARE_API_TOKEN`。CI 只做 `terraform fmt -check` 与 `terraform validate`，不连账号。步骤、import 与回滚见[防护手册](../../docs/runbooks/cloudflare-protection.md)（已归档，Terraform 部分仍适用）。

Cloudflare Workers 上的 TypeScript 中继已删除（#214）。`relay.armadra.com` 改由 Go 个人中转承载，部署方式与自建相同，见[部署](../README.md#托管的-relayarmadracom)；它的 DNS 记录灰云直连，不经 Cloudflare，`relay_hosts` 收窄的 WAF 与限速规则对它不生效。切换稳定后由维护者执行 `wrangler delete` 下线旧 Worker（会销毁 Durable Object 里的数据，不可恢复）。

- **Bot Fight Mode 保持关闭**（Security → Bots）：它会挑战原生 App 与 core 的非浏览器请求，WAF 规则绕不过。
- **桌面端网络**：本机代理（如 fake-ip 模式）可能把 `*.armadra.com` 解析成假 IP 而连不上。让 `armadra.com` 走直连并使用正常 DNS。
