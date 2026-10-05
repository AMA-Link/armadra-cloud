# 部署

| 文件                                         | 用途                                              |
| -------------------------------------------- | ------------------------------------------------- |
| [compose.yml](compose.yml)                   | 参考部署：cloud + relay + PostgreSQL 17 + Redis 8 |
| [personal/compose.yml](personal/compose.yml) | 单人中转，一个服务、不带数据库                    |
| [dev/compose.yml](dev/compose.yml)           | 本地开发依赖（`pnpm dev:up`）                     |
| [compat.json](compat.json)                   | 验证过的 Armadra 服务器镜像区间与协议版本         |

## 环境变量

控制面（`armadra-cloud serve`）：

| 变量                           | 必填 | 说明                                       |
| ------------------------------ | ---- | ------------------------------------------ |
| `ARMADRA_CLOUD_ISSUER`         | 是   | 对外 issuer URL（JWT `iss`）               |
| `ARMADRA_CLOUD_APP_ORIGIN`     | 是   | 页面来源（Cookie 与 CSP）                  |
| `ARMADRA_CLOUD_LISTEN`         |      | 缺省 `0.0.0.0:8100`                        |
| `ARMADRA_CLOUD_DATABASE_URL`   | 是   | `postgres://…`                             |
| `ARMADRA_CLOUD_REDIS_URL`      | 是   | `redis://…`                                |
| `ARMADRA_CLOUD_MASTER_KEY`     |      | 32 字节 hex（账号功能落地后必填）          |
| `ARMADRA_CLOUD_RELAY_DOMAIN`   |      | `<sourceId>.src.<域>` 的 `<域>`            |
| `ARMADRA_CLOUD_MIGRATIONS_DIR` |      | 镜像里是 `/app/migrations`                 |
| `ARMADRA_CLOUD_LOG_LEVEL`      |      | `debug` / `info`（缺省）/ `warn` / `error` |

中继（`armadra-relay saas serve`）：

| 变量                          | 必填 | 说明                             |
| ----------------------------- | ---- | -------------------------------- |
| `RELAY_LISTEN`                |      | 缺省 `0.0.0.0:8443`              |
| `RELAY_NODE_ID`               |      | 节点名，缺省取主机名             |
| `RELAY_DOMAIN`                | 是   | `<sourceId>.src.<域>` 的 `<域>`  |
| `RELAY_CLOUD_ISSUER`          | 是   | 控制面 issuer（JWKS 从这里拉）   |
| `RELAY_REDIS_URL`             | 是   | `redis://…`                      |
| `RELAY_ALLOW_INSECURE_TUNNEL` |      | `1` 允许明文隧道，只用于本地开发 |
| `RELAY_LOG_LEVEL`             |      | 缺省 `info`                      |

中继（`armadra-relay personal serve`，参数优先，其次环境变量）：

| 参数 / 变量                                 | 必填 | 说明                                                |
| ------------------------------------------- | ---- | --------------------------------------------------- |
| `--data-dir` / `RELAY_DATA_DIR`             | 是   | 数据目录（权限 0700）                               |
| `--host` / `RELAY_HOST`                     | 是   | 对外的 IP 或域名                                    |
| `--port`                                    |      | 缺省 8443                                           |
| `--tls` / `RELAY_TLS`                       |      | 缺省 `self-signed`；现阶段只支持 `plain`            |
| `--listen` / `RELAY_LISTEN`                 |      | 缺省：回环 host 绑 `127.0.0.1`，否则 `0.0.0.0`      |
| `--metrics-listen` / `RELAY_METRICS_LISTEN` |      | `/metrics` 的单独监听（只绑内网地址），缺省不开     |
| `RELAY_METRICS_TOKEN`                       |      | 设了则 `/metrics` 要 `Authorization: Bearer <令牌>` |

## 端点

- `GET /health`：进程存活，200 即活着（镜像的 `HEALTHCHECK` 用它）。
- `GET /ready`：依赖就绪（cloud 报 PostgreSQL 与 Redis，saas relay 报 Redis，personal relay 报数据目录），任一不可达答 503。
- 控制面的 `/v1/*` 与 `/.well-known/*` 业务路由现在是预留，答 501 `{ code: "not_implemented", message, requestId }`。

## 升级顺序

先升 relay、后升 cloud；数据库迁移由 cloud 执行，relay 不碰数据库。
