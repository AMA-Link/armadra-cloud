# 文档索引

| 文档                                                                         | 内容                                                      |
| ---------------------------------------------------------------------------- | --------------------------------------------------------- |
| [contracts/cloud-api.md](contracts/cloud-api.md)                             | 控制面 `/v1/*` 与中继客户端面的契约（§N 只追加，不改号）  |
| [design/README.md](design/README.md)                                         | 本仓自己的设计补充                                        |
| [design/saas-design.md](design/saas-design.md)                               | SaaS 能力设计（现行）：个人空间与团队、主机授予、计费     |
| [design/go-relay-design.md](design/go-relay-design.md)                       | 中继 Go 重写（现行）：架构、部署拓扑与分批                |
| [design/saas-m1-implementation.md](design/saas-m1-implementation.md)         | SaaS 控制面 M1 实施设计（G6-M1）：迁移、Go 包、HTTP、分批 |
| [design/relay-on-cloudflare-design.md](design/relay-on-cloudflare-design.md) | 中继上 Cloudflare、防护与成本、Node 中继存储可选          |
| [design/multi-tenant-saas-design.md](design/multi-tenant-saas-design.md)     | 多端入口、多中继、多租户与 SaaS（已被取代，留作追溯）     |
| [design/relay-e2e-only.md](design/relay-e2e-only.md)                         | 中继只转端到端通道（0.5.0）：删掉的明文路径与上线顺序     |
| `runbooks/`                                                                  | 运维手册：部署、轮换密钥、故障处理                        |
| [../deploy/README.md](../deploy/README.md)                                   | 部署：环境变量、compose、滚动升级顺序                     |
