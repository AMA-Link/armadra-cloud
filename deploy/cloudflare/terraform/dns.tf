# DNS（中继设计 §2.11）。relay、relay-staging、edge 是 Worker 的 Custom Domain，由 wrangler 建记录与
# 证书，这里不管。49 服务器上的主机走 DNS only（灰云）：大陆直达，证书由服务器自己签。
# 没给 origin_ipv4 时不建任何 49 的记录（CI 的 validate 与首次 plan 不需要它）。

variable "origin_ipv4" {
  description = "49 服务器的公网 IPv4（cn.relay、app、status 指向它）；null = 不建这些记录。"
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
    "cn.relay" = { proxied = false, comment = "49 的 Node 中继（大陆直达，灰云）" }
    "app"      = { proxied = false, comment = "SaaS 控制面（M1 在 49，灰云）" }
    "status"   = { proxied = var.status_proxied, comment = "状态页（49）" }
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
