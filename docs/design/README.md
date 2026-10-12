# 设计补充

平台的上位设计在 Armadra 仓 `docs/design/platform-saas-architecture.md` 与 `docs/design/platform/`。本目录放本仓自己的补充设计：

| 文档                                                           | 内容                                                                                      |
| -------------------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| [中继上 Cloudflare](relay-on-cloudflare-design.md)             | Workers 中继（SourceDO / ControlDO）、防护与成本、Node 中继存储可选（file / SQLite / PG） |
| [多端入口、多中继、多租户与 SaaS](multi-tenant-saas-design.md) | 选择服务入口、多中继、租户模型与 SaaS 控制面的审查与设计                                  |
| [中继只转端到端通道](relay-e2e-only.md)                        | 0.5.0 删除明文转发、明文 WS 桥、媒体票与预检；中继还能看到什么；上线顺序                  |
