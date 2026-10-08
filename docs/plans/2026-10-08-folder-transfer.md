# 文件夹传输实施计划

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 选中一个文件夹即可发送：发送端把目录枚举成一批「排队任务」（每个文件一条，空目录一条），复用既有的等待队列与三个拉起触发点；接收端按相对路径逐层清洗并在下载目录里还原出同样的目录树；整批在聊天框表现为一条可整体取消的聚合卡片。

**Architecture:** 不引入新调度：目录枚举后写入的轻量任务状态为 `StateIdle` + 新等待原因 `WaitQueued`，由既有的 `SweepWaiting`（1 秒节拍）/`WakePeer`（对端上线）/`Recover`（启动）拉起，并且 `startJobs` 已经保证「同一对端同时只传一个」。协议只在 `FileMeta` 上追加三个 JSON 字段（`batch_id`/`rel_path`/`is_dir`），DB 只追加 v4 迁移的三列。接收端在 `FileSink` 上新增 `CreateIn`，把相对路径逐段清洗后落到批次根目录下（根目录只分配一次，重启靠 `AdoptRoot` 认领）。

**Tech Stack:** Go 1.26、SQLite（`modernc.org/sqlite`，迁移带 checksum）、进程内事件总线、Wails3 beta.24、Vue 3 + Pinia + TypeScript。

**设计与依据：** `docs/plans/2026-10-08-folder-transfer-design.md`

---

## 前置约定

- 所有命令都在仓库根目录 `d:\code\OpenSource\swarm-link` 执行（前端命令先 `cd frontend`）。
- 提交信息沿用仓库风格：`feat(transfer): 中文描述`、`feat(chat): …`、`docs: …`。**每个 Task 结束都提交**（分任务的提交历史是回滚锚点）。
- **提交纪律（重要）**：本计划所有 `git add` 都写成**显式文件列表**，不要用 `git add <目录>` —— 工作区里有不属于本功能的未跟踪文件（`frontend/tests/message.test.ts`、`swarmlink-1.png`、`swarmlink-2.png`）与一个被改动过的 `frontend/package-lock.json`，目录级 add 会把它们扫进本功能的提交。
- **既有失败基线（2026-10-08 实测，Task 1 后修正）**：`go test -tags wails ./...` 有 **4 个与本次无关的失败**，全部属于「文件名安全化」那条既有工作线 —— `internal/infra/fsutil` 的 `TestSanitizeFileNameRejectsUnsafeNames`、`internal/adapters/filesink` 的 `TestCreateRejectsUnsafeName`、`internal/adapters/keystore` 的 `TestLoadOrCreateIsStableAndPersists` / `TestLoadOrCreateCreatesDirWith0700`。本次会改 `fsutil` 与 `filesink`，但**不要去修这些既有失败**；判断"是否弄坏了什么"时只看它们之外的输出。
- **验证必须跑全仓，不能只跑「受影响的包」**（Task 1 的教训）：给 `ports` 里的接口加一个方法，会波及【所有】实现者，包括散落在各测试文件里的测试替身 —— 只跑 `store`/`domain`/`app` 会漏掉 `internal/adapters/wails` 里那个替身，结果是 `go build` 通过而 `go vet ./internal/...` 与 `go test ./...` 全红。因此每个 Task 的验证命令固定为：

  ```
  go test -tags wails ./... -count=1
  ```

  只有在开发中的快速反馈阶段才用 `-run` 缩小范围。
- 前端 `npm run typecheck` 当前**是干净的**，必须一直保持干净。前端测试用 `npm run test`（Node 内置 test runner，`node --test "tests/**/*.test.ts"`，**不是** vitest）。
- 绑定产物 `frontend/bindings/` 已入库。重新生成的正确命令（`-ts` 与 `-i` 不能省，cmd.exe 下必须双引号）：

  ```
  wails3 generate bindings -f "-tags wails" -clean=true -ts -i .
  ```

  生成后 `git status --short -- frontend/bindings` 应当只出现本次新增/改动的方法。
- 测试里**不要新造构造脚手架**：Go 侧沿用各文件既有写法（`transfer_waiting_test.go` 用 `clock.NewFake(...)` + `mem.NewPeers(...)` + `NewTransferApp(identity.NodeID{}, repo, sink, conns, dir, eventbus.New(), clk, nil)`；`sqlite_test.go` 用其顶部的建库辅助）—— 新造一套会让后续维护面对两套时好时坏的行为。
- 任务顺序有依赖：Task 1 必须最先（字段与迁移），Task 5 在 Task 7 之前（sink 能力先于接收端使用），Task 11 在 Task 12/13 之前（绑定先于前端调用）。

---

## Task 1: 领域字段、迁移 v4 与仓储

**Files:**
- Modify: `internal/domain/transfer/job.go`
- Modify: `internal/adapters/store/sqlite/migrations.go`（在 v3 之后追加）
- Modify: `internal/adapters/store/sqlite/transfers.go`
- Modify: `internal/domain/ports/ports.go`（`TransferRepo` 加 `ListByBatch`）
- Modify: `internal/adapters/store/mem/`（同步实现 `ListByBatch`）
- Test: `internal/adapters/store/sqlite/sqlite_test.go`

**Step 1: 写失败的测试**

在 `sqlite_test.go` 里追加（放在既有 transfer 用例附近，沿用本文件既有的建库辅助与 nodeID 写法）：

```go
// 批次字段必须能落库再读回来：目录树还原、批次聚合进度、整体取消都靠它们。
func TestJobBatchFieldsRoundTrip(t *testing.T) {
	db := newTestDB(t) // 沿用本文件既有构造
	repo := NewTransfers(db)

	id := mustNodeID(t)
	job := transfer.Job{
		JobID:       "job-batch-1",
		PeerID:      id,
		FileName:    "a.jpg",
		FileSize:    1024,
		ChunkSize:   512,
		TotalChunks: 2,
		Direction:   transfer.DirectionSend,
		LocalPath:   `C:\照片\2024\a.jpg`,
		Bitmap:      transfer.NewChunkBitmap(2),
		Status:      transfer.StateIdle,
		WaitReason:  transfer.WaitQueued,
		BatchID:     "batch-1",
		RelPath:     "照片/2024/a.jpg",
		IsDir:       false,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := repo.UpsertJob(job); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := repo.GetJob(job.JobID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BatchID != "batch-1" {
		t.Errorf("batch_id = %q，期望 batch-1", got.BatchID)
	}
	if got.RelPath != "照片/2024/a.jpg" {
		t.Errorf("rel_path = %q，期望原样读回", got.RelPath)
	}

	// 空目录条目：is_dir 必须真的存成 1，否则接收端不会建目录
	dirJob := job
	dirJob.JobID = "job-batch-2"
	dirJob.FileName = "空的"
	dirJob.FileSize = 0
	dirJob.TotalChunks = 0
	dirJob.RelPath = "照片/空的"
	dirJob.IsDir = true
	if err := repo.UpsertJob(dirJob); err != nil {
		t.Fatalf("upsert dir: %v", err)
	}
	gotDir, err := repo.GetJob("job-batch-2")
	if err != nil {
		t.Fatalf("get dir: %v", err)
	}
	if !gotDir.IsDir {
		t.Error("is_dir 读回为 false，空目录会丢失")
	}

	// 更新回零值也要正确（msOf/timeOf 的零值往返是既有坑）
	dirJob.IsDir = false
	dirJob.BatchID = ""
	dirJob.RelPath = ""
	if err := repo.UpsertJob(dirJob); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	back, err := repo.GetJob("job-batch-2")
	if err != nil {
		t.Fatalf("get back: %v", err)
	}
	if back.IsDir || back.BatchID != "" || back.RelPath != "" {
		t.Errorf("零值往返失败：isDir=%v batch=%q rel=%q", back.IsDir, back.BatchID, back.RelPath)
	}
}

// ListByBatch 是批次聚合与整体取消的唯一查询入口。
func TestListByBatch(t *testing.T) {
	db := newTestDB(t)
	repo := NewTransfers(db)
	id := mustNodeID(t)

	base := transfer.Job{
		PeerID: id, ChunkSize: 512, Direction: transfer.DirectionSend,
		Status: transfer.StateIdle, WaitReason: transfer.WaitQueued,
		BatchID: "b1", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	// 同批三条 + 别的批一条 + 单文件一条
	for _, spec := range []struct{ jobID, rel string }{
		{"j1", "照片/a.txt"}, {"j2", "照片/b.txt"}, {"j3", "照片/子/c.txt"},
	} {
		j := base
		j.JobID, j.RelPath = spec.jobID, spec.rel
		if err := repo.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}
	other := base
	other.JobID, other.BatchID, other.RelPath = "j9", "b2", "别的/x.txt"
	if err := repo.UpsertJob(other); err != nil {
		t.Fatal(err)
	}
	single := base
	single.JobID, single.BatchID, single.RelPath = "j10", "", ""
	if err := repo.UpsertJob(single); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ListByBatch("b1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("条数 = %d，期望 3（不能把别的批或单文件算进来）", len(got))
	}
	// 顺序按 created_at：枚举顺序即发送顺序，界面与后续确认都依赖它
	if got[0].RelPath != "照片/a.txt" {
		t.Errorf("首条 rel_path = %q，期望按创建顺序", got[0].RelPath)
	}
}

// 排队中的任务必须被 ListWaiting 一起返回，否则对方上线后整批不会自动开始。
func TestListWaitingIncludesQueued(t *testing.T) {
	db := newTestDB(t)
	repo := NewTransfers(db)
	id := mustNodeID(t)

	mk := func(jobID string, reason transfer.WaitReason) {
		if err := repo.UpsertJob(transfer.Job{
			JobID: jobID, PeerID: id, ChunkSize: 512, Direction: transfer.DirectionSend,
			Status: transfer.StateIdle, WaitReason: reason,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("w1", transfer.WaitPeerOffline)
	mk("w2", transfer.WaitQueued)
	mk("w3", transfer.WaitNone) // 用户取消过 → 绝不能被自动拉起

	got, err := repo.ListWaiting()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, j := range got {
		ids[j.JobID] = true
	}
	if !ids["w1"] || !ids["w2"] {
		t.Errorf("ListWaiting 漏了可自动拉起的任务：%v", ids)
	}
	if ids["w3"] {
		t.Error("WaitNone（用户取消）的任务不能被 ListWaiting 返回")
	}
}
```

**Step 2: 运行测试确认失败**

```
go test ./internal/adapters/store/sqlite/ -run "TestJobBatchFieldsRoundTrip|TestListByBatch|TestListWaitingIncludesQueued" -v
```

期望：编译失败（`transfer.Job` 没有 `BatchID` 字段、`WaitQueued` 未定义）。

**Step 3: 写最小实现**

`internal/domain/transfer/job.go` —— `WaitReason` 常量块里补一个，`Job` 结构体补三个字段：

```go
	// WaitQueued 表示「排在批次队列里，轮到就传」。
	//
	// 与 WaitPeerOffline 同属「我们自己会拉起」的任务；用户取消会把
	// WaitReason 清成 WaitNone，因此取消过的任务永远不会被 sweep 自动拉起。
	WaitQueued WaitReason = "queued"
```

```go
	// 文件夹批次（BatchID 非空）。单文件传输时三者都是零值。
	BatchID string // 所属批次
	RelPath string // 相对文件夹根的路径，含根名，如 `照片/2024/a.jpg`
	IsDir   bool   // 目录条目（空目录占位）：零字节、无分块
```

同时给它一个方法（guard 只写一遍，`ListWaiting` 与 `startJobs` 共用）：

```go
// AutoStartable 报告该等待原因是否属于「可以自动拉起」的一类。
//
// 存在的理由：这个判断同时被仓储（ListWaiting 的 SQL）与应用层
// （startJobs 的守卫）使用，两处各写一遍迟早会分叉 —— 分叉的方向是
// 「用户取消过的任务自己又跑起来」，那是这个功能最不能出的问题。
func (r WaitReason) AutoStartable() bool {
	return r == WaitPeerOffline || r == WaitQueued
}
```

`migrations.go` —— 在 v3 之后**追加**（不要动 v1–v3 的 SQL 文本）：

```go
	{
		Version: 4,
		Name:    "transfer_batch",
		SQL: `
ALTER TABLE transfer_jobs ADD COLUMN batch_id TEXT    NOT NULL DEFAULT '';
ALTER TABLE transfer_jobs ADD COLUMN rel_path TEXT    NOT NULL DEFAULT '';
ALTER TABLE transfer_jobs ADD COLUMN is_dir   INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS ix_transfer_batch ON transfer_jobs(batch_id);
`,
	},
```

`transfers.go`：
- `transferColumns` 末尾追加 `batch_id, rel_path, is_dir`（放在 `updated_at` 之后，保持"新增列加在尾部"的一致做法）；
- INSERT 列清单与 `VALUES` 同步加 3 个 `?`（总数 23），`ON CONFLICT` 补 `batch_id = excluded.batch_id`、`rel_path = excluded.rel_path`、`is_dir = excluded.is_dir`；
- 参数顺序与列同序：`j.BatchID, j.RelPath, j.IsDir`；
- `scanTransfer` 补三个局部变量（`batchID string`、`relPath string`、`isDir int64`），Scan 目标位置与列严格对齐，赋值 `out.BatchID = batchID`、`out.RelPath = relPath`、`out.IsDir = isDir != 0`；
- `ListWaiting` 的 WHERE 改为 `AND wait_reason IN (?, ?)`，参数 `string(transfer.WaitPeerOffline), string(transfer.WaitQueued)`；**顺序按 `created_at`**（批次内先枚举的先传）；
- 新增：

```go
// ListByBatch 按批次取任务（聚合进度、整体取消用）。
//
// 顺序按 created_at：枚举顺序即发送顺序，界面上的文件顺序与之一致。
func (t *Transfers) ListByBatch(batchID string) ([]transfer.Job, error) {
	rows, err := t.db.Query(`
SELECT `+transferColumns+` FROM transfer_jobs
WHERE batch_id = ?
ORDER BY created_at`, batchID)
	// …与 ListWaiting 相同的扫描循环…
}
```

`ports.go` 的 `TransferRepo` 补：

```go
	// ListByBatch 返回同一文件夹批次的全部任务（按 created_at 升序）。
	//
	// 单独一个方法而不是复用 ListActive：批次聚合进度与「取消全部」都要
	// 看到已结束的任务（用户在卡片上看到的必须是完整的 N 条）。
	ListByBatch(batchID string) ([]transfer.Job, error)
```

`internal/adapters/store/mem/` 里同步实现（内存实现要与 SQL 实现语义一致，否则单进程测试与真实运行行为分叉）。

**Step 4: 运行测试确认通过**

```
go test ./internal/adapters/store/... ./internal/domain/transfer/... -count=1
```

期望：全部 `ok`。

**Step 5: 提交**

```
git add internal/domain/transfer/job.go internal/domain/ports/ports.go internal/adapters/store/sqlite/migrations.go internal/adapters/store/sqlite/transfers.go internal/adapters/store/sqlite/sqlite_test.go internal/adapters/store/mem/
git commit -m "feat(transfer): 批次字段与迁移 v4（batch_id/rel_path/is_dir）"
```

---

## Task 2: 可自动拉起的等待原因（应用层守卫）

**Files:**
- Modify: `internal/app/transfer_waiting.go:336-363`（`startJobs` 的守卫）
- Test: `internal/app/transfer_waiting_test.go`

**Step 1: 写失败的测试**

沿用本文件既有的构造方式（`clock.NewFake` + 内存 store + `NewTransferApp(...)`）：

```go
// 批次里的排队任务（WaitQueued）必须能被自动拉起；
// 而用户取消过的（WaitNone）永远不能 —— 这是「取消之后文件自己又跑起来」
// 唯一的一道防线。
func TestStartJobsOnlyPicksAutoStartable(t *testing.T) {
	// 构造三个任务：peer_offline、queued、none（已取消）
	// 调用 startJobs（或让它经由 SweepWaiting 走到同一处）
	// 断言：前两个被交给发送路径（用 fake conn 记录 dial 次数 / 或断言
	// claimStarting 的抢占被消费），none 那条原封不动。
}
```

测试要点：不要断言 `startJobs` 的返回值（它没有），而是断言**行为**——用既有的 fake `ConnManager`/`Session` 让前两者的发送路径真的跑起来（哪怕随后因文件不存在而失败），并断言 `WaitNone` 那条**没有被抢占**（`isStarting` 为 false 且其 `Status` 仍是 `idle`）。

**Step 2: 运行确认失败**

```
go test ./internal/app/ -run TestStartJobsOnlyPicksAutoStartable -v
```

期望：FAIL（当前 `startJobs` 只认 `WaitPeerOffline`，`queued` 那条不会被启动）。

**Step 3: 写最小实现**

`transfer_waiting.go` 的 `startJobs` 里，把

```go
		if j.WaitReason != transfer.WaitPeerOffline {
			continue
		}
```

改为

```go
		if !j.WaitReason.AutoStartable() {
			continue
		}
```

并把上方那条注释从"只自动拉起『等对方上线』的任务"改成"只自动拉起『可自动拉起』的任务（等待对方上线 / 批次排队）；用户取消会把 WaitReason 清成 WaitNone，因此取消过的任务不会被这里拉起"。

**Step 4: 运行确认通过**

```
go test ./internal/app/ -count=1
```

期望：`ok`（既有用例全部仍然通过 —— 这条改动不能改变 `peer_offline` 的行为）。

**Step 5: 提交**

```
git add internal/app/transfer_waiting.go internal/app/transfer_waiting_test.go
git commit -m "feat(transfer): 批次排队任务纳入自动拉起"
```

---

## Task 3: 协议字段与能力位

**Files:**
- Modify: `internal/domain/protocol/wire.go:137-146`
- Modify: `internal/adapters/net/tcp/handshake.go:49`
- Test: `internal/domain/protocol/wire_test.go`、`internal/adapters/net/tcp/handshake_test.go`

**Step 1: 写失败的测试**

`wire_test.go`：

```go
// 追加字段必须向后兼容：老对端发来的报文没有这三个 key，
// 解码后应当是零值，而不是报错。
func TestFileMetaFolderFieldsBackwardCompatible(t *testing.T) {
	// 老格式：只有既有字段
	old := []byte(`{"job_id":"j1","file_id":"h","file_name":"a.txt","file_size":10,"chunk_size":512,"total_chunks":1}`)
	var m FileMeta
	if err := DecodeJSON(old, &m); err != nil {
		t.Fatalf("老报文必须还能解码: %v", err)
	}
	if m.BatchID != "" || m.RelPath != "" || m.IsDir {
		t.Errorf("老报文解出批次字段 = %q/%q/%v，期望零值", m.BatchID, m.RelPath, m.IsDir)
	}

	// 新格式：三个字段原样往返
	full := FileMeta{JobID: "j2", FileID: "h", FileName: "a.txt", FileSize: 10,
		ChunkSize: 512, TotalChunks: 1,
		BatchID: "b1", RelPath: "照片/a.txt", IsDir: true}
	b, err := EncodeJSON(full)
	if err != nil {
		t.Fatal(err)
	}
	var back FileMeta
	if err := DecodeJSON(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.BatchID != "b1" || back.RelPath != "照片/a.txt" || !back.IsDir {
		t.Errorf("往返失败：%q/%q/%v", back.BatchID, back.RelPath, back.IsDir)
	}
}
```

`handshake_test.go`：

```go
// 我们自己必须声明支持目录传输，否则任何对端都不会认为我们能收文件夹。
func TestDefaultCapsAdvertisesFolderTransfer(t *testing.T) {
	c := DefaultHandshakeConfig()
	if !c.Caps.Has(peer.CapsFolderTransfer) {
		t.Fatal("默认能力位必须包含 CapsFolderTransfer")
	}
}
```

**Step 2: 运行确认失败**

```
go test ./internal/domain/protocol/ -run TestFileMetaFolderFieldsBackwardCompatible -v
go test ./internal/adapters/net/tcp/ -run TestDefaultCapsAdvertisesFolderTransfer -v
```

期望：第一条编译失败（字段不存在），第二条 FAIL。

**Step 3: 写最小实现**

`wire.go` 的 `FileMeta` 追加（**必须带 `omitempty`**：单文件传输不应该在线上多出三个空字段）：

```go
	// 文件夹批次（单文件传输时这三个字段不出现在报文里）。
	BatchID string `json:"batch_id,omitempty"`
	RelPath string `json:"rel_path,omitempty"` // 相对文件夹根，含根名
	IsDir   bool   `json:"is_dir,omitempty"`   // 空目录条目
```

`handshake.go` 的 `DefaultHandshakeConfig()`：

```go
		Caps: peer.CapsChatReliable | peer.CapsGroupChat | peer.CapsResumeBitmap |
			peer.CapsSlidingWindow | peer.CapsFolderTransfer,
```

并确认 `bootstrap` 未另行覆盖 `Caps`（搜索 `HandshakeConfig{` 的使用处；若某处显式赋值了 `Caps`，同处补上）。

**Step 4: 运行确认通过**

```
go test ./internal/domain/protocol/ ./internal/adapters/net/tcp/ -count=1
```

**Step 5: 提交**

```
git add internal/domain/protocol/wire.go internal/domain/protocol/wire_test.go internal/adapters/net/tcp/handshake.go internal/adapters/net/tcp/handshake_test.go
git commit -m "feat(protocol): FileMeta 支持 batch_id/rel_path/is_dir，并声明目录传输能力位"
```

---

## Task 4: 相对路径清洗 `SanitizeRelPath`

**Files:**
- Create: `internal/infra/fsutil/relpath.go`
- Test: `internal/infra/fsutil/relpath_test.go`

**Step 1: 写失败的测试**（表驱动，**拒绝集是这个任务的主体**）

```go
func TestSanitizeRelPath(t *testing.T) {
	ok := []struct {
		in   string
		want []string
	}{
		{"照片/a.jpg", []string{"照片", "a.jpg"}},
		{`照片\a.jpg`, []string{"照片", "a.jpg"}}, // 远端可能是 Windows 分隔符
		{"照片/子/深/c.txt", []string{"照片", "子", "深", "c.txt"}},
		{" 间距 /a.txt", []string{" 间距 ", "a.txt"}}, // 段内空格合法，只去尾随点与空格
	}
	for _, tc := range ok {
		got, err := SanitizeRelPath(tc.in)
		if err != nil {
			t.Errorf("SanitizeRelPath(%q) 不应报错: %v", tc.in, err)
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("SanitizeRelPath(%q) = %v，期望 %v", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"",                       // 空
		"../../etc/passwd",       // 穿越
		"照片/../../evil.txt",     // 中段穿越
		"/etc/passwd",            // 绝对路径
		`C:\Windows\evil.dll`,    // 盘符（会被当成段并按保留字符拒绝）
		`照片/..`,                // 尾段是 ..
		"照片/.",                 // 段是 .
		"照片//a.txt",            // 空段
		"照片/CON.txt",           // Windows 保留名（沿用 SanitizeFileName 的规则）
		"照片/" + strings.Repeat("a", 260) + ".txt", // 单段超长
		"a/" + strings.Repeat("b/", 40) + "c",       // 深度超限
	}
	for _, in := range bad {
		if got, err := SanitizeRelPath(in); err == nil {
			t.Errorf("SanitizeRelPath(%q) 必须被拒绝，实际返回 %v", in, got)
		}
	}
}
```

**Step 2: 运行确认失败**

```
go test ./internal/infra/fsutil/ -run TestSanitizeRelPath -v
```

期望：编译失败（函数不存在）。注意：本包另有 1 个**既有失败** `TestSanitizeFileNameRejectsUnsafeNames`，忽略它。

**Step 3: 写最小实现**

`internal/infra/fsutil/relpath.go`：

```go
package fsutil

import (
	"fmt"
	"strings"
)

// MaxRelPathDepth 是相对路径的最大层数。
//
// 给一个上限的理由不是洁癖：远端可以发来 1000 层嵌套的 rel_path，
// 每一层都要创建目录、都要写进 DB —— 一条恶意任务就能拖垮磁盘布局。
const MaxRelPathDepth = 32

// SanitizeRelPath 把远端发来的相对路径切成【已清洗的段】。
//
// 与 SanitizeFileName 的关系：那个函数负责「一个名字」，本函数负责「一条路径」——
// 逐段调用它，因此保留设备名（CON/NUL）、尾随点与空格、空字节等规则自动继承。
// 额外拦住的是路径层面的东西：绝对路径、盘符、`..`、空段、层数与长度上限。
//
// 返回值一定是「安全的段列表」，调用方只需逐段 SecureJoin，不必自己再判断。
func SanitizeRelPath(rel string) ([]string, error) {
	if rel == "" {
		return nil, fmt.Errorf("%w: empty rel path", ErrBadName)
	}
	// 两种分隔符都要切：对端可能是 Windows，且不允许把 `\` 当合法文件名字符。
	raw := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: no path segment", ErrBadName)
	}
	if len(raw) > MaxRelPathDepth {
		return nil, fmt.Errorf("%w: depth %d exceeds %d", ErrBadName, len(raw), MaxRelPathDepth)
	}

	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		// 绝对路径与盘符（`C:`）会在 SanitizeFileName 里因含冒号或为空被拒；
		// 这里显式再挡一次空段与点段，给出更准确的原因。
		if seg == "" || seg == "." || seg == ".." {
			return nil, fmt.Errorf("%w: bad segment %q", ErrBadName, seg)
		}
		clean, err := SanitizeFileName(seg)
		if err != nil {
			return nil, err
		}
		out = append(out, clean)
	}
	return out, nil
}
```

**Step 4: 运行确认通过**

```
go test ./internal/infra/fsutil/ -run TestSanitizeRelPath -v
```

期望：PASS。

**Step 5: 提交**

```
git add internal/infra/fsutil/relpath.go internal/infra/fsutil/relpath_test.go
git commit -m "feat(fsutil): 新增相对路径逐段清洗 SanitizeRelPath"
```

---

## Task 5: `FileSink.CreateIn` 与批次根目录

**Files:**
- Modify: `internal/domain/ports/ports.go:249-264`
- Modify: `internal/adapters/filesink/filesink.go`
- Test: `internal/adapters/filesink/filesink_test.go`

**Step 1: 写失败的测试**

本文件是包内测试（`package filesink`），直接用 `t.TempDir()` 构造 sink：

```go
// 批次内的文件必须落到同一个根目录下，且把相对路径还原出来。
func TestSinkCreateInRestoresTree(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	h1, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/2024/a.jpg"})
	if err != nil {
		t.Fatalf("createIn: %v", err)
	}
	if want := filepath.Join(dir, "照片", "2024", "a.jpg"); h1.FinalPath != want {
		t.Errorf("final = %q，期望 %q", h1.FinalPath, want)
	}
	// 临时文件必须与最终文件【同目录】：跨目录 os.Rename 不保证原子（ADR-005）
	if filepath.Dir(h1.TempPath) != filepath.Dir(h1.FinalPath) {
		t.Errorf("临时文件目录 = %q，必须与最终目录 %q 相同", filepath.Dir(h1.TempPath), filepath.Dir(h1.FinalPath))
	}
	if _, err := os.Stat(filepath.Dir(h1.TempPath)); err != nil {
		t.Errorf("父目录应已创建: %v", err)
	}

	// 同批第二个文件必须在【同一个根】下，不能又建一个「照片 (1)」
	h2, err := s.CreateIn(ports.SinkRequest{JobID: "j2", BatchID: "b1", RelPath: "照片/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "照片", "b.txt"); h2.FinalPath != want {
		t.Errorf("同批第二个文件 final = %q，期望 %q", h2.FinalPath, want)
	}
}

// 目标目录已存在同名文件夹：整批退到「照片 (1)」，两份内容不混合。
func TestSinkBatchRootRenamedWhenExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "照片"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "照片 (1)", "a.jpg"); h.FinalPath != want {
		t.Errorf("final = %q，期望改名为 %q", h.FinalPath, want)
	}
}

// 重启后认领已有的根目录：否则续传会把一个文件夹拆成「照片」和「照片 (1)」两份。
func TestSinkAdoptRootKeepsSameRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "照片"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.AdoptRoot("b1", filepath.Join(dir, "照片"))

	h, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "照片", "b.txt"); h.FinalPath != want {
		t.Errorf("认领后 final = %q，期望 %q（不能重新分配）", h.FinalPath, want)
	}
}

// 目录条目：只建目录，不建临时文件。
func TestSinkCreateInDir(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/空目录", IsDir: true})
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(h.FinalPath)
	if err != nil || !st.IsDir() {
		t.Fatalf("目录未创建: %v", err)
	}
	if h.TempPath != "" {
		t.Errorf("目录条目不应当有临时文件，得到 %q", h.TempPath)
	}
}

// 恶意相对路径必须被拒（逐段清洗），且不能创建任何目录。
func TestSinkCreateInRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../../evil.txt", "/tmp/evil.txt", "照片/../../evil.txt"} {
		if _, err := s.CreateIn(ports.SinkRequest{JobID: "j", BatchID: "b", RelPath: rel}); err == nil {
			t.Errorf("rel_path = %q 必须被拒绝", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "evil.txt")); !os.IsNotExist(err) {
		t.Error("拒绝的路径不应在上级目录留下文件")
	}
}
```

**Step 2: 运行确认失败**

```
go test ./internal/adapters/filesink/ -run "TestSinkCreateIn|TestSinkBatchRoot|TestSinkAdoptRoot" -v
```

期望：编译失败（`CreateIn`/`AdoptRoot` 未定义、`ports.SinkRequest` 不存在）。

**Step 3: 写最小实现**

`ports.go`：

```go
// SinkRequest 是一次「落在目录树里」的写入请求。
//
// 单独一个结构体而不是给 Create 加参数：单文件路径（BatchID/RelPath 都为空）
// 的行为一个字都不能变 —— 那是已经上线并测过的路径。
type SinkRequest struct {
	JobID   string
	BatchID string // 空串 = 单文件，行为与 Create 完全一致
	RelPath string // 目录树中的相对路径（含根名）
	IsDir   bool   // 只建目录、不写文件
}

type FileSink interface {
	Create(jobID, rawName string) (SinkHandle, error)
	// CreateIn 按相对路径落盘；实现内做逐段清洗与批次根目录分配。
	CreateIn(req SinkRequest) (SinkHandle, error)
	// AdoptRoot 认领一个已经分配过的批次根目录（重启恢复用）。
	//
	// 不认领的后果：进程重启后续传会重新分配出「照片 (1)」，
	// 把一个文件夹拆成两份，用户看到的是「文件少了一半」。
	AdoptRoot(batchID, absPath string)
	Commit(h SinkHandle) (finalPath string, err error)
	Abort(h SinkHandle) error
}
```

`filesink.go` 实现要点：

- `Sink` 增加 `roots map[string]string`（batchID → 已分配的绝对根目录），与 `reserved` 共用同一把 `mu`。
- `CreateIn(req)`：
  1. `segs, err := fsutil.SanitizeRelPath(req.RelPath)`；
  2. `root, err := s.rootLocked(req.BatchID, segs[0])` —— 已分配则直接用；否则在**接收目录下**用 `uniqueLocked(segs[0])` 分配（复用现有去重逻辑，天然得到 `照片 (1)`），记进 `roots`，并 `os.MkdirAll(root)`；
  3. 中间段逐层 `SecureJoin` 后 `MkdirAll(parent)`；
  4. 最终路径 = `SecureJoin(parent, segs[len-1])`，若已存在则按现有规则加 `(1)`（复用 `uniqueLocked` 的思路，注意 `uniqueLocked` 目前是「在 s.dir 下」——把它重构成接受一个基准目录的内部函数，`Create` 传 `s.dir`）；
  5. `IsDir` 为真 → `MkdirAll(final)`，返回 `SinkHandle{JobID: req.JobID, FinalPath: final}`（`TempPath` 留空）；
  6. 否则在**最终目录内**创建临时文件（`SecureJoin(filepath.Dir(final), fsutil.TempPartName(req.JobID))`）。
- `AdoptRoot`：加锁写入 `roots[batchID] = absPath`（做一次 `filepath.Abs` 规整；空值忽略）。
- `Create` 保持不变（内部可改为共用重构后的分配函数，但行为与错误文案必须一致）。

**Step 4: 运行确认通过**

```
go test ./internal/adapters/filesink/ -count=1 -v
```

期望：新旧用例全 PASS（既有单文件用例必须有变化的一并说明）。

**Step 5: 提交**

```
git add internal/domain/ports/ports.go internal/adapters/filesink/filesink.go internal/adapters/filesink/filesink_test.go
git commit -m "feat(transfer): FileSink 支持相对路径落盘与批次根目录认领"
```

---

## Task 6: 发送侧 `SendFolderAs`

**Files:**
- Create: `internal/app/transfer_folder.go`
- Modify: `internal/app/transfer_app.go`（`sendFile` 里的批次字段承接）
- Test: `internal/app/transfer_folder_test.go`

**Step 1: 写失败的测试**（用 `t.TempDir()` 造目录树）

```go
func TestSendFolderEnumeratesTree(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "照片", "a.jpg"), 100)
	mkFile(t, filepath.Join(root, "照片", "2024", "b.jpg"), 200)
	mustMkdir(t, filepath.Join(root, "照片", "空目录"))

	// 沿用本包既有构造（clock.NewFake + 内存 repo + NewTransferApp）
	a, repo := newFolderFixture(t, peer.CapsFolderTransfer)

	sum, err := a.SendFolderAs(context.Background(), peerID, filepath.Join(root, "照片"), "batch-1")
	if err != nil {
		t.Fatalf("SendFolderAs: %v", err)
	}
	if sum.FileCount != 3 { // 2 个文件 + 1 个空目录
		t.Errorf("file_count = %d，期望 3（空目录也算一条）", sum.FileCount)
	}
	if sum.TotalSize != 300 {
		t.Errorf("total_size = %d，期望 300", sum.TotalSize)
	}
	if sum.Skipped != 0 {
		t.Errorf("skipped = %d，期望 0", sum.Skipped)
	}

	jobs, err := repo.ListByBatch("batch-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("落库任务数 = %d，期望 3", len(jobs))
	}
	for _, j := range jobs {
		if j.Status != transfer.StateIdle || j.WaitReason != transfer.WaitQueued {
			t.Errorf("任务 %s 状态 = %s/%s，期望 idle/queued（等 sweep 拉起）", j.JobID, j.Status, j.WaitReason)
		}
		if j.FileHash != "" {
			t.Errorf("任务 %s 在枚举阶段不该有哈希（那会把大目录卡死）", j.JobID)
		}
		if !strings.HasPrefix(j.RelPath, "照片/") {
			t.Errorf("任务 %s 的 rel_path = %q，必须含根名", j.JobID, j.RelPath)
		}
	}
}

// 空文件夹本身也要成为一条目录条目，否则接收端不会建出那个空目录。
func TestSendFolderEmptyRoot(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "空的"))
	// …SendFolderAs…
	// 断言：1 条任务、IsDir 为真、RelPath == "空的"、FileCount == 1
}

// 符号链接不跟随（避免循环与越权读取），但必须如实告诉用户跳过了几项。
func TestSendFolderSkipsSymlinks(t *testing.T) {
	// 目录里放一个指向外部的符号链接（Windows 上创建符号链接可能失败 →
	// 用 t.Skip 跳过该平台分支，但测试本身要留着）
	// 断言：Skipped == 1，且没有任何任务指向链接目标
}

// 超过条目上限：整批拒绝，一条任务都不许留下。
func TestSendFolderRejectsTooManyEntries(t *testing.T) {
	// 把上限通过字段调小（与 ChunkSize 等可调字段同一风格），而不是真的造 2 万个文件
	// 断言：返回错误、ListByBatch 为空
}

// 对端不支持目录传输时明确拒绝，而不是「偷偷打包」或平铺过去。
func TestSendFolderRequiresCapability(t *testing.T) {
	a, _ := newFolderFixture(t, peer.CapsChatReliable) // 无 CapsFolderTransfer
	_, err := a.SendFolderAs(context.Background(), peerID, dir, "batch-1")
	if err == nil {
		t.Fatal("对端不支持目录传输时必须拒绝发送")
	}
	if !strings.Contains(err.Error(), "不支持") {
		t.Errorf("错误信息应说清原因，得到 %q", err)
	}
}

// 批次字段必须像 RetryCount 一样「承接历史」：sendFile 从零构造 Job 再落库，
// 漏掉这条会让传输列表里的相对路径消失、聚合进度算不出来。
func TestCarryOverBatchFields(t *testing.T) {
	prev := transfer.Job{JobID: "j1", BatchID: "b1", RelPath: "照片/a.jpg", IsDir: false}
	job := carryOverBatch(prev, transfer.Job{JobID: "j1", RetryCount: 2})
	if job.BatchID != "b1" || job.RelPath != "照片/a.jpg" {
		t.Errorf("批次字段丢失：%+v", job)
	}
}
```

`newFolderFixture` 是本任务新增的测试辅助：用 `clock.NewFake` + 内存 store + `NewTransferApp(...)` + 一个记录 dial 的 fake `ConnManager`（沿用 `transfer_waiting_test.go` 的既有 fake，**不要新造**）。peer 的能力位通过 fake `PeerDirectory.Get` 返回。

**Step 2: 运行确认失败**

```
go test ./internal/app/ -run "TestSendFolder|TestCarryOverBatchFields" -v
```

期望：编译失败（`SendFolderAs`/`carryOverBatch` 不存在）。

**Step 3: 写最小实现**

`internal/app/transfer_folder.go`：

```go
// FolderSummary 是 SendFolderAs 的返回值：界面用它渲染一条文件夹卡片。
type FolderSummary struct {
	BatchID    string
	FolderName string
	FileCount  int   // 含目录条目
	TotalSize  int64 // 目录条目算 0
	Skipped    int   // 被跳过的符号链接与超限条目
}

// SendFolderAs 枚举目录、建好一批排队任务后立即返回。
//
// 为什么立即返回而不是「等整批传完」：传输由既有的三个触发点
// （SweepWaiting / WakePeer / Recover）驱动，且 startJobs 已经保证同一对端
// 同时只传一个 —— 整批天然串行、天然继承离线等待与断点续传。
//
// 为什么枚举不哈希：一个 10GB 的文件夹为了「开始发送」要先读一整遍磁盘，
// 用户会以为程序卡死。哈希仍旧在单个文件真的要传的那一刻做（同 sendFile）。
func (a *TransferApp) SendFolderAs(ctx context.Context, peerID identity.NodeID, dir, batchID string) (FolderSummary, error) {
	// 1) 能力检查：不支持就明确拒绝（见设计文档 3.2，不猜用户意图）
	// 2) os.Stat 目录存在且真的是目录
	// 3) filepath.WalkDir 枚举，收集 fileEntry{abs, rel, size, isDir}
	//    - 符号链接：d.Type()&fs.ModeSymlink != 0 → skipped++
	//    - 超过 MaxFolderEntries → 直接返回错误（一条任务都不建）
	//    - 空目录 → 追加一条 isDir 条目
	//    - 目录为空 → 为根目录本身追加一条 isDir 条目
	// 4) 为每条条目建一个轻量 job（照 newWaitingJob 的字段，改成 WaitQueued，
	//    RelPath/BatchID/IsDir 填好），UpsertJob 落库
	// 5) a.startJobs(ctx, jobs) 立刻试第一轮，不让用户等下一个 1 秒节拍
	return summary, nil
}
```

`TransferApp` 新增可调字段（与 `ChunkSize` 等同一风格，`NewTransferApp` 里给默认值 20000）：

```go
	// MaxFolderEntries 是一次文件夹传输允许的最大条目数（文件 + 目录）。
	//
	// 上限不是洁癖：一次误操作（选了 C:\Users）会产生几十万条任务，
	// 枚举与落库都在同步路径上，界面会先卡死。
	MaxFolderEntries int
```

`transfer_app.go` 的 `sendFile` 里，在已有的「承接 RetryCount」处（`prev, gerr := a.store.GetJob(jobID)`）一并承接批次字段——抽成小函数以便单测：

```go
// carryOverBatch 把上一次落库行里的批次字段带进新构造的任务。
//
// 为什么必须有：sendFile 与 newWaitingJob 都是从零构造 Job 再 Upsert，
// 漏掉这里，批次字段会在文件真正开始传的那一刻被抹成零值 ——
// 表现为「传输列表里的相对路径没了、文件夹卡片的进度永远算不出来」。
// 与 RetryCount 的承接是同一个道理，因此写在同一个位置。
func carryOverBatch(prev, job transfer.Job) transfer.Job {
	job.BatchID = prev.BatchID
	job.RelPath = prev.RelPath
	job.IsDir = prev.IsDir
	return job
}
```

`newWaitingJob` 的调用处同样处理（它的三个调用点都发生在批次任务上）。

**Step 4: 运行确认通过**

```
go test ./internal/app/ -count=1
```

**Step 5: 提交**

```
git add internal/app/transfer_folder.go internal/app/transfer_folder_test.go internal/app/transfer_app.go internal/app/transfer_waiting.go
git commit -m "feat(transfer): 支持发送文件夹（枚举成一批排队任务）"
```

---

## Task 7: 接收侧还原目录树与目录条目

**Files:**
- Modify: `internal/app/transfer_app.go`（`HandleFileMeta`、`HandleFileDone`、`HandleFileChunk` 的目录分支）
- Test: `internal/app/transfer_folder_recv_test.go`

**Step 1: 写失败的测试**

```go
// 带 rel_path 的 META：落到子目录、字段落库、续传键仍是 file_id。
func TestHandleFileMetaWithRelPath(t *testing.T) {
	// sink 指向 t.TempDir()；构造一个 fake Session 把 META 丢给 HandleFileMeta
	// 断言：sink 的最终路径在 <dir>/照片/2024/a.jpg，且 job 落库带 batch_id/rel_path
}

// 空目录：total_chunks=0 也必须放行（现有实现会报 invalid meta），只建目录。
func TestHandleFileMetaDirEntry(t *testing.T) {
	// 发一个 is_dir=true、file_size=0、total_chunks=0 的 META
	// 断言：不报错、目录已创建、回了一张 ACK；随后 FILE_DONE 直接判完成
}

// 回归：非目录条目的空文件仍然是协议非法 —— 这条校验不能被放宽。
func TestHandleFileMetaStillRejectsEmptyFile(t *testing.T) { /* … */ }

// 恶意 rel_path 只让这一条失败，其余文件不受影响。
func TestHandleFileMetaBadRelPathFailsOnlyThatJob(t *testing.T) {
	// 先发一条正常 META（成功），再发一条 rel_path="../../evil"（失败）
	// 断言：第二次返回错误、无文件写到达上级目录、第一条任务不受影响
}

// is_dir 任务不需要任何分块；收到 chunk 时应被忽略而不是 panic。
func TestHandleFileChunkIgnoresDirJob(t *testing.T) { /* … */ }

// 【本任务必须新增】同一批次里两个内容相同的文件，不能共享临时文件与最终路径。
//
// 现状的续传键是 (peer, file_hash)（transfers.go:70），命中后 HandleFileMeta 会
// 继承前一条的 TempPath/LocalPath/位图。单文件重复发送时这问题偶发，批次把
// 「同一 hash 出现两次」变成常态（一张文件夹里两份一样的图很常见）——
// 后果是目录里少一个文件、另一个内容错位。
func TestHandleFileMetaSameHashInBatchKeepsSeparateTargets(t *testing.T) {
	// 同一批两条 META：file_id 相同、rel_path 不同（照片/a.txt 与 照片/备份/a.txt）
	// 断言：两次的最终路径不同、第二条不继承第一条的 TempPath
}
```

**Step 2: 运行确认失败**

```
go test ./internal/app/ -run "TestHandleFileMetaWithRelPath|TestHandleFileMetaDirEntry" -v
```

**Step 3: 写最小实现**

`HandleFileMeta`（`transfer_app.go:517-595`）：

- 解析后，若 `meta.RelPath != "" || meta.BatchID != ""` → 走 `sink.CreateIn(ports.SinkRequest{JobID, BatchID, RelPath, IsDir})`；否则保持现有 `sink.Create(meta.JobID, meta.FileName)` **一字不改**。
- `total == 0` 的校验改成：

```go
	// 目录条目（空目录）天生没有分块，必须放行；
	// 其余情况下 total_chunks == 0 仍按协议非法处理（对方算错了）。
	if total == 0 && !meta.IsDir {
		return fmt.Errorf("invalid meta (total_chunks=0)")
	}
```

- 落库的 `job` 填 `BatchID/RelPath/IsDir`；`IsDir` 时 `Bitmap` 长度为 0，`handle.TempPath` 为空 → 不要调用 `OpenTemp`（现有代码在 `recvJob` 里打开临时文件，需按 `IsDir` 分支跳过）。
- **续传键必须按批次收紧**（本任务的关键修正）：把 `FindResumable(peerID, fileHash)` 升为 `FindResumable(peerID, fileHash, relPath)` —— `relPath == ""` 时保持单文件的旧语义（`(peer, hash)`），非空时按 `rel_path` 区分。端口、sqlite、mem 三处同步改，并补一条「同 hash 不同 rel_path 互不命中」的仓储用例。不修的话，一张文件夹里两份内容相同的文件会共享临时文件与最终路径（目录里少一个文件、另一个内容错位）。
- `IsDir` 条目**显式不进**续传查找（它没有文件内容，`FileID` 为空）：现在是靠 `existing.TempPath != ""` 这个次生条件偶然挡住的，不要依赖它。
- `HandleFileChunk`：`IsDir` 的接收任务直接 `return nil`（防御性，正常不会收到）。
- `HandleFileDone`：`IsDir` 时跳过 `verify`（没有块可校验）、直接 `Status = StateDone`、`UpsertJob`、回 ACK — 目录在 META 阶段已经建好，这里只收尾。

**Step 4: 运行确认通过**

```
go test ./internal/app/ -count=1
```

**Step 5: 提交**

```
git add internal/app/transfer_app.go internal/app/transfer_folder_recv_test.go
git commit -m "feat(transfer): 接收端按相对路径还原目录树，支持空目录条目"
```

---

## Task 8: 取消中断点与整体取消

**Files:**
- Modify: `internal/app/transfer_app.go`（`sendJob` 增加取消信号、`runSender` 检查、失败出口先判取消）
- Modify: `internal/app/transfer_waiting.go`（`CancelJob` 放开在传任务、新增 `CancelBatch`）
- Modify: `internal/adapters/wails/services.go:629-637`（错误文案与注释）
- Test: `internal/app/transfer_cancel_test.go`

**Step 1: 写失败的测试**

```go
// 在传的任务被取消：必须是 cancelled，【不能】掉进「转等待」——
// 否则用户点了取消、对方一上线文件又自己跑起来。
func TestCancelRunningJobDoesNotBecomeWaiting(t *testing.T) {
	// 用 fake Session 让发送进入 transferring 阶段（慢速 fake 喂块），
	// 在传输中调 CancelJob / CancelBatch
	// 断言：落库 Status == StateCancelled、WaitReason == WaitNone、NextTryAt 为零
	//     且随后 WakePeer(该 peer) 不会让它重新启动
}

// 整体取消：排队的与在传的都要停，已结束的不动。
func TestCancelBatchCancelsAllUnfinished(t *testing.T) {
	// 造 3 条同批任务：1 条 done、1 条 queued、1 条在传
	// 断言：返回 1（在传）+ 1（排队）= 2，done 那条保持 done
}

// 取消一个已结束的批次：不报错、返回 0（按钮可能被连点）。
func TestCancelBatchOnFinishedBatchIsNoop(t *testing.T) { /* … */ }
```

**Step 2: 运行确认失败**

```
go test ./internal/app/ -run "TestCancelRunningJob|TestCancelBatch" -v
```

**Step 3: 写最小实现**

`sendJob`（`transfer_app.go` 里那个结构体）加：

```go
	cancel    chan struct{} // 关闭即要求本次传输尽快干净退出
	cancelled atomic.Bool
```

- 创建 `sendJob` 时初始化 `cancel: make(chan struct{})`；在 `senders` 表里按 jobID 查得到 → 取消时取出并 `close`。
- `runSender` 的循环里（每轮开头与等待 select 处）检查：

```go
	select {
	case <-sj.cancel:
		return errJobCancelled
	default:
	}
```

  并把 `sj.cancel` 加进该循环里所有 `select` 的 `case`（否则阻塞在等 ACK 时会拖到超时才响应取消）。

```go
// errJobCancelled 表示用户主动取消。
//
// 必须是独立的哨兵值：顶层失败出口要据此【先判取消】，
// 否则取消会掉进「对端不可达 → 转等待」那条分支，
// 变成一个用户点了取消、对方一上线又自己跑起来的任务。
var errJobCancelled = errors.New("transfer: 任务已取消")
```

- 顶层统一失败出口（`transfer_app.go:207-239`）在 `networkAttempted && shouldWaitForPeer(...)` 判定**之前**插入：

```go
		if errors.Is(err, errJobCancelled) {
			job.Status = transfer.StateCancelled
			job.WaitReason = transfer.WaitNone
			job.NextTryAt = time.Time{}
			job.UpdatedAt = a.clk.Now()
			_ = a.store.UpsertJob(job)
			a.publishState(job, string(transfer.StateMetaExchange), string(transfer.StateCancelled), "")
			err = nil
			return
		}
```

  （`from` 用 `job` 当时的状态变量，不要硬编码；照 `markWaiting` 的写法取值即可。）
- **保留半截临时文件**：`defer a.dropSender(...)` 只做内存表清理，`sink.Abort` 仅在真失败路径上调用 —— 取消不删 `.part`，下次重发可按位图续传。

`CancelJob` 改为：

```go
// CancelJob 取消一条任务：排队/等待中的直接置取消；正在传的通过取消信号
// 让它干净退出并置取消。
//
// 为什么现在能取消在传的任务：sender 有了中断点（sendJob.cancel）。
// 过去明确拒绝是因为「假取消比拒绝更糟」—— 那个理由仍然成立，
// 因此这里只在【确实能把任务停下来】的前提下放行。
func (a *TransferApp) CancelJob(jobID string) error
```

实现要点：`Direction != send` 与已终态（done/cancelled）仍然拒绝或直接返回；`StateIdle` 走原有路径；`MetaExchange/Transferring/Verifying` → 从 `senders` 取出 `sj` 并 `close(sj.cancel)`（**幂等**：已关闭过就不要再关，用一个 `sync.Once` 或 `cancelled.CompareAndSwap` 兜住连点）。

```go
// CancelBatch 取消整个批次里所有未结束的任务，返回取消条数。
func (a *TransferApp) CancelBatch(batchID string) (int, error) {
	if batchID == "" {
		return 0, fmt.Errorf("transfer: batch_id 不能为空")
	}
	jobs, err := a.store.ListByBatch(batchID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if j.Status == transfer.StateDone || j.Status == transfer.StateCancelled {
			continue
		}
		if err := a.CancelJob(j.JobID); err != nil {
			// 单条失败不中断整批：用户点的是「全停」，尽力而为比半途而废好。
			if a.lg != nil {
				a.lg.Warn("取消批次中的任务失败", "job_id", j.JobID, "err", err)
			}
			continue
		}
		n++
	}
	return n, nil
}
```

**Step 4: 运行确认通过**

```
go test ./internal/app/ -count=1 -race -run "TestCancel|TestStartJobs|TestFallsBack"
```

**Step 5: 提交**

```
git add internal/app/transfer_app.go internal/app/transfer_waiting.go internal/app/transfer_cancel_test.go internal/adapters/wails/services.go
git commit -m "feat(transfer): 支持取消正在传输的任务与整批取消"
```

---

## Task 9: 重启后认领批次根目录

**Files:**
- Modify: `internal/app/transfer_waiting.go`（`Recover`）
- Test: `internal/app/transfer_recover_test.go`

**Step 1: 写失败的测试**

```go
// 重启续传不能把同一个文件夹拆成两份：Recover 必须把已有的根目录认领回来。
func TestRecoverAdoptsBatchRoots(t *testing.T) {
	dir := t.TempDir()
	// 上一轮运行留下的痕迹：<dir>/照片/a.jpg 已存在，库里有一条同批任务，
	// local_path 指向 <dir>/照片/a.jpg
	// 构造一个全新的 sink + TransferApp（模拟重启），调用 Recover
	// 断言：随后 CreateIn 同批的另一个文件落到的根仍然是 <dir>/照片，
	//      而不是被重新分配成 <dir>/照片 (1)
}
```

**Step 2: 运行确认失败**

```
go test ./internal/app/ -run TestRecoverAdoptsBatchRoots -v
```

**Step 3: 写最小实现**

`Recover`（`transfer_waiting.go:378-412`）在收尾 `active` 任务的那段循环里，对带 `batch_id` 的任务顺带采集根目录：

```go
	// 批次根目录必须在重启后认领回来：否则续传会重新分配出「照片 (1)」，
	// 把一个文件夹拆成两份（用户看到的是「文件少了一半」）。
	//
	// 根目录用 local_path 的「相对路径层数」反推：接收端落盘时
	// <root>/照片/2024/a.jpg ← rel_path 是 照片/2024/a.jpg，
	// 因此从 local_path 去掉 rel_path 的段数即得根目录。
	for _, j := range active {
		if j.BatchID == "" || j.Direction != transfer.DirectionReceive || j.LocalPath == "" {
			continue
		}
		if root, ok := batchRootOf(j); ok {
			a.sink.AdoptRoot(j.BatchID, root)
		}
	}
```

`batchRootOf` 是本任务新增的纯函数（放在 `transfer_folder.go`，便于表驱动测试）：

```go
// batchRootOf 从一条落库任务反推它所属批次的根目录。
func batchRootOf(j transfer.Job) (string, bool) {
	if j.RelPath == "" || j.LocalPath == "" {
		return "", false
	}
	segs, err := fsutil.SanitizeRelPath(j.RelPath)
	if err != nil {
		return "", false
	}
	root := j.LocalPath
	for range segs { // 去掉与 rel_path 段数相同的层级
		root = filepath.Dir(root)
	}
	return root, true
}
```

配套测试（表驱动，纯函数）：

```go
func TestBatchRootOf(t *testing.T) {
	j := transfer.Job{RelPath: "照片/2024/a.jpg", LocalPath: filepath.Join("C:", "dl", "照片", "2024", "a.jpg")}
	got, ok := batchRootOf(j)
	if !ok || got != filepath.Join("C:", "dl") {
		t.Errorf("root = %q ok=%v，期望 .../dl", got, ok)
	}
}
```

同时确认 `Recover` 之后 `ListWaiting()` 会把 `WaitQueued` 的批次任务一起拉起（Task 1 的 SQL 已保证），并在测试里断言这一点。

**Step 4: 运行确认通过**

```
go test ./internal/app/ -count=1
```

**Step 5: 提交**

```
git add internal/app/transfer_waiting.go internal/app/transfer_folder.go internal/app/transfer_recover_test.go
git commit -m "feat(transfer): 重启后认领批次根目录，续传不再拆成两份"
```

---

## Task 10: 文件夹消息类型（Go 侧）

**Files:**
- Modify: `internal/domain/message/message.go:35-44`（`MsgType`）与内容结构体区
- Modify: `internal/app/chat_app.go`（`SendFolder`，照 `SendFile` 的样子）
- Test: `internal/domain/message/message_test.go`（或该包既有测试文件）

**Step 1: 写失败的测试**

```go
// 文件夹消息的 content 必须是可解析的 JSON 信封（历史坑：字段没有 tag，
// 序列化出 JobID/FileName，前端一个字段都读不到，只能显示一段裸 JSON）。
func TestFolderMessageContentJSONTags(t *testing.T) {
	c := FolderMessageContent{BatchID: "b1", FolderName: "照片", FileCount: 32, TotalSize: 1024, Skipped: 2}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"batch_id", "folder_name", "file_count", "total_size", "skipped"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺少 snake_case 字段 %q（前端按它解析）", k)
		}
	}
}
```

**Step 2: 运行确认失败**

```
go test ./internal/domain/message/ -run TestFolderMessageContentJSONTags -v
```

**Step 3: 写最小实现**

`message.go`：

```go
	// MsgTypeFolder 的 content 是 FolderMessageContent 的 JSON 信封。
	// 与 MsgTypeFile 同构：只让两端聊天框出现一张文件夹卡片，
	// 传输本身由 TransferApp 的独立通道完成。
	MsgTypeFolder MsgType = "folder"
```

```go
// FolderMessageContent 是文件夹消息的内容。
//
// 与 FileMessageContent 一样【不】含发送方本地路径：这条消息会投递给对端。
// file_count / total_size 是发送端枚举后的最终数字 —— 接收端不必自己数，
// 两端数字必须一致，否则卡片上会出现两个不同的「共 N 个文件」。
type FolderMessageContent struct {
	BatchID    string `json:"batch_id"`
	FolderName string `json:"folder_name"`
	FileCount  int    `json:"file_count"`
	TotalSize  int64  `json:"total_size"`
	Skipped    int    `json:"skipped"`
}
```

`chat_app.go` 加 `SendFolder`：照 `SendFile`（`services.go:436` 调用的那个）的写法 —— 构造 `message.Message`、`Content` 为 `FolderMessageContent` 的 JSON、`MsgType: message.MsgTypeFolder`，走同一套「先落库（pending）+ 入 outbox」的原子写入路径，返回消息实体。

**Step 4: 运行确认通过**

```
go test ./internal/domain/message/ ./internal/app/ -count=1
```

**Step 5: 提交**

```
git add internal/domain/message/message.go internal/domain/message/message_test.go internal/app/chat_app.go
git commit -m "feat(chat): 新增文件夹消息类型"
```

---

## Task 11: Wails 服务层与绑定

**Files:**
- Modify: `internal/adapters/wails/services.go`（`TransferService` 三个方法 + `ChatService.SendFolderMessage` + DTO 字段）
- Test: `internal/adapters/wails/services_test.go`

**Step 1: 写失败的测试**

```go
// 与 SendFile 同一条约定：最廉价的校验在返回前同步做，
// 路径打错要立刻报错，而不是让用户对着一个「排队中」的任务猜。
func TestSendFolderValidatesSynchronously(t *testing.T) {
	// 不存在的路径 → 立即返回错误
	// 传的是一个【文件】而不是目录 → 立即返回错误且信息可读
}

// 空 batchId 的取消必须拒绝：否则会退化成「把 batch_id='' 的任务全取消」，
// 那正是所有单文件任务。
func TestCancelBatchRejectsEmptyID(t *testing.T) { /* … */ }
```

**Step 2: 运行确认失败**

```
go test -tags wails ./internal/adapters/wails/ -run "TestSendFolder|TestCancelBatch" -v
```

**Step 3: 写最小实现**

`services.go` 的 `TransferService`：

```go
// SendFolder 异步发送一个文件夹：枚举与落库在返回前完成（只 stat，不哈希），
// 传输由 app 层的排队机制驱动，因此界面上会立刻出现一条文件夹卡片与
// 一批「排队中」的任务。
//
// batchID 必须在这里用 message.NewID() 生成随机值（与 job_id 同一套）：
// 它是全局唯一的批次标识，不是文件夹名 —— 两个对端各发一个「照片」时，
// 同名 batch 会让卡片把两个文件夹合并、取消也连带停掉另一批。
func (s *TransferService) SendFolder(peerID, path string) (FolderDTO, error)

// CancelBatch 取消整个文件夹批次里未结束的任务，返回取消条数。
func (s *TransferService) CancelBatch(batchID string) (int, error)

// ListBatch 返回一个批次的全部任务（卡片上的聚合进度用）。
//
// 不能拿 List() 代替：它走 ListRecent(50) 的窗口，
// 500 个文件的文件夹会算缺进度。
func (s *TransferService) ListBatch(batchID string) ([]TransferDTO, error)
```

`FolderDTO` = `{BatchID, FolderName, FileCount, TotalSize, Skipped}`；`TransferDTO` 追加 `BatchID/RelPath/IsDir`（注意 DTO 是显式字段映射，不是结构体直传 —— 照现有写法逐个填）。

`ChatService` 加 `SendFolderMessage(peerID, batchID, folderName string, fileCount int, totalSize int64, skipped int) (MessageDTO, error)`，照 `SendFileMessage`（`services.go:425-441`）的写法。

**Step 4: 重新生成绑定**

```
wails3 generate bindings -f "-tags wails" -clean=true -ts -i .
git status --short -- frontend/bindings
```

期望：只出现 `TransferService` 的新方法、`ChatService.SendFolderMessage` 与 DTO 字段相关的改动；**不应**出现无关的整文件重写。

**Step 5: 运行测试并提交**

```
go test -tags wails ./internal/adapters/wails/ -count=1
git add internal/adapters/wails/services.go internal/adapters/wails/services_test.go frontend/bindings
git commit -m "feat(transfer): 暴露发送文件夹、整体取消与批次查询的服务方法"
```

---

## Task 12: 前端数据层

**Files:**
- Modify: `frontend/src/api/types.ts`（`SwarmApi` 三个方法、`TransferJob` 三个字段）
- Modify: `frontend/src/api/bindings.ts`
- Modify: `frontend/src/api/mock.ts`（预览模式必须同步，否则 typecheck 立刻失败）
- Modify: `frontend/src/stores/transfer.ts`（`isWaiting`/状态判定、批次聚合）
- Modify: `frontend/src/stores/chat.ts`（`sendFolder`）
- Create: `frontend/src/utils/batch.ts`
- Test: `frontend/tests/batch.test.ts`

**Step 1: 写失败的测试**

前端测试用 Node 内置 test runner（`npm run test`）。纯函数放 `utils/batch.ts` 才可测：

```ts
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { folderProgress } from '../src/utils/batch.ts'

test('folderProgress 只统计本批次的任务', () => {
  const jobs = [
    { jobId: '1', batchId: 'b1', status: 'done', fileSize: 100 },
    { jobId: '2', batchId: 'b1', status: 'failed', fileSize: 50 },
    { jobId: '3', batchId: 'b2', status: 'done', fileSize: 999 }
  ]
  const p = folderProgress(jobs, 'b1')
  assert.equal(p.total, 2)
  assert.equal(p.done, 1)
  assert.equal(p.failed, 1)
  assert.equal(p.allFinished, true)
})

test('有人还在传时不算结束', () => {
  const jobs = [{ jobId: '1', batchId: 'b1', status: 'transferring' }]
  assert.equal(folderProgress(jobs, 'b1').allFinished, false)
})
```

**Step 2: 运行确认失败**

```
cd frontend && npm run test
```

期望：FAIL（模块不存在）。

**Step 3: 写最小实现**

- `utils/batch.ts`：`folderProgress(jobs, batchId)` 返回 `{total, done, failed, cancelled, transferred, allFinished, hasFailure}`；`allFinished` 的判据是「没有非终态任务」，`hasFailure` 只认 `failed`。
- `types.ts`：`TransferJob` 补 `batchId/relPath/isDir`；`SwarmApi` 补 `sendFolder(peerId, path): Promise<FolderSummary>`、`cancelBatch(batchId): Promise<number>`、`batchJobs(batchId): Promise<TransferJob[]>`、`sendFolderMessage(...)`。
- `bindings.ts`：接上 Task 11 生成的绑定。
- `mock.ts`：为四个新方法提供降级实现（预览模式里造几条假的 `batchId` 任务即可，**必须让 typecheck 干净**）。
- `stores/transfer.ts`：`applyProgress`/`applyState` 已按 jobId 合并，只要 DTO 带上新字段即可；新增 `sendFolder`；把「等待对方上线」的判据保留为 `waitReason === 'peer_offline'`，并为 `waitReason === 'batch_queued'` 增加「排队中」文案（**两者不同**：一个是等对端，一个是等前一个文件传完）。
- `stores/chat.ts`：新增 `sendFolder(conv, summary)`，照 `sendFile`（`chat.ts:273-282`）的写法，只支持单聊。

**Step 4: 运行确认通过**

```
cd frontend && npm run test && npm run typecheck
```

期望：全绿。

**Step 5: 提交**

```
git add frontend/src/api/types.ts frontend/src/api/bindings.ts frontend/src/api/mock.ts frontend/src/stores/transfer.ts frontend/src/stores/chat.ts frontend/src/utils/batch.ts frontend/tests/batch.test.ts
git commit -m "feat(transfer): 前端数据层支持文件夹批次"
```

---

## Task 13: 前端界面

**Files:**
- Modify: `frontend/src/components/ChatBubble.vue`（文件夹卡片分支）
- Modify: `frontend/src/components/TransferList.vue`（相对路径 + 排队中文案）
- Modify: `frontend/src/composables/useFileSend.ts`（`attachFolder`）
- Modify: `frontend/src/api/dialog.ts`（`pickFolder`）
- Modify: `frontend/src/utils/format.ts`（状态文案：注意**界面文案有两份 switch**，`format.ts` 与 `ChatBubble.vue` 的本地 computed，两处都要加分支）

**Step 1: 目录选择对话框**

`dialog.ts` 新增：

```ts
/** 选择一个文件夹发送。CanChooseDirectories 由 Wails 内置 dialog 服务支持，Go 侧零改动。 */
export async function pickFolder(): Promise<PickOutcome> {
  try {
    const res = await Dialogs.OpenFile({
      Title: '选择要发送的文件夹',
      CanChooseFiles: false,
      CanChooseDirectories: true,
      AllowsMultipleSelection: false,
      ButtonText: '发送'
    })
    const paths = (Array.isArray(res) ? res : [res]).filter((p): p is string => !!p)
    return { picked: paths.length > 0, paths, available: true }
  } catch {
    return { picked: false, paths: [], available: false }
  }
}
```

**Step 2: 发送入口**

`useFileSend.ts` 新增 `attachFolder()`：取当前会话（无会话时给出与 `sendPaths` 一致的提示）→ `pickFolder()` → `transfers.sendFolder(peerId, path)` 拿到 `FolderSummary` → `chat.sendFolder(conv, summary)` → `ui.focusTransfer()`。异常处理照 `sendPaths`：传输已发起但卡片插不进去时，只提示「卡片未显示」，**不要**把这次发送报成失败（否则用户会重复发送）。

**Step 3: 文件夹卡片**

`ChatBubble.vue` 增加 `isFolder` 分支（与 `isFile`/`isFileFailed` 并列）：

- 展示：`文件夹名/`、`N 个文件 · 大小`、进度 `已完成 x/N`（`folderProgress` 从 transfer store 按 batchId 聚合）、`已跳过 3 项`（`skipped > 0` 时）；
- 运行时（非终态）显示「取消全部」按钮 → `transfers.cancelBatch(batchId)`；
- 终态且 `hasFailure` 时显示 `x/N 完成，M 个失败`（危险色），不显示「已完成」；
- 进度数据来源：优先用 store 里的任务；store 里没有（重启后）→ 调 `api.batchJobs(batchId)` 拉一次。

**Step 4: 传输列表**

`TransferList.vue`：有 `relPath` 时显示相对路径（而不是只有文件名）；`排队中` 用 `waitReason === 'batch_queued'` 判定；`format.ts` 与 `ChatBubble.vue` 两处状态文案同步加分支。

**Step 5: 验证**

```
cd frontend && npm run typecheck && npm run test
```

再按设计文档第 6 节的验收标准做一遍手工验证（见 Task 14）。

**Step 6: 提交**

```
git add frontend/src/components/ChatBubble.vue frontend/src/components/TransferList.vue frontend/src/composables/useFileSend.ts frontend/src/api/dialog.ts frontend/src/utils/format.ts
git commit -m "feat(ui): 文件夹卡片、目录发送入口与排队中文案"
```

---

## Task 14: 文档更新与端到端验收

**Files:**
- Modify: `README.md:105`（「明确不做」清单里删掉「文件夹传输」，并说明现状）
- Modify: `设计书.md:30`（同上）
- Modify: `docs/superpowers/specs/2026-09-20-swarmlink-v1-design.md:25`（同上）
- Modify: `架构设计书.md`（231、1439 行的批量承诺；688 行的能力位表把 bit 6 标为已实现；协议字段表补 `batch_id`/`rel_path`/`is_dir`）

**Step 1: 文档改完**

改法要求：不是简单删掉一行，而是把「不做」改成**已做**并指向设计文档 —— 否则下一个读文档的人会以为功能不存在。

**Step 2: 全量验证**

```
go build -tags wails -o NUL .
go vet -tags wails ./internal/... . 
go test -tags wails ./... 2>&1 | findstr /v "no test files"
cd frontend && npm run typecheck && npm run test
```

期望：除前置约定里那 3 个既有失败外全绿。

**Step 3: 手工验收（需要两个真实实例）**

跑两个实例（同一个可执行文件、不同配置目录），逐条核对设计文档第 6 节：

1. 发一个多层子目录的文件夹 → 接收端出现**完整同构目录树**；
2. 空子目录也被创建；
3. 删掉其中一个源文件再发 → **只有那一行失败**，其余传完，卡片显示 `N-1/N 完成，1 个失败`；
4. 对方离线时发 → 整批「排队中」，对方上线后自动传完（无需操作）；
5. 传一半关掉 A 再打开 → 整批继续，且**不会**多出 `照片 (1)`；
6. 点「取消全部」→ 排队中与在传的都变 `cancelled`，且不会自己复活；
7. 目标已存在同名文件夹 → 落到 `照片 (1)/`，不混合；
8. 对不支持能力位的对端 → 明确报错、不发任何文件（可临时把 `DefaultHandshakeConfig` 的 caps 去掉一位来模拟）；
9. 单文件传输的既有行为（离线等待、续传、取消）无回归。

**Step 4: 提交**

```
git add README.md 设计书.md 架构设计书.md docs/superpowers/specs/2026-09-20-swarmlink-v1-design.md
git commit -m "docs: 文件夹传输从不做清单移入已实现能力"
```

---

## 交付清单

- [ ] 迁移 v4 三列（`batch_id`/`rel_path`/`is_dir`）+ `ListByBatch` + `ListWaiting` 支持两类等待原因
- [ ] `WaitReason.AutoStartable()` 守卫应用层与仓储层共用
- [ ] `FileMeta` 三字段 + 自己声明 `CapsFolderTransfer`
- [ ] `fsutil.SanitizeRelPath`（逐段清洗 + 深度上限）
- [ ] `FileSink.CreateIn`/`AdoptRoot`（同目录临时文件、批次根目录只分配一次）
- [ ] `SendFolderAs`（枚举即排队、不哈希、符号链接跳过、条目上限）
- [ ] 接收端目录树还原 + 空目录条目
- [ ] 取消中断点 + `CancelBatch`（取消绝不能退化成等待）
- [ ] `Recover` 认领批次根目录
- [ ] `MsgTypeFolder` + 服务方法与绑定
- [ ] 前端聚合卡片、目录入口、逐文件传输行与整体取消
- [ ] 批次内续传键按 `rel_path` 收紧（Task 7，防同 hash 文件互相覆盖）
- [ ] 文档从不做清单移到已实现

---

## 实施差异记录

按仓库存量约定，实施中与计划的偏离记在这里（不回头改上面已执行的任务步骤）。

### Task 1（提交 `cf30ced`、`32a6863`、`dd4ba86`）

计划与实现的差异，以及原因：

1. **`WaitQueued` 的落库取值改为 `"batch_queued"`**（计划写的是 `"queued"`）。原因：同一句 SQL 里紧挨着还有 `status = 'queued'`（`StateIdle` 的落库值），两个 `queued` 并排会让读代码的人以为写重了；本仓已有过同类同名撞车。
2. **新增 `transfer.AutoStartableReasons()`**，`ListWaiting` 的 `IN` 占位符由它派生。计划让仓储写死 `IN (?, ?)`，而 `AutoStartable()` 是另一处独立判断 —— 那就是第二份真相，分叉方向正是「取消过的任务被重新拉起」。
3. **`ListByBatch("")` 与 `ListWaiting` 都拒绝/排除空值**：空 `batch_id` 会退化成「返回所有单文件任务」，`CancelBatch("")` 就成了「停掉本机全部单文件传输」。
4. **`ListByBatch`/`ListWaiting` 增加第二排序键 `job_id`**（mem 侧同步）。批次是同一个循环里建的，整批落在同一毫秒是常态，只按 `created_at` 排会让文件次序由引擎决定、且两种存储给出不同结果。
5. **计划里的测试辅助名是占位符**：实际用 `sqlite_test.go` 已有的 `openTest`/`mkID`（计划写的 `newTestDB`/`mustNodeID` 不存在）。
6. **`internal/app/transfer_app_test.go` 的 `stubTransferRepo` 与 `internal/adapters/wails/services_test.go` 的 `memTransferRepo` 都必须补 `ListByBatch`**：计划漏了后者的连带影响，第一次提交因此让 `internal/adapters/wails` 测试包编译失败（`go build` 通过、`go vet ./internal/...` 全红）。这条固化成了前置约定里的「验证必须跑全仓」。
7. **补 `TestMigrateFromV3AppliesV4`**：计划只写了「v3→v4 迁移回归」为可选，实测发现退库并重放是唯一能证明「老库升级不炸」的路径，且必须先删索引才能 `DROP COLUMN`。

### 尚未执行、但已挂上验收条件

- **Task 7 必须同时收紧续传键**：`FindResumable(peer, file_hash)` → 批次内按 `rel_path`。否则一张文件夹里两份内容相同的文件会共享临时文件与最终路径（目录里少一个文件、另一个内容错位）。这是代码质量审查发现的既有语义在批次下被放大，必须在批次上线前解决。
- **`ListRecent(50)` 与 `PurgeFinished(keep)` 的保留窗口以 job 计数**：一个 20000 文件的文件夹会把单文件历史挤出窗口。本次不动，Task 6 之后需要重新考虑保留策略。
