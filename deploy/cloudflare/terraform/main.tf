# armadra.com 的区域设置（中继设计 §4.5）：WAF 自定义规则、托管规则、区域级限速、Bot 开关与 DNS。
#
# - 维护者在本机执行：`terraform init && terraform plan`，核对后再 `terraform apply`；CI 只做
#   `terraform fmt -check` 与 `terraform validate`（不连账号）。步骤与回滚见
#   docs/runbooks/cloudflare-protection.md。
# - API Token 只走环境变量 CLOUDFLARE_API_TOKEN（Zone:Read、Zone WAF:Edit、DNS:Edit、
#   Bot Management:Edit），不写进任何文件。
# - zone 是 Free Website：缺省只写 Free 能用的规则；付费能力（托管 WAF、OWASP、更长的限速窗口）
#   用变量开关，升级后再打开。
# - Worker 的 Custom Domain（relay、relay-staging）由 wrangler 建与管，这里不碰，免得两边抢同一条记录。
# - state 缺省在本目录（已在 .gitignore）；要多人共用时换成 R2 的 s3 后端（见 runbook）。

terraform {
  required_version = ">= 1.6.0"
  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "5.27.0"
    }
  }
}

provider "cloudflare" {}

variable "account_id" {
  description = "Cloudflare 账号 id（不是秘密）。"
  type        = string
  default     = "a27d9dab9190150768845b62cfd57cfe"
}

variable "zone_name" {
  description = "区域名。"
  type        = string
  default     = "armadra.com"
}

variable "zone_plan" {
  description = "区域套餐：free（缺省）或 pro。决定区域级限速的窗口与封禁时长、能不能开付费托管规则。"
  type        = string
  default     = "free"
  validation {
    condition     = contains(["free", "pro"], var.zone_plan)
    error_message = "zone_plan 只能是 free 或 pro。"
  }
}

variable "relay_hosts" {
  description = "Workers 中继的主机名（Custom Domain，由 wrangler 管）；WAF 规则按它们收窄。"
  type        = list(string)
  default     = ["relay.armadra.com", "relay-staging.armadra.com"]
}

data "cloudflare_zone" "this" {
  filter = {
    name    = var.zone_name
    account = { id = var.account_id }
  }
}

# relay_host_set 是规则表达式里的主机集合：{"relay.armadra.com" "relay-staging.armadra.com"}。
locals {
  zone_id        = data.cloudflare_zone.this.id
  relay_host_set = "{${join(" ", [for host in var.relay_hosts : "\"${host}\""])}}"
  paid           = var.zone_plan != "free"
}

output "zone_id" {
  description = "解析到的区域 id（kill-switch 与排障用）。"
  value       = local.zone_id
}
