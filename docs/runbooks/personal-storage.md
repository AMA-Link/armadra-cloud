# 个人中转的存储：SQLite 与状态文件

`armadra-relay personal` 的全部状态（账号、口令哈希、签名钥、源 / 设备 / 链接登记、设置）放在数据目录里，二选一：

| `--store` | 文件                    | 说明                                                                     |
| --------- | ----------------------- | ------------------------------------------------------------------------ |
| `sqlite`  | `relay.db`（WAL，0600） | 新装缺省。`node:sqlite`，`serve` 与 CLI 同时打开同一个库，写在事务里     |
| `file`    | `state.json`（0600）    | 0.3.x 之前的部署。目录锁 + 原子改名；只有它时不写 `--store` 也自动沿用它 |

都不存消息内容、令牌明文或口令。私钥以 PKCS#8 存在库或文件里，靠文件权限 0600 与数据目录 0700 保护。

## 怎么选

- 新装：什么都不用写，`init` 或 `serve` 的首次自动初始化建 `relay.db`。
- 已有 `state.json`：照旧用它，`serve` 启动日志提示可以迁移。想继续用文件可以显式写 `--store file`（或 `RELAY_STORE=file`）。
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

停服后 `personal migrate --data-dir /data --to file`：从库导出 schema v1 的 `state.json`（读回来比对），原库改名为 `relay.db.migrated-<时间>`。也可以直接把上一步留下的 `state.json.migrated-<时间>` 改回 `state.json`，但会丢掉迁移之后的改动。

## 备份与恢复

- 在线备份：`personal backup --data-dir /data --out /data/backup-$(date +%F).db`。SQLite 用 `VACUUM INTO`，`serve` 运行时也一致；状态文件是拷一份。备份文件 0600，目标已存在就拒绝。
- 恢复：停服，把备份文件放回数据目录改名为 `relay.db`（状态文件则为 `state.json`），确认目录里没有另一种存储文件，起服。
- 也可以停服后 `tar` 整个数据目录（连同 `tls/`、`acme/`）。

## 升级与降级

- 升级：新版本打开 `relay.db` 时自动补跑新迁移（`packages/control-store-sql/migrations/`，账本 `relay_migrations` 记编号与 SHA-384）。升级前先备份。
- 降级：旧程序看到库里记着它不认识的迁移会拒绝启动。从升级前的备份恢复，或用新版本 `migrate --to file` 导出后再降级。

## 体检

```sh
docker compose -f deploy/personal/compose.yml exec relay node /app/out/cli.js personal doctor --data-dir /data
```

逐项报「通过 / 警告 / 失败」，有失败时退出码为 1：

- 数据目录（0700）、`relay.db` / `-wal` / `-shm` 或 `state.json`（0600）、`tls/` 与 `acme/` 下的私钥（0600）；
- 存储能否打开，`PRAGMA integrity_check`；
- 监听地址：`--tls plain` 只允许回环（在反向代理之后）；
- 受信代理：明文在代理之后却没配受信代理、网段过宽（IPv4 短于 /8、IPv6 短于 /16）；
- 口令：哈希参数是否低于当前缺省、账号是否锁定、首次初始化用过的口令文件是否还在（还在就可以删掉）；
- 自签 CA 的剩余有效期。

`doctor` 收 `serve` 的同名参数与环境变量（`--listen`、`--tls`、`--trusted-proxy`、`--password-file`），只看不改。
