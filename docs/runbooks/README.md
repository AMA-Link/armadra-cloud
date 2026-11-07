# 运维手册

- 本地开发环境：见仓库根 [README](../../README.md)。
- 部署与环境变量：见 [deploy/README.md](../../deploy/README.md)。
- 滚动升级顺序：先 relay 后 cloud。
- 个人中转的存储（SQLite / 状态文件、迁移、备份、体检）：见 [personal-storage.md](personal-storage.md)。
- 托管的 `relay.armadra.com`：Go 个人中转，与自建同一套，见 [deploy/README.md](../../deploy/README.md#托管的-relayarmadracom)。
- 已归档（#214，Workers 中继已删除，只作追溯）：[relay-on-cloudflare.md](relay-on-cloudflare.md)（Workers 中继运维）。
- SaaS 控制面与节点（`deploy/saas`：首次部署、节点令牌签发与撤销、升级与回滚、备份、Redis 故障时登录失败关闭、排空与事故开关、Prometheus 告警规则、日志与数据保留）：见 [saas.md](saas.md)。
- Cloudflare 区域规则（Terraform）：见 [deploy/cloudflare/README.md](../../deploy/cloudflare/README.md)；原防护手册 [cloudflare-protection.md](cloudflare-protection.md) 已归档（#214），Terraform 的步骤与回滚仍可参考。
