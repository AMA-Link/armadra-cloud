# 部署

| 文件                                         | 用途                                               |
| -------------------------------------------- | -------------------------------------------------- |
| [compose.yml](compose.yml)                   | 参考部署：cloud + relay + PostgreSQL 17 + Redis 8  |
| [personal/compose.yml](personal/compose.yml) | 单人中转，一个服务、不带数据库                     |
| [dev/compose.yml](dev/compose.yml)           | 本地开发依赖（`pnpm dev:up`）                      |
| [e2e/compose.yml](e2e/compose.yml)           | 跨仓端到端（`pnpm e2e --mode personal`，脚本驱动） |
| [compat.json](compat.json)                   | 验证过的 Armadra 服务器镜像区间与协议版本          |

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

| 参数 / 变量                                                   | 必填 | 说明                                                                     |
| ------------------------------------------------------------- | ---- | ------------------------------------------------------------------------ |
| `--data-dir` / `RELAY_DATA_DIR`                               | 是   | 数据目录（0700）：`state.json`、`tls/`、`acme/`                          |
| `--host` / `RELAY_HOST`                                       | 是   | 对外的 IP 或域名（issuer 与证书 SAN）                                    |
| `--port` / `RELAY_PORT`                                       |      | 对外端口，缺省 8443（443 时 issuer 省略端口）                            |
| `--public-origin` / `RELAY_PUBLIC_ORIGIN`                     |      | 对外来源，缺省 `https://<host>:<port>`                                   |
| `--tls` / `RELAY_TLS`                                         |      | `self-signed`（缺省）/ `acme` / `file` / `plain`（只允许回环监听）       |
| `--tls-cert`、`--tls-key` / `RELAY_TLS_CERT`、`RELAY_TLS_KEY` |      | `--tls file` 的证书与私钥，变化时热加载                                  |
| `--acme-email` / `RELAY_ACME_EMAIL`                           |      | `--tls acme` 必填                                                        |
| `--acme-directory` / `RELAY_ACME_DIRECTORY`                   |      | 缺省 Let's Encrypt 生产目录                                              |
| `--acme-ca-bundle` / `RELAY_ACME_CA_BUNDLE`                   |      | 信任 ACME 目录服务器的 PEM（测试 CA）                                    |
| `--acme-http-listen` / `RELAY_ACME_HTTP_LISTEN`               |      | `http-01` 挑战监听，缺省 `0.0.0.0:80`                                    |
| `--web-root` / `RELAY_WEB_ROOT`                               |      | Armadra `apps/web` 构建产物；镜像缺省 `/app/web`（构建参数 `WEB_IMAGE`） |
| `--account` / `RELAY_PERSONAL_ACCOUNT`                        |      | 状态文件不存在时自动初始化的账号                                         |
| `--password-file` / `RELAY_PERSONAL_PASSWORD_FILE`            |      | 自动初始化的口令文件（之后不再读取）                                     |
| `--listen` / `RELAY_LISTEN`                                   |      | 缺省：回环 host 绑 `127.0.0.1`，否则 `0.0.0.0`                           |
| `--metrics-listen` / `RELAY_METRICS_LISTEN`                   |      | `/metrics` 的单独监听（只绑内网地址），缺省不开                          |
| `RELAY_METRICS_TOKEN`                                         |      | 设了则 `/metrics` 要 `Authorization: Bearer <令牌>`                      |
| `--limit-anonymous` / `RELAY_LIMIT_ANONYMOUS`                 |      | 无令牌或令牌无效的请求，按 IP 每分钟上限，缺省 60                        |
| `--limit-preflight` / `RELAY_LIMIT_PREFLIGHT`                 |      | CORS 预检，按 IP 每分钟上限（单独的桶），缺省 1200                       |
| `--limit-token` / `RELAY_LIMIT_TOKEN`                         |      | 有效中继令牌每分钟上限，缺省 1200                                        |
| `--limit-account` / `RELAY_LIMIT_ACCOUNT`                     |      | 同一账号全部令牌每分钟上限，缺省 3600                                    |

限流额度也可写在状态文件 `settings.rateLimits`（`anonymous`、`preflight`、`token`、`account`，都是 1–1000000 的整数，可省略），优先级：命令行 > 环境变量 > 状态文件 > 缺省；改动后重启 `serve` 生效。登录、链接接受与隧道登记的限流不可配。

其余子命令（`init`、`status`、`passwd`、`rotate-key`、`links list|revoke`）只读写状态文件，可以在 `serve` 运行时执行（`docker compose exec relay node /app/out/cli.js personal …`），改动一秒内生效。

## 端点

- `GET /health`：进程存活，200 即活着（镜像的 `HEALTHCHECK` 经 `armadra-relay health` 打它，先 HTTPS 再明文）。
- `GET /ready`：依赖就绪（cloud 报 PostgreSQL 与 Redis，saas relay 报 Redis，personal relay 报数据目录），任一不可达答 503。
- 控制面的 `/v1/*` 与 `/.well-known/*` 业务路由现在是预留，答 501 `{ code: "not_implemented", message, requestId }`。
- 个人中转另有 `/v1/*` 子集、`/.well-known/*`、`/ca.crt`、`/app/*` 与 `/j/<linkId>`（[契约](../docs/contracts/cloud-api.md) §10–§11）。

## 升级顺序

先升 relay、后升 cloud；数据库迁移由 cloud 执行，relay 不碰数据库。
