# 同对端并行传输（D-11）· 实施计划

**日期**：2026-10-15
**依据**：`docs/plans/2026-10-15-parallel-transfer-design.md`
**提交节奏**：每个 Task 一组改动，自成回滚锚点。（本仓约定：不由工具执行 git 命令，提交由人来做。）
**无新增 ADR**：不新增协议帧、不改线上格式。

---

## T1 调度器：取消每对端串行 + 槽位先占 + 释放补位

- [ ] `internal/app/transfer_waiting.go`：`startJobs` 删除 `busyPeers` 过滤与 `busy[peer]` 记账；
      改为 `trySendSlot()` 占位，占不到即 `break`（其余留给下一轮）；`busyPeers()` 删除。
- [ ] `internal/app/transfer_app.go`：
      - `sendFile` 增加 `slotHeld bool`；为 false 时保持阻塞获取 + `defer` 归还。
      - `SendFileAs` / `RetryNow` 传 `false`（直连路径语义不变）。
      - 新增 `trySendSlot()` / `releaseSendSlot()`；`releaseSendSlot` 顺带 `kickSweep()`。
      - 新增 `kick chan struct{}`（容量 1）、`kickSweep()`、`schedulerLoop(ctx)`；`Start(ctx)` 启动该循环。
- [ ] `internal/app/transfer_folder.go`：改掉「整批天然串行 / 同一对端同时只传一个」的过时注释。

## T2 ACK 投递的 stopped 守卫

- [ ] `internal/app/transfer_app.go`：`sendJob` 增加 `stopped chan struct{}` + `stoppedOne sync.Once` +
      `markStopped()` / `deliverAck()` / `deliverDone()`；`sendFile` 构造 `sj` 时初始化，
      `defer sj.markStopped()` 与 `defer a.dropSender(...)` 同处。
- [ ] `HandleFileMetaAck` / `HandleFileChunkAck` / `HandleFileDoneAck` 改用 `deliverAck` / `deliverDone`。

## T3 早退路径不再用零值覆盖整行 + 取消检查前移

- [ ] `internal/app/transfer_app.go`：`GetJob` 之后 `if prevErr == nil { job = prev }`。
- [ ] `sendFile` 拿到槽位后、哈希前插 `isCancelling` 检查。

## T4 测试

- [ ] `internal/app/transfer_parallel_test.go`（新）：
      - 同一对端两条等待任务都被拉起（对照：今天只起一条）。
      - 额度为 1：只起一条；归还槽位后起下一条。
      - 同一会话两个 job 的 ACK / 位图按 `job_id` 隔离。
      - sender 退出后满缓冲的 ACK 投递不阻塞。
      - 取消标记下的任务拿到槽位后立刻落 cancelled 且不丢行内字段。
      - 重试时源文件被删：行落 failed 且批次字段仍在。
- [ ] `internal/app/transfer_waiting_test.go`：更新 `TestStartJobsOnlyPicksAutoStartable`
      里「刻意用不同对端」的过时注释（不再需要）。
- [ ] `test/e2e/parallel_test.go`（新）：同一对端同时发两个文件都完成且内容一致。

## T5 文档与配置注释

- [ ] `internal/adapters/config/config.go`：`MaxConcurrent` 字段注释收正为「全局并发，不再有每对端限制」。
- [ ] `README.md`：`[transfer]` 参数说明补一句 `max_concurrent` 的新语义。
- [ ] `docs/plans/2026-10-14-PRD差距与待开发清单.md`：
      - D-11 行改 ✅（2026-10-15），指向 §2.15；
      - §2.2 表格新增 **D-41**（接收侧无并发上限）；
      - FR-07 行去掉「同一对端串行」的旧描述；
      - §5 批次 H 的备注更新（D-11 已落地）；
      - 新增 §2.15 实施记录。

## 回归

- [ ] `go build ./...`、`go vet ./internal/...`
- [ ] `go test ./... -count=1`、`go test -tags wails ./... -count=1`（含 archtest 红线与 gofmt 闸门）
- [ ] `go test ./test/e2e/... -count=1`
- [ ] `cd frontend && npm run typecheck`
