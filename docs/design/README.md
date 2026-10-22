# 设计补充

SaaS 与中继的现行设计是下表前两份（2026-10-22 定稿）；Armadra 仓 `docs/design/platform-saas-architecture.md` 与 `docs/design/platform/` 的 SaaS 部分已被它们取代，只留作追溯。

| 文档                                                           | 内容                                                                                      |
| -------------------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| [SaaS 能力设计](saas-design.md)                                | 现行：个人空间与团队、角色、能力清单、数据模型、协议 1.4、分期，以及合并自旧方案的细节    |
| [中继 Go 重写](go-relay-design.md)                             | 现行：Go 数据面、个人中转、SaaS 节点与控制面的架构、部署拓扑与分批                        |
| [中继上 Cloudflare](relay-on-cloudflare-design.md)             | Workers 中继（SourceDO / ControlDO）、防护与成本、Node 中继存储可选（file / SQLite / PG） |
| [多端入口、多中继、多租户与 SaaS](multi-tenant-saas-design.md) | 已被前两份取代，只留作追溯                                                                |
| [中继只转端到端通道](relay-e2e-only.md)                        | 0.5.0 删除明文转发、明文 WS 桥、媒体票与预检；中继还能看到什么；上线顺序                  |
