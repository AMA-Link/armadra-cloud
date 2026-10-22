# 个人中转的存储：SQLite、状态文件与 PostgreSQL

`armadra-relay personal` 的全部状态（账号、口令哈希、签名钥、源 / 设备 / 链接登记、设置）三选一：

| `--store`  | 位置                                     | 说明                                                        |
| ---------- | ---------------------------------------- | ----------------------------------------------------------- |
| `sqlite`   | `relay.db`（WAL，0600）                  | 新装缺省。`serve` 与 CLI 同时打开同一个库，写在事务里       |
| `file`     | `state.json`（0600）                     | 0.3.x 之前的部署。Go 版（0.10.0 起）只读不写：先迁到 SQLite |
| `postgres` | PostgreSQL（数据目录留 `postgres.json`） | 多节点，显式选。几个节点共用一个库；不选就不加载驱动        |

都不存消息内容、令牌明文或口令。私钥以 PKCS#8 存在库或文件里，靠文件权限 0600 与数据目录 0700（PostgreSQL 靠库的访问控制）保护。

## 怎么选

- 新装：什么都不用写，`init` 或 `serve` 的首次自动初始化建 `relay.db`。
- 已有 `state.json`：Go 版的 `serve` 拒绝启动并提示 `personal migrate --to sqlite`；升级前按下面的步骤迁过去（Node 版 0.9.x 照旧能用它）。
- 两种文件都在、`--store` 与目录里已有的不符、库认不出（别的程序的 SQLite、空文件）、库校验不过、迁移账本对不上、库是更新的版本写的：一律拒绝启动，不自动清库或重建。按提示处理，不要删文件碰运气。

SQLite 模式下没有可手改的 JSON：限流额度、受信代理、服务端名称用 `serve` 的参数或环境变量（`--limit-*`、`--trusted-proxy`、`--name`）给，它们本来就优先于存储里的值。

## 从状态文件迁到 SQLite

1. 停服：`docker compose -f deploy/personal/compose.yml stop relay`（源码运行时结束 `serve` 进程）。`serve` 在运行时迁移会被拒绝（看 `<data-dir>/serve.pid`）。
2. 先备份：`personal backup --data-dir /data --out /data/state.backup.json`。
3. 迁移：

   ```sh
   docker compose -f deploy/personal/compose.yml run --rm relay personal migrate --data-dir /data --to sqlite
   ```

   在目录锁内读 `state.json`、建临时库、一个事务写入全部行、读回来逐字段比对（修订号也一致），通过后改名为 `relay.db`，原文件改名为 `state.json.migrated-<时间>` 留作回退。任何一步失败都删掉临时库，原文件不动。

4. 起服：`docker compose -f deploy/personal/compose.yml up -d relay`，`personal status` 第一行是 `存储：SQLite`。已登记的源、设备与链接不受影响，客户端不需要重新登录。

## 回退到状态文件

只有 Node 版（0.9.x 及以前的镜像）能写状态文件，Go 版拒绝 `--to file`。用 Node 版镜像停服后 `personal migrate --data-dir /data --to file`：从库导出 schema v1 的 `state.json`（读回来比对），原库改名为 `relay.db.migrated-<时间>`。也可以直接把上一步留下的 `state.json.migrated-<时间>` 改回 `state.json`，但会丢掉迁移之后的改动。

## 备份与恢复

- 在线备份：`personal backup --data-dir /data --out /data/backup-$(date +%F).db`。SQLite 用 `VACUUM INTO`，`serve` 运行时也一致；状态文件是拷一份。备份文件 0600，目标已存在就拒绝。
- 恢复：停服，把备份文件放回数据目录改名为 `relay.db`（状态文件则为 `state.json`），确认目录里没有另一种存储文件，起服。
- 也可以停服后 `tar` 整个数据目录（连同 `tls/`、`acme/`）。

## 多节点：PostgreSQL 存储与控制面

单节点什么都不用加（`--control memory` 是缺省）。几个 `serve` 共用一份数据时：

- 存储：`--store postgres`，连接串给 `--database-url`、`RELAY_DATABASE_URL`，或从文件读（`--database-url-file` / `RELAY_DATABASE_URL_FILE`，口令不出现在进程参数里）；`--database-schema` 选 schema（缺省连接的缺省 schema，给了不存在的就建一个空的）。打开时自动迁移（`packages/control-store-postgres/migrations/`，账本 `cloud_migrations`，与 `armadra-cloud migrate` 同一份）。
- 控制面：`--control postgres`（同一个库：路由 / 撤销 / 限流表 + `LISTEN/NOTIFY`）或 `--control redis --redis-url redis://…`。路由互见、在线事件与关隧道广播到各节点，撤销与速率限流各节点共用一个桶；并发限流按节点。
- 每个节点 `--node-id` 各不相同（缺省 `--host`），数据目录各自一个（证书、ACME 账号与 `postgres.json` 标记在里面）。

第一个节点给了 `--account` 与口令文件就自动初始化；之后的节点看到库里已有个人中转直接打开。`passwd` / `rotate-key` / `links revoke` 在任一节点执行，所有节点一秒内察觉（看租户修订号）并断开撤销掉的连接。

库里只能有个人中转那一个租户；连不上、库不认识（没有账本却已有表）、账本对不上、库比程序新：一律拒绝启动，不自动清库或重建。数据目录有 `postgres.json` 而没给连接串也拒绝——不会悄悄新建一个本地库。

迁入：停掉所有节点，`personal migrate --data-dir /data --to postgres --database-url …`（`state.json` 或 `relay.db` 整份写进库、读回比对，原文件改名留作回退）。迁出：`--to sqlite` 或 `--to file`，库里的数据原样保留（不删），只把 `postgres.json` 改名。PostgreSQL 的备份用库自己的（`pg_dump` 或托管服务的快照），`personal backup` 会提示。

## 流量统计

`serve` 按「源 × 账号 / 访客 × 设备 × 类别 × 小时」累计经中继的连接数与两个方向的字节（cloud-api §19），在内存里累加，每 60 秒与停服时批量写进存储的 `relay_traffic_buckets`（SQLite 迁移 0002、PostgreSQL 迁移 0003），不按帧写库；不记路径、头、IP 与正文。

- 保留：`--traffic-retention-days`（或 `RELAY_TRAFFIC_RETENTION_DAYS`），缺省 90 天；启动时与之后每天清一次更早的桶。`0` = 不记录。
- 查看：`personal traffic --data-dir /data [--since 7d] [--source <id>]` 按源、账号、设备、类别合计成表；客户端与网页经 `GET /v1/me/traffic`（账号本人看全部源，访客只看自己）。刚发生的流量最多晚一分钟才查得到。
- 状态文件存储（`--store file`，只有 Node 版）：统计只在 `serve` 的内存里，接口照常答，重启归零，`personal traffic` 读不到；迁到 SQLite 后才落盘。`personal migrate` 不搬运统计。

## 升级与降级

- 升级：新版本打开 `relay.db` 时自动补跑新迁移（`packages/control-store-sql/migrations/`，账本 `relay_migrations` 记编号与 SHA-384）。升级前先备份。
- 降级：旧程序看到库里记着它不认识的迁移会拒绝启动。从升级前的备份恢复，或用 Node 版 `migrate --to file` 导出后再降级。加了流量统计表（SQLite 0002 / PostgreSQL 0003）的版本同样如此。

## 切换到 Go 版中继（0.10.0）与回滚

0.10.0 起 `ghcr.io/ama-link/armadra-relay` 镜像里是 Go 实现。镜像名、入口（tini + `armadra-relay`）、缺省命令、端口 8443、uid 10002、`/data` 卷、健康检查与全部参数、环境变量都不变；库格式也不变（同一份迁移与账本），Go 版与 Node 版可以交替打开同一个数据目录。

升级：

1. 先备份（这时还是升级前的 Node 镜像，容器里的命令行是 `node /app/out/cli.js`）：

   ```sh
   docker compose -f deploy/personal/compose.yml exec relay node /app/out/cli.js personal backup --data-dir /data --out /data/backup-$(date +%F).db
   ```

   PostgreSQL 存储用 `pg_dump`。

2. 数据目录还是 `state.json` 的，用新镜像迁到 SQLite（Go 版的 `migrate` 读得懂状态文件）：

   ```sh
   docker compose -f deploy/personal/compose.yml stop relay
   docker compose -f deploy/personal/compose.yml run --rm relay personal migrate --data-dir /data --to sqlite
   ```

3. 换镜像起服：`ARMADRA_RELAY_IMAGE=ghcr.io/ama-link/armadra-relay:0.10.0 docker compose -f deploy/personal/compose.yml up -d relay`。CA 指纹、账号口令、签名钥、已登记的源、设备与链接、流量统计都原样，客户端不需要重新添加或登录。
4. 核对：`docker compose -f deploy/personal/compose.yml exec relay armadra-relay personal status`，再 `personal doctor`。

回滚：停服，把 `ARMADRA_RELAY_IMAGE` 换回上一版（如 `ghcr.io/ama-link/armadra-relay:0.9.3`）再起；Go 版写过的 `relay.db` 与 PostgreSQL 库，Node 版照常读写（两版之间没有新迁移，0.9.3 → 0.10.0 就是这样；有新迁移时旧程序拒绝启动，按上一节从备份恢复）。第 2 步迁过的目录回滚后仍是 SQLite；要回到状态文件见上面「回退到状态文件」。

容器里执行子命令的写法变了：`node /app/out/cli.js personal …` 改成 `armadra-relay personal …`。Go 版另有一处差别：`--node-id` 同时作用于隧道层（Node 版隧道层用 `--host`）。缺省命令 `saas serve` 与 Node 版一样只是答 `/health`、`/ready` 的占位，SaaS 中继节点随 #59 的 G5 接替。

## 体检

```sh
docker compose -f deploy/personal/compose.yml exec relay armadra-relay personal doctor --data-dir /data
```

逐项报「通过 / 警告 / 失败」，有失败时退出码为 1：

- 数据目录（0700）、`relay.db` / `-wal` / `-shm` 或 `state.json`（0600）、`tls/` 与 `acme/` 下的私钥（0600）；
- 存储能否打开，`PRAGMA integrity_check`；
- 监听地址：`--tls plain` 只允许回环（在反向代理之后）；
- 受信代理：明文在代理之后却没配受信代理、网段过宽（IPv4 短于 /8、IPv6 短于 /16）；
- 口令：哈希参数是否低于当前缺省、账号是否锁定、首次初始化用过的口令文件是否还在（还在就可以删掉）；
- 自签 CA 的剩余有效期。

`doctor` 收 `serve` 的同名参数与环境变量（`--listen`、`--tls`、`--trusted-proxy`、`--password-file`），只看不改。
