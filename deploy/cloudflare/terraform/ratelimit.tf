# 区域级限速（全局计数，兜底 Worker 里按节点各自计数的 Rate Limiting 绑定）。
#
# Free 只有 1 条：表达式只能用路径（不能按主机），按 IP（+ 节点）计数，10 秒窗、封 10 秒。登录与接受
# 分享链接合写一条（中继设计 §4.6）。Pro 改成 1 分钟窗、封 10 分钟，并按主机收窄。
# 只有经 Cloudflare 代理的主机吃这条规则；灰云的 app 不受影响。

variable "login_requests_per_period" {
  description = "每个 IP 在一个窗口内的登录 + 接受链接请求上限（Free 窗口 10 秒，Pro 60 秒）。"
  type        = number
  default     = 10
}

locals {
  login_paths_free = "(http.request.uri.path eq \"/v1/auth/login\" or http.request.uri.path wildcard \"/v1/links/*/accept\")"
  login_paths_paid = "(http.host in ${local.relay_host_set} and ${local.login_paths_free})"
}

resource "cloudflare_ruleset" "ratelimit" {
  zone_id     = local.zone_id
  name        = "armadra rate limits"
  description = "登录与接受分享链接的区域级限速（deploy/cloudflare/terraform/ratelimit.tf）"
  kind        = "zone"
  phase       = "http_ratelimit"
  rules = [
    {
      ref         = "login_and_accept"
      description = "登录与接受分享链接：按 IP 限速"
      expression  = local.paid ? local.login_paths_paid : local.login_paths_free
      action      = "block"
      enabled     = true
      ratelimit = {
        characteristics     = ["cf.colo.id", "ip.src"]
        period              = local.paid ? 60 : 10
        requests_per_period = var.login_requests_per_period
        mitigation_timeout  = local.paid ? 600 : 10
      }
    },
  ]
}
