# 离线文件传输 v2 设计（等待对方上线 + 自动续传）

- 日期：2026-10-08
- 状态：已评审，待实施
- 取代：`docs/superpowers/specs/2026-09-24-offline-file-transfer-design.md`（方案 C 已被回滚，见下）
- 相关：ADR-004（消息可靠性与 outbox）、ADR-005（滑动窗口与位图续传）、ADR-009（在线态与连接解耦）

## 1. 问题

对方离线时，聊天消息会进 outbox 排队、等对方上线自动投递；**文件传输不会** —— 它当场失败。

用户可见的现象：点发送文件 → 任务条目一闪即红（`失败`），错误是 `tcp: dial …` 或 `transfer: no known address for peer …`；等对方上线后什么都不会发生（任务已进 `failed` 终态，前端 `TERMINAL` 把它排除在活动之外，永远不会再动）。

目标：**对方离线时点发送，任务进入「等待对方上线」；对方上线后自动开始传输；传到一半对方掉线也回到等待，对方回来后按位图续传。等待状态可持久化，重启后继续等。**

## 2. 现状（已核实）

| 事实 | 位置 |
| --- | --- |
| 三条失败路径：无地址 / `tcp: dial` 失败 / 等 `FILE_META_ACK` 超时(10s) | `internal/app/transfer_app.go:152-168`、`:223-226`、`:258-265` |
| 失败即终态：落库 `failed` + 发 `transfer:error` | `transfer_app.go:158-162` |
| 前端 `TERMINAL={done,failed,cancelled}`，终态任务不再接受进度更新 | `frontend/src/stores/transfer.ts:18`、`:110` |
| 先抢信号量、再**整文件哈希**、最后才拨号 | `transfer_app.go:170`（sem）→ `:182`（hash）→ `:223`（dial） |
| 传输侧**没有任何总线订阅** | `transfer_app.go` 无 `bus.Subscribe` |
| `TopicTransferState` 已定义但全仓无人 Publish | `internal/infra/eventbus/eventbus.go:18` |
| 并发只有阻塞信号量 `sendSem`（默认 3），无队列、不落库、重启即蒸发 | `transfer_app.go:50/112/170-175` |
| `ListActive()` 注释写"供启动时全局恢复"，**无任何调用方** | `internal/adapters/store/sqlite/transfers.go:91-109` |
| **没有任何取消能力**（`func ...Cancel(` 全仓 0 匹配） | — |

### 2.1 为什么不能复用消息 outbox（09-24 方案的死因）

那份设计让 `trySend` 拦截 file 消息、等对方上线重发。实施记录（`docs/superpowers/implementation-plans/2026-09-24-offline-file-transfer-implementation.md:516-546`）记录了它为什么被删：拦截导致消息**从不经过 `sess.Send`**，对端看不到卡片也拿不到 ACK → outbox 无限重试 → 每次重试造一条假的 `file_failed`（FilePath 为空 → `os.Stat("")` → "文件已被删除或移动"）→ 接收方聊天框堆满裸 JSON。

**结论：文件消息（卡片）继续走 outbox，文件本体由 TransferApp 自己重试。** 本次设计严格遵循这条边界。

## 3. 设计

### 3.1 状态与落库：不动状态机，不改 CHECK

领域态**复用 `StateIdle`**（"还没开始"），`transfer.State` 与 `transitions` 一个字不改：

```go
StateIdle: {StateMetaExchange, StateCancelled},   // → 对方上线开跑 / 用户取消
```

DB 侧 `status` 继续用 `'queued'`（`StateIdle.DBStatus()` 的既有映射，CHECK 里已有该值），从而**避免 SQLite 改 CHECK 必须重建表的数据搬迁**。迁移 **v3** 只做两次 `ALTER TABLE ADD COLUMN`：

| 列 | 类型 | 含义 |
| --- | --- | --- |
| `wait_reason` | `TEXT NOT NULL DEFAULT ''` | `'peer_offline'`；空串留给未来的并发额度排队 |
| `next_try_at` | `INTEGER NOT NULL DEFAULT 0` | 与 `outbox.next_try_at` 同构的退避时间；0 = 不在等待 |

`wait_reason` 不是为了文案：**自动重试必须只挑 `peer_offline`**。将来加「用户暂停」时，绝不能被 sweep 自动拉起。

清理策略天然兼容：`PurgeFinished` 只删 `done/failed/cancelled`（`transfers.go:151`），等待中的 `queued` 不受影响。

### 3.2 失败分类：等待 vs 真失败

主判据是**「对端还可达吗」**，不是字符串匹配（错误文案会变）：

- 发起时：`dir.Get(peer)` 为 `offline/unknown/blocked` → **直接等待，连哈希都不做**；`file_size` 用 `stat` 拿，`file_hash` 留空，等真开始传时再算。
- 尝试中：拨号失败、`SessionOf` 失效、会话写失败、等 `FILE_META_ACK` / `FILE_DONE_ACK` 超时 → **同样是等待**。
- 兜底才看错误文本（`no known address`、`transfer: timeout waiting for`）——用于 peer 目录显示在线但对端其实已死的情况。

真失败保持原样，**不掩盖问题**：文件被删/不可读、权限、磁盘满、校验失败（`peer verify failed`）、协议非法（`invalid meta`）→ `failed`。

分类实现为**纯函数**（`internal/app` 或 `internal/domain/transfer` 内的无 I/O 判定），以便表驱动测试，且明确列出"哪些错误必须仍然是 failed"。

### 3.3 调度：复用现有驱动，幂等优先

- `TransferApp.Start(ctx)`：订阅 `TopicPeerOnline`（TCP 握手成功）与 `TopicPeerDiscovered`（对方刚启动、我们还没连）→ 立即尝试该 peer 的等待任务。
- 兜底：挂进 `bootstrap` 里已有的 1 秒 ticker（`bootstrap.go:272-289`，现在跑 `SweepOutbox`），调 `TransferApp.SweepWaiting(ctx)`。
- 退避：1s → 2s → 4s → … → 5min 封顶，与 `chat_app.backoff()` 同参数。
- **幂等守卫（两条）**：
  1. 每个 job 在途只跑一个 —— 用既有的 `a.senders[jobID]` 判在途，重复触发直接跳过；
  2. 同一 peer 每轮最多启动一个等待任务 —— 否则对方一上线，10 个等待任务同时开始全文件哈希，磁盘先崩；其余的下一轮（或下一个 tick）再来。
- **重试用同一个 `job_id`**（不新造 id）：进度条、聊天卡片、传输列表三处才对得上同一条任务。接收侧 `HandleFileMeta` 按 `(peer, file_hash)` 继承临时文件与位图（`:443-461`），与发送侧的 job_id 无关，因此复用 id 不会破坏续传。

### 3.4 中断续传

传一半对方掉线 → 同一分类 → 等待 → 对方回来 → `SendFileAs(ctx, peer, path, 同一 jobID)`：重发 `FILE_META`（全量 `chunk_hashes`）→ 对端 `FindResumable` 命中 → 回全量位图 → 只补缺失块。这条链路 ADR-005 已建好并有 e2e（`test/e2e/e2e_test.go:189`），本次只是把"当场失败"改成"等一会再试"。

**成本**：重试会重算一次整文件 SHA-256（`FILE_META` 必须带全量 chunk_hashes）。本次**不**持久化 chunk_hashes（10GB 文件约 320KB/任务，且必须配 mtime 校验才安全），先接受"一次等待 ≈ 一次哈希"，真痛了再加列。

### 3.5 重启恢复

启动时（`bootstrap`）：

1. `ListWaiting()` —— `direction='send' AND status='queued' AND wait_reason='peer_offline'` → 捡起来立即尝试一轮；
2. 收尾崩溃残留的 `active` 任务：发送侧转入等待；接收侧标 `failed("传输被中断（应用已退出）")`。
   - 接收侧标 failed **不影响续传**：`FindResumable` 只排除 `done/cancelled`（`transfers.go:70`），failed 行照样被继承；
   - 这顺手修掉一个既有僵尸行问题：进程崩过一次，那条任务会永远显示"传输中"。

`ListWaiting` 是新端口方法，**不复用 `ListActive`**：后者包含 `failed/paused/verifying`，拿它做自动重试会把不该重试的任务一起拉起来。

### 3.6 界面

- 新增 `transfer:state` 事件（Publish 到既有的 `TopicTransferState`），载荷 `{jobId, status, reason}`；事件桥转发为前端 `transfer:state`，store 新增 `applyState`。
- `transferStatus()` 增加「等待对方上线」（warn 色、非终态）。判据是 `waitReason === 'peer_offline'` 而**不是** status：等待态与"还没轮到"共用 `idle/queued`。
- 注意界面文案有**两份 switch**（`frontend/src/utils/format.ts` 与 `ChatBubble.vue` 的本地 computed），本次两处都要加分支 —— 这是既有的重复，本次不顺手合并它。
- **取消 / 立即重试两个按钮只对「等待中」的任务出现**：正在传输的任务会被后端明确拒绝取消（当前 sender 没有中断点，假取消比拒绝更糟），所以干脆不在它上面放这个按钮 —— 按钮点了没反应是最招人烦的那种交互。
- 传输列表新增两个按钮：
  - **取消** → 新 RPC `CancelTransfer(jobId)`：置 `cancelled`、清 `next_try_at`、不再重试（需要新端口方法，因为当前全仓没有取消能力）；
  - **立即重试** → 新 RPC `RetryTransfer(jobId)`：把 `next_try_at` 置为现在，等 sweep 拾取（同一 job）。
- 对方上线后**自动开跑时不提示**（不做通知）。

## 4. 明确不做

1. **并发排队位置**（架构书 972-977 的 `queued` 队列 + UI 排队序号）——本次不实现，但 `wait_reason` 为它留好了位置。
2. **持久化 chunk_hashes 缓存** —— 见 3.4。
3. **取消/失败时给对方发 `file_failed` 消息** —— 现状就是由传输面板承担反馈。
4. **群聊文件传输** —— 前端已有"文件传输暂不支持群聊"的明确拦截，与本次无关。

## 5. 风险

| 风险 | 应对 |
| --- | --- |
| 等待任务无限堆积（用户离线时连点 100 个文件） | 每 peer 每轮只启动一个；`PurgeFinished` 不动它们；列表按 `updated_at` 排序，退避重试会刷新该字段 |
| 重试触发过密导致重复哈希 | 退避 + 在途守卫 + 只在 `peer.online` / 退避到点时尝试 |
| `peer.online` 在空闲回收后会反复发（`manager.go:378`） | 在途守卫与 `next_try_at` 共同兜住 |
| 陈旧 `active` 行被误当"正在传" | 启动收尾（3.5） |
| 长等待任务落到 `ListRecent` 的 50 条窗口外 | 退避重试会刷新 `updated_at`；仍出窗口时列表看不到但任务照常重试（已知小瑕疵，可后续让 `ListRecent` 优先返回未结束任务） |

## 6. 验收标准

1. B 离线时 A 发文件：任务显示「等待对方上线」，**不做整文件哈希**，不报错。
2. B 上线：无需任何用户操作，文件自动传完，进度与完成事件都落在**同一个 `job_id`** 上。
3. A 与 B 传一半时 B 进程退出：任务转「等待对方上线」；B 回来后按位图续传（只补缺失块，不重传全量）。
4. A 在等待中点「取消」：任务变 `cancelled`，对方上线后不再自动传输。
5. A 重启：等待中的任务仍在列表且自动继续等；崩溃残留的 `active` 任务不再显示"传输中"。
6. 文件被删除/不可读时仍然立即 `failed`，不进入等待。
7. 真失败（校验失败、协议非法）不被误判成"等待"，不会无限重试。
