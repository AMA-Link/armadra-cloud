# DNS（中继设计 §2.11）。relay、edge 是 Worker 的 Custom Domain，由 wrangler 建记录与
# 证书，这里不管。SaaS 控制面的 app / status 记录是可选的：部署位置待定（Render 或 Cloudflare），缺省不建。
# 没给 origin_ipv4 时不建任何记录（CI 的 validate 与首次 plan 不需要它）。

variable "origin_ipv4" {
  description = "控制面服务器的公网 IPv4（app、status 指向它；部署位置待定）；null = 不建这些记录。"
  type        = string
  default     = null
}

variable "status_proxied" {
  description = "status 主机是否经 Cloudflare 代理（橙云，顺便吃 DDoS 防护）。"
  type        = bool
  default     = true
}

variable "enable_saas_wildcard" {
  description = "SaaS 的源地址通配记录 *.src（橙云，AAAA 100:: 占位）；要先订 Advanced Certificate Manager。"
  type        = bool
  default     = false
}

locals {
  origin_records = var.origin_ipv4 == null ? {} : {
    "app"    = { proxied = false, comment = "SaaS 控制面（灰云）" }
    "status" = { proxied = var.status_proxied, comment = "状态页" }
  }
}

resource "cloudflare_dns_record" "origin" {
  for_each = local.origin_records
  zone_id  = local.zone_id
  name     = "${each.key}.${var.zone_name}"
  type     = "A"
  content  = var.origin_ipv4
  ttl      = 1
  proxied  = each.value.proxied
  comment  = "${each.value.comment}；Terraform 管理"
}

resource "cloudflare_dns_record" "saas_wildcard" {
  count   = var.enable_saas_wildcard ? 1 : 0
  zone_id = local.zone_id
  name    = "*.src.${var.zone_name}"
  type    = "AAAA"
  content = "100::"
  ttl     = 1
  proxied = true
  comment = "SaaS 源地址（Worker Route *.src.armadra.com/*）；Terraform 管理"
}
