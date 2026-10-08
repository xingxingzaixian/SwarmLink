# 离线文件传输 v2 实施计划

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 对方离线时发文件不再当场失败，而是进入「等待对方上线」并落库；对方上线（或退避到点）自动开始传输；传输中途掉线同样回到等待，对方回来后按位图续传；等待状态跨重启保留，界面上可取消、可立即重试。

**Architecture:** 复用既有 `StateIdle` 与 DB `'queued'`（不动状态机、不改 CHECK），新增两列 `wait_reason` / `next_try_at` 表达"在等谁、什么时候再试"。判定"等待 vs 真失败"用纯函数（主判据是对端可达性，错误文本只做兜底）。调度复用现成的两条驱动：`TopicPeerOnline`/`TopicPeerDiscovered` 订阅 + `bootstrap` 里已有的 1 秒节拍。

**Tech Stack:** Go 1.26、SQLite（`modernc.org/sqlite`，迁移带 checksum）、进程内事件总线、Wails3 beta.24、Vue 3 + Pinia + TypeScript。

**设计与依据：** `docs/plans/2026-10-08-offline-file-transfer-v2-design.md`

---

## 前置约定

- 所有命令都在仓库根目录 `d:\code\OpenSource\swarm-link` 执行（前端命令先 `cd frontend`）。
- 提交信息用仓库既有风格：`feat(transfer): 中文描述`。
- **提交纪律（重要）**：本计划里所有 `git add` 都写成了**显式文件列表**，不要用 `git add <目录>` —— 工作区里有上一轮「系统通知」功能的未提交改动（`internal/app/notify_app.go`、`internal/adapters/systemnotify/` 等），目录级 add 会把它们扫进本功能的提交。
- 已知有 3 个文件被两个功能同时改到，它们的提交会**不可避免地带上上一轮的通知改动**：`internal/adapters/wails/eventbridge.go`（通知的事件常量）、`frontend/src/api/types.ts`（`EV.notifyActivated`）、`frontend/src/events/index.ts`（通知点击订阅）。若这不可接受，需要先把上一轮提交掉。
- **`go test -tags wails ./...` 目前有 4 个与本次无关的既有失败**（`internal/adapters/keystore`、`internal/infra/fsutil` 的文件名安全化用例）。判断"我是否弄坏了什么"时，只对比这些包之外的输出。
- **`npm run typecheck` 目前有既有失败**（`src/api/mock.ts` 缺 `autoStart*` 字段，`SwarmApi` 与 mock 不同步）。本计划会碰到 mock.ts，**顺手把这两处补上**（Task 11 步骤 3），因为不改就无法判断我们引入的新错误。
- 绑定产物 `frontend/bindings/` 已入库。**重新生成的正确命令**（`-ts -i` 不能省，且 cmd.exe 下必须用双引号）：

  ```
  wails3 generate bindings -f "-tags wails" -clean=true -ts -i .
  ```

  生成后 `git status --short -- frontend/bindings` 应只出现本次真正新增/改动的方法。

---

## Task 1: 领域字段与迁移 v3

**Files:**
- Modify: `internal/domain/transfer/job.go`
- Modify: `internal/adapters/store/sqlite/migrations.go:152-153`
- Modify: `internal/adapters/store/sqlite/transfers.go:21-54`、`:171-203`
- Test: `internal/adapters/store/sqlite/sqlite_test.go`

**Step 1: 写失败的测试**

在 `internal/adapters/store/sqlite/sqlite_test.go` 里追加（放在既有的 transfer 用例附近）：

```go
// 等待态与退避时间必须能落库再读回来：重启恢复、sweep 是否到点全靠这两列。
func TestJobWaitingFieldsRoundTrip(t *testing.T) {
	db := newTestDB(t) // 沿用本文件已有的构造方式
	repo := NewTransfers(db)

	id := mustNodeID(t)
	next := time.UnixMilli(1_760_000_000_000)
	job := transfer.Job{
		JobID:       "job-wait-1",
		PeerID:      id,
		FileName:    "报告.pdf",
		FileSize:    4096,
		ChunkSize:   1024,
		TotalChunks: 4,
		Direction:   transfer.DirectionSend,
		LocalPath:   `C:\tmp\报告.pdf`,
		Bitmap:      transfer.NewChunkBitmap(4),
		Status:      transfer.StateIdle,
		WaitReason:  transfer.WaitPeerOffline,
		NextTryAt:   next,
		RetryCount:  3,
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
	if got.WaitReason != transfer.WaitPeerOffline {
		t.Errorf("wait_reason = %q, want %q", got.WaitReason, transfer.WaitPeerOffline)
	}
	if !got.NextTryAt.Equal(next) {
		t.Errorf("next_try_at = %v, want %v", got.NextTryAt, next)
	}
	if got.RetryCount != 3 {
		t.Errorf("retry_count = %d, want 3", got.RetryCount)
	}

	// 零值必须干净地回到零值（不能变成 -6795364578871）
	job.NextTryAt = time.Time{}
	job.WaitReason = transfer.WaitNone
	if err := repo.UpsertJob(job); err != nil {
		t.Fatalf("upsert zero: %v", err)
	}
	got, err = repo.GetJob(job.JobID)
	if err != nil {
		t.Fatalf("get zero: %v", err)
	}
	if !got.NextTryAt.IsZero() {
		t.Errorf("next_try_at = %v, want zero", got.NextTryAt)
	}
}
```

> `newTestDB(t)` / `mustNodeID(t)` 是占位名：用本文件里**已有**的辅助函数（打开临时库、构造 NodeID 的那两个），不要新造。若本文件用的是别的写法（例如直接 `Open(filepath.Join(t.TempDir(), "t.db"))`），照抄它。

**Step 2: 跑测试确认失败**

Run: `go test ./internal/adapters/store/sqlite/ -run TestJobWaitingFieldsRoundTrip -v`
Expected: FAIL —— 编译不过（`unknown field WaitReason`）。

**Step 3: 加领域字段**

`internal/domain/transfer/job.go`，在 `Direction` 常量后面加类型：

```go
// WaitReason 说明一个【还没开始】的任务在等什么。
//
// 空串 = 不在等待。它必须落库而不只是内存标记：sweep 只允许自动拉起
// WaitPeerOffline 的任务 —— 将来加了「用户暂停」时，它绝不能被自动重试。
type WaitReason string

const (
	WaitNone        WaitReason = ""
	WaitPeerOffline WaitReason = "peer_offline"
)
```

然后在 `Job` 结构体里，`RetryCount` 之后加两个字段：

```go
	// WaitReason / NextTryAt 只在 Status 为 StateIdle（"还没开始"）时有意义：
	// 前者说明在等谁，后者说明什么时候可以再试。
	WaitReason WaitReason
	NextTryAt  time.Time
```

**Step 4: 加迁移 v3**

`internal/adapters/store/sqlite/migrations.go`，在 `migrations` 切片末尾（现有 version 2 之后）追加：

```go
	{
		version: 3,
		name:    "transfer_waiting",
		// 「等待对方上线」的两个落库字段。
		//
		// 为什么加列而不是加 status 取值：SQLite 改 CHECK 约束必须重建整表
		// （建新表 → 拷数据 → 换名），而 'queued' 本来就在取值域里、
		// 领域态 StateIdle 本来就叫"还没开始"。加两列是纯追加、零搬迁。
		//
		// 注意迁移是【只追加、永不修改】的：已应用过的 SQL 一旦改动，
		// checksum 校验会直接让应用启动失败。
		sql: `
ALTER TABLE transfer_jobs ADD COLUMN wait_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE transfer_jobs ADD COLUMN next_try_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS ix_transfer_waiting ON transfer_jobs(direction, status, next_try_at);
`,
	},
```

**Step 5: 读写这两列**

`internal/adapters/store/sqlite/transfers.go`：

```go
const transferColumns = `job_id, peer_id, file_name, file_size, file_hash, chunk_size, total_chunks,
                         direction, local_path, temp_path, completed, chunk_bitmap, status,
                         window_size, retry_count, wait_reason, next_try_at, error, created_at, updated_at`
```

`UpsertJob` 的 SQL：INSERT 列表加 `wait_reason, next_try_at`（放在 `retry_count` 之后、`error` 之前），VALUES 相应变成 20 个 `?`，`ON CONFLICT` 段加：

```sql
  wait_reason  = excluded.wait_reason,
  next_try_at  = excluded.next_try_at,
```

参数行改成：

```go
		j.JobID, j.PeerID.String(), j.FileName, j.FileSize, j.FileHash, j.ChunkSize, j.TotalChunks,
		string(j.Direction), j.LocalPath, j.TempPath, j.Completed, j.Bitmap.Bytes(), j.Status.DBStatus(),
		j.WindowSize, j.RetryCount, string(j.WaitReason), msOf(j.NextTryAt), j.Error, msOf(j.CreatedAt), msOf(j.UpdatedAt),
```

`scanTransfer` 加两个局部变量与两次 Scan：

```go
		var (
			...
			waitReason string
			nextTryAt  int64
		)
		if err := s.Scan(
			&out.JobID, &peerID, &out.FileName, &out.FileSize, &out.FileHash, &out.ChunkSize,
			&out.TotalChunks, &direction, &out.LocalPath, &tempPath, &out.Completed, &bitmap,
			&status, &out.WindowSize, &out.RetryCount, &waitReason, &nextTryAt, &errMsg, &createdAt, &updatedAt,
		); err != nil {
			return out, err
		}
		...
		out.WaitReason = transfer.WaitReason(waitReason)
		out.NextTryAt = timeOf(nextTryAt)
```

> `msOf` / `timeOf`（`db.go:95-107`）已经把零值处理成 `0` ↔ `time.Time{}`，**不要**自己写 UnixMilli。

**Step 6: 跑测试**

Run: `go test ./internal/adapters/store/sqlite/ -run TestJobWaitingFieldsRoundTrip -v`
Expected: PASS

Run: `go test ./internal/adapters/store/... ./internal/domain/transfer/...`
Expected: PASS（全部 ok）

**Step 7: 提交**

```
git add internal/domain/transfer/job.go internal/adapters/store/sqlite/transfers.go internal/adapters/store/sqlite/migrations.go internal/adapters/store/sqlite/sqlite_test.go
git commit -m "feat(transfer): 新增等待态落库字段与迁移 v3"
```

---

## Task 2: `ListWaiting` 端口与两个实现

**Files:**
- Modify: `internal/domain/ports/ports.go:96-110`
- Modify: `internal/adapters/store/sqlite/transfers.go`
- Modify: `internal/adapters/store/mem/mem.go:305-319`
- Modify: `internal/app/transfer_app_test.go:16-45`（stub 仓储）
- Test: `internal/adapters/store/sqlite/sqlite_test.go`、`internal/adapters/store/mem/mem_test.go`

**Step 1: 写失败的测试**

`internal/adapters/store/mem/mem_test.go`（sqlite 那份同构，两处都要加）：

```go
// ListWaiting 只认「发送方向 + 等待对方上线」：其余状态一律不返回。
//
// 这条守卫的意义：sweep 拿它去自动重试，多返回一条就是把不该动的任务拉起来。
func TestListWaitingFiltersByReasonAndDirection(t *testing.T) {
	repo := mem.NewMessages(clock.New())
	peerID := mustNodeID(t)

	base := transfer.Job{
		PeerID: peerID, FileName: "a.bin", FileSize: 4096, ChunkSize: 1024,
		TotalChunks: 4, LocalPath: "/tmp/a.bin", Bitmap: transfer.NewChunkBitmap(4),
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	mk := func(id string, st transfer.State, reason transfer.WaitReason, dir transfer.Direction) transfer.Job {
		j := base
		j.JobID, j.Status, j.WaitReason, j.Direction = id, st, reason, dir
		return j
	}

	for _, j := range []transfer.Job{
		mk("wait-send", transfer.StateIdle, transfer.WaitPeerOffline, transfer.DirectionSend),
		mk("idle-no-reason", transfer.StateIdle, transfer.WaitNone, transfer.DirectionSend),
		mk("running", transfer.StateTransferring, transfer.WaitNone, transfer.DirectionSend),
		mk("wait-recv", transfer.StateIdle, transfer.WaitPeerOffline, transfer.DirectionRecv),
		mk("failed", transfer.StateFailed, transfer.WaitNone, transfer.DirectionSend),
	} {
		if err := repo.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.ListWaiting()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].JobID != "wait-send" {
		t.Fatalf("ListWaiting = %v, want 只有 wait-send", got)
	}
}
```

**Step 2: 跑测试确认失败**

Run: `go test ./internal/adapters/store/mem/ -run TestListWaitingFiltersByReasonAndDirection -v`
Expected: FAIL —— 编译不过（`repo.ListWaiting undefined`）。

**Step 3: 加端口**

`internal/domain/ports/ports.go` 的 `TransferRepo` 里，紧跟 `ListActive()` 之后：

```go
	// ListWaiting 返回「等待对方上线」的发送任务（重启恢复与周期性 sweep 用）。
	//
	// 【不要】用 ListActive 代替：那个语义是「未结束」，把 failed/paused/
	// verifying 都算进来，拿它驱动自动重试会把不该重试的任务一起拉起来。
	ListWaiting() ([]transfer.Job, error)
```

**Step 4: sqlite 实现**

`internal/adapters/store/sqlite/transfers.go`，放在 `ListActive` 之后：

```go
// ListWaiting 返回等待对方上线的发送任务（语义见 ports.TransferRepo.ListWaiting）。
func (t *Transfers) ListWaiting() ([]transfer.Job, error) {
	rows, err := t.db.Query(`
SELECT `+transferColumns+` FROM transfer_jobs
WHERE direction = 'send' AND status = 'queued' AND wait_reason = ?
ORDER BY created_at`, string(transfer.WaitPeerOffline))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []transfer.Job
	for rows.Next() {
		item, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
```

> 到点判断（`next_try_at <= now`）刻意留在应用层：那里有 `ports.Clock`，可以用 `clock.Fake` 做确定性测试；塞进 SQL 就把时间语义钉死在真实时钟上。

**Step 5: mem 实现**

`internal/adapters/store/mem/mem.go`，放在 `ListActive` 之后：

```go
// ListWaiting 返回等待对方上线的发送任务（语义见 ports.TransferRepo.ListWaiting）。
func (s *Messages) ListWaiting() ([]transfer.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]transfer.Job, 0)
	for _, j := range s.jobs {
		if j.Direction != transfer.DirectionSend ||
			j.Status != transfer.StateIdle ||
			j.WaitReason != transfer.WaitPeerOffline {
			continue
		}
		j.Bitmap = j.Bitmap.Copy()
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.Before(out[k].CreatedAt) })
	return out, nil
}
```

**Step 6: 补 stub 仓储**

`internal/app/transfer_app_test.go` 的 `stubTransferRepo` 加：

```go
// ListWaiting 返回按 job_id 排好序的等待任务，供调度类测试断言。
func (s *stubTransferRepo) ListWaiting() ([]transfer.Job, error) {
	out := make([]transfer.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if j.JobID == "" {
			continue
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].JobID < out[k].JobID })
	return out, nil
}
```

> 这里刻意【不】按领域语义过滤：调度测试要能塞进任意状态的任务，由被测代码自己判断该不该动。（需要在文件头 import `"sort"`。）

**Step 7: 跑测试**

Run: `go test ./internal/adapters/store/... ./internal/app/...`
Expected: PASS

**Step 8: 提交**

```
git add internal/domain/ports/ports.go internal/adapters/store/sqlite/transfers.go internal/adapters/store/sqlite/sqlite_test.go internal/adapters/store/mem/mem.go internal/adapters/store/mem/mem_test.go internal/app/transfer_app_test.go
git commit -m "feat(transfer): 新增 ListWaiting 端口与 sqlite/mem 实现"
```

---

## Task 3: 失败分类（纯函数）

**Files:**
- Create: `internal/app/transfer_waiting.go`
- Test: `internal/app/transfer_waiting_test.go`

**Step 1: 写失败的测试**

新建 `internal/app/transfer_waiting_test.go`：

```go
package app

import (
	"errors"
	"fmt"
	"testing"
)

// 分类规则表：主判据是「对端可达性」，错误文本只做兜底。
//
// 反例比正例更重要 —— 把真失败误判成"等对方上线"会让用户永远等一个
// 不可能成功的任务，比当场报错糟糕得多。
func TestShouldWaitForPeer(t *testing.T) {
	dialErr := errors.New(`tcp: dial 192.168.1.9:41641: connectex: No connection could be made`)

	cases := []struct {
		name        string
		reachable   bool
		sessionLive bool
		err         error
		want        bool
	}{
		{"对端不在目录里 → 等", false, false, errors.New("transfer: no known address for peer abc"), true},
		{"对端不在目录里，哪怕错误看不懂 → 等", false, false, errors.New("some brand new error"), true},
		{"会话没了（对方掉线） → 等", true, false, errors.New("write tcp: connection reset by peer"), true},
		{"看起来可达但发不出去（拨号失败） → 等", true, true, dialErr, true},
		{"等 FILE_META_ACK 超时 → 等", true, true, errors.New("transfer: timeout waiting for FILE_META_ACK"), true},
		{"等 FILE_DONE_ACK 超时 → 等", true, true, errors.New("transfer: timeout waiting for FILE_DONE_ACK"), true},

		{"文件不存在 → 真失败", true, true, errors.New("transfer: open /tmp/gone.bin: no such file or directory"), false},
		{"校验失败 → 真失败", true, true, errors.New("transfer: peer verify failed: bad chunk 3"), false},
		{"元数据非法 → 真失败", true, true, errors.New("transfer: invalid meta (total_chunks=0)"), false},
		{"空文件 → 真失败", true, true, errors.New("transfer: empty file"), false},
		{"ctx 取消 → 真失败", true, true, fmt.Errorf("transfer: hash /tmp/a.bin: %w", errors.New("context canceled")), false},
		// 本地读文件读到一半文件被截断：看起来像 EOF，但绝不能当成"等对方上线"
		{"读文件时 unexpected EOF → 真失败", true, true, errors.New("read /tmp/a.bin: unexpected EOF"), false},
		{"nil 错误 → 不算等待（调用方只在 err != nil 时问）", false, false, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldWaitForPeer(failureEvidence{
				PeerReachable: tc.reachable,
				SessionAlive:  tc.sessionLive,
				Err:           tc.err,
			})
			if got != tc.want {
				t.Errorf("shouldWaitForPeer = %v, want %v", got, tc.want)
			}
		})
	}
}
```

**Step 2: 跑测试确认失败**

Run: `go test ./internal/app/ -run TestShouldWaitForPeer -v`
Expected: FAIL —— 编译不过（`undefined: shouldWaitForPeer`）。

**Step 3: 实现**

新建 `internal/app/transfer_waiting.go`：

```go
package app

import (
	"strings"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
)

// failureEvidence 是判定「等一会儿还是就此失败」所需的证据快照。
//
// 做成一个结构体而不是三个参数：判定必须是【纯函数】才能表驱动测试，
// 而证据的采集（读目录、读连接管理器）留在调用点。
type failureEvidence struct {
	// PeerReachable：对端此刻看起来可达（本机持有它的会话，或目录里在线/已发现且有地址）。
	PeerReachable bool
	// SessionAlive：本机此刻仍持有它的会话。对方掉线后这里会变成 false。
	SessionAlive bool
	Err          error
}

// permanentPatterns 是「看起来像连接问题、其实是永久失败」的错误特征。
//
// 必须先判它们：`unexpected EOF` 是本地读文件读到一半文件被截断，
// 而裸 `EOF` 是连接断开 —— 两者都有 "EOF"，判错方向就再也发不出去。
var permanentPatterns = []string{
	"unexpected EOF",
	"peer verify failed",
	"invalid meta",
	"empty file",
	"no such file",
	"is a directory",
	"permission denied",
	"no space left",
}

// unreachablePatterns 是「对端此刻够不着」的错误特征（兜底用）。
//
// 只在主判据（可达性）看不出问题时才看它：文案会随版本变化，
// 把它当主判据早晚会失效。
var unreachablePatterns = []string{
	"no known address for peer",
	"timeout waiting for FILE_META_ACK",
	"timeout waiting for FILE_DONE_ACK",
	"connection reset",
	"broken pipe",
	"connection refused",
	"use of closed network connection",
	"i/o timeout",
}

// shouldWaitForPeer 判断一次失败是「对端暂时够不着」还是「真的失败了」。
//
// 顺序刻意如此：
//  1. 永久失败特征优先 —— 把「文件被删」误判成等待，用户会永远等一个
//     不可能成功的任务；
//  2. 对端不可达/会话已消失 → 等（这是本功能的主路径，不依赖任何文案）；
//  3. 兜底看文案（覆盖"目录说在线、对端其实已死"的情况）。
func shouldWaitForPeer(e failureEvidence) bool {
	if e.Err == nil {
		return false
	}
	msg := e.Err.Error()
	if containsAny(msg, permanentPatterns) {
		return false
	}
	if !e.PeerReachable || !e.SessionAlive {
		return true
	}
	return containsAny(msg, unreachablePatterns)
}

func containsAny(s string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// peerReachable 判断对端是否「值得一试」。
//
// 刻意宽松：目录里 online/discovered 且有地址就算可达 —— 拨号是按需的
// （ADR-009），先试一次再失败，比"没连上就不发"更符合直觉，
// 也不会让"能发但现在没连"这种寻常状态退化成等待。
func (a *TransferApp) peerReachable(peerID identity.NodeID) bool {
	if a.sessionAlive(peerID) {
		return true
	}
	if a.dir == nil {
		return false
	}
	p, ok := a.dir.Get(peerID)
	if !ok || p.LastAddr == "" {
		return false
	}
	return p.State == peer.StateOnline || p.State == peer.StateDiscovered
}

// sessionAlive 报告本机此刻是否仍持有该对端的会话。
func (a *TransferApp) sessionAlive(peerID identity.NodeID) bool {
	if a.conns == nil {
		return false
	}
	_, ok := a.conns.SessionOf(peerID)
	return ok
}
```

**Step 4: 跑测试**

Run: `go test ./internal/app/ -run TestShouldWaitForPeer -v`
Expected: PASS（13 个子用例全过）

**Step 5: 提交**

```
git add internal/app/transfer_waiting.go internal/app/transfer_waiting_test.go
git commit -m "feat(transfer): 新增失败分类，区分等待对方上线与真失败"
```

---

## Task 4: `SendFileAs` 重排顺序 + 离线直接落等待

**要解决的问题**：现在 `SendFileAs` 是「抢信号量 → 整文件哈希 → 建 job → 拨号」（`transfer_app.go:170`→`:182`→`:223`）。对方离线时点发送，会先对 10GB 文件算一遍全量 SHA-256，然后才失败。新顺序：**本地文件检查 → 对端可达性 → （不可达就落等待，不哈希）→ 抢信号量 → 哈希 → 建 job → 传输**。

**Files:**
- Modify: `internal/app/transfer_app.go:135-303`
- Modify: `internal/app/transfer_waiting.go`（新增 `newWaitingJob` / `markWaiting` / `publishState` / `claimStarting` / `releaseStarting`）
- Modify: `internal/app/transfer_app.go:46-51`、`:99-114`（`starting` 集合）
- Modify: `internal/infra/eventbus/payloads.go:44-49`（`TransferState` 在 payloads.go 里，eventbus.go 不需要改）（`TransferState.Reason`）
- Test: `internal/app/transfer_waiting_test.go`

**Step 1: 写失败的测试**

追加到 `internal/app/transfer_waiting_test.go`：

```go
// 对方离线时点发送：立刻落一条「等待对方上线」的任务，不报错、不哈希。
//
// 「不哈希」是这条用例的核心价值：10GB 文件为了等对方而先读一遍磁盘，
// 是这次改动要消灭的浪费，而它只能靠"不碰文件内容"来证明 ——
// 因此这里给一个存在但不可读的路径是不行的，用一个【大】文件也不直观，
// 改用「文件存在但 dir 里查不到对端」+ 断言返回得快、且没有 transfer 事件。
func TestSendFileWhilePeerOfflineWaitsWithoutHashing(t *testing.T) {
	repo := &stubTransferRepo{}
	bus := eventbus.New()
	states := make(chan eventbus.TransferState, 4)
	bus.Subscribe(eventbus.TopicTransferState, func(p any) {
		if ev, ok := p.(eventbus.TransferState); ok {
			states <- ev
		}
	})
	bus.Subscribe(eventbus.TopicTransferError, func(any) {
		t.Error("等待不是失败：不该发 transfer:error")
	})

	// dir/conns 为 nil → peerReachable 一律 false
	a := NewTransferApp(identity.NodeID{}, repo, nil, nil, nil, bus, clock.New(), nil)
	peerID := mustNodeID(t) // 同上：用本包测试里已有的构造方式

	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{7}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	const jobID = "job-offline-send"
	job, err := a.SendFileAs(context.Background(), peerID, path, jobID)
	if err != nil {
		t.Fatalf("等待不该返回错误，得到 %v", err)
	}
	if job.Status != transfer.StateIdle {
		t.Errorf("status = %q, want %q（Idle 就是"还没开始"）", job.Status, transfer.StateIdle)
	}
	if job.WaitReason != transfer.WaitPeerOffline {
		t.Errorf("waitReason = %q, want %q", job.WaitReason, transfer.WaitPeerOffline)
	}
	if job.FileSize != 4096 {
		t.Errorf("fileSize = %d, want 4096（等等也要能显示进度口径）", job.FileSize)
	}
	if job.TotalChunks != 4 {
		t.Errorf("totalChunks = %d, want 4", job.TotalChunks)
	}
	if job.FileHash != "" {
		t.Errorf("还没开始传就不该有 file_hash，得到 %q", job.FileHash)
	}
	if job.NextTryAt.IsZero() {
		t.Error("下次重试时间必须落下，否则 sweep 不会拾取它")
	}

	stored, err := repo.GetJob(jobID)
	if err != nil {
		t.Fatalf("等待任务必须落库（否则重启后丢失）: %v", err)
	}
	if stored.WaitReason != transfer.WaitPeerOffline {
		t.Errorf("落库的 waitReason = %q", stored.WaitReason)
	}

	select {
	case ev := <-states:
		if ev.To != string(transfer.StateIdle) || ev.Reason != string(transfer.WaitPeerOffline) {
			t.Errorf("state 事件 = %+v，期望 idle/peer_offline", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("进入等待必须发 transfer.state，否则界面永远显示「协商中」")
	}
}

// 真失败仍然必须是失败：不能被分类逻辑顺手"等"过去。
func TestSendFileMissingFileStillFails(t *testing.T) {
	repo := &stubTransferRepo{}
	bus := eventbus.New()
	got := make(chan eventbus.TransferError, 1)
	bus.Subscribe(eventbus.TopicTransferError, func(p any) {
		if ev, ok := p.(eventbus.TransferError); ok {
			got <- ev
		}
	})
	a := NewTransferApp(identity.NodeID{}, repo, nil, nil, nil, bus, clock.New(), nil)

	if _, err := a.SendFileAs(context.Background(), mustNodeID(t), filepath.Join(t.TempDir(), "nope.bin"), "job-missing"); err == nil {
		t.Fatal("文件不存在必须是错误")
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("失败路径必须发 transfer:error")
	}
	j, err := repo.GetJob("job-missing")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != transfer.StateFailed {
		t.Errorf("status = %q, want failed", j.Status)
	}
}
```

> 已存在的 `TestSendFileAsPublishesErrorOnEarlyFailure` 覆盖了同一件事（用 `/definitely/not/here.bin`）。它与上面这条不冲突，两条都留着：前者锁定"事件带上调用方的 job_id"，后者锁定"本地文件错误优先于可达性判定"。

**Step 2: 跑测试确认失败**

Run: `go test ./internal/app/ -run "TestSendFileWhilePeerOfflineWaitsWithoutHashing|TestSendFileMissingFileStillFails" -v`
Expected: FAIL —— 现有实现对离线是 `failed`（`job.Status` 为 failed，或返回 dial 错误）。

**Step 3: 加 `starting` 集合**

`internal/app/transfer_app.go` 的结构体尾部，`sendSem chan struct{}` 之前加：

```go
	// starting 记录「已经交给传输 goroutine、但还可能在建任务路上」的 job_id。
	//
	// 为什么不能只看 senders：senders 是在 SendFileAs 内部（抢到信号量之后）
	// 才登记的，而 sweep 与 peer.online 两个触发点完全可能同时命中同一条
	// 等待任务 —— 那样会同时跑两个 goroutine、两次全文件哈希。
	starting map[string]struct{}
```

`NewTransferApp` 的返回值里加 `starting: make(map[string]struct{}),`。

**Step 4: 加等待任务的构造与标记**

追加到 `internal/app/transfer_waiting.go`：

```go
// newWaitingJob 组装一条「等对方上线」的轻量任务。
//
// 刻意只做 stat：等待期不读文件内容（不哈希）。哈希只在真要传的那一刻
// 发生，file_hash 也留到那时再填 —— 它唯一的用处是让对端按
// (peer, file_hash) 找到可续传的临时文件。
func (a *TransferApp) newWaitingJob(jobID string, peerID identity.NodeID, path string, size int64) transfer.Job {
	chunkSize := a.ChunkSize
	if chunkSize <= 0 {
		chunkSize = transfer.DefaultChunkSize
	}
	now := a.clk.Now()
	return transfer.Job{
		JobID:       jobID,
		PeerID:      peerID,
		FileName:    fileBase(path),
		FileSize:    size,
		ChunkSize:   chunkSize,
		TotalChunks: transfer.ChunkCount(size, chunkSize),
		Direction:   transfer.DirectionSend,
		LocalPath:   path,
		Bitmap:      transfer.NewChunkBitmap(transfer.ChunkCount(size, chunkSize)),
		Status:      transfer.StateIdle,
		WindowSize:  a.windowSize(),
		WaitReason:  transfer.WaitPeerOffline,
		NextTryAt:   now.Add(backoff(1, a.BaseBackoff, a.MaxBackoff)),
		RetryCount:  1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// markWaiting 把一个已有任务转入「等待对方上线」并落库。
//
// 不写 job.Error：那不是失败，把原因写进去只会让传输列表整行标红。
// 原因交给日志（排障用）与 WaitReason（界面用）。
func (a *TransferApp) markWaiting(job transfer.Job, jobID string, peerID identity.NodeID, path string, cause error) transfer.Job {
	if job.JobID == "" {
		job = a.newWaitingJob(jobID, peerID, path, sizeOf(path))
	}
	from := string(job.Status)
	next := job.RetryCount + 1
	job.RetryCount = next
	job.Status = transfer.StateIdle
	job.WaitReason = transfer.WaitPeerOffline
	job.Error = ""
	job.NextTryAt = a.clk.Now().Add(backoff(next, a.BaseBackoff, a.MaxBackoff))
	job.UpdatedAt = a.clk.Now()
	_ = a.store.UpsertJob(job)
	if a.lg != nil && cause != nil {
		a.lg.Info("transfer: 对端不可达，转入等待",
			"job_id", job.JobID, "peer_id", peerID.String(), "attempt", next, "err", cause)
	}
	a.publishState(job, from, string(transfer.StateIdle), string(transfer.WaitPeerOffline))
	return job
}

// sizeOf 取文件大小；取不到返回 0（调用点随后会因空文件而真失败）。
func sizeOf(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// publishState 广播一次任务状态变化。UI 靠它把「等待对方上线」显示出来。
func (a *TransferApp) publishState(job transfer.Job, from, to, reason string) {
	if a.bus == nil {
		return
	}
	a.bus.Publish(eventbus.TopicTransferState, eventbus.TransferState{
		JobID: job.JobID, From: from, To: to, Reason: reason,
	})
}

// claimStarting 抢占一个 job；已被抢占返回 false。
func (a *TransferApp) claimStarting(jobID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.starting == nil {
		a.starting = make(map[string]struct{})
	}
	if _, ok := a.starting[jobID]; ok {
		return false
	}
	a.starting[jobID] = struct{}{}
	return true
}

// releaseStarting 释放抢占。
func (a *TransferApp) releaseStarting(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.starting, jobID)
}

// isStarting 报告任务是否已被交给传输 goroutine（取消时要拦住它）。
func (a *TransferApp) isStarting(jobID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.starting[jobID]
	return ok
}
```

**Step 5: `eventbus.TransferState` 加 Reason**

`internal/infra/eventbus/payloads.go`：

```go
// TransferState 是 transfer.state 的载荷。
//
// Reason 目前只用于等待态（"peer_offline"）；其余状态为空串。
type TransferState struct {
	JobID  string
	From   string
	To     string
	Reason string
}
```

**Step 6: 重排 `SendFileAs`**

`internal/app/transfer_app.go`：把 `SendFileAs` 拆成"带 force 的实现 + 保持原签名的包装"，并在统一出口里加分类。整段替换 `:135`-`:226`（从函数头到 `sess, err := a.session(...)` 之前）：

```go
// SendFileAs 与 SendFile 完全一致，只是 job_id 由调用方指定。
//
// 存在的唯一理由：异步调用方（桌面外壳）必须在任务【真正开始之前】就拿到
// job_id，才能在同一个 RPC 的返回值里把它交给界面，让界面据此把任务条目
// 和随后到达的 transfer:progress / done / error 事件对上。
// 若 id 在本层生成，异步调用方就只能另造一个 id 返回 —— 那个 id
// 与任何事件都对不上，表现为「发了文件但进度永远不出现」。
func (a *TransferApp) SendFileAs(
	ctx context.Context,
	peerID identity.NodeID,
	path string,
	jobID string,
) (transfer.Job, error) {
	// 抢占：同一条任务同一时刻只有一个 goroutine 在跑（sweep 与 peer.online
	// 都可能来拉它）；初始发送也走这里，因此那次抢占与后续重试是同一把锁。
	if !a.claimStarting(jobID) {
		return transfer.Job{}, fmt.Errorf("transfer: job %s 已在传输中", jobID)
	}
	defer a.releaseStarting(jobID)
	return a.sendFile(ctx, peerID, path, jobID, false)
}

// sendFile 是传输尝试的实现。force 为真时【跳过】"对端看起来不可达"的
// 快速判定 —— 只给「立即重试」用：用户点了按钮就是想要真的试一次，
// 哪怕目录说对方离线（目录可能滞后于现实）。
func (a *TransferApp) sendFile(
	ctx context.Context,
	peerID identity.NodeID,
	path string,
	jobID string,
	force bool,
) (job transfer.Job, err error) {
	if jobID == "" {
		return transfer.Job{}, fmt.Errorf("transfer: job_id 不能为空")
	}

	// 统一出口：任何失败路径都必须让前端收到反馈，且只发一次。
	//
	// 与过去唯一的区别：以前【所有】失败都置 failed，现在先问一句
	// "是不是对方暂时够不着" —— 是就转入等待（不是错误），
	// 由 sweep 在对方上线或退避到点时重试同一条任务。
	defer func() {
		if err == nil {
			return
		}
		if job.JobID == "" {
			job.JobID = jobID
		}
		if shouldWaitForPeer(failureEvidence{
			PeerReachable: a.peerReachable(peerID),
			SessionAlive:  a.sessionAlive(peerID),
			Err:           err,
		}) {
			job = a.markWaiting(job, jobID, peerID, path, err)
			err = nil // 「等一会儿」不是错误
			return
		}
		job.Status = transfer.StateFailed
		job.Error = err.Error()
		job.WaitReason = transfer.WaitNone
		job.NextTryAt = time.Time{}
		job.UpdatedAt = a.clk.Now()
		_ = a.store.UpsertJob(job)
		if a.bus != nil {
			a.bus.Publish(eventbus.TopicTransferError, eventbus.TransferError{
				JobID: job.JobID, PeerID: peerID.String(), Err: err,
			})
		}
	}()

	// 1) 本地文件先判。顺序是刻意的：文件不在/是空文件时不必等对方上线
	//    （等多久都发不出去），也不该占用并发额度。
	chunkSize := a.ChunkSize
	if chunkSize <= 0 {
		chunkSize = transfer.DefaultChunkSize
	}
	st, serr := os.Stat(path)
	if serr != nil {
		return transfer.Job{}, fmt.Errorf("transfer: %w", serr)
	}
	if st.IsDir() {
		return transfer.Job{}, fmt.Errorf("transfer: %s is a directory", path)
	}
	statSize := st.Size()
	if transfer.ChunkCount(statSize, chunkSize) == 0 {
		return transfer.Job{}, fmt.Errorf("transfer: empty file")
	}

	// 2) 对端明显不在 → 直接落等待，连哈希都不做。
	//
	//    这是本次改动的核心：过去这里会先算完整文件 SHA-256 再拨号失败。
	if !force && !a.peerReachable(peerID) {
		job = a.newWaitingJob(jobID, peerID, path, statSize)
		if uerr := a.store.UpsertJob(job); uerr != nil {
			return job, uerr
		}
		if a.lg != nil {
			a.lg.Info("transfer: 对端离线，任务进入等待",
				"job_id", jobID, "peer_id", peerID.String(), "file", job.FileName)
		}
		a.publishState(job, "", string(transfer.StateIdle), string(transfer.WaitPeerOffline))
		return job, nil
	}

	select {
	case a.sendSem <- struct{}{}:
		defer func() { <-a.sendSem }()
	case <-ctx.Done():
		return transfer.Job{}, ctx.Err()
	}

	fileHash, chunkHashes, size, err := hashFileChunks(path, chunkSize)
	if err != nil {
		return transfer.Job{}, fmt.Errorf("transfer: hash %s: %w", path, err)
	}
	total := transfer.ChunkCount(size, chunkSize)
	if total == 0 {
		return transfer.Job{}, fmt.Errorf("transfer: empty file")
	}

	window := a.WindowSize
	if window <= 0 {
		window = transfer.DefaultWindow
	}

	// job 是命名返回值，此处用 = 而非 :=
	job = transfer.Job{
		JobID:       jobID,
		PeerID:      peerID,
		FileName:    fileBase(path),
		FileSize:    size,
		FileHash:    fileHash,
		ChunkSize:   chunkSize,
		TotalChunks: total,
		Direction:   transfer.DirectionSend,
		LocalPath:   path,
		Bitmap:      transfer.NewChunkBitmap(total),
		Status:      transfer.StateMetaExchange,
		WindowSize:  window,
		CreatedAt:   a.clk.Now(),
		UpdatedAt:   a.clk.Now(),
	}
	if err := a.store.UpsertJob(job); err != nil {
		return job, err
	}
	// ……以下保持原样（打开文件、session、FILE_META、等 ACK、runSender、FILE_DONE）
	// 注意：job 字面量里【不要】带 WaitReason / NextTryAt，
	//      这样重试成功开始时它们会被写回空值（等待态自动解除）。
```

**Step 7: 跑测试**

Run: `go test ./internal/app/ -v -run "TestSendFile"`
Expected: PASS（含既有的 `TestSendFileAsPublishesErrorOnEarlyFailure`）

Run: `go test ./internal/app/... ./internal/adapters/...`
Expected: PASS

**Step 8: 提交**

```
git add internal/app/transfer_app.go internal/app/transfer_waiting.go internal/app/transfer_waiting_test.go internal/infra/eventbus/payloads.go
git commit -m "feat(transfer): 离线发起直接落等待，不再先哈希再失败"
```

---

## Task 5: 会话中断也进等待（stub 连接单测）

**Files:**
- Test: `internal/app/transfer_waiting_test.go`

**Step 1: 写测试（含 fake）**

追加到 `internal/app/transfer_waiting_test.go`：

```go
// ---------- 可控的假连接 / 假目录 ----------

type fakeSession struct {
	peer    identity.NodeID
	mu      sync.Mutex
	failAt  int // 第几次 Send 起开始失败
	sends   int
	sendErr error
	frames  []protocol.Frame
}

func (s *fakeSession) PeerID() identity.NodeID { return s.peer }
func (s *fakeSession) Send(f protocol.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends++
	if s.sendErr != nil && s.sends >= s.failAt {
		return s.sendErr
	}
	s.frames = append(s.frames, f)
	return nil
}
func (s *fakeSession) Recv() (protocol.Frame, error) { select {} } // 永不返回
func (s *fakeSession) RTT() time.Duration            { return time.Millisecond }
func (s *fakeSession) Close() error                  { return nil }

type fakeConns struct {
	mu      sync.Mutex
	session *fakeSession
	alive   bool
}

func (c *fakeConns) Dial(context.Context, identity.NodeID, string) (ports.Session, error) {
	if c.session == nil {
		return nil, errors.New("fake: no route")
	}
	return c.session, nil
}
func (c *fakeConns) Accept(context.Context) (ports.Session, error) { return nil, errors.New("fake: unused") }
func (c *fakeConns) SessionOf(id identity.NodeID) (ports.Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil || !c.alive {
		return nil, false
	}
	return c.session, true
}
func (c *fakeConns) Broadcast(protocol.Frame, ...identity.NodeID) error { return nil }
func (c *fakeConns) Close(identity.NodeID) error                       { return nil }

// 传一半对方掉线：任务必须转入等待（而不是 failed），
// 并在状态事件里说清楚原因。
func TestMidTransferDisconnectWaitsForPeer(t *testing.T) {
	peerID := mustNodeID(t)
	repo := &stubTransferRepo{}
	bus := eventbus.New()
	states := make(chan eventbus.TransferState, 4)
	errorsCh := make(chan eventbus.TransferError, 4)
	bus.Subscribe(eventbus.TopicTransferState, func(p any) {
		if ev, ok := p.(eventbus.TransferState); ok {
			states <- ev
		}
	})
	bus.Subscribe(eventbus.TopicTransferError, func(p any) {
		if ev, ok := p.(eventbus.TransferError); ok {
			errorsCh <- ev
		}
	})

	sess := &fakeSession{
		peer:    peerID,
		failAt:  2, // 第 1 帧（FILE_META）通过，之后断开
		sendErr: errors.New("use of closed network connection"),
	}
	conns := &fakeConns{session: sess, alive: false} // 断线后 SessionOf 已查不到

	a := NewTransferApp(identity.NodeID{}, repo, nil, conns, nil, bus, clock.New(), nil)
	a.ChunkSize = 1024

	path := filepath.Join(t.TempDir(), "half.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{9}, 8*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	const jobID = "job-mid-drop"
	job, err := a.SendFileAs(context.Background(), peerID, path, jobID)
	if err != nil {
		t.Fatalf("中途掉线应转入等待而不是报错，得到 %v", err)
	}
	if job.Status != transfer.StateIdle || job.WaitReason != transfer.WaitPeerOffline {
		t.Fatalf("status/reason = %q/%q, want idle/peer_offline", job.Status, job.WaitReason)
	}

	select {
	case ev := <-states:
		if ev.Reason != string(transfer.WaitPeerOffline) {
			t.Errorf("state 事件 reason = %q", ev.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("转入等待必须发 transfer.state")
	}
	select {
	case ev := <-errorsCh:
		t.Fatalf("等待不是失败，不该发 transfer:error（得到 %v）", ev.Err)
	default:
	}
}
```

> `conns` 的 `alive=false` 让 `sessionAlive` 为 false，从而**不依赖任何错误文案**就判定为等待 —— 这正是设计里"主判据不是字符串"的意思。

**Step 2: 跑测试**

Run: `go test ./internal/app/ -run TestMidTransferDisconnectWaitsForPeer -v`
Expected: PASS（若 Task 4 已正确落地）

**Step 3: 提交**

```
git add internal/app/transfer_waiting_test.go
git commit -m "test(transfer): 锁定中途掉线转入等待的行为"
```

---

## Task 6: 等待调度（Start / SweepWaiting / wakePeer）

**Files:**
- Modify: `internal/app/transfer_waiting.go`
- Test: `internal/app/transfer_waiting_test.go`

**Step 1: 写失败的测试**

```go
// sweep 只挑「到点」的等待任务，且同一对端每轮只启动一个。
//
// 后半句是硬要求：对方一上线，10 个等待任务同时开始整文件哈希，
// 磁盘会先崩 —— 而"同时"在这里是可断言的（启动假会话的 Send 次数）。
func TestSweepWaitingRespectsBackoffAndPerPeerSerialization(t *testing.T) {
	peerID := mustNodeID(t)
	repo := &stubTransferRepo{}
	bus := eventbus.New()
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))

	sess := &fakeSession{peer: peerID}
	conns := &fakeConns{session: sess, alive: true}

	a := NewTransferApp(identity.NodeID{}, repo, conns, nil, nil, bus, fc, nil)
	a.ChunkSize = 1024
	a.BaseBackoff = time.Second

	dir := t.TempDir()
	var paths []string
	for _, id := range []string{"job-1", "job-2", "job-3"} {
		p := filepath.Join(dir, id+".bin")
		if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 4*1024), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
		j := a.newWaitingJob(id, peerID, p, 4096)
		if err := repo.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	// 未到点（NextTryAt = now+1s）：一条都不该动
	a.SweepWaiting(context.Background())
	waitNoSend(t, sess) // 见下方辅助：断言此刻还没有任何帧发出

	// 到点：同一对端这一轮只启动一条
	fc.Advance(2 * time.Second)
	a.SweepWaiting(context.Background())
	if got := startingCount(a); got != 1 {
		t.Fatalf("同一对端一轮启动了 %d 条，want 1", got)
	}
}

// waitNoSend 断言短时间内没有任何帧发出（避免用 sleep 猜时序）。
func waitNoSend(t *testing.T, s *fakeSession) {
	t.Helper()
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := s.sends
		s.mu.Unlock()
		if n != 0 {
			t.Fatalf("不该发帧，却发了 %d 次", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startingCount(a *TransferApp) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.starting)
}
```

> 提示：这一轮里那个被启动的任务会走完 `hashFileChunks` 并发出 FILE_META，随后因为假会话 `Recv()` 永不返回而阻塞在"等 FILE_META_ACK"。测试结束前不必清理它（进程退出即回收），但**不要**断言它的最终状态。

**Step 2: 跑测试确认失败**

Run: `go test ./internal/app/ -run TestSweepWaitingRespectsBackoffAndPerPeerSerialization -v`
Expected: FAIL —— `a.SweepWaiting undefined`。

**Step 3: 实现**

追加到 `internal/app/transfer_waiting.go`：

```go
// Start 订阅「对端上线」类事件，作为等待任务的主要触发源。
//
// 与 ChatApp.Start 同构：peer.online（TCP 握手成功）与 peer.discovered
// （UDP 通告，覆盖"对方刚启动、我们还没连"）都要订阅 —— 只订阅前者会漏掉
// 「对端一直在广播但没建连」这种最常见的离线恢复场景。
func (a *TransferApp) Start(ctx context.Context) {
	if a.bus == nil {
		return
	}
	a.bus.Subscribe(eventbus.TopicPeerOnline, func(payload any) {
		ev, ok := payload.(eventbus.PeerOnline)
		if !ok {
			return
		}
		id, err := identity.ParseNodeID(ev.NodeID)
		if err != nil {
			return
		}
		go a.WakePeer(ctx, id)
	})
	a.bus.Subscribe(eventbus.TopicPeerDiscovered, func(payload any) {
		p, ok := payload.(peer.Peer)
		if !ok {
			return
		}
		if p.State != peer.StateOnline && p.State != peer.StateDiscovered {
			return
		}
		go a.WakePeer(ctx, p.NodeID)
	})
}

// SweepWaiting 扫描等待任务，启动「到点」的那些。
//
// 由 bootstrap 的 1 秒节拍驱动（与 outbox 同一套）：peer.online 是主触发源，
// 这里兜住其余情况 —— 对端早就在线、退避到期、以及事件丢了。
func (a *TransferApp) SweepWaiting(ctx context.Context) {
	jobs, err := a.store.ListWaiting()
	if err != nil {
		return
	}
	a.startDue(ctx, jobs)
}

// WakePeer 对端刚上线：无视退避，立刻试它那些等待任务。
func (a *TransferApp) WakePeer(ctx context.Context, peerID identity.NodeID) {
	jobs, err := a.store.ListWaiting()
	if err != nil {
		return
	}
	due := make([]transfer.Job, 0, len(jobs))
	for _, j := range jobs {
		if j.PeerID == peerID {
			due = append(due, j)
		}
	}
	a.startJobs(ctx, due)
}

// startDue 启动到点的任务（退避未到就等下一轮）。
func (a *TransferApp) startDue(ctx context.Context, jobs []transfer.Job) {
	now := a.clk.Now()
	due := make([]transfer.Job, 0, len(jobs))
	for _, j := range jobs {
		if !j.NextTryAt.IsZero() && j.NextTryAt.After(now) {
			continue
		}
		due = append(due, j)
	}
	a.startJobs(ctx, due)
}

// startJobs 尝试启动给定任务：同一对端同一时刻只启动一个。
//
// 这一条是为了防哈希风暴：对方一上线，10 个等待任务同时开始整文件哈希，
// 磁盘会先崩。其余任务留到下一轮（或下一个事件）——它们不会丢。
func (a *TransferApp) startJobs(ctx context.Context, jobs []transfer.Job) {
	busy := a.busyPeers()
	for _, j := range jobs {
		if busy[j.PeerID] {
			continue
		}
		if a.claimStarting(j.JobID) {
			busy[j.PeerID] = true
			go func(job transfer.Job) {
				defer a.releaseStarting(job.JobID)
				_, _ = a.sendFile(ctx, job.PeerID, job.LocalPath, job.JobID, false)
			}(j)
		}
	}
}

// busyPeers 返回「已经有任务在传」的对端集合。
func (a *TransferApp) busyPeers() map[identity.NodeID]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[identity.NodeID]bool, len(a.senders))
	for _, sj := range a.senders {
		out[sj.job.PeerID] = true
	}
	return out
}
```

**Step 4: 跑测试**

Run: `go test ./internal/app/ -run "TestSweepWaiting" -v`
Expected: PASS

**Step 5: 提交**

```
git add internal/app/transfer_waiting.go internal/app/transfer_waiting_test.go
git commit -m "feat(transfer): 等待任务由上线上报与 1 秒节拍驱动重试"
```

---

## Task 7: 取消与立即重试（app 层）

**Files:**
- Modify: `internal/app/transfer_waiting.go`
- Modify: `internal/app/transfer_app.go`（`BaseBackoff` / `MaxBackoff` 字段）
- Test: `internal/app/transfer_waiting_test.go`

**Step 1: 写失败的测试**

```go
// 取消：任务变 cancelled、不再等待（sweep 拾取不到它）。
func TestCancelJobStopsWaiting(t *testing.T) {
	peerID := mustNodeID(t)
	repo := &stubTransferRepo{}
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	a := NewTransferApp(identity.NodeID{}, repo, nil, nil, nil, eventbus.New(), fc, nil)

	j := a.newWaitingJob("job-cancel", peerID, "/tmp/x.bin", 4096)
	if err := repo.UpsertJob(j); err != nil {
		t.Fatal(err)
	}

	if err := a.CancelJob("job-cancel"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, _ := repo.GetJob("job-cancel")
	if got.Status != transfer.StateCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if got.WaitReason != transfer.WaitNone || !got.NextTryAt.IsZero() {
		t.Errorf("取消后必须清掉等待标记：%q / %v", got.WaitReason, got.NextTryAt)
	}
	if waiting, _ := repo.ListWaiting(); len(waiting) != 0 {
		t.Errorf("取消后不该再被 sweep 拾取：%v", waiting)
	}
}

// 不能取消已经在传的任务：当前没有中断点，假取消比拒绝更糟。
func TestCancelJobRejectsRunningTransfer(t *testing.T) {
	repo := &stubTransferRepo{}
	a := NewTransferApp(identity.NodeID{}, repo, nil, nil, nil, nil, clock.New(), nil)
	j := transfer.Job{JobID: "job-running", PeerID: mustNodeID(t), Status: transfer.StateTransferring}
	if err := repo.UpsertJob(j); err != nil {
		t.Fatal(err)
	}
	if err := a.CancelJob("job-running"); err == nil {
		t.Fatal("已在传输的任务必须拒绝取消")
	}
}

// 立即重试：把退避推到当下并真的启动一次（force 绕过"目录说离线"）。
func TestRetryNowStartsAttempt(t *testing.T) {
	peerID := mustNodeID(t)
	repo := &stubTransferRepo{}
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	conns := &fakeConns{session: &fakeSession{peer: peerID}, alive: true}
	a := NewTransferApp(identity.NodeID{}, repo, conns, nil, nil, eventbus.New(), fc, nil)
	a.ChunkSize = 1024

	p := filepath.Join(t.TempDir(), "r.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte{2}, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	j := a.newWaitingJob("job-retry", peerID, p, 2048)
	j.NextTryAt = fc.Now().Add(time.Hour) // 退避还很久
	if err := repo.UpsertJob(j); err != nil {
		t.Fatal(err)
	}

	if err := a.RetryNow(context.Background(), "job-retry"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	waitFor(t, time.Second, "attempt to start", func() bool { return !a.isStarting("job-retry") || a.isStarting("job-retry") })
	// 真正的断言：下一次 sweep 不该再把它当成"未到点"的任务继续压着
	got, _ := repo.GetJob("job-retry")
	if got.NextTryAt.After(fc.Now()) {
		t.Errorf("next_try_at 没被推到当下：%v", got.NextTryAt)
	}
}
```

> `waitFor` 若本包没有，自己写一个 20ms 轮询的辅助（e2e 包里那个不通用）。上面那句断言写法很绕，**实现时直接断言"抢占位短暂出现过"**：更简单的做法是断言 `RetryNow` 返回后 `got.NextTryAt` 不再在未来，然后 `time.Sleep(50ms)` 让 goroutine 把状态推进到 `StateMetaExchange` 或重新回到 `StateIdle`。

**Step 2: 跑测试确认失败**

Run: `go test ./internal/app/ -run "TestCancelJob|TestRetryNow" -v`
Expected: FAIL —— `a.CancelJob undefined`。

**Step 3: 加退避参数**

`internal/app/transfer_app.go` 结构体里，`ProgressInterval` 之后加：

```go
	// BaseBackoff / MaxBackoff 是等待重试的退避参数（与消息 outbox 同一套）。
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
```

`NewTransferApp` 里加 `BaseBackoff: time.Second, MaxBackoff: 5 * time.Minute,`。

**Step 4: 实现**

追加到 `internal/app/transfer_waiting.go`：

```go
// CancelJob 取消一个【还没开始】的任务（等待中）。返回错误表示不可取消。
//
// 只允许取消等待中的任务：真正在传的任务取消需要给每条任务一条取消信号，
// 而当前 sender 是「一个任务一个 goroutine + 一条共享 ctx」，没有中断点。
// 与其给一个点了不动的假取消（或者传了一半留下半截文件），不如明确拒绝。
func (a *TransferApp) CancelJob(jobID string) error {
	j, err := a.store.GetJob(jobID)
	if err != nil {
		return fmt.Errorf("transfer: 任务不存在: %w", err)
	}
	if j.Direction != transfer.DirectionSend {
		return fmt.Errorf("transfer: 只能取消本机发出的任务")
	}
	if j.Status != transfer.StateIdle {
		return fmt.Errorf("transfer: 传输已开始，无法取消")
	}
	if a.isStarting(jobID) {
		// 已被交给 goroutine、但还没把状态写成 active：此刻放行会把
		// cancelled 覆盖回"传输中"（那个 goroutine 随后会 UpsertJob）。
		return fmt.Errorf("transfer: 传输已开始，无法取消")
	}

	from := string(j.Status)
	j.Status = transfer.StateCancelled
	j.WaitReason = transfer.WaitNone
	j.NextTryAt = time.Time{}
	j.UpdatedAt = a.clk.Now()
	if err := a.store.UpsertJob(j); err != nil {
		return err
	}
	a.publishState(j, from, string(transfer.StateCancelled), "")
	return nil
}

// RetryNow 立刻重试一条等待中的任务：不等退避，也不要求目录里显示在线
// （目录可能滞后）。
func (a *TransferApp) RetryNow(ctx context.Context, jobID string) error {
	j, err := a.store.GetJob(jobID)
	if err != nil {
		return fmt.Errorf("transfer: 任务不存在: %w", err)
	}
	if j.Status != transfer.StateIdle || j.WaitReason != transfer.WaitPeerOffline {
		return fmt.Errorf("transfer: 该任务不在等待中")
	}
	j.NextTryAt = a.clk.Now()
	j.UpdatedAt = j.NextTryAt
	if err := a.store.UpsertJob(j); err != nil {
		return err
	}
	if !a.claimStarting(jobID) {
		return nil // 已经在传了，什么都不用做
	}
	go func() {
		defer a.releaseStarting(jobID)
		_, _ = a.sendFile(ctx, j.PeerID, j.LocalPath, j.JobID, true) // force
	}()
	return nil
}
```

**Step 5: 跑测试**

Run: `go test ./internal/app/ -v -run "TestCancelJob|TestRetryNow"`
Expected: PASS

**Step 6: 提交**

```
git add internal/app/transfer_app.go internal/app/transfer_waiting.go internal/app/transfer_waiting_test.go
git commit -m "feat(transfer): 支持取消等待任务与立即重试"
```

---

## Task 8: 启动恢复

**Files:**
- Modify: `internal/app/transfer_waiting.go`
- Test: `internal/app/transfer_waiting_test.go`

**Step 1: 写失败的测试**

```go
// 启动收尾：崩溃残留的 active 任务按方向处理 ——
// 发送侧转入等待（会自动续传），接收侧标失败（对方的下一轮
// FILE_META 仍会按 FindResumable 继承它的临时文件与位图）。
func TestRecoverHandlesLeftoverActiveJobs(t *testing.T) {
	peerID := mustNodeID(t)
	repo := &stubTransferRepo{}
	a := NewTransferApp(identity.NodeID{}, repo, nil, nil, nil, eventbus.New(), clock.New(), nil)

	send := transfer.Job{
		JobID: "job-send-zombie", PeerID: peerID, Direction: transfer.DirectionSend,
		Status: transfer.StateTransferring, LocalPath: "/tmp/gone.bin",
		FileName: "gone.bin", ChunkSize: 1024, TotalChunks: 4, FileSize: 4096,
	}
	recv := transfer.Job{
		JobID: "job-recv-zombie", PeerID: peerID, Direction: transfer.DirectionRecv,
		Status: transfer.StateTransferring, LocalPath: "/tmp/in.bin",
		FileName: "in.bin", ChunkSize: 1024, TotalChunks: 4, FileSize: 4096,
	}
	for _, j := range []transfer.Job{send, recv} {
		if err := repo.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	a.Recover(context.Background())

	gotSend, _ := repo.GetJob("job-send-zombie")
	if gotSend.Status != transfer.StateIdle || gotSend.WaitReason != transfer.WaitPeerOffline {
		t.Errorf("发送侧残留 = %q/%q, want idle/peer_offline", gotSend.Status, gotSend.WaitReason)
	}
	gotRecv, _ := repo.GetJob("job-recv-zombie")
	if gotRecv.Status != transfer.StateFailed {
		t.Errorf("接收侧残留 = %q, want failed", gotRecv.Status)
	}
	if gotRecv.Error == "" {
		t.Error("接收侧残留要写清楚原因，否则界面只有一条"失败"")
	}
}

// 重启后等待中的任务要继续等（而不是因为没有 peer.online 事件就永远躺着）。
func TestRecoverStartsWaitingJobs(t *testing.T) {
	peerID := mustNodeID(t)
	repo := &stubTransferRepo{}
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	sess := &fakeSession{peer: peerID}
	a := NewTransferApp(identity.NodeID{}, repo, &fakeConns{session: sess, alive: true}, nil, nil, eventbus.New(), fc, nil)
	a.ChunkSize = 1024

	p := filepath.Join(t.TempDir(), "w.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte{3}, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	j := a.newWaitingJob("job-wait-recover", peerID, p, 2048)
	j.NextTryAt = fc.Now().Add(time.Hour) // 退避还很久，重启也该立刻试一次
	if err := repo.UpsertJob(j); err != nil {
		t.Fatal(err)
	}

	a.Recover(context.Background())
	waitFor(t, time.Second, "recovered attempt to send something", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.sends > 0
	})
}
```

**Step 2: 跑测试确认失败**

Run: `go test ./internal/app/ -run TestRecover -v`
Expected: FAIL —— `a.Recover undefined`。

**Step 3: 实现**

追加到 `internal/app/transfer_waiting.go`：

```go
// Recover 收尾上一次运行留下的任务，并立刻试一轮等待中的任务。
//
// 两件事都只在启动时做一次：
//  1. 崩溃残留的 active 任务 —— 它们既不会自己继续、也不会失败，
//     界面上表现为"永远传输中"的僵尸行；
//  2. 等待中的任务 —— 上次退出时对端可能已经在线，不该干等到下一个事件。
func (a *TransferApp) Recover(ctx context.Context) {
	if active, err := a.store.ListActive(); err == nil {
		for _, j := range active {
			switch j.Status {
			case transfer.StateMetaExchange, transfer.StateTransferring, transfer.StateVerifying:
			default:
				continue
			}
			if j.Direction == transfer.DirectionSend {
				// 发送侧：转入等待，随后立刻重试（续传靠对端回传的位图）。
				a.markWaiting(j, j.JobID, j.PeerID, j.LocalPath, errAppInterrupted)
				continue
			}
			// 接收侧：标失败即可 —— FindResumable 只排除 done/cancelled，
			// failed 行照样会被对方的下一轮 FILE_META 继承并续传。
			j.Status = transfer.StateFailed
			j.Error = errAppInterrupted.Error()
			j.WaitReason = transfer.WaitNone
			j.NextTryAt = time.Time{}
			j.UpdatedAt = a.clk.Now()
			_ = a.store.UpsertJob(j)
			if a.bus != nil {
				a.bus.Publish(eventbus.TopicTransferError, eventbus.TransferError{
					JobID: j.JobID, PeerID: j.PeerID.String(), Err: errAppInterrupted,
				})
			}
		}
	}

	// 等待中的任务：无视退避立刻试一轮（可能在退出期间对端就已经上线了）。
	if jobs, err := a.store.ListWaiting(); err == nil {
		a.startJobs(ctx, jobs)
	}
}

// errAppInterrupted 标记「上次运行没走完」——它不是对端的问题，
// 也不该被当成可以继续等的原因（等待原因由 markWaiting 统一写）。
var errAppInterrupted = errors.New("传输被中断（应用已退出）")
```

> `errors` 需要在 `transfer_waiting.go` 的 import 里加上。

**Step 4: 跑测试**

Run: `go test ./internal/app/ -v -run TestRecover`
Expected: PASS

**Step 5: 提交**

```
git add internal/app/transfer_waiting.go internal/app/transfer_waiting_test.go
git commit -m "feat(transfer): 启动时恢复等待任务并收尾僵尸任务"
```

---

## Task 9: 组合根与测试夹具接线

**Files:**
- Modify: `internal/bootstrap/bootstrap.go:266-289`
- Modify: `test/harness/harness.go:227-235`

**Step 1: 接线**

`internal/bootstrap/bootstrap.go`，紧接 `n.DiscApp.Start(ctx)`（`:268`）之后加：

```go
	// 文件传输的等待重试：对端上线是主触发源，1 秒节拍兜底（与 outbox 同构）。
	n.TransferApp.Start(ctx)
	n.TransferApp.Recover(ctx)
```

1 秒节拍的循环里（`:283-285`）加一行：

```go
				n.TransferApp.SweepWaiting(ctx)
```

`test/harness/harness.go` 的 `Tick()`（`:230`）里加一行，让 e2e 能确定性地推进等待重试：

```go
	n.TransferApp.SweepWaiting(n.ctx)
```

**Step 2: 跑测试**

Run: `go test -tags wails ./internal/... ./test/... -count=1 2>&1 | findstr /v "no test files"`
Expected: 除 `keystore` / `fsutil` 的既有失败外全 PASS。

**Step 3: 提交**

```
git add internal/bootstrap/bootstrap.go test/harness/harness.go
git commit -m "feat(bootstrap): 接入传输等待任务的启动恢复与周期重试"
```

---

## Task 10: Wails 服务、DTO 与事件桥

**Files:**
- Modify: `internal/adapters/wails/services.go`（`TransferService`）
- Modify: `internal/adapters/wails/dto.go:100-132`
- Modify: `internal/adapters/wails/eventbridge.go:24-49`、`Start()`
- Test: `internal/adapters/wails/services_test.go`
- Regenerate: `frontend/bindings/`

**Step 1: 写失败的测试**

`internal/adapters/wails/services_test.go`：

```go
// 取消与立即重试的入参校验（不做业务判断，业务在 app 层）。
func TestTransferServiceCancelAndRetryValidateInput(t *testing.T) {
	svc := &TransferService{}
	if err := svc.CancelTransfer(""); err == nil {
		t.Error("空 job_id 必须被拒绝")
	}
	if err := svc.RetryTransfer(""); err == nil {
		t.Error("空 job_id 必须被拒绝")
	}
}

// 等待原因要透给前端：界面靠它区分「等待对方上线」与「排队中」。
func TestTransferDTOCarriesWaitReason(t *testing.T) {
	j := transfer.Job{
		JobID: "j1", Status: transfer.StateIdle,
		WaitReason: transfer.WaitPeerOffline, ChunkSize: 1024, FileSize: 2048, TotalChunks: 2,
	}
	if got := ToTransferDTO(j).WaitReason; got != string(transfer.WaitPeerOffline) {
		t.Errorf("waitReason = %q", got)
	}
}
```

**Step 2: 跑测试确认失败**

Run: `go test ./internal/adapters/wails/ -run "TestTransferServiceCancelAndRetry|TestTransferDTOCarriesWaitReason" -v`
Expected: FAIL —— 编译不过。

**Step 3: 实现**

`internal/adapters/wails/services.go`，紧跟 `TransferService.SendFile` 之后：

```go
// CancelTransfer 取消一条尚未开始的传输任务（等待中的那种）。
//
// 已在传输的任务会被 app 层拒绝：当前 sender 没有中断点，假取消比拒绝更糟。
func (s *TransferService) CancelTransfer(jobID string) error {
	if jobID == "" {
		return fmt.Errorf("请选择任务")
	}
	return s.d.Transfer.CancelJob(jobID)
}

// RetryTransfer 立刻重试一条等待中的传输任务（不等退避，也不要求对端在目录里在线）。
func (s *TransferService) RetryTransfer(jobID string) error {
	if jobID == "" {
		return fmt.Errorf("请选择任务")
	}
	return s.d.Transfer.RetryNow(context.Background(), jobID)
}
```

`internal/adapters/wails/dto.go`：`TransferDTO` 加字段，`ToTransferDTO` 加赋值。

```go
	Status      string  `json:"status"`
	// WaitReason 非空表示这条任务在等（目前只有 "peer_offline"）。
	// 界面靠它区分「等待对方上线」与「排队中」——两者在 status 上都是 idle/queued。
	WaitReason  string  `json:"waitReason,omitempty"`
```

```go
		Status:      string(j.Status),
		WaitReason:  string(j.WaitReason),
```

`internal/adapters/wails/eventbridge.go`：事件常量加 `EventTransferState = "transfer:state"`，`Start()` 里加订阅：

```go
	b.on(eventbus.TopicTransferState, func(p any) {
		ev, ok := p.(eventbus.TransferState)
		if !ok {
			return
		}
		b.debug(eventbus.TopicTransferState, p)
		b.emitter.Emit(EventTransferState, map[string]any{
			"jobId":  ev.JobID,
			"status": ev.To,
			"reason": ev.Reason,
		})
	})
```

**Step 4: 跑测试**

Run: `go test ./internal/adapters/wails/ -count=1`
Expected: PASS

**Step 5: 重新生成绑定**

Run:
```
wails3 generate bindings -f "-tags wails" -clean=true -ts -i .
```
Expected: `Processed: … Services, … Methods`（方法数比之前多 2）

Run: `git status --short -- frontend/bindings`
Expected: 只有 `transferservice.ts` 被修改（新增 `CancelTransfer` / `RetryTransfer`），没有别的文件被删或新增。**若出现 `.js` 文件或大量删除，说明命令参数写错了（少了 `-ts -i`），执行 `git checkout -- frontend/bindings` 后重来。**

**Step 6: 提交**

```
git add internal/adapters/wails/services.go internal/adapters/wails/dto.go internal/adapters/wails/eventbridge.go internal/adapters/wails/services_test.go frontend/bindings/github.com/swarmlink/swarmlink/internal/adapters/wails/transferservice.ts
git commit -m "feat(transfer): 暴露取消/重试接口并转发 transfer:state 事件"
```

---

## Task 11: 前端

**Files:**
- Modify: `frontend/src/api/types.ts`
- Modify: `frontend/src/api/bindings.ts`
- Modify: `frontend/src/api/mock.ts`
- Modify: `frontend/src/stores/transfer.ts`
- Modify: `frontend/src/events/index.ts`
- Modify: `frontend/src/utils/format.ts`
- Modify: `frontend/src/components/TransferList.vue`
- Modify: `frontend/src/components/ChatBubble.vue`
- Modify: `frontend/src/components/Icon.vue`

**Step 1: 类型与事件名**

`frontend/src/api/types.ts`：

- `TransferJob` 加字段
```ts
  /**
   * 非空表示这条任务在等（目前只有 'peer_offline'）。
   *
   * 判据必须看它而不是 status：等待中的任务 status 是 idle（DB 里是 queued），
   * 与「还没轮到」共用同一个状态。
   */
  waitReason?: string
```
- `SwarmApi` 的「传输」段加两个方法
```ts
  /** 取消一条尚未开始的传输（等待中的那种）。已在传的任务会被后端拒绝。 */
  cancelTransfer(jobId: string): Promise<void>
  /** 立刻重试一条等待中的传输（不等退避，也不要求对端在目录里在线）。 */
  retryTransfer(jobId: string): Promise<void>
```
- `EV` 加 `transferState: 'transfer:state'`

**Step 2: 绑定映射**

`frontend/src/api/bindings.ts` 的 `realApi` 里，紧跟 `sendFile` 之后：

```ts
    cancelTransfer: async (jobId) => TransferService.CancelTransfer(jobId),
    retryTransfer: async (jobId) => TransferService.RetryTransfer(jobId),
```

**Step 3: mock 与既有类型错误**

`frontend/src/api/mock.ts`：补上 `cancelTransfer` / `retryTransfer`（本地把对应任务置为 `cancelled` / 清掉 `waitReason` 即可），**并顺手补齐当前 typecheck 报的两处既有缺失**（`getSettings()` 返回值缺 `autoStart`、`SwarmApi` 缺 `autoStartEnable/Disable/IsEnabled`）——否则无法判断我们是否引入了新错误。

**Step 4: store**

`frontend/src/stores/transfer.ts`：

```ts
/** 该任务是否在等对方上线（判据在 waitReason 上，不在 status 上）。 */
export function isWaiting(j: TransferJob): boolean {
  return j.waitReason === 'peer_offline'
}
```

store 内新增 action 并导出：

```ts
  /**
   * 后端状态变化（transfer:state 事件）。
   *
   * 不用 progress 事件表达这件事：progress 的语义是「进度在动」，
   * 而"进入等待"恰恰是停下来了。
   */
  function applyState(s: { jobId: string; status: string; reason?: string }): void {
    const i = jobs.value.findIndex((j) => j.jobId === s.jobId)
    if (i < 0) {
      scheduleHydrate()
      return
    }
    jobs.value[i] = {
      ...jobs.value[i],
      status: s.status,
      waitReason: s.reason || undefined,
      speed: undefined,
      etaMs: undefined,
      updatedAt: Date.now()
    }
  }

  async function cancelJob(jobId: string): Promise<void> {
    await (await getApi()).cancelTransfer(jobId)
    applyState({ jobId, status: 'cancelled' })
  }

  async function retryJob(jobId: string): Promise<void> {
    // 不做乐观更新：真正的状态由后端的 transfer:state / 进度事件推来，
    // 乐观改成"传输中"会在对端仍然离线时骗用户。
    await (await getApi()).retryTransfer(jobId)
  }
```

同时把 `refresh()` 的合并逻辑保留 `waitReason`（`List()` 已带该字段，无需特殊处理）。

**Step 5: 事件订阅**

`frontend/src/events/index.ts` 的 `registerEvents()` 里，`on(EV.transferError, ...)` 之后加：

```ts
  on(EV.transferState, (data: { jobId: string; status: string; reason?: string }) => {
    transfers.applyState(data)
  })
```

**Step 6: 文案**

`frontend/src/utils/format.ts`：

```ts
export function transferStatus(status: string, waitReason?: string): Tone {
  switch (status) {
    case 'done':
      return { text: '已完成', cls: 'ok', final: true }
    case 'failed':
      return { text: '失败', cls: 'err', final: true }
    case 'cancelled':
      return { text: '已取消', cls: '', final: true }
    case 'verifying':
      return { text: '校验中', cls: 'warn', final: false }
    case 'paused':
      return { text: '已暂停', cls: 'warn', final: false }
    // 等待对方上线与「还没轮到」共用 idle/queued 状态，靠 waitReason 区分。
    // 等待是【非终态】：它会自己继续，不能标成结束。
    case 'idle':
    case 'queued':
      if (waitReason === 'peer_offline') {
        return { text: '等待对方上线', cls: 'warn', final: false }
      }
      return status === 'queued'
        ? { text: '排队中', cls: '', final: false }
        : { text: '协商中', cls: 'brand', final: false }
    // 任务已建但还没开始发块（等对端位图）
    case 'meta_exchange':
      return { text: '协商中', cls: 'brand', final: false }
    default:
      return { text: '传输中', cls: 'brand', final: false }
  }
}
```

`frontend/src/components/ChatBubble.vue` 的本地 `transferStatus` computed 里，在 `if (job.status === 'paused')` 之后加一行：

```ts
  // 等待对方上线（与传输列表同一套判据：看 waitReason 而不是 status）
  if (job.waitReason === 'peer_offline') return '等待对方上线'
```

**Step 7: 传输列表的按钮**

`frontend/src/components/Icon.vue` 的图标表里加 `close`（若已有同名图标则跳过）：

```ts
  close: ['M18 6 6 18', 'M6 6l12 12'],
```

`frontend/src/components/TransferList.vue`：

- 引入 `isWaiting` 与动作
```ts
import { isActive, isWaiting } from "../stores/transfer";

async function retry(j: TransferJob): Promise<void> {
  try {
    await (await getApi()).retryTransfer(j.jobId);
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || "重试失败");
  }
}

async function cancel(j: TransferJob): Promise<void> {
  try {
    await (await getApi()).cancelTransfer(j.jobId);
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || "取消失败");
  }
}
```
- 模板里把两处 `transferStatus(j.status)` 改成 `transferStatus(j.status, j.waitReason)`
- 在「定位」按钮那一组里加两个按钮（等待中才出现）：
```html
            <button
              v-if="isWaiting(j)"
              class="reveal"
              title="立即重试"
              aria-label="立即重试"
              @click="retry(j)"
            >
              <Icon name="refresh" :size="13" />
            </button>
            <button
              v-if="isWaiting(j)"
              class="reveal"
              title="取消传输"
              aria-label="取消传输"
              @click="cancel(j)"
            >
              <Icon name="close" :size="13" />
            </button>
```
- **注意占位符**：原模板末尾是 `<button v-if="done">` + `<span v-else class="reveal-ph" />`。加了两个条件按钮后，占位逻辑要跟着改，保证各行末尾仍然对齐（例如把占位判据改成 `v-else-if="!isWaiting(j)"`，并按需要补一个更宽的占位）。**这一条要在浏览器里实际看一眼**（`npm run dev`，等待态与已完成态混排时行尾是否对齐）。

**Step 8: 验证**

Run: `cd frontend && npm run typecheck`
Expected: 无输出（0 错误）。**若有错，只允许是我们刚改的文件**。

Run: `cd frontend && npm run test`（若 `tests/` 下有相关用例）
Expected: PASS

**Step 9: 提交**

```
git add frontend/src/api/types.ts frontend/src/api/bindings.ts frontend/src/api/mock.ts frontend/src/stores/transfer.ts frontend/src/events/index.ts frontend/src/utils/format.ts frontend/src/components/TransferList.vue frontend/src/components/ChatBubble.vue frontend/src/components/Icon.vue
git commit -m "feat(ui): 传输列表与聊天卡片显示等待对方上线，支持取消与立即重试"
```

---

## Task 12: 端到端验收

**Files:**
- Test: `test/e2e/e2e_test.go`

**Step 1: 写测试**

追加到 `test/e2e/e2e_test.go`：

```go
// 对方离线时发文件 → 任务进入等待 → 对方上线后自动传完（无需任何用户操作）。
//
// 这是本次改动的核心验收：全过程不需要再点一次「发送」。
func TestFileTransferWaitsForPeerOnline(t *testing.T) {
	a := newNode(t, "alice", harness.Options{ChunkSize: 64 * 1024})
	b := newNode(t, "bob", harness.Options{ChunkSize: 64 * 1024})

	src, data := randomFile(t, 1<<20+1234)

	// 此刻 a 还不知道 b（没有 Link）：发起发送应当落成"等待对方上线"
	job, err := a.TransferApp.SendFileAs(context.Background(), b.ID(), src, "job-offline-1")
	if err != nil {
		t.Fatalf("离线发起不该报错: %v", err)
	}
	if job.Status != transfer.StateIdle || job.WaitReason != transfer.WaitPeerOffline {
		t.Fatalf("job = %s/%s, want idle/peer_offline", job.Status, job.WaitReason)
	}

	// 对方上线（等价于收到 UDP ANNOUNCE）→ 自动开跑，不需要再调 SendFile
	if err := harness.Link(a, b); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "transfer to complete automatically", func() bool {
		a.Tick() // 驱动 sweep（退避到点由真实时钟推进）
		j, err := a.Messages.GetJob("job-offline-1")
		return err == nil && j.Status == transfer.StateDone
	})

	got, err := os.ReadFile(filepath.Join(b.DownloadDir, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("received content mismatch: want %d bytes got %d", len(data), len(got))
	}
}

// 传到一半对方掉线 → 进入等待 → 对方恢复后按位图续传完成。
func TestFileTransferWaitsWhenPeerDrops(t *testing.T) {
	a := newNode(t, "alice", harness.Options{ChunkSize: 64 * 1024})
	b := newNode(t, "bob", harness.Options{ChunkSize: 64 * 1024})
	if err := harness.Link(a, b); err != nil {
		t.Fatal(err)
	}
	// 让接收侧频繁回 ACK 并落盘位图，中断后一定有可续传的位图
	b.TransferApp.AckEveryChunks = 4
	b.TransferApp.AckInterval = 5 * time.Millisecond
	b.TransferApp.FlushEveryChunks = 4

	src, data := randomFile(t, 4<<20)

	go func() { _, _ = a.TransferApp.SendFileAs(context.Background(), b.ID(), src, "job-drop-1") }()

	// 等确实传了一部分，再掐断对端会话
	waitFor(t, 5*time.Second, "first chunks to arrive", func() bool {
		if jobs, err := b.Messages.ListActive(); err == nil {
			for _, j := range jobs {
				if j.Direction == transfer.DirectionRecv && j.Completed > 0 {
					return true
				}
			}
		}
		return false
	})
	if err := b.Conn.Close(a.ID()); err != nil {
		t.Fatalf("close session: %v", err)
	}

	// 掉线必须变成"等待"，而不是失败
	waitFor(t, 10*time.Second, "job to wait for peer", func() bool {
		a.Tick()
		j, err := a.Messages.GetJob("job-drop-1")
		return err == nil && j.Status == transfer.StateIdle && j.WaitReason == transfer.WaitPeerOffline
	})

	// 对端恢复（重新握手）→ 自动续传完成
	if err := harness.Link(a, b); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "resumed transfer to complete", func() bool {
		a.Tick()
		j, err := a.Messages.GetJob("job-drop-1")
		return err == nil && j.Status == transfer.StateDone
	})

	got, err := os.ReadFile(filepath.Join(b.DownloadDir, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("content mismatch after resume")
	}
}
```

**Step 2: 跑测试**

Run: `go test ./test/e2e/ -run "TestFileTransferWaits" -v -count=1`
Expected: PASS

**Step 3: 若 `TestFileTransferWaitsWhenPeerDrops` 不稳定**

掉线用例的时序依赖真实 TCP 缓冲，若在不同机器上偶发失败：
- 先试提高文件大小 / 缩小 `ChunkSize` 让它更早进入"发不出去"的状态；
- 仍不稳定则给该用例加 `t.Skip` 并把它的意图降级到 Task 5 的单测（那条是确定性的），**在提交信息里写明原因** —— 不允许留一个偶发红的用例在 CI 里。

**Step 4: 提交**

```
git add test/e2e/e2e_test.go
git commit -m "test(e2e): 离线发起与中途掉线都能等对方上线后自动续传"
```

---

## Task 13: 全量验证与文档一致性

**Step 1: Go 全量**

Run: `go vet -tags wails ./... && go build -tags wails -o NUL .`
Expected: 无输出（`VET_OK`/`BUILD_OK` 由你自己的 echo 确认）

Run: `go test -tags wails ./... -count=1 2>&1 | findstr /v "no test files"`
Expected: 只有 `keystore` / `fsutil` 的既有失败

**Step 2: 前端**

Run: `cd frontend && npm run typecheck && npm run build`
Expected: 0 错误，产物落到 `frontend/dist`

**Step 3: 文档对齐**

- 设计文档 `docs/plans/2026-10-08-offline-file-transfer-v2-design.md` 的「界面」一节把"聊天卡片与传输列表共用 `transferStatus`"改成准确说法：**两者各自有一份 switch（`utils/format.ts` 的 `transferStatus` 与 `ChatBubble.vue` 的本地 computed），本次两处都加分支**；并补一句"取消/重试只对等待中的任务开按钮"。`docs/plans/2026-09-23-tray-notification.md` 之类的旧文档不动。
- 在 `docs/plans/2026-10-08-offline-file-transfer-v2.md`（本文件）末尾加一节「实施差异记录」，记录实施过程中与计划的偏离（这个仓库的习惯，见 09-24 那份计划）。

**Step 4: 手工验收（无法自动化的部分，逐条过）**

1. 断开对方（或让对方退出），点发送一个 1GB 级文件：任务立刻显示「等待对方上线」，**没有**长时间磁盘读（任务管理器里看磁盘活动），也没有红色失败。
2. 让对方上线：任务自动开始（**注意**：不做任何提示，这是刻意的），进度条动起来。
3. 传一半时让对方退出：任务回到「等待对方上线」；对方回来后继续，且**不从头传**（已传字节数不清零）。
4. 等待中重启本机：任务仍在列表且显示「等待对方上线」；对方上线后自动继续。
5. 等待中点「取消」：任务变「已取消」，对方上线后**不会**再自动传输。
6. 等待中点「立即重试」：立刻发起一次尝试（对端仍离线时会很快回到等待，且不再有整文件哈希的停顿）。
7. 传一个已被删除的文件：立刻失败（**不能**进入等待）。
8. 崩溃残留：传输中直接杀进程，重启后那条任务不再显示「传输中」（发送侧变等待、接收侧变失败）。

**Step 5: 提交**

```
git add docs/plans/2026-10-08-offline-file-transfer-v2-design.md docs/plans/2026-10-08-offline-file-transfer-v2.md
git commit -m "docs(plans): 补齐离线文件传输的实施差异与界面说明"
```

---

## 附：本计划刻意没做的事

| 没做 | 原因 |
| --- | --- |
| 并发排队位置（架构书 972-977 的队列与序号） | 用户明确选了不做；`wait_reason` 为它留了位置 |
| 持久化 chunk_hashes 缓存 | 需要配 mtime 校验才安全，收益不明；先接受"一次等待 ≈ 一次哈希" |
| 取消/失败时给对方发 `file_failed` 消息 | 现状就是由传输面板承担反馈（09-24 回滚时的结论） |
| 取消**正在进行**的传输 | 当前 sender 没有中断点（一条共享 ctx），假取消比拒绝更糟 |
| 群聊文件传输 | 前端已有明确拦截，与本次无关 |

---

## 实施差异记录（2026-10-08 实施后追加）

计划在执行中被三处**真实缺陷**和一轮整体审查修正。记下来，免得下一个人以为是随意发挥。

### 执行方式

环境里只有 `code-explorer`（只读）与 `code-reviewer` 两个子代理，**没有实现型子代理**，所以技能里"每个任务派一个全新实现子代理"这一步无法照搬：实现由主代理逐任务 TDD 完成，审查交给 `code-reviewer`（Task 1 一轮、Task 2–5 一轮、全量完成后一轮同步整体审查）。

### 计划外的必要改动

1. **`networkAttempted` 显式分界**（`internal/app/transfer_app.go`）：计划让分类"靠错误文案兜底本地失败"，实测在 Windows 上直接翻车 —— `os.Stat` 的文案是 `The system cannot find the file specified`，不含 POSIX 的 `no such file`，于是"文件不存在"被误判成等待（用户会永远等一个不可能成功的任务）。改为记录"失败是否已进入网络阶段"：本地阶段（stat/打开/哈希）一律真失败，只有网络阶段才可能等待。
2. **Windows 本地 IO 文案**（`transfer_waiting.go` 的 `permanentPatterns`）：补 `cannot find the file specified` / `cannot find the path specified` / `access is denied`。它们仍然有用 —— 网络阶段发块时 `f.ReadAt` 也可能读到刚被删/被截断的文件。
3. **`RetryCount` 的历史承接**：计划没写这件事，实施中发现两处都会把它归零（尝试时从零构造的任务字面量、早退路径的 `newWaitingJob`），后果是**退避永远停在第一档**（每秒重试一次，也到不了 5min 上限）。现在由 `markWaiting` 统一承接（取 max），并补了 2 条标注了"来自真实缺陷"的回归用例。
4. **退避起点**：`newWaitingJob` 原本把 `RetryCount` 置 1，叠加 `markWaiting` 的 `++` 使首档变成 2s；改为从 0 起，恢复设计里的 1s→2s→4s。
5. **取消/启动竞态守卫**（整体审查发现）：`RetryNow`/`startJobs` 与 `CancelJob` 之间有窗口 —— 取消可能被随后"写回传输中"的任务字面量覆盖，正是验收标准 4 明令禁止的"取消后仍自动传输"。`sendFile` 现在在抢占成功后再复读一次状态，非 `StateIdle` 就干净放弃（不写库、不发事件）。
6. **`session()` 的 `dir == nil` 保护**：`force=true` 的重试路径可能在没装配目录的情况下进入，原来会 nil 解引用把进程带走。
7. **计划外的测试替身修补**：`internal/adapters/wails/services_test.go` 的 `memTransferRepo` 也要实现 `ListWaiting`（计划漏了这个文件），单独一个提交。
8. **绑定额外改动**：`frontend/bindings/.../models.ts` 也变了（`TransferDTO` 多了 `waitReason`），计划里只提了 `transferservice.ts`。
9. **Task 8 的测试放在独立文件** `internal/app/transfer_recover_test.go`（避免与已提交的测试文件相互干扰）。
10. **e2e 稳定性调整**：中途掉线用例的文件从 4MB 提到 8MB（保证"掐断"一定发生在传输途中）、首个等待循环也加 `a.Tick()`（否则首轮偶发拨号失败后没人驱动重试）、两处 `waitFor` 加诊断回调（失败时直接打出 `status/waitReason/retry/err`）。连跑 10 次稳定。
11. **`applyProgress` 清理等待标记**（审查发现）：重试真正开始后后端只发进度事件，前端若不清理 `waitReason`，界面会一边显示进度一边挂着"取消 / 立即重试"按钮。
12. **`PurgeFinished` 的守卫用例**：断言"等待中的任务不会被清理"，把这条不变量从注释变成测试。

### 提交范围的重要说明

本功能区间混入了上一轮「系统通知」功能的改动（`internal/app/notify_app.go`、`internal/adapters/systemnotify/`、`frontend/src/events/index.ts`、`frontend/src/api/types.ts`、`ChatBubble.vue` 等 **7+ 个文件**）。原因：负责人选择"先不提交上一轮改动，用显式文件列表提交"，而两个功能都改到了 `eventbridge.go`、`types.ts`、`events/index.ts`、`ChatBubble.vue`。计划只预告了 3 个文件，实际更多 —— 功能上互不破坏（测试全绿），但审查边界被污染。若要干净的提交历史，需要单独整理。
