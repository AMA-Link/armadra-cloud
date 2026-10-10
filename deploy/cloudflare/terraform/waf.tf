# WAF：自定义规则（Free 5 条，不支持正则）、托管规则（Free Managed Ruleset；付费的托管 WAF 与 OWASP
# 用 enable_paid_managed_waf 开）、Bot Fight Mode 保持关闭（会挑战原生 App 与 core 的非浏览器请求，
# 自定义规则绕不过）。自定义规则在 Worker 之前执行：在这里挡掉的请求不计 Workers 请求数。

variable "enable_managed_waf" {
  description = "部署 Cloudflare Free Managed Ruleset（所有套餐可用）。区域里已有这个阶段的入口规则集时先 import（见 runbook）。"
  type        = bool
  default     = true
}

variable "enable_paid_managed_waf" {
  description = "部署 Cloudflare Managed Ruleset 与 OWASP Core Ruleset（Pro 及以上）。"
  type        = bool
  default     = false
}

variable "blocked_countries" {
  description = "对中继主机直接拦截的国家 / 地区代码（ISO 3166-1 alpha-2），缺省空。"
  type        = list(string)
  default     = []
}

variable "manage_ddos_override" {
  description = "管理 HTTP DDoS 托管规则的覆盖（Free 只能有 1 条、不带表达式）。"
  type        = bool
  default     = false
}

variable "ddos_sensitivity" {
  description = "HTTP DDoS 托管规则的灵敏度：default / medium / low / eoff。"
  type        = string
  default     = "default"
  validation {
    condition     = contains(["default", "medium", "low", "eoff"], var.ddos_sensitivity)
    error_message = "ddos_sensitivity 只能是 default、medium、low 或 eoff。"
  }
}

locals {
  # 中继只认这些方法（WebSocket 升级是 GET）。
  relay_methods = "{\"GET\" \"HEAD\" \"POST\" \"PUT\" \"PATCH\" \"DELETE\" \"OPTIONS\"}"

  custom_rules = concat(
    [
      {
        ref         = "relay_methods"
        description = "中继：只放行 GET / HEAD / POST / PUT / PATCH / DELETE / OPTIONS"
        expression  = "(http.host in ${local.relay_host_set} and not http.request.method in ${local.relay_methods})"
        action      = "block"
      },
      {
        ref         = "relay_scanners"
        description = "中继：源地址之外的常见扫描路径（.php、wp-*、.env、.git）直接拦截"
        expression  = "(http.host in ${local.relay_host_set} and not http.request.uri.path wildcard \"/s/*\" and (http.request.uri.path wildcard \"*.php\" or http.request.uri.path wildcard \"/wp-*\" or http.request.uri.path wildcard \"/.env*\" or http.request.uri.path wildcard \"/.git*\"))"
        action      = "block"
      },
    ],
    length(var.blocked_countries) == 0 ? [] : [
      {
        ref         = "relay_countries"
        description = "中继：按国家 / 地区拦截（blocked_countries）"
        expression  = "(http.host in ${local.relay_host_set} and ip.src.country in {${join(" ", [for code in var.blocked_countries : "\"${upper(code)}\""])}})"
        action      = "block"
      },
    ],
  )
}

resource "cloudflare_ruleset" "custom" {
  zone_id     = local.zone_id
  name        = "armadra custom rules"
  description = "Armadra 中继的自定义规则（deploy/cloudflare/terraform/waf.tf）"
  kind        = "zone"
  phase       = "http_request_firewall_custom"
  rules = [for rule in local.custom_rules : {
    ref         = rule.ref
    description = rule.description
    expression  = rule.expression
    action      = rule.action
    enabled     = true
  }]
}

resource "cloudflare_ruleset" "managed" {
  count       = var.enable_managed_waf || var.enable_paid_managed_waf ? 1 : 0
  zone_id     = local.zone_id
  name        = "armadra managed rules"
  description = "托管 WAF 规则（deploy/cloudflare/terraform/waf.tf）"
  kind        = "zone"
  phase       = "http_request_firewall_managed"
  rules = concat(
    # 付费的 Cloudflare Managed Ruleset 已经包含 Free 的规则，两者不同时部署。
    var.enable_paid_managed_waf ? [] : [
      {
        ref               = "free_managed"
        description       = "Cloudflare Free Managed Ruleset"
        expression        = "true"
        action            = "execute"
        enabled           = true
        action_parameters = { id = "77454fe2d30c4220b5701f6fdfb893ba" }
      },
    ],
    var.enable_paid_managed_waf ? [
      {
        ref               = "cloudflare_managed"
        description       = "Cloudflare Managed Ruleset（Pro 及以上）"
        expression        = "true"
        action            = "execute"
        enabled           = true
        action_parameters = { id = "efb7b8c949ac4650a09736fc376e9aee" }
      },
      {
        ref               = "owasp_core"
        description       = "Cloudflare OWASP Core Ruleset（Pro 及以上）"
        expression        = "true"
        action            = "execute"
        enabled           = true
        action_parameters = { id = "4814384a9e5d4991b9815dcfc25d2f1f" }
      },
    ] : [],
  )
  lifecycle {
    precondition {
      condition     = !var.enable_paid_managed_waf || local.paid
      error_message = "enable_paid_managed_waf 要 zone_plan = pro（Free 区域没有付费托管规则）。"
    }
  }
}

resource "cloudflare_ruleset" "ddos" {
  count       = var.manage_ddos_override ? 1 : 0
  zone_id     = local.zone_id
  name        = "armadra ddos override"
  description = "HTTP DDoS 托管规则的覆盖（deploy/cloudflare/terraform/waf.tf）"
  kind        = "zone"
  phase       = "ddos_l7"
  rules = [
    {
      ref         = "ddos_l7"
      description = "HTTP DDoS Attack Protection"
      expression  = "true"
      action      = "execute"
      enabled     = true
      action_parameters = {
        id        = "4d21379b4f9f4bb088e0729962c8b3cf"
        overrides = { sensitivity_level = var.ddos_sensitivity }
      }
    },
  ]
}

resource "cloudflare_bot_management" "this" {
  zone_id    = local.zone_id
  fight_mode = false
}
