package personal

// Usage 是顶层用法。
const Usage = `用法: armadra-relay <命令> [选项]

  personal init|serve|status|passwd|rotate-key|links|traffic|migrate|backup|doctor
                                                       单人中转；缺省不带外部数据库，多节点可选 PostgreSQL / Redis
  dev --jwks F --issuer ISS [--listen H:P]             开发装配：只有数据面，内存控制面，回环明文监听
  health                                               探活本机 /health（镜像的 HEALTHCHECK 用）
  version                                              打印版本

personal --help 列出 personal 的子命令与选项。`

// PersonalUsage 是 personal 子命令的用法。
const PersonalUsage = `用法: armadra-relay personal <子命令> [选项]

  init         建账号与签名钥：--data-dir --account [--password-file F] --host [--port] [--tls] [--store S] [--database-url URL]
  serve        起个人中转
  status       打印存储、账号、源、链接与 CA 指纹：--data-dir
  passwd       改口令并让账号的全部设备重新登录：--data-dir [--password-file F]
  rotate-key   启用新签名钥，旧钥一小时后退役：--data-dir
  links        list | revoke <linkId>：--data-dir
  traffic      按源、账号、设备与类别合计中继流量：--data-dir [--since 7d] [--source <id>]
  migrate      换存储（先停 serve），原文件改名留作回退：--data-dir --to sqlite|postgres [--database-url URL]
  backup       在线备份存储到一个新文件：--data-dir --out F
  doctor       检查权限、存储完整性、监听地址、受信代理与口令：--data-dir，其余同 serve

数据目录用 PostgreSQL 时，每个子命令都要 --database-url（或 RELAY_DATABASE_URL / RELAY_DATABASE_URL_FILE）。

serve:
  --data-dir DIR           数据目录（或 RELAY_DATA_DIR）
  --store S                sqlite | file | postgres（或 RELAY_STORE）；默认按数据目录里已有的，新装 sqlite（file 只能用 migrate 导入）
  --database-url URL       PostgreSQL 连接串（或 RELAY_DATABASE_URL）；--store postgres 与 --control postgres 用
  --database-url-file F    从文件读连接串（或 RELAY_DATABASE_URL_FILE），口令不出现在进程参数里
  --database-schema S      用哪个 schema（或 RELAY_DATABASE_SCHEMA），默认连接的缺省 schema
  --control C              memory | redis | postgres（或 RELAY_CONTROL），默认 memory；多节点选 redis 或 postgres
  --redis-url URL          Redis 地址（或 RELAY_REDIS_URL），--control redis 用
  --node-id ID             本节点的名字（或 RELAY_NODE_ID），多节点时各不相同，默认 --host
  --host HOST              对外的 IP 或域名（或 RELAY_HOST）
  --port PORT              端口，默认 8443
  --public-origin URL      对外来源（或 RELAY_PUBLIC_ORIGIN），默认 https://<host>:<port>
  --tls MODE               self-signed | acme | file | plain，默认 self-signed；plain 只允许回环监听
  --tls-cert F --tls-key F 证书文件（--tls file；或 RELAY_TLS_CERT / RELAY_TLS_KEY），变化时热加载
  --acme-email E           ACME 账号邮箱（--tls acme）
  --acme-directory URL     ACME 目录，默认 Let's Encrypt
  --acme-ca-bundle F       信任 ACME 目录服务器的 PEM（测试 CA）
  --acme-http-listen H:P   http-01 挑战监听，默认 0.0.0.0:80
  --web-root DIR           Armadra apps/web 的构建产物（或 RELAY_WEB_ROOT），托管在 /app 与 /j
  --account NAME           首次启动自动初始化的账号（或 RELAY_PERSONAL_ACCOUNT）
  --password-file F        首次启动自动初始化的口令文件（或 RELAY_PERSONAL_PASSWORD_FILE）
  --listen HOST:PORT       监听地址（或 RELAY_LISTEN），默认回环 --host 用 127.0.0.1，否则 0.0.0.0
  --log-level LEVEL        debug | info | warn | error，默认 info
  --metrics-listen H:P     指标监听（或 RELAY_METRICS_LISTEN），默认不开；RELAY_METRICS_TOKEN 设访问令牌
  --limit-anonymous N      无令牌或令牌无效的请求按 IP 每分钟上限（或 RELAY_LIMIT_ANONYMOUS），默认 60
  --limit-preflight N      0.5.0 起不再使用（或 RELAY_LIMIT_PREFLIGHT）
  --limit-token N          有效令牌每分钟上限（或 RELAY_LIMIT_TOKEN），默认 1200
  --limit-account N        同一账号全部令牌每分钟上限（或 RELAY_LIMIT_ACCOUNT），默认 3600
  --trusted-proxy LIST     受信反向代理的 IP / CIDR，逗号分隔（或 RELAY_TRUSTED_PROXY），如 127.0.0.1/32,::1/128；
                           直连对端在列表内才读 X-Forwarded-For / X-Real-IP 取客户端 IP，默认空（不信任任何代理）
  --trusted-proxy-prefer H 先读哪个头：forwarded-for（默认）| real-ip（或 RELAY_TRUSTED_PROXY_PREFER）
  --name NAME              服务端名称（或 RELAY_NAME），客户端添加时的缺省显示名；默认取状态文件 settings.displayName，再取 --host
  --challenge P            人机挑战：turnstile | off（或 RELAY_CHALLENGE），默认 off
  --turnstile-site-key K   Turnstile 站点密钥（或 RELAY_TURNSTILE_SITE_KEY），不是秘密
  --turnstile-secret-file F Turnstile secret 文件（或 RELAY_TURNSTILE_SECRET_FILE）
  --turnstile-hostnames L  允许的 siteverify 主机名，逗号分隔（或 RELAY_TURNSTILE_HOSTNAMES）；空 = 拒绝一切；https 公网地址下不得含本机
  --challenge-scope LIST   要挑战的 procedure：auth.login、links.accept、auth.changePassword，逗号分隔（或 RELAY_CHALLENGE_SCOPE），默认 auth.login
  --traffic-retention-days N 流量统计保留天数（或 RELAY_TRAFFIC_RETENTION_DAYS），默认 90，0 = 不记录`
