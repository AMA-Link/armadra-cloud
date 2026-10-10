# 运维手册

- 本地开发环境：见仓库根 [README](../../README.md)。
- 部署与环境变量：见 [deploy/README.md](../../deploy/README.md)。
- 滚动升级顺序：先 relay 后 cloud。
- 个人中转的存储（SQLite / 状态文件、迁移、备份、体检）：见 [personal-storage.md](personal-storage.md)。
- Cloudflare Workers 上的个人中转（初始化、心跳、日配额、撤销、存储拒绝服务、回滚、排障）：见 [relay-on-cloudflare.md](relay-on-cloudflare.md)；部署步骤见 [deploy/cloudflare/README.md](../../deploy/cloudflare/README.md)。
