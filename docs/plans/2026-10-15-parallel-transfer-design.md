# 同对端并行传输（D-11）· 设计

**日期**：2026-10-15
**依据**：`docs/plans/2026-10-14-PRD差距与待开发清单.md` §2.2 D-11（原文：「同一对端串行（`transfer_waiting.go:341-370 busyPeers`），全局默认 3 → 升为会话调度器」）
**范围**：取消「同一对端同时只传一条」的调度限制；把全局并发预算从「起 goroutine 后阻塞抢信号量」升为显式调度；修掉并行之后才会致命的两个前置缺陷。
**不做**：每对端配额、跨对端公平轮转（那是清单里的选项 C，本次明确不做）；接收侧限流（另立 D-41，见 §3）。

---

## 1. 背景与现状（均为实测）

### 1.1 现有的两道闸门

| 闸门 | 位置 | 语义 |
|---|---|---|
| 全局并发 | `TransferApp.sendSem`（`transfer_app.go:104,215`），容量 `transfer.max_concurrent`（默认 3） | 在 `sendFile` 里**阻塞获取**（`:413`），横跨「整文件哈希 → META 协商 → 推块 → DONE」整段 |
| 每对端串行 | `startJobs`（`transfer_waiting.go:354,370,376`） | 「已经有任务在传」的对端**不启动下一条**，只作用于**调度路径**（sweep / peer.online / Recover / 文件夹批次） |

### 1.2 关键事实：每对端串行从来不是协议不变式

它只写在 `startJobs` 里，而**用户直接发起的单个文件走的是另一条路**：`TransferService.SendFile` → `go TransferApp.SendFileAs(...)` → `sendFile` 直接抢全局槽位，**一次都没看过 `busyPeers`**。也就是说，用户多选三个文件发给同一个对端，今天就已在同一对端上并行传输（各自占一个全局槽位）。

把这条写下来有两个作用：一是它说明 D-11 并非「引入一个新能力」，而是把调度路径对齐到既有事实；二是它说明这条规则不是任何人依赖的正确性来源，去掉它的下游风险远小于清单里「风险最高」的初判。清单里「升为会话调度器」的写法保留其意：把这件事从「散落的两个隐式闸门」收进一处显式调度。

### 1.3 协议与适配器已经支持多任务复用一条会话（逐条核实）

- `tcp.session.Send` 有 `writeMu`（`session.go:38,126`）：并发写不会撕帧。
- 同一会话的入站帧在 `pump` goroutine 内**串行**处理（`manager.go:298` 的注释即此意），处理函数里所有 ACK 都按 `job_id` 路由（`getSender(ack.JobID)`，`transfer_app.go:1100,1109,1125,1137`）——帧交错不会串味。
- 接收侧每个任务一个 `recvJob` + 自己的文件句柄；`filesink` 已有「同批并发不抢同名」的处理（`TestCreateDedupesWithinConcurrentBatch`）。
- 全局槽位在**哈希之前**获取，所以「同时进行的哈希数 ≤ N」与今天完全一致：并行化**不会**引入哈希风暴。

### 1.4 顺带查出的两个真实缺陷

**（a）哈希窗口内同一对端可以并行。** `busyPeers()` 只读 `a.senders`，而 sender 在**哈希之后**才登记（`putSender` 在 `:512`）。于是一条任务正在哈希（只占 `starting`，无 sender）时，下一次 sweep 会认为该对端空闲，再起一条 —— 与 `startJobs` 注释里「防哈希风暴：同一对端同一时刻只启动一个」自相矛盾。现有用例 `TestStartJobsOnlyPicksAutoStartable` 用三个**不同**对端，正好绕开了这条规则，所以看不见。

**（b）已退出任务的 ACK 会卡住整条会话。** `HandleFileMetaAck` / `HandleFileChunkAck` / `HandleFileDoneAck` 用**阻塞 send** 把结果投进 `sj.ackCh`（容量 32）/ `sj.doneCh`（容量 1），而它们运行在会话读循环里。一条已经退出的任务若缓冲区满了，会把该对端上**所有**任务（以及心跳、聊天）一起堵死。今天窗口很窄（同一对端只有一条任务），并行之后同一会话上任务变多，这从「理论风险」变成「迟早发生」。

**（c）早退路径会用零值 Job 覆盖整行。** `sendFile` 的命名返回值 `job` 在 `:451` 才被填满；本地检查失败（文件被删 / 变成目录 / 空文件）发生在 `:381-398`，此时 `job` 除 `JobID` 外全是零值，而统一失败出口会 `UpsertJob(job)` —— 一条**重试/排队中**的任务因此丢掉 `file_name` / `batch_id` / `rel_path`（文件夹卡片随即失去相对路径）。这是既有缺陷，但本设计必须一并修：取消检查前移（§2.5）之后，取消会**更常**落在「job 还没填满」的窗口里。

---

## 2. 设计

### 2.1 取消每对端串行

`startJobs` 去掉 `busyPeers` 过滤与 `busy[j.PeerID] = true` 记账，`busyPeers()` 删除。这是本项的行为核心：一个文件夹批次不再「一次只传一个文件」，而是同时跑满可用额度。

`SendFolderAs` / `SendFolderTo` 里「整批天然串行」的注释（`transfer_folder.go:82-85,151-152`）随之改成「同时最多 N 个，其余排队」。

### 2.2 调度器：先占槽位，再起 goroutine

现在的顺序是「到点就起 goroutine → 在 `sendSem` 上阻塞」。去掉每对端串行后，一个几千条的文件夹批次会在一次 sweep 里起几千个 goroutine 全阻塞在信号量上；更糟的是这些任务的**取消只会留个标记**，等它们真拿到槽位时整文件哈希已经白算了一遍。

改为：

```go
// 非阻塞占位；失败表示额度已满
func (a *TransferApp) trySendSlot() bool
func (a *TransferApp) releaseSendSlot()

// startJobs 逐条占位，占不到就【停止本轮】，其余留给下一轮
for _, j := range jobs {
    if !j.WaitReason.AutoStartable() { continue }
    if !a.claimStarting(j.JobID) { continue }
    if !a.trySendSlot() { a.releaseStarting(j.JobID); break }  // 额度满：本轮到此为止
    go func(job transfer.Job) {
        defer a.releaseStarting(job.JobID)
        defer a.releaseSendSlot()   // 槽位由调度器预留，由这里归还
        _, _ = a.sendFile(ctx, job.PeerID, job.LocalPath, job.JobID, false, true)
    }(j)
}
```

`sendFile` 增加入参 `slotHeld bool`：

- `slotHeld == false`（直连路径：`SendFileAs`、`RetryNow`）：自己阻塞获取、自己 `defer` 归还 —— 用户点的按钮最终一定会跑，这是既有语义，不改。
- `slotHeld == true`（调度路径）：不再获取、也不归还（由调用方的 goroutine 归还）。

这样一来，「起 goroutine 的数量」与「并发额度」严格相等，队列里几千条任务不再制造几千个阻塞的 goroutine。

### 2.3 槽位一释放就补位（kick）

只有 sweep 的 1 秒节拍的话，额度虽为 N，补位却要等下一拍。对「10 个小文件」这种场景，节拍而不是带宽才是瓶颈。

新增 `kick chan struct{}`（容量 1）+ `kickSweep()`（非阻塞投递，天然合并）：`releaseSendSlot` 时投一次；`Start(ctx)` 起一个循环 select `a.kick` 与 `ctx.Done()`，收到就走一次 `SweepWaiting`。

不会自旋：每轮至少启动一条**新的**等待任务，而任务一旦启动就会离开等待集合（转 done / failed / 带退避的等待），集合单调缩小；没有到点任务时不会有槽位释放，也就不会有 kick。

### 2.4 ACK 投递加 stopped 守卫

`sendJob` 增加 `stopped chan struct{}`（`sync.Once` 关闭），在 sender goroutine 退出时关闭（紧挨 `dropSender`）。三处投递改为：

```go
select {
case sj.ackCh <- ck:   // 活着：照旧投（不丢帧）
case <-sj.stopped:     // 已退出：直接放弃，绝不阻塞会话读循环
}
```

`doneCh` 同理。**不做**「缓冲区满就丢弃」的写法：位图 ACK 是累积全量、丢一帧虽然无害，但 META 阶段的 `reject` 是一次性信号，丢不起；用 `stopped` 精确区分「消费者已死」与「消费者暂时慢」。

### 2.5 取消检查前移 + 早退不再零值覆盖（§1.4c）

- `sendFile` 在拿到槽位后、开始哈希前插一次 `if a.isCancelling(jobID) { return job, errJobCancelled }`：省掉一次注定白费的整文件哈希。
- `sendFile` 在 `GetJob` 之后立刻 `if prevErr == nil { job = prev }`：让统一失败出口写出的终态行**保留**书签式的身份字段（文件名、批次、相对路径、进度）。后续 `:451` 的整份字面量赋值不受影响。

### 2.6 配置与文案

`transfer.max_concurrent` 的含义从「全局并发（且隐含每对端 1）」收正为「**全局**并发，不再有每对端限制」。设置页已是「全局并发上限」，队列浮层的 tooltip 已是「只约束本机发起的传输」——两处文案本就与新语义一致，无需改动；需要改的是 `config.go` 的字段注释与 README 的参数表注解。

---

## 3. 代价、不做项与已知风险

**必然的代价（选项 B 的既定取舍，写进文档而不是藏起来）**

1. 一个文件夹批次现在同时传 N 个文件：`.part` 文件数、峰值磁盘写入、接收侧并发校验数都会上升。
2. 单一对端可以把全局额度占满（大文件夹 + 另一对端的一个小文件，小文件要等）—— 跨对端公平是选项 C，本次不做。
3. 同一对端上传入帧变多，`pump` 的单线程处理成为新的观测点（§2.4 已消除其中唯一的阻塞点）。

**不做**

- 每对端配额（可配并发数）。
- 跨对端公平轮转 / 加权。
- 接收侧限流：接收侧每帧成本很低（`WriteAt` + 计数），真正重的是 DONE 时的整文件校验；而「拒收」会改变对端可见行为，值得独立决策。**登记为 D-41**（清单 §2.2 表格）。

## 4. 风险

| 编号 | 风险 | 处置 |
|---|---|---|
| P-1 | 并行后同一对端的帧交错导致串味 | 已核实按 `job_id` 路由 + `writeMu`；补两条测试（路由隔离、真机并行 e2e） |
| P-2 | 调度器「占不到就停本轮」让批次推进依赖 sweep 节拍 | §2.3 的 kick 把补位收敛到「槽位一空即补」，并保留 1s sweep 兜底 |
| P-3 | kick 引发自旋 | 合并投递（容量 1）+ 等待集合单调缩小；实现与验证见 §2.3 |
| P-4 | 早退零值覆盖的修复改变终态行内容 | 只影响「曾经写坏」的那几行；补一条「重试时源文件被删，行不丢批次字段」的用例 |
| P-5 | 直连路径仍会阻塞等槽位（多选几十个文件时几十个 goroutine） | 与今天完全一致，不在本次范围内；前端多选发送本就是逐个 RPC |

## 5. 验收（可执行）

1. `go test ./... -count=1`、`go test -tags wails ./... -count=1`、archtest（三条红线 + gofmt）全绿。
2. 单元：
   - 同一对端的两条等待任务在 `startJobs` 后**都被拉起**（今天只起一条）。
   - 额度为 1 时同一对端只起一条；归还槽位后能起下一条。
   - 同一会话上两个 job 的 ACK / 位图互不串味（按 `job_id` 回帧）。
   - sender 退出后，满缓冲的 ACK 投递不再阻塞（会话读循环不被卡住）。
   - 处于取消标记下的任务在拿到槽位后立刻落 cancelled，且**不丢**行内字段。
   - 重试时源文件已删：行落 failed 且 `file_name` / `batch_id` / `rel_path` 仍在。
3. e2e：同一对端同时发两个文件，两个都完成且内容一致；一个文件夹批次在并行下仍全部完成并正确还原目录树。
4. 前端 `npm run typecheck` 通过（本项无前端改动）。
