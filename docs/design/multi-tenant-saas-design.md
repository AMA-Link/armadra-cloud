# 多端入口、多中继、多租户与 SaaS：审查与设计（2026-10-10）

> **2026-10-22 起由 [SaaS 能力设计](saas-design.md) 与 [中继 Go 重写](go-relay-design.md) 取代，只留作追溯。** §1、§2.3、§2.4、§3.4 的客户端与 core 部分已实现；§3.1–§3.2 的「组织 → 团队」、§4.5 的语言结论、§5 的 49 服务器部署、§6 的 C8-x 工作包都已作废；仍有效的细节已合并进新文档（见 SaaS 设计 §12、Go 重写 §10）。
> 状态：设计稿（只审查与设计，不改代码）。输入：用户需求（2026-10-10）；Armadra `docs/design/platform-saas-architecture.md`（§4、§9、§17）、`docs/design/platform/{relay,cloud-control-plane,client-packages,core-packages}.md`、`docs/design/platform-implementation-plan.md` §12、`docs/design/server-accounts-and-sharing.md`、`docs/contracts/core-json-api.md` §31–§33；armadra-cloud `docs/contracts/cloud-api.md`、`CHANGELOG.md`、`deploy/`；两仓源码（行号以 2026-10-10 两仓 `main` 为准：Armadra `9cd38293`，armadra-cloud `1e9c959`）。
> 现状（2026-10-11）：中继只部署在 Cloudflare 正式环境（`relay.armadra.com`），不再有 staging 与 49 服务器部署；人机验证缺省关闭。运维以 [runbooks/relay-on-cloudflare.md](../runbooks/relay-on-cloudflare.md) 为准，文中个人域名已换成 `example.org`。
> 编号基线（核对自 main）：Armadra 契约最大节号 §54，契约 minor 最大 `1.26`，SQLite 迁移最大 `0043`；armadra-cloud 协议包与两个 app 均为 `0.2.1`，协议 `1.0`，PostgreSQL 迁移目录为空（下一个 `0001`）。
> 本文不出现任何第三方参考项目名字。

## §0 结论

1. **多端入口**：手机与 iPad 启动默认进「选择服务」页；它就是现有连接页（`ConnectScreen`）的列表态升级，不另起一套。状态机只加一个缺省分支（表非空时不再自动 `enterConnection(active)`），其余入口（分享链接、配对深链、推送深链 `?s=`）保持原样。桌面端用同一个列表组件做「切换服务」对话框：切换当前源，或在新窗口打开。
2. **冲突审查结论：两个中继本身不冲突，但「同一主机经两条中继」在客户端侧会丢一条路、并错位覆盖凭据槽**。core 一侧的登记表、隧道表、源密钥、信任来源、JWKS 缓存、分享链接存储、owner 绑定都按 `issuer` 分槽，可以同时登记 LAN 与公网两个中转（§2.1）。问题在客户端源表：`client_sources` 以 `source_id` 为主键、只有一个 `relay_origin` / `cloud_issuer`（迁移 0039），`sources.mount` 又用旧行的 `relayOrigin` 作凭据键却写入新 issuer（`core/sources/service.ts:870-902`）；手机连接表同样一源一路（`mobile/connections.ts:72-99`）。修正：迁移 `0044_client_source_routes`（一源多路），契约 §55，手机连接表 v2（§2.3）。
3. **部署前必须补的中继改动**：中继把 `req.socket.remoteAddress` 当客户端 IP（`apps/relay/src/edge/server.ts:135-137`），放在 OpenResty 后面所有人都是 `127.0.0.1`，按 IP 的限流（匿名 60/分、登录 20/分）会把全部用户一起锁住。加 `--trust-proxy`（§2.4、§5.3）。
4. **多租户模型**：组织 → 团队（部门）→ 主机（源）→ 工作空间 → 会话；云端只管「谁能到达哪台主机、以什么角色」，源内权限仍由 core 的 `identity_grants` 判定；云端的意图经断言的 `scp` / `role` / `org` / `team` 声明带进 core，core 在每次 `cloud/login` 时同步，撤销走「中继拒门 + core 停用主体」两道门（§3）。
5. **分享**：账号分享 = 成员邀请；工作区分享 = 现有分享链接（已带 `workspaceId`）；会话分享 = 新增只读焦点页范围（`scp: sess:<id>`），第一版只读。链接（匿名接受、带秘密）与邀请（定向到账号、要登录）两种到达方式，终点都是 core 的一张邀请（§3.4）。
6. **抽离**：把 `apps/relay/src/{tunnel,edge,auth,control}` 抽成 `packages/relay-kernel`，把个人中转的账号 / 会话 / 签名 / 源 / 链接业务抽成 `packages/control-core`（端口接口 + oRPC 路由工厂）；个人中转 = `control-core` + 状态文件实现；SaaS = `control-core` + PostgreSQL / Redis 实现 + 组织 / 团队 / 计费（§4.1）。
7. **语言**：控制面留 TypeScript；数据面（relay-kernel）当前也留 TypeScript，写死三个切换到 Go 的触发条件（单节点 > 1500 条隧道或 > 5000 条边缘 WebSocket、2 vCPU 持续 CPU > 60%、协议 major 2 的 TLS 直通）；届时类型统一靠协议包导出 JSON Schema + 黄金夹具生成 Go 代码，TS 仍是唯一真相（§4.5）。
8. **部署**：49 服务器上个人中转一个容器、`network_mode: host`、只监听 `127.0.0.1:8443`、`--tls plain`，OpenResty 终止 TLS（1Panel 签发证书）；证书签发前不给客户端登记，只做内部冒烟；SaaS 日后同机：cloud 容器 + relay saas 容器 + 复用已有 PostgreSQL（独立库与角色）+ **独立** Redis 容器（现有 Redis 对公网开放，不复用）（§5）。
9. **分阶段**：三个波次、十四个包（§6）。第一波四个包可并行：R6 反代信任（cloud，Sonnet）、A7-1 服务选择页（Armadra，Opus）、A7-2 多路由源表（Armadra，Opus）、C7-1 共享包抽离（cloud，Opus）。
10. 需要用户拍板的点集中在 §7。

## §1 多端入口：选择服务页

### 1.1 现状核对

| 事实                                                                                                                                             | 位置                                                                                                            |
| ------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------- |
| 原生 App 启动：有分享落地页 → `join`；有 `#link=` 深链 → `connect(link)`；地址是 `#connections` → 连接页（`manage`）；**否则有当前连接就直接进** | `apps/web/src/mobile/entry.ts:277-309`（`prepareEntry`：`if (active !== null) return enterConnection(active)`） |
| 连接页已经是「列表 + 添加连接」两态：列表行（名字、主机、直连 / 经中转徽标）、添加三选一（扫码 / 配对链接 / 配对码 / 中转账号）                  | `apps/web/src/mobile/ConnectScreen.tsx:168-500`、`MobileRoot.tsx:84-120`                                        |
| 从画布回连接页的办法：把地址改成 `#connections` 然后 `location.reload()`（原生源地址一次加载只算一次）                                           | `apps/web/src/mobile/MobileRoot.tsx:105-139`                                                                    |
| 手机连接表：`armadra.sources`（无凭据）、`armadra.sources.active`、`armadra.sources.remoteSlots`（源 → 远程服务）；凭据按源进钥匙串              | `apps/web/src/mobile/connections.ts:11-16`、`native-bridge.ts`                                                  |
| 推送深链 `armadra://w/<ws>[/n/<node>][?s=<sourceId>]`：`s` 是签发通知的 core `hostId`（契约 §19.4）；`here` / `switch` / `unknown` 三种路由      | `apps/web/src/mobile/push-open.ts:17-60`                                                                        |
| 桌面：所有源同时挂着（`SourceRegistry`，侧栏按源分组），「当前源」是查询缓存的作用域（`scope.ts`、`useSourceSwitchCacheReset`）                  | `apps/web/src/sources/registry.ts:74-280`、`app/use-sources-bootstrap.ts:26-70`                                 |
| 中继托管页面（`/app/`）已有「登录中继 → 挑主机」页                                                                                               | `apps/web/src/shell/RelaySignIn.tsx`、`sources/hosted.ts:339-420`                                               |
| 设置导航 id：`remoteAccess`（远程访问页，含「中转账号」「我连接的其他 Armadra」两组）、`devices`、`accounts`                                     | `apps/web/src/panels/settings/nav.ts:216-247`、`pages/RemoteAccessPage.tsx:282-394`                             |

结论：不需要新页面骨架。做三件事：改启动缺省分支、把连接页列表态升级成「选择服务」（分组、最近使用、在线状态）、把同一个列表组件给桌面当对话框。

### 1.2 状态机（原生 App 与 iPad；`entry.ts`）

```text
launch
 ├─ pathname /j/<linkId> ─────────────────────────────▶ join
 ├─ #link=armadra://pair|join|oauth ───────────────────▶ connect(link)        （不变）
 ├─ #connections ─────────────────────────────────────▶ pick(manage)         （不变，改名）
 ├─ 推送深链 ?s=<src>（push-open 的 switch）───────────▶ enter(src, deepLink)（不变：先记为当前再重载）
 ├─ 连接表为空 ───────────────────────────────────────▶ pick(empty)          （现状是 connect(mode: native)）
 ├─ 偏好 services.autoEnter = on 且 active 存在 ───────▶ enter(active)        （现状的缺省，改为可选）
 └─ 其余 ──────────────────────────────────────────────▶ pick                 （新缺省）

pick ──点一行──▶ enter(src) ──成功──▶ app
                      └──失败 { code }──▶ pick(failure 挂在那一行)
app ──设置「切换服务」/ 通知的 switch ──▶ #connections + reload ──▶ pick(manage, current = src)
pick(manage) ──「返回」──▶ app      （只在 current 可用时出现）
```

- `Entry` 类型只加两个字段：`connect(native)` 分支加 `pick: true`（代替 `manage` 的语义：`manage` 仍表示「从画布回来的」，`pick` 表示「启动落在这里」）与 `recent: string[]`（最近使用的 `sourceId`，最多 3 个，`armadra.sources.recent`，每次 `enter` 成功时前插去重）。
- `enterConnection` 不变；`autoEnter` 偏好存 `localStorage`（`armadra.services.autoEnter`，缺省 `false`），与现有偏好同一个 `preferences-store`。
- 服务器壳的手机网页（`mode: "web"`）不变：它背后只有一个源。
- 中继托管页面（`kind: "relay"`）的「挑主机」已经是选择页，只换成同一个列表组件（§1.3）。

### 1.3 页面结构

一个组件 `apps/web/src/services/ServicePicker.tsx`（新目录 `services/`：`ServicePicker.tsx`、`probe.ts`、`recent.ts`、`rows.ts`），三处使用：原生 App 的 `MobileRoot`（整页）、中继托管页的 `RelaySignIn`（登录后那一步）、桌面 `ResponsiveDialog`（手机 ≤ 767 是 `Sheet side="bottom"`，桌面是 `Dialog max-w-lg`）。

```text
390 宽（原生 App / iPad 竖）                         1440 宽（桌面对话框 max-w-lg）
┌──────────────────────────────────────┐           ┌ Dialog 「切换服务」 ───────────────────────┐
│  [BrandMark 28]  选择服务            │           │  本机                                     │
│                                      │           │  ┌ Item ─ MacBook Pro  ● 当前           ┐ │
│  最近使用                            │           │  中转 · 家里                              │
│  ┌ Item ────────────────────────────┐│           │  ┌ Item ─ 工作站      在线   [↗] [切换] ┐ │
│  │ MacBook Pro          在线 (pill) ││           │  直连                                     │
│  │ 经 家里中转 · 直连 · 2 小时前     ││ 11px      │  ┌ Item ─ 旧笔记本    离线   [↗] [切换] ┐ │
│  └──────────────────────────────────┘│           │                               [添加连接]  │
│  全部                                │           └───────────────────────────────────────────┘
│  中转 · 家里                         │  组头 = 远程服务名（`remote_services.label`）
│  ┌ Item ─ MacBook Pro     在线      ┐│
│  ┌ Item ─ 工作站          离线      ┐│
│  中转 · 公网                         │
│  ┌ Item ─ MacBook Pro     在线      ┐│  ← §2 修正后，同一主机经两条中继只出现一次：行在「全部」里按主机合并，
│  直连                                │     第二行列出可用的到达方式；这里的分组是示意，见 1.3 的合并规则
│  ┌ Item ─ 家里 NAS        在线      ┐│
│                                      │
│  [ 添加连接 ]           （outline）  │
└──────────────────────────────────────┘
```

- **行** = `ui/item`：首行名字（`label`，缺省主机名）+ 右侧 `StatusPill`（在线 `tone=positive`、离线 `muted`、未知时 `pulse` 无文字）；第二行 11px `text-muted-foreground`：到达方式（`直连` / `经 <服务名>`，多条用 `·` 连）+ 上次使用（`<相对时间>`，`format.ts` 已有相对时间）。当前行加 `Badge outline`「当前」（复用 `mobileConnect.current`）。
- **合并规则**：列表按 `sourceId` 一行（手机连接表与 core 源表都以它为键），到达方式来自 §2.3 的多路由；「最近使用」按 `recent` 顺序取行；「全部」按远程服务分组时，一台主机只放在它**首选路由**所属的组，其余路由写在第二行，不重复出现。
- **点行**：进入（`enter`）。长按 / 右滑（手机）或行尾 `IconButton inline` 菜单（桌面）：「移除」（`AlertDialog`：「移除「{name}」？」，主动作 destructive「移除」）。行上失败：`Alert destructive` 挂在那一行下方（「这台机器当前不在线」/「需要重新登录」+ 动作「登录」），不弹全局。
- **空态**：`Empty`：「还没有连接」+ default「添加连接」。只有一个连接且 `autoEnter` 关：照样显示选择页（用户要的就是先选）。
- **添加连接**：沿用 `ConnectScreen` 的添加态（扫码 / 配对链接 / 配对码 / 中转账号），不改。
- **在线状态探测**（`services/probe.ts`）：进入页面与回到前台时各一次；直连路由 `GET <base>/api/identity/hello`（1.5 秒）；经中继的路由按远程服务一次 `GET <issuer>/v1/me/sources`（答案里的 `online` 覆盖该服务下全部源），用钥匙串里那个服务的刷新令牌换访问令牌；远程服务登录失效 → 该组组头显示「已登出」+ 「登录」（复用 `remote.signedOut` / `remote.signIn`），组内行状态为未知。最多 3 个并发，结果缓存 30 秒；探测不阻塞点行进入。
- **iPad 768–1023**：同一页居中 `max-w-md`；添加连接走 `Sheet side="bottom"`。
- **桌面**：行尾两个动作：「切换」（`registry.current = src` + `useSourceSwitchCacheReset`，对话框关闭，侧栏滚到该组）与 `IconButton`「在新窗口打开」（桌面壳 IPC `window:open { sourceId }`：主进程新建窗口，页面地址带 `?source=<sourceId>`，`SourceRegistry` 以它为初始当前源；服务器壳与手机不显示这个按钮）。本机行只有「切换」。

### 1.4 设置里的入口

- 手机 / iPad：设置页顶部一组 `SettingsGroup`「服务」：`SettingsRow`「当前服务」（值 = 当前源名字 + 到达方式）→ `Button outline`「切换服务」（= `#connections` + reload，沿用 `MobileRoot.tsx:136-139`）；`SettingsRow`「启动时直接进入上次的服务」`Switch`（`autoEnter`）。
- 桌面与服务器壳：设置 → 远程访问（`remoteAccess`）页顶部同一组「服务」：「当前服务」行 → 「切换服务」打开 §1.3 的对话框；命令面板加 `app.switchService`（不预设快捷键）。侧栏组头点击也可以切当前源（已有分组，只加 `onSelect`）。
- 「退出登录」（账号）与「切换服务」分开，两者不混在一个按钮里。

### 1.5 深链与推送的 `?s=`

不改语义：`?s=` 是 `hostId`，与经哪条中继无关；`push-open.ts` 的 `switch` 分支在 §2.3 之后按多路由选路进入。唯一新增：`unknown`（签发通知的主机不在连接表里）时，提示里给一个动作「选择服务」（= 回选择页），代替现在只有提示。

### 1.6 i18n（新模块 `apps/web/src/i18n/services.ts`，中英同步；能复用的沿用 `mobileConnect.*` / `remote.*`）

| 键                        | zh-CN                    | en                              |
| ------------------------- | ------------------------ | ------------------------------- |
| `services.title`          | 选择服务                 | Choose a service                |
| `services.recent`         | 最近使用                 | Recent                          |
| `services.all`            | 全部                     | All                             |
| `services.group.local`    | 本机                     | This device                     |
| `services.group.direct`   | 直连                     | Direct                          |
| `services.group.relay`    | 中转 · {name}            | Relay · {name}                  |
| `services.via.direct`     | 直连                     | Direct                          |
| `services.via.relay`      | 经 {name}                | via {name}                      |
| `services.lastUsed`       | {time}                   | {time}                          |
| `services.status.online`  | 在线                     | Online                          |
| `services.status.offline` | 离线                     | Offline                         |
| `services.status.unknown` | 未知                     | Unknown                         |
| `services.switch`         | 切换服务                 | Switch service                  |
| `services.current`        | 当前服务                 | Current service                 |
| `services.autoEnter`      | 启动时直接进入上次的服务 | Open the last service on launch |
| `services.openWindow`     | 在新窗口打开             | Open in new window              |
| `services.select`         | 切换                     | Switch                          |
| `services.remove`         | 移除                     | Remove                          |
| `services.remove.confirm` | 移除「{name}」？         | Remove "{name}"?                |
| `services.empty`          | 还没有连接               | No connections yet              |
| `services.pick`           | 选择服务                 | Choose a service                |

复用：`mobileConnect.add`（添加连接）、`mobileConnect.current`（当前）、`mobileConnect.back`、`remote.signIn` / `remote.signedOut`、`remote.error.*`（按 `code`）。文案规则按设计系统 §6：按钮动词、状态两到四字、不写错误码。

### 1.7 测试与验收

- `services/probe.test.ts`（直连超时、中继登出、并发上限、缓存）、`ServicePicker.test.tsx`（分组与合并、最近使用、空态、行内失败、桌面两个动作）、`entry.test.ts` 补：表非空缺省落 `pick`、`autoEnter` 开才直进、推送 `switch` 不受影响。
- B 档 `mobile-shell-e2e`：「两个连接 → 启动落在选择页 → 选第二个进画布 → 设置里切换 → 回到选择页」。
- 桌面 A 档探针 `multi-source`：加「对话框切换当前源后侧栏与查询缓存一致」。
- 交付前 390 / 1440 明暗截图各一套（总计划 §12.4）。

## §2 冲突审查：LAN 个人中转 + 公网个人中转 + 将来的 SaaS

现场：Mac 上 `~/armadra-relay-lan`（`issuer https://192.0.2.10:8443`，`state.json` 里一个源 `e5df662b…`（MacBookPro.local）、5 条链接、15 台设备）；桌面 core 已登记并开着隧道。新增 `https://armadra-personal.example.org`，之后 `https://armadra-saas.example.org`。

### 2.1 逐项结论

| #   | 项                                      | 结论         | 证据（file:line）                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | 需要的修正                                                                                                                                                      |
| --- | --------------------------------------- | ------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | core 同时登记多个中转                   | 不冲突       | `0040_cloud_identity.sql` `cloud_registrations(issuer PK)`；`core/identity/cloud/store.ts:80-105` 按 issuer 读写；`register.ts` 同 issuer 才 `cloud_already_registered`                                                                                                                                                                                                                                                                                                                                                                                                                                                                   | 无                                                                                                                                                              |
| 2   | 隧道数                                  | 不冲突       | `core/relay/index.ts:107-160`：`Map<issuer, TunnelClient>`，`start()` 对每条有效登记各起一条；状态事件 `cloud.tunnel { issuer, state }`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   | 远程访问页已按 issuer 显示各自隧道状态，无改动                                                                                                                  |
| 3   | 源 id 与源密钥                          | 不冲突       | §31.2：`sourceId = hostId`，Ed25519 源密钥 `armadra-cloud-source-key` 所有 issuer 共用；两个中继各自存同一把公钥，互不知道对方                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | 无                                                                                                                                                              |
| 4   | JWKS 缓存、信任来源、中继来源           | 不冲突       | 三者都是 `cloud_registrations` 的列；两个个人中转签名钥 `kid` 都从 `k1` 起，但按 issuer 行缓存，不串                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | 无                                                                                                                                                              |
| 5   | owner 自己经中继进来的主体              | 不冲突       | 映射凭据 `provider = cloud:<sha256(iss)>`（`core/identity/cloud/login.ts:37,100-104`）；「分享本机」流程登记后立刻 `identity.cloud.bind`（`apps/web/src/api/remote-services.ts:281-299`）→ 每个 issuer 一条映射，都指向 owner                                                                                                                                                                                                                                                                                                                                                                                                             | 无                                                                                                                                                              |
| 6   | 别人经两条中继进来的主体                | 按设计       | 同上：同一个人经两个 issuer 是两个 member 主体（两套授予、两个显示名）                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    | SaaS 时用 `sub = 云账号 id` 且 issuer 唯一，自然合一；个人中转不处理                                                                                            |
| 7   | 分享链接归属                            | 不冲突       | 本机存 `armadra-share-links-<serviceId>`（`core/sources/secrets.ts:36`）；中继侧 `links[].sourceId`；`sources.shareLinks { serviceId }` 按服务列                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | 账号与共享页的邀请列表加「签发方」列（§6 A7-3）                                                                                                                 |
| 8   | **core 作为客户端：同一主机经两条中继** | **冲突**     | `0039_client_sources.sql:3-14`：`source_id PK`，只有一个 `relay_origin` / `cloud_issuer`。`service.ts:845-902`（`mount`）：`key = existing?.relayOrigin \|\| relayOrigin`——保留**旧**中继来源作凭据键，却把 `cloudIssuer` 写成**新** issuer（`:902`）；`:873-876` 用旧来源键写入新中继换来的刷新令牌，**覆盖** LAN 中转那一份。`mountByLink` 同样（`:1016-1050`）。之后 `session()`（`:553-566`）只认 `row.cloudIssuer` 那一个服务，LAN 这条路从此不可选；`relayed()`（`:490-520`）用 `relayKey` = 旧来源的令牌去新中继刷新 → core 会话绑在登记的中继来源上（§32.3）→ 401 → 重新 `cloud/login` → 每次切换都在 core 上多一台设备、一条会话 | 迁移 `0044_client_source_routes`，契约 §55，`mount` / `mountByLink` 以**本次**中继来源为凭据键（§2.3）                                                          |
| 9   | **手机：同一主机经两条中继**            | **丢路**     | `mobile/connections.ts:72-99`：一源一行，`relayOrigin` / `cloudIssuer` 后到者覆盖；`:140-146` `remoteSlots[sourceId]` 一源一个远程服务槽。不会出现两次（去重正确），但 LAN 那条路被静默替换，回家后也走公网                                                                                                                                                                                                                                                                                                                                                                                                                               | 连接表 v2：行上 `routes[]`，槽按 `(sourceId, origin)`；钥匙串已按 `(sourceId, origin)` 分槽（`native-bridge.ts` `setSession({ sourceId, origin, … })`），只改表 |
| 10  | 页面源表去重                            | 不冲突       | `sources/registry.ts` 以 `sourceId` 为键；`SourceDescriptor` 单路                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | `SourceDescriptor` 加 `routes[]`（§2.3），`routing.pick` 按路由列表选                                                                                           |
| 11  | 推送签发方                              | 不冲突       | `push-open.ts:17-46`：`?s=` 是 core `hostId`，推送登记在源 core（契约 §19），与中继无关                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   | 无                                                                                                                                                              |
| 12  | 设备与会话                              | 膨胀         | 每次 `cloud/login` 在 core 建一台设备与一条会话（§31.3）；LAN 中继 `devices[]` 已 15 台；#8 的重复登录会继续增加                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | #8 修好后不再重复登录；设备页按 `(来源, 名字)` 折叠显示（A7-3）；中继 `me.deviceRevoke` 已有                                                                    |
| 13  | 中继放在反向代理后的客户端 IP           | **部署阻塞** | `apps/relay/src/edge/server.ts:135-137` `remoteIpOf = req.socket.remoteAddress`；限流 `edge.connect.ip` 60/分、`login.ip` 20/分、`link.accept.ip` 20/分（`control/types.ts` `DEFAULT_LIMIT_RULES`）按这个 IP 计；`forward.ts` 写给 core 的 `X-Forwarded-For` 也是它                                                                                                                                                                                                                                                                                                                                                                       | `--trust-proxy`（§2.4）                                                                                                                                         |
| 14  | `--tls plain` 只许回环监听              | 部署约束     | `personal/args.ts:294-297`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                | 中继容器用 `network_mode: host` 监听 `127.0.0.1:8443`（§5.1）；R6 顺便放开「信任代理网段内」监听                                                                |
| 15  | SaaS 与个人中转并存                     | 不冲突       | `remote_services.issuer UNIQUE`、`kind personal/saas`；`cloud_registrations.mode`；`assertionFor` 对 `saas` 答 `not_implemented`（`service.ts:538`）                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | §6 A7-5 打开 saas 分支                                                                                                                                          |

### 2.2 不冲突的部分为什么安全

- 中继之间零共享：各自的 `state.json`、签名钥、设备表、链接表；core 对它们各开一条隧道、各信一套 JWKS。撤销一个（`identity.cloud.revoke { issuer }`）不影响另一个（`relay/index.ts:168-172` 只停那一个）。
- 分享链接各自签发：LAN 链接 `https://192.0.2.10:8443/j/…` 只在局域网可达；公网链接 `https://armadra-personal.example.org/j/…`。两种链接指向 core 上不同的邀请（`shareLinkCreate` 每次签新邀请），互不干扰。
- 隧道流量：两条隧道都经 `createListener({ admitted: true })` 进同一个 core HTTP 栈，准入按各自登记的可信来源判（§32.3）。

### 2.3 修正设计：一源多路

**迁移 `0044_client_source_routes.sql`**（Armadra，编号接 main 最大号 0043）：

```sql
-- 一台主机的多条到达方式（契约 §55）。`client_sources` 上原有的 base_url / relay_origin /
-- cloud_issuer 保留为「首选路由」的镜像（§33 形状不变）。
CREATE TABLE client_source_routes (
  source_id    TEXT NOT NULL REFERENCES client_sources(source_id) ON DELETE CASCADE,
  via          TEXT NOT NULL CHECK(via IN ('direct', 'relayed')),
  origin       TEXT NOT NULL CHECK(length(origin) BETWEEN 1 AND 2048),   -- direct：Gateway 来源；relayed：中继来源
  cloud_issuer TEXT NOT NULL DEFAULT '' CHECK(length(cloud_issuer) <= 2048),
  fingerprint  TEXT NOT NULL DEFAULT '' CHECK(length(fingerprint) IN (0, 64)),
  preferred    INTEGER NOT NULL DEFAULT 0 CHECK(preferred IN (0, 1)),
  added_at_ms  INTEGER NOT NULL CHECK(added_at_ms > 0),
  last_ok_at_ms INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (source_id, via, origin)
) WITHOUT ROWID;
-- 回填：有 base_url 的建一条 direct，有 relay_origin 的建一条 relayed（preferred = direct 优先）。
```

**契约 §55「客户端源表追加：多条到达方式」**（协议 minor `1.27`）：

- `ClientSource` 加 `routes: { via, origin, cloudIssuer, fingerprint, preferred, lastOkAtMs }[]`（加字段，旧字段照旧 = 首选路由）。
- `sources.session` 入参加可选 `route?: { via, origin }`；省略时选路：直连探测（1.5 秒）→ `preferred` 的中继 → 其余中继按 `lastOkAtMs`，最多并行 3 条断言请求，第一条成功的用。
- 新 procedure `sources.routePrefer { sourceId, via, origin }`、`sources.routeRemove { sourceId, via, origin }`（删最后一条路即删源，答 `conflict`）。
- `mount` / `mountByLink`：凭据键 = **本次**的 `assertion.relayOrigin`（修 `service.ts:872`、`:1018`），路由 upsert 一行；`cloudIssuer` 列只在该行是首选路由时镜像到 `client_sources`。
- SecretStore 形状不变：`armadra-source-<sourceId>.byOrigin[origin]` 已按来源分槽（`secrets.ts:4`），只是以前键取错了。
- `sources.list` 的 `remotes[].sources`（`remoteSources`）里的 `mounted` 改为「这台主机经这个服务有路由」。

**手机连接表 v2**（`mobile/connections.ts`）：行加 `routes[]`（同形），`remoteSlots` 改键 `${sourceId}\u0000${origin}`；读到 v1 行时一次性拆成路由（`baseUrl` → direct，`relayOrigin` → relayed），`armadra.sources.version = 2`；`descriptorOf` 的 `kind` 由首选路由决定。`SourceDescriptor`（`sources/types.ts`）加 `routes`，`routing.pick` 遍历路由；`credentials.ts` 的 mobile 实现按 `(sourceId, origin)` 取钥匙串（桥接口已支持）。

### 2.4 修正设计：反向代理信任（armadra-cloud R6）

- 新参数 `--trust-proxy <cidr>[,<cidr>…]` / `RELAY_TRUST_PROXY`（personal 与 saas 共用，缺省空 = 不信任）。连接的对端地址落在网段内时：客户端 IP 取 `X-Forwarded-For` 从右往左第一个不在信任网段内的地址；`X-Forwarded-Proto` 为 `https` 时按 TLS 对待（影响 `Secure` 判断与 `issuer` 校验）；不在网段内的连接忽略这两个头。
- `--tls plain` 的监听限制放宽为「回环，或 `--trust-proxy` 覆盖的网段内的地址」。
- 写给 core 的 `X-Forwarded-For` 与 `OPEN.remoteIp`（`forward.ts`）用解析后的地址；审计与限流随之正确。
- cloud-shared 的 HTTP 监听（控制面）同一套解析，一处实现（`packages/cloud-shared/src/http/`）。
- 契约 `cloud-api.md` §13「反向代理约定」；部署文档加 OpenResty 样例（§5.2）。

## §3 多租户模型

### 3.1 实体与层级

```text
组织 organization            计费、目录、审计的边界（租户）
 └ 团队 team（部门）          一批账号；授予的主体
    └ 成员 member             账号在组织里的角色：owner / admin / member；在团队里：lead / member
主机 host（= 源 source）      一台 core（桌面 / 服务器壳 / 托管），owner 是一个账号，可挂到一个组织
 └ 工作空间 workspace          core 内；分享与授予的最小单位（S4 不变）
    └ 会话 session             终端 / ACP 会话；只能作为「只读查看」的分享范围（§3.4）
设备 device / 云会话 session   账号的登录设备；每台设备在每台主机上又是 core 的一台 `identity_devices`
```

「集群」= 组织下的一组主机（`host_groups`，可选的标签而非层级），授予可以指向组或单台主机。

**两层真相**：

| 层                      | 存哪里                    | 管什么                                                                                                                     | 不管什么                         |
| ----------------------- | ------------------------- | -------------------------------------------------------------------------------------------------------------------------- | -------------------------------- |
| 云端（SaaS / 个人中转） | PostgreSQL / `state.json` | 账号、组织、团队、主机目录、**谁能请求哪台主机的断言、以什么意图**（`source_grants`）、分享链接、配额、云端审计            | 任何画布 / 终端 / 文件内容       |
| core                    | SQLite `identity_*`       | 主体、授予（`identity_grants`）、设备、会话、邀请、core 审计、终端所有权与接管（`creator_principal_id`、`terminal:drive`） | 账号口令（经云登录的人没有口令） |

云端的意图经断言声明进入 core；core 是最终判定点（平台设计 §9.1 不变）。

### 3.2 角色矩阵

| 角色                                     | 范围                               | 能做                                                                        | 编译成                                       |
| ---------------------------------------- | ---------------------------------- | --------------------------------------------------------------------------- | -------------------------------------------- |
| 组织 `owner` / `admin`                   | 组织                               | 建团队、加成员、挂主机、签组织加入链接、看组织审计、改计划                  | 不编译成任何源内 scope（§9.1）               |
| 组织 `member`                            | 组织                               | 看目录、接受授予                                                            | —                                            |
| 团队 `lead`                              | 团队                               | 增删本团队成员（对应 core 的组管理员，`server-accounts-and-sharing.md` §6） | —                                            |
| 主机 `owner`                             | 主机                               | 登记 / 撤销、签主机链接、授予团队或账号、撤销可达                           | core 的 owner（本地）                        |
| 主机授予 `viewer/editor/operator/driver` | 主机（全部工作空间）或工作空间列表 | 同 core 的四档（`roleScopes`）                                              | `identity_grants` 行，`origin = cloud:<iss>` |
| 会话查看 `session-viewer`                | 单个会话                           | 只读焦点页                                                                  | 新 scope 维度（§3.4）                        |

### 3.3 三种场景

**(a) 部门共用一个集群，经中转分发给很多人**

- 主机是服务器壳 core（一个 core 一个团队，平台设计 §10.1 / §4.4），组织 admin 把它挂到组织（`orgs.sourceAttach`），对团队授予角色：`source_grants(source_id, subject_kind='team', subject_id, role, workspace_ids NULL=全部)`。
- 成员登录 SaaS → `sources.assertion` → 断言带 `org`、`team`、`role`、`scp`（`ws:<id>` 列表或空 = 全部）→ 经中继 `cloud/login` → core：没有映射则**不需要邀请令牌**建成员（断言带 `org` 且登记行 `mode = saas` 时，`cloud.orgDefaultRole` 的分支改为「按 `role` 声明」），有映射则**同步**授予：把该主体 `origin = cloud:<iss>` 的授予整份替换成声明里的（本地 owner 手工给的授予 `origin = ''` 不动）。
- 并发编辑：现状——画布 CAS + `replayLocalEdits`、Yjs 实时板、`canvas.presence`；终端：`creator_principal_id` + `terminal:drive`；审批：`approval:answer`（driver）。不新增机制。
- 配额（按组织）：`maxSources`、`maxMembers`、`maxConcurrentWs`（中继 `edge.concurrent.org`，新限流种类）、`tunnelBytes`；超出 `429 limit_reached`（已有码）。
- 审计：云端 `audit_log` 记「谁对哪台主机取了断言（`jti`）」；core 审计 `cloud.login` 已记 `{ iss, sub }`，加 `jti` 列入 `details`，两边按 `jti` 对账。core 内动作审计照旧（主体就是云账号映射的 member）。
- 撤销：组织移除成员 / 团队撤授予 → 云端写 `revoked:jti`、中继 `kick`（现有 `relay:ctl`）→ 下一次 `cloud/login` 同步时该主体的云端授予清空；core 侧 `identity_grants` 加 `expires_at_ms`（随断言 `exp` 之后的租约，缺省 30 天，每次登录续）兜底——即使此人改走局域网直连，租约到期授予也失效。

**(b) 分享给他人查看**

| 分享什么         | 云端                                                                                                        | core                                                                     | 只读 / 可写               | 过期                   | 撤销                                                                                                               |
| ---------------- | ----------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------ | ------------------------- | ---------------------- | ------------------------------------------------------------------------------------------------------------------ |
| 账号（长期成员） | `invitations`（SaaS：定向到邮箱 / 账号；个人中转：没有，只用链接）                                          | 一张邀请（`identity_invitations`，`target_workspace_id = NULL`）+ 建成员 | 角色四档                  | 邀请到期；授予租约     | 云端 `source_access` 撤 + core 停用主体（`accounts.principals.disable`，已有）                                     |
| 单个工作区       | `links.create { kind: source_invite, scope: { workspaceId } }`（现有 `shareLinkCreate` 已带 `workspaceId`） | 邀请 `target_workspace_id = ws`（已有）                                  | `viewer` = 只读；其余可写 | 链接 ≤ 30 天；授予租约 | `shareLinkRevoke`（已有：撤链接 + 作废邀请）+ 新增「连同已进来的人」：停用 `subject` 前缀 `guest:<linkId>:` 的主体 |
| 单个会话         | `links.create { scope: { sessionId } }`                                                                     | 邀请 `target_session_id`（新列）→ 授予 `target_kind = 'session'`         | 只读（第一版）            | 同上                   | 同上                                                                                                               |

- **链接 vs 邀请**：链接 = 匿名接受 + `#secret`（个人中转建访客账号；SaaS 缺省要登录云账号，组织可开 `allowGuests`）；邀请 = 定向、要登录、能撤单个人。两者在 core 都落为一张邀请，所以 core 的接受 / 消费逻辑不变（§31.3）。
- **可写**只在工作区级（editor 以上）；会话级分享不提供可写——向他人终端写入会替 Agent 答权限提示（S5），必须是工作区 driver。

**(c) 个人多设备**

- 同一账号的每台设备 = 云端 `devices` 一行 + 各主机上 `identity_devices` 一行；全部 `via = owner`；owner 在每个 issuer 上 `bind` 过（§2.1 #5）。本节没有新东西；体验上靠 §1 的选择页与 §2.3 的多路由。

### 3.4 与 core 现有模型的对应与演进

| core 现有                                                         | 对应                            | 要改                                                                                                                                                                                                |
| ----------------------------------------------------------------- | ------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `identity_principals.kind owner/member`                           | 云账号 = member；owner 仍是本地 | 不改                                                                                                                                                                                                |
| `identity_credentials(kind oauth, provider cloud:<iss>, subject)` | 云身份映射                      | 不改；SaaS 的 `sub` 是账号 uuid                                                                                                                                                                     |
| `identity_grants(subject, workspace_id, role)`                    | 主机 / 工作区授予               | 迁移 `0045`：加 `origin TEXT DEFAULT ''`（`cloud:<iss>`）、`expires_at_ms INTEGER DEFAULT 0`、`target_kind TEXT DEFAULT 'workspace'`、`target_id`（会话 id）                                        |
| `identity_invitations(target_workspace_id, role, max_uses)`       | 链接 / 邀请的终点               | 加 `target_session_id`（0045）                                                                                                                                                                      |
| `roleScopes(role, workspaceId)`                                   | 角色编译                        | 加 `sessionViewerScopes(workspaceId, sessionId)`：`events:read@ws`、`terminal:read@ws#session`、`acp:read@ws#session`；`permits` 的 scope 加可选 `resource` 维度，路由门只在终端 / ACP 读路径上比对 |
| 终端 `creator_principal_id`、`terminal:drive`、接管（租约）       | 不变                            | —                                                                                                                                                                                                   |
| `cloud.orgDefaultRole`                                            | 被断言 `role` 取代              | 保留设置键，SaaS 登记时忽略                                                                                                                                                                         |
| 审计 `cloud.login { iss, sub }`                                   | 对账                            | 加 `jti`、`org`、`team`                                                                                                                                                                             |

演进路径：个人中转今天的「链接 → 邀请 → 成员」原样保留；SaaS 只是在同一条路上多了「谁签发断言」和「断言里多几条声明」。`scp` 为空时 core 行为与现在逐字相同，所以老中继签的断言照常可用（协议 minor 兼容）。

### 3.5 数据面隔离与令牌 scope

- 隧道按源：`route:src:<sourceId>`、`tunnel.connect.src`；一个源只属于一个租户（`sources.org_id`），跨租户没有共享隧道。
- 中继令牌（`aud: "relay"`）加 `org`（租户）与 `ro`（只读）两个声明：`org` 用于 `edge.concurrent.org` / `edge.connect.org` 限流与计量；`ro` 时边缘只放行安全方法与身份域路径（`GET`/`HEAD`/`OPTIONS`，`/api/identity/*`、`/api/rpc/identity/*`、`/api/identity/ws-ticket`、WebSocket 升级），其余 `403 forbidden`——纵深防御，core 仍是判定点。（0.3.11 起停用：core 的 RPC 一律 `POST`，边缘不再按 `ro` 过滤，见 cloud-api §14。）
- 断言加 `scp: string[]`（`ws:<id>`、`sess:<id>`、`ro`）、`role`、`org`、`team`；core 按 §3.3 编译。
- SaaS 源地址用子域 `<sourceId>.src.<域>`：每个源一个浏览器来源，租户间的 localStorage / Cookie 天然隔离；个人中转的路径前缀 `/s/<id>/` 保持（单账号）。
- 云端查询一律带 `org_id` / `account_id` 过滤（控制面设计 §7），审计不写令牌。

## §4 SaaS 架构与抽离

### 4.1 包划分（armadra-cloud）

```text
packages/platform-protocol   不变：schema、帧、声明、错误码、夹具（两仓共用，npm 发布）
packages/cloud-shared        不变：config / log / health / http 监听 / db client / redis client
packages/relay-kernel  NEW   ← apps/relay/src/{tunnel,edge,auth,control/types,control/memory,metrics}
                             隧道终端、边缘、令牌校验、ControlPlane 接口与内存实现；不含 CLI、不含 personal
packages/control-core  NEW   ← apps/relay/src/personal/{accounts,sessions,signer,sources,links,stream}.ts 的业务部分
                             端口（ports）：AccountStore / DeviceStore / SourceStore / LinkStore / GrantStore /
                                           RevocationStore / RateLimiter / PresenceBus / KeyStore / Clock
                             服务：accounts（口令、锁定）、sessions（访问 / 刷新旋转）、signer（四种令牌）、
                                   sources（注册令牌、注册、heartbeatHint、断言）、links（创建 / 接受 / 撤销）、
                                   me-stream（扇出）
                             createCloudRouter(services, { capabilities })：implement(cloudContract) 的唯一实现；
                             未启用的 procedure 统一答 404（personal）或 501（saas 未实现）
apps/relay                   CLI + 装配：
                               personal = relay-kernel + control-core + adapters/state-file（state.json 实现全部端口）
                               saas     = relay-kernel + control/redis（ControlPlane 的 Redis 实现）
apps/cloud                   control-core + adapters/postgres（端口的 PG 实现）+ adapters/redis（限流 / 撤销 / 在线）
                             + 只有多租户才有的模块：orgs、teams、grants、billing、jobs、mail、oauth/passkey/totp
```

- 租户参数：端口方法第一个参数是 `tenantId`（personal 恒为 `"default"`）；账号标识统一为不透明字符串 `sub`（personal `acct:<name>`，saas uuid）。
- 边界测试（`boundaries.test.ts` 扩展）：`relay-kernel` 不 import `control-core`；`control-core` 不 import `node:fs` / `postgres` / `redis`（只经端口）；`apps/relay` 的 personal 不 import `postgres` / `redis`（现有规则不变）。
- `@orpc/*` 允许出现的位置改为：协议包 `cloud-api/`、`control-core/router.ts`、`tools/contract/`（原来的 `apps/cloud/src/http/rpc.ts` 与 `apps/relay/src/personal/api.ts` 变成对 `createCloudRouter` 的调用）。

### 4.2 PostgreSQL 与 Redis 的分工（SaaS）

| 存储       | 内容                                                                                                                               | 备注                                                                             |
| ---------- | ---------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------- |
| PostgreSQL | 账号、凭据、设备、云会话（刷新哈希）、组织、团队、主机目录、授予、链接、链接使用、审计、签名公钥目录、计划与用量、`jobs`           | 真相；每条查询带租户过滤                                                         |
| Redis      | `route:src:*`、`relay:nodes`、`presence:*`、`rl:*`、`revoked:jti:*`、`wsticket:*`、`relay:ctl` / `relay:node:*` pub/sub、JWKS 缓存 | 全部可丢；`maxmemory-policy noeviction`（撤销记录不能被挤掉），`maxmemory 128mb` |

### 4.3 迁移（PG，目录 `apps/cloud/src/db/migrations/`，账本 `cloud_migrations`，lock 在仓库根）

| 编号 | 内容                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | 包   |
| ---- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---- |
| 0001 | `accounts`、`account_credentials`、`account_mfa`、`account_recovery_codes`、`account_lockouts`、`devices`、`account_sessions`、`device_codes`、`email_tokens`、`signing_keys`、`audit_log`（控制面设计 §3.1 原样）                                                                                                                                                                                                                                                            | C8-1 |
| 0002 | `sources`、`source_registration_tokens`、`source_access`（§3.2 原样）+ `host_groups`、`host_group_members`                                                                                                                                                                                                                                                                                                                                                                    | C8-2 |
| 0003 | `organizations`、`org_members`、`org_teams(team_id, org_id, name, created_at)`、`org_team_members(team_id, account_id, role lead/member)`、`links`（`sources_json` 外加 `scope_json`）、`link_uses`、`source_grants(grant_id, source_id, subject_kind account/team/org, subject_id, role, scope_json, expires_at, revoked_at, granted_by)`、`invitations(invitation_id, org_id, source_id, email citext, role, scope_json, token_hash, expires_at, accepted_by, accepted_at)` | C8-2 |
| 0004 | `jobs(job_id bigserial, kind, payload jsonb, run_at, attempts, locked_until, done_at, error)`；索引 `(run_at) WHERE done_at IS NULL`                                                                                                                                                                                                                                                                                                                                          | C8-4 |
| 0005 | `billing_plans`、`billing_subscriptions`、`usage_counters`、`hosted_sources`（§3.4 原样）                                                                                                                                                                                                                                                                                                                                                                                     | 后续 |

### 4.4 其他组件选型

| 组件     | 推荐                                                                                                                                                        | 理由                                                                                             |
| -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| 迁移工具 | 保留自写运行器（控制面设计 §3.5，SHA-384 账本、拒绝条件同 core）                                                                                            | 与两仓迁移纪律一致；第三方工具的账本与拒绝条件不能照搬                                           |
| 队列     | PostgreSQL `jobs` 表 + `FOR UPDATE SKIP LOCKED` + `LISTEN/NOTIFY` 唤醒，worker 跑在 cloud 进程内                                                            | 2 核 1.4G 不值得再起一个进程；邮件、清理、用量汇总都允许秒级延迟；重启不丢；Redis 只放可丢的东西 |
| 邮件     | `nodemailer` SMTP（已选）；dev 用 mailpit                                                                                                                   | SMTP 端点可换：服务器上已有 `mailserver` 应用（1Panel 应用列表），或外部 SMTP（§7 Q5）           |
| 对象存储 | M1 不需要；托管源备份时用 S3 兼容接口（`@aws-sdk/client-s3` 精确版本）                                                                                      | 账号 / 组织等结构化数据都在 PG；没有文件正文经过云端                                             |
| 监控     | `/metrics`（中继已有，cloud 补同形）+ 服务器上已有的 uptime-kuma 盯 `/health` 与 `/ready`；日志 stdout → docker `json-file`（`max-size 10m`、`max-file 5`） | 不在 3.6G 的机器上再放 Prometheus / Grafana；需要时把 `/metrics` 拉到别的机器                    |
| 错误上报 | 结构化日志里的 `requestId` + 错误码计数（metrics）                                                                                                          | 不引第三方 SDK；两仓都禁止正文进日志                                                             |
| 密钥     | 签名钥 JSON 文件（docker secret，0600）、`ARMADRA_CLOUD_MASTER_KEY` 在 `.env`（0600）                                                                       | 已定；轮换走 `cloud keys rotate`                                                                 |
| 备份     | PG：1Panel 的 PostgreSQL 应用自带定时备份（按库）；中继 `state.json` 每夜 `tar` 到 `/opt/armadra/backups`，保留 30 天                                       | 两者都小；恢复步骤写进 runbook                                                                   |

### 4.5 语言评估：继续 TypeScript，还是 Go

| 维度               | TypeScript（现状）                                                                                | Go                                                                            |
| ------------------ | ------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| 类型与契约统一     | 协议包就是源码：zod schema、oRPC 契约、错误码注册表、黄金夹具，Armadra 与 cloud 两仓同一份        | 需要从 TS 生成；生成链路可建（见下），但多一道工序                            |
| 数据面性能         | 中继是字节透传（64 KiB 分块、无解析）；估算单核 200–400 MB/s 级别，瓶颈在 TLS 与系统调用，不在 JS | 更高吞吐、更低 p99；goroutine 比 Node 的事件循环更省心                        |
| 内存               | 进程基线 ~80–120 MB；每条隧道 ~100 KB、每条边缘 WS ~50 KB（估算，V 探针量）                       | 基线 ~15–30 MB；每连接 ~10–20 KB                                              |
| 部署               | 镜像 + Node 运行时；或 esbuild 单文件 + `node`                                                    | 单二进制                                                                      |
| 迁移成本           | —                                                                                                 | 重写 `tunnel/edge/auth`（~4k 行）、JWS 验签、帧编解码；两套实现要同时跟协议走 |
| 2 核 3.6G 现实约束 | 个人中转 + SaaS 控制面 + saas 中继三个 Node 进程 ≈ 350 MB，在 1.4G 可用之内                       | 省 200 MB 左右；对当前规模不是决定性的                                        |

**结论**：控制面留 TypeScript（oRPC + zod + PG 驱动 + 共享契约，这是整个平台「契约优先」的根）。数据面当前也留 TypeScript，因为 relay-kernel 刚稳定、夹具驱动的跨仓测试已经在跑，而且现阶段的负载远在 Node 的能力之内。**写死切换触发条件**（任一满足即开 Go 版 relay-kernel 的包）：

1. 单节点需要承载 > 1500 条隧道或 > 5000 条边缘 WebSocket（按 V 探针与 `/metrics` 的实测）；
2. 2 vCPU 上 `relay_*` 持续 CPU > 60% 或 p99 转发延迟 > 50 ms；
3. 协议 major 2 的 TLS 直通（`OPEN kind=tls`）——那时中继只剩字节搬运，是 Go 的最佳形态。

**届时如何保证两端类型统一**（现在就把前两步做进 C7-1，成本小）：

- 协议包增加构建产物 `schema/*.json`：zod 4 的 `z.toJSONSchema()` 导出断言声明、隧道 `hello / challenge / auth / ready / reject` 的 JSON 帧、`cloud-api` 的 OpenAPI（已有 `cloud-openapi.json`）、错误码注册表（`errors.json`：码 → 状态）、`identity-vectors` 常量；随 npm 包与 tarball 发布。
- 黄金夹具（`fixtures/`）是语言无关的字节 / JSON：Go 实现用同一份夹具做表驱动测试，与 TS 的 `fixtures:check` 对等。
- Go 侧 `go generate`：JSON Schema → 结构体；OpenAPI → 客户端 / 服务端桩；`errors.json` → 常量表。CI 里「协议包版本 == Go 模块版本」钉死，与现有「cloud 版本 lockstep 协议包」同一条规则。
- 隧道二进制帧的布局（`tunnel/frames.ts`）另写一份 `tunnel/FRAMES.md` 规范（已由夹具约束），作为 Go 实现的文档。

### 4.6 资源预算（49 服务器，2 vCPU / 3.6 GB，可用 ~1.4 GB）

| 进程                        | 常驻内存（估算） | 说明                          |
| --------------------------- | ---------------- | ----------------------------- |
| armadra-relay personal      | 90–130 MB        | 单账号、几条隧道、几十条 WS   |
| armadra-cloud（SaaS，日后） | 120–160 MB       | 含进程内 jobs worker          |
| armadra-relay saas（日后）  | 90–130 MB        |                               |
| armadra-redis（独立，日后） | 10–30 MB         | `maxmemory 128mb`             |
| PostgreSQL（复用已有容器）  | +20–40 MB        | 多一个库与连接池（`max 8`）   |
| 合计（SaaS 上线后）         | ~350–500 MB      | 留 ~900 MB 给已有服务与页缓存 |

Node 进程统一 `--max-old-space-size=256`；`ulimit -n 65536`。

## §5 部署：49 服务器（CentOS Stream 9，1Panel + OpenResty）

现场核对（只读）：OpenResty 容器以主机网络占 `0.0.0.0:80/443`（站点配置在 `/opt/1panel/apps/openresty/openresty/conf/conf.d/<域名>.conf`，证书在 `/www/sites/<域名>/ssl/`）；`pgsql` postgres 18 绑 `127.0.0.1:5432`；`redis` 7.4 与 `mysql` 8 **绑 `0.0.0.0`**（对公网开放）；Gitea `127.0.0.1:3000`；可用内存 1467 MB；磁盘余 33 GB；应用列表里有 `uptime-kuma`、`mailserver`、`postgresql`、`redis`。

### 5.1 拓扑

```text
公网 ──443──▶ OpenResty（主机网络，终止 TLS，1Panel 证书）
               ├─ armadra-personal.example.org ──▶ 127.0.0.1:8443  armadra-relay personal（network_mode: host，--tls plain）
               ├─ armadra-saas.example.org      ──▶ 127.0.0.1:8100  armadra-cloud（日后；/v1、/app、/.well-known、/j）
               └─ *.src.armadra-saas.example.org ─▶ 127.0.0.1:8101  armadra-relay saas（日后；/t/v1 隧道与边缘）
容器网络 1panel-network：armadra-cloud ⇄ pgsql:5432（独立库 armadra_cloud）、armadra-redis:6379（独立容器，不发布端口）
```

- 中继只绑回环；`--metrics-listen 127.0.0.1:9443`（uptime-kuma 在同机可拉）。
- 为什么 `network_mode: host`：`--tls plain` 只许回环监听（`args.ts:294`），而容器内的回环不能被主机上的 OpenResty 访问；主机网络 + `--listen 127.0.0.1:8443` 不用改代码就满足。R6 合入后可改成加入 `1panel-network` + `--trust-proxy 172.16.0.0/12`。
- 安全组 / firewalld：只开 22、80、443；**关掉 3306 与 6379 的公网暴露**（§7 Q2）。

### 5.2 OpenResty 站点配置（放 1Panel 站点的 `proxy/*.conf` 或直接写在 `conf.d/armadra-personal.example.org.conf`）

在 1Panel 建「反向代理」站点 `armadra-personal.example.org` → `http://127.0.0.1:8443`，再把下面内容放进该站点的 `proxy/armadra.conf`（1Panel 模板已带 `Host`、`X-Forwarded-For`、`X-Real-IP`、`Upgrade` / `Connection`、HSTS、HTTP→HTTPS）：

```nginx
# http 块（1Panel 的 conf.d/00.default.conf 或站点 conf 顶部）
map $http_upgrade $armadra_connection { default upgrade; "" ""; }
limit_req_zone $binary_remote_addr zone=armadra_auth:1m rate=30r/m;
upstream armadra_personal { server 127.0.0.1:8443; keepalive 32; }

# server 块内
client_max_body_size 64m;              # 经中继上传到 core 的文件
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection $armadra_connection;
proxy_set_header X-Forwarded-Proto https;
proxy_read_timeout 3600s;              # 隧道与终端 WS 长连接；中继自身心跳 20s 保活
proxy_send_timeout 3600s;
proxy_connect_timeout 5s;
proxy_buffering off;                   # 事件流 / 终端输出不缓冲
proxy_request_buffering off;           # 上传与流式请求体
keepalive_timeout 75s;                 # 与中继 75s 空闲 keep-alive 对齐（0.2.1 修复）
location = /v1/auth/login            { limit_req zone=armadra_auth burst=10 nodelay; proxy_pass http://armadra_personal; }
location ~ ^/v1/links/[^/]+/accept$  { limit_req zone=armadra_auth burst=10 nodelay; proxy_pass http://armadra_personal; }
location /                           { proxy_pass http://armadra_personal; }
```

- HTTP/2 保持开（浏览器 WebSocket 会回落 HTTP/1.1；core 的隧道 `wss://…/t/v1` 也走 1.1）。
- `ssl_protocols` 建议改成只留 `TLSv1.2 TLSv1.3`（1Panel 模板带了 1.0 / 1.1）。
- 不改 `access_log` 格式；中继自己不记请求路径以外的头。

### 5.3 中继命令与环境（`/opt/armadra/personal/compose.yml`）

```yaml
services:
  relay:
    image: ghcr.io/ama-link/armadra-relay:0.2.2 # R6 合入并发布后的版本（含 --trust-proxy）
    network_mode: host
    command:
      [
        "personal",
        "serve",
        "--host",
        "armadra-personal.example.org",
        "--port",
        "443",
        "--public-origin",
        "https://armadra-personal.example.org",
        "--listen",
        "127.0.0.1:8443",
        "--tls",
        "plain",
        "--trust-proxy",
        "127.0.0.1/32",
        "--metrics-listen",
        "127.0.0.1:9443",
      ]
    environment:
      RELAY_DATA_DIR: /data
      RELAY_PERSONAL_ACCOUNT: me
      RELAY_PERSONAL_PASSWORD_FILE: /run/secrets/relay_password
      NODE_OPTIONS: --max-old-space-size=256
    secrets: [relay_password]
    volumes: ["/opt/armadra/personal/data:/data"] # chown 10002:10002，0700
    logging: { driver: json-file, options: { max-size: 10m, max-file: "5" } }
    restart: unless-stopped
secrets:
  relay_password: { file: /opt/armadra/personal/relay_password.txt } # 0600，首次初始化后可删
```

- 页面产物：镜像自带 `/app/web`（`deploy/compat.json` 的 `web` 版本）；要和桌面同版本时用带 `WEB_IMAGE` 构建参数的镜像。
- `issuer` 是 `https://armadra-personal.example.org`（443 省略端口），与桌面「中转账号」里填的地址逐字一致。
- 核对：`curl -s https://armadra-personal.example.org/.well-known/armadra-platform`（`mode: personal`、`issuer` 正确）、`/health`、`docker compose exec relay node /app/out/cli.js personal status`。

### 5.4 数据卷与备份

- `/opt/armadra/personal/data/state.json`（账号、口令哈希、签名钥、源 / 设备 / 链接；0600）。没有 TLS 目录（plain）。
- 每夜 `tar czf /opt/armadra/backups/personal-$(date +%F).tgz -C /opt/armadra/personal data` + 保留 30 天；1Panel 计划任务即可。恢复 = 停容器、解包、起容器。
- 日后 SaaS：PG 库 `armadra_cloud` 用 1Panel 的 PostgreSQL 应用备份（按库），签名钥文件与 `.env` 一并入备份；Redis 不备份。

### 5.5 PostgreSQL / Redis：复用还是独立

| 组件       | 结论                                                 | 做法                                                                                                                                                                                                                                                                         |
| ---------- | ---------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| PostgreSQL | **复用**现有 `pgsql`（18.6，只绑回环）               | `CREATE ROLE armadra_cloud LOGIN PASSWORD '…'; CREATE DATABASE armadra_cloud OWNER armadra_cloud;`；cloud 容器加入 `1panel-network`，`DATABASE_URL=postgres://armadra_cloud:…@pgsql:5432/armadra_cloud`；不给超级权限；`citext` 扩展需要先以超级用户 `CREATE EXTENSION` 一次 |
| Redis      | **不复用**现有 `redis`（`0.0.0.0:6379`，疑似无口令） | 另起 `armadra-redis`（`redis:8-alpine`，`--requirepass`、`--maxmemory 128mb --maxmemory-policy noeviction --save ""`，不发布端口，只在 `1panel-network`）；现有 Redis 建议在 1Panel 里关闭「允许外部访问」并设口令（§7 Q2）                                                  |
| MySQL      | 不用                                                 | 同样建议关闭公网暴露                                                                                                                                                                                                                                                         |

个人中转不需要 PG / Redis（AGENTS.md 红线不变）。

### 5.6 证书签发完成前的过渡

- 1Panel 对 `armadra-personal.example.org` 申请证书（HTTP-01 经 OpenResty 的 `.well-known/acme-challenge`，无需开放新端口；泛域名 `*.src.armadra-saas.example.org` 要 DNS-01，需要 DNS 服务商 API，§7 Q1）。
- **签发前不给任何客户端登记**：客户端会钉住信任锚指纹（`remote_services.fingerprint`，`cloud_registrations` 按它钉扎），临时自签证书换成正式证书后所有客户端都会 `fingerprint_mismatch`，只能逐个 `remoteRemove` 再加。
- 过渡期只做内部冒烟：中继以 `--tls self-signed --listen 127.0.0.1:8444` 另起一个临时实例（issuer `https://127.0.0.1:8444`），在服务器上 `curl -k` 走通 `/.well-known`、登录、注册令牌；冒烟完即删数据目录。
- 证书就绪后：在桌面「远程访问 → 添加中转账号」填 `https://armadra-personal.example.org`（系统信任，不需要指纹）→ 「分享本机」→ 手机添加中转账号。LAN 中转保留不动。

### 5.7 SaaS 上线时的增量（日后）

`armadra-saas.example.org` 一个来源同时作 issuer 与页面来源（`ARMADRA_CLOUD_ISSUER` = `ARMADRA_CLOUD_APP_ORIGIN`，单域名省一张证书，`__Host-` Cookie 可用）；`*.src.armadra-saas.example.org` 泛域名指向同机，OpenResty 一条 `server_name ~^(?<src>[0-9a-f]{32})\.src\.armadra-saas\.example\.org$` 反代到 `127.0.0.1:8101`；隧道入口 `wss://relay.armadra-saas.example.org/t/v1`（或复用泛域名下一个固定子域）。cloud 与 relay saas 两个容器 + `armadra-redis`，`.env` 按 `deploy/README.md` 的变量表；升级顺序先 relay 后 cloud。

## §6 分阶段实施计划

规则：一包一 worktree 一 PR，CI 绿后合并（合并提交，不 squash）；Armadra 契约节号从 §55 起追加、minor 从 1.27 起；迁移接 main 最大号（Armadra 0044 起，cloud PG 0001 起）；cloud 仓的 app 版本与协议包 lockstep；跨仓改形状先改协议包。Opus = 复杂 / 安全相关；Sonnet = 机械、范围清楚。

### 6.1 第一波（四包并行；R6 最先合并，因为它挡着部署）

| 包   | 仓库    | worktree                             | 文件边界                                                                                                                                                                                                                                                                                                                                                   | 契约 / 版本                                                                     | 迁移 | 测试与验收                                                                                                                                                                 | 模型   | 规模 |
| ---- | ------- | ------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------- | ---- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------ | ---- |
| R6   | cloud   | `armadra-cloud-wt/relay-proxy-trust` | `apps/relay/src/{config.ts,personal/args.ts,edge/server.ts,edge/forward.ts,http/server.ts}`、`packages/cloud-shared/src/http/`、`deploy/personal/{compose.behind-proxy.yml,openresty.conf.example}`、`deploy/README.md`、`docs/runbooks/personal-behind-reverse-proxy.md`                                                                                  | cloud-api §13「反向代理约定」；协议包 `0.2.2`（无 schema 改动）、两 app `0.2.2` | —    | 单测：信任网段内取 XFF 最右不可信地址、网段外忽略、`plain` 监听放宽；集成：假代理头下限流按真实 IP 计；`pnpm check && pnpm test`                                           | Sonnet | S    |
| A7-1 | Armadra | `Armadra-worktrees/service-picker`   | `apps/web/src/services/`（新）、`mobile/{entry.ts,MobileRoot.tsx,ConnectScreen.tsx}`、`shell/RelaySignIn.tsx`、`panels/settings/pages/RemoteAccessPage.tsx`、手机设置页顶部组、`i18n/services.ts`、命令面板注册、桌面壳 `window:open { sourceId }`（`apps/desktop/src/main/` 与 preload 各一处，page 读 `?source=`）                                       | 无 core 契约；桌面壳 IPC 新增一条（`docs/guides/architecture.md` 记一笔）       | —    | §1.7；B 档 `mobile-shell-e2e`；A 档 `multi-source`；390 / 1440 明暗截图                                                                                                    | Opus   | L    |
| A7-2 | Armadra | `Armadra-worktrees/source-routes`    | `core/db/migrations/0044_client_source_routes.sql` + `migrations.lock`、`core/sources/{store,service,secrets}.ts`、`packages/shared/src/contract/sources.ts`、`docs/contracts/core-json-api.md` §55、`core-openapi.json`、`apps/web/src/sources/{types,registry,routing,connection,credentials}.ts`、`mobile/{connections,credentials}.ts`（表 v2 + 迁移） | §55，minor `1.27`                                                               | 0044 | 单测：同一主机经第二个中继挂载后两条路都在、凭据各槽、`session` 选路顺序、删最后一路即删源；迁移回填；手机表 v1→v2；凭据不出现在响应；`pnpm check`（lock、contract:check） | Opus   | L    |
| C7-1 | cloud   | `armadra-cloud-wt/extract-core`      | 新 `packages/relay-kernel`、`packages/control-core`（端口、服务、`createCloudRouter`、`schema/` 导出）、`apps/relay/src/personal/` 缩成 state-file 适配器 + 装配、`apps/cloud/src/http/rpc.ts` 改为调用路由工厂、`boundaries.test.ts`、`knip.json`、`repo.rules.json`、`eslint` 的 `@orpc` 白名单                                                          | 行为零改动；协议包 `0.2.3`（加 `schema/` 产物）、两 app `0.2.3`                 | —    | 全部既有测试搬家后通过；`personal-e2e`、`pnpm e2e --mode personal` 不变；边界扫描测试三条新规则；`protocol:pack` 的 tarball 含 `schema/`                                   | Opus   | XL   |

**依赖**：R6 与 C7-1 都改 `apps/relay/src`，R6 先合并、C7-1 在合并后变基（R6 很小）。A7-1 与 A7-2 都碰 `mobile/connections.ts` 的读取方：A7-1 只读 `SourceDescriptor`，A7-2 加字段，A7-2 先合，A7-1 变基时把第二行的「到达方式」改成读 `routes`。

### 6.2 第二波

| 包   | 仓库     | worktree                           | 文件边界                                                                                                                                                                                                                                                                                                                                                         | 契约 / 版本                                                                       | 迁移 | 测试与验收                                                                                                                                                             | 模型   | 规模 |
| ---- | -------- | ---------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- | ---- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------ | ---- |
| D1   | 用户操作 | —                                  | §5.1–§5.6 的部署（compose、站点、证书）；runbook 由 R6 交付                                                                                                                                                                                                                                                                                                      | —                                                                                 | —    | `/.well-known`、`/health`、桌面登记、手机挂载、`node tools/probes/personal-roundtrip.mjs --issuer https://armadra-personal.example.org`（探针加 `--issuer` 参数属 V3） | —      | —    |
| C7-2 | cloud    | `armadra-cloud-wt/assertion-scope` | 协议包 `assertion/claims.ts`（`scp`、`role`、`org`、`team`）、`tunnel`/relay token（`org`、`ro`）、`cloud-api` `links.create` 的 `scope`、`schemas`、夹具与向量重生成；`control-core` links / sources 按 scope 签发；`relay-kernel` 边缘 `ro` 门；个人中转 `links.create` 收 `scope.workspaceId / sessionId / readOnly`                                          | cloud-api §14「断言范围与团队声明」；协议包 `0.3.0`、两 app `0.3.0`（协议 `1.1`） | —    | 夹具全过；边缘 `ro` 对非安全方法 403、对身份域放行；`personal-e2e` 加「工作区范围链接」                                                                                | Opus   | M    |
| A7-4 | Armadra  | `Armadra-worktrees/share-scope`    | 迁移 `0045_grant_scope.sql`、`core/identity/{scopes,authorize,route-access}.ts`（`resource` 维度）、`core/identity/cloud/login.ts`（按 `scp` / `role` 同步授予、租约）、`core/identity/accounts.ts`（邀请 `target_session_id`）、`core/sources/share-links.ts`（scope 透传）、`ShareDialog`（范围：整台 / 工作区 / 会话；只读开关）、`i18n/sharing.ts`、契约 §56 | §56「断言范围、授予租约与会话分享」，minor `1.28`；钉协议包 `0.3.0`               | 0045 | 单测：`scp` 空 = 旧行为逐字；会话查看者只能读那一条终端 / ACP、画布 403；租约到期授予失效；撤链接连同访客停用；fixtures 对偶；A 档 `link-join` 加会话范围              | Opus   | L    |
| A7-3 | Armadra  | `Armadra-worktrees/share-origins`  | `AccountsSharingPage`（邀请 / 成员加「签发方」列）、`DevicesPage`（按来源折叠）、`i18n`                                                                                                                                                                                                                                                                          | 读现有字段（`provider`、`createdVia`），无契约                                    | —    | 页面测试；i18n 守卫                                                                                                                                                    | Sonnet | S    |

### 6.3 第三波（SaaS M1）

| 包   | 仓库     | worktree                             | 文件边界                                                                                                                                                                                                          | 契约 / 版本                                                                                     | 迁移          | 测试与验收                                                                                         | 模型   | 规模 |
| ---- | -------- | ------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------- | ------------- | -------------------------------------------------------------------------------------------------- | ------ | ---- |
| C8-1 | cloud    | `armadra-cloud-wt/saas-accounts`     | `apps/cloud/src/adapters/postgres/{accounts,devices,sessions,keys}.ts`、`adapters/redis/{limits,revocation,presence}.ts`、`auth/{device-code,email-verify,password-reset}.ts`、`mail/`、`signing/`、`cli.ts keys` | cloud-api §2 从 501 变实现；版本 `0.4.0`                                                        | PG 0001       | 集成：migrate 拒绝条件四种、注册 / 登录 / 刷新旋转 / 设备码 / Cookie + CSRF；`contract:check`      | Opus   | XL   |
| R3   | cloud    | `armadra-cloud-wt/relay-redis`       | `apps/relay/src/control/redis.ts`、`edge/internode.ts`、`saas/`                                                                                                                                                   | —                                                                                               | —             | `control/redis.test`、`two-nodes`（需 Redis）                                                      | Opus   | L    |
| C8-2 | cloud    | `armadra-cloud-wt/saas-orgs-sources` | `adapters/postgres/{sources,orgs,teams,grants,links,invitations}.ts`、`orgs/`、`teams/`、`grants/`、断言签发按 `source_grants` 合成 `scp` / `role`                                                                | cloud-api §15「团队与主机授予」（`orgs.teams.*`、`sources.grants.*`、`invitations.*`）；`0.5.0` | PG 0002、0003 | 集成：链接并发 10 对 5；团队授予 → 断言声明；撤授予 → `revoked:jti` + kick；`pnpm e2e --mode saas` | Opus   | L    |
| C8-3 | cloud    | `armadra-cloud-wt/saas-stream`       | `stream/{me-stream,relay-ctl}.ts`、`sources/relay-nodes.ts`                                                                                                                                                       | —                                                                                               | —             | `me-stream` 事件 2 秒内到                                                                          | Sonnet | M    |
| C8-4 | cloud    | `armadra-cloud-wt/saas-jobs`         | `jobs/`（PG 队列、worker）、`audit/`、用量汇总、清理任务                                                                                                                                                          | —                                                                                               | PG 0004       | `SKIP LOCKED` 并发；崩溃后重投                                                                     | Sonnet | M    |
| A7-5 | Armadra  | `Armadra-worktrees/saas-client`      | `core/sources/service.ts`（`assertionFor` saas 分支、`remoteAdd saas` 设备码、`remoteDevicePoll`）、`RemoteAccessPage`（SaaS 入口与设备码）、手机「添加连接 → SaaS」、`i18n/remote.ts`                            | 契约 §33 的 `not_implemented` 退场记在 §57 追加；minor `1.29`                                   | —             | 单测；dev-stack `platform` profile；探针 `relay-roundtrip`                                         | Sonnet | M    |
| V3   | Armadra  | `Armadra-worktrees/probes-platform`  | `tools/probes/{personal-roundtrip,relay-roundtrip,multi-source,link-join}.mjs` 加 `--issuer` 与多路由用例、`tools/ci/e2e.d/`                                                                                      | —                                                                                               | —             | `pnpm platform:e2e`；对 `armadra-personal.example.org` 跑一次（只读探针）                          | Sonnet | M    |
| D2   | 用户操作 | —                                    | §5.7                                                                                                                                                                                                              | —                                                                                               | —             | `pnpm e2e --mode saas` 指向线上                                                                    | —      | —    |

**依赖图**：R6 → D1；C7-1 → C7-2 → A7-4（A7-4 还要 A7-2 的 0044 先合，取 0045）；C7-1 → C8-1 ∥ R3 → C8-2 → C8-3 ∥ C8-4；C8-2 → A7-5 → V3 → D2。A7-1、A7-2、A7-3 与 cloud 仓无依赖。

### 6.4 编号预分配汇总

| 仓库    | 契约                                                                                      | 协议 / 版本                                                                                                     | 迁移                                                                           |
| ------- | ----------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------ |
| Armadra | §55 多路由源表；§56 断言范围、授予租约、会话分享；§57 SaaS 入口（`not_implemented` 退场） | minor 1.27 / 1.28 / 1.29                                                                                        | 0044 `client_source_routes`；0045 `grant_scope`                                |
| cloud   | cloud-api §13 反向代理；§14 断言范围与团队声明；§15 团队与主机授予                        | 协议包 0.2.2（R6）→ 0.2.3（C7-1）→ 0.3.0（C7-2，协议 1.1）→ 0.4.0（C8-1）→ 0.5.0（C8-2）；app 与协议包 lockstep | PG 0001 accounts、0002 sources、0003 orgs_teams_links、0004 jobs、0005 billing |

## §7 需要用户决定的问题

| #   | 问题                                                                                                                                                       | 默认（实现先按它走）                                       |
| --- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------- |
| Q1  | SaaS 源地址用子域（`*.src.armadra-saas.example.org`，要泛域名 DNS + DNS-01 证书，需提供 DNS 服务商 API 给 1Panel）还是路径 `/s/<id>`（各源共享浏览器来源） | 子域                                                       |
| Q2  | 49 服务器上 Redis（`0.0.0.0:6379`）与 MySQL（`0.0.0.0:3306`）对公网开放：是否授权改为只绑回环并设口令；SaaS 的 Redis 用独立容器                            | 授权加固；独立容器                                         |
| Q3  | 公网个人中转是否等 1Panel 证书签发后再给客户端登记（过渡期只做内部冒烟，避免指纹切换导致全部客户端重登记）                                                 | 等证书                                                     |
| Q4  | 数据面暂留 TypeScript，按 §4.5 的三个触发条件再评估 Go；现在只把 JSON Schema 导出与夹具做进 C7-1                                                           | 接受                                                       |
| Q5  | SaaS 邮件走服务器上已有的 `mailserver` 应用还是外部 SMTP；M1 是否要求邮箱验证后才能登记主机 / 签链接                                                       | 外部 SMTP 待定，先用 mailserver 做开发验证；要求验证（开） |

附：启动是否直接进入上次服务做成了开关（`services.autoEnter`，缺省关），不单独请示。
