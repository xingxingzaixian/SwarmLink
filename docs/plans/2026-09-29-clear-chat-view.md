# 清空聊天视图（软清空）实施计划

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 给每个会话一个「清空点」——一键把聊天气泡藏起来（不删数据），并可一键恢复显示；清空点持久化在后端，跨重启保持。

**Architecture:** 后端只存/取清空点（SQLite 新表 `conv_clears` + `ports.ChatStore` 三个方法 + `ChatApp` 三个方法 + `ChatService` 三个方法）。**过滤发生在前端渲染层**（`activeMessages`），因为 `Conversations()` 与左栏预览复用同一个 `ChatApp.History` 查询，在后端过滤会连带清空预览（详见设计文档「后端设计 2」）。

**Tech Stack:** Go 1.x + Wails3（服务绑定自动生成）+ Vue3/Pinia；前端测试用 Node 内置 `node:test`。

**设计文档:** `docs/plans/2026-09-29-clear-chat-view-design.md`

**约定:** 本计划不含 `git commit` 步骤（用户未要求提交）。每个 Task 的收尾验证必须实跑并贴出输出。

## 已确认的行为前提（不可动摇）

1. 只清**当前打开的会话**；
2. 跨重启保持（存后端）；
3. 「查看历史」= **撤销清空**，点一次后一直全显示；
4. 未读数与左栏预览**不动**。

---

### Task 1: 存储层——清空点

**Files:**
- Modify: `internal/domain/ports/ports.go`（`ChatStore` 接口，约 :147-154）
- Modify: `internal/adapters/store/sqlite/migrations.go`（`migrations` 切片追加 version 2，约 :137）
- Create: `internal/adapters/store/sqlite/convclear.go`
- Modify: `internal/adapters/store/mem/mem.go`（结构体字段、`NewMessages`、三个新方法）
- Modify: `internal/adapters/store/sqlite/sqlite_test.go`（表清单补 `conv_clears`）
- Test: `internal/adapters/store/sqlite/sqlite_test.go`、`internal/adapters/store/mem/mem_test.go`（若无则新建）

**Step 1: 写失败测试**

在 `sqlite_test.go` 追加（用现成的 `openTest(t)` 建临时库）：

```go
// 清空点必须能覆盖：用户清空 → 又聊了几句 → 再清空一次，边界要往前推。
func TestConvClearRoundTripAndOverwrite(t *testing.T) {
	db := openTest(t)
	m := NewMessages(db)

	if _, ok, err := m.ConvClearedAt("c1"); err != nil || ok {
		t.Fatalf("未清空时应 ok=false，得到 ok=%v err=%v", ok, err)
	}

	t1 := time.UnixMilli(1_000)
	if err := m.SetConvCleared("c1", t1); err != nil {
		t.Fatalf("SetConvCleared: %v", err)
	}
	got, ok, err := m.ConvClearedAt("c1")
	if err != nil || !ok || got.UnixMilli() != 1_000 {
		t.Fatalf("读回 = %v ok=%v err=%v，期望 1000/true", got, ok, err)
	}

	// 覆盖为更晚的时刻
	if err := m.SetConvCleared("c1", time.UnixMilli(2_000)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := m.ConvClearedAt("c1"); got.UnixMilli() != 2_000 {
		t.Fatalf("覆盖失败，仍是 %d", got.UnixMilli())
	}

	// 两个会话互不影响
	if _, ok, _ := m.ConvClearedAt("c2"); ok {
		t.Fatal("清空 c1 不应影响 c2")
	}

	// 撤销（幂等：删两次都不报错）
	for i := 0; i < 2; i++ {
		if err := m.RemoveConvClear("c1"); err != nil {
			t.Fatalf("RemoveConvClear: %v", err)
		}
	}
	if _, ok, _ := m.ConvClearedAt("c1"); ok {
		t.Fatal("撤销后仍读到清空点")
	}
}
```

在 `mem_test.go` 追加**同一组断言**（把 `openTest` 换成 `NewMessages(clock.New())`），防止两个实现对等性漂移。

**Step 2: 跑测试确认失败**

```
go test ./internal/adapters/store/... -run ConvClear -count=1
```
预期：编译失败（`m.ConvClearedAt undefined`）。

**Step 3: 实现**

`ports.go` 的 `ChatStore` 追加（`time` 已在该文件导入）：

```go
	// 会话「清空点」：早于它的消息不在聊天区显示（消息本身不删除）。
	SetConvCleared(convID string, at time.Time) error
	ConvClearedAt(convID string) (time.Time, bool, error)
	RemoveConvClear(convID string) error
```

`migrations.go` 的 `migrations` 切片**追加**（已发布的 v1 永不修改，checksum 会拒绝）：

```go
	{
		version: 2,
		name:    "conv_clears",
		sql: `
CREATE TABLE IF NOT EXISTS conv_clears (
    conv_id    TEXT PRIMARY KEY,
    cleared_at INTEGER NOT NULL
);
`,
	},
```

新建 `sqlite/convclear.go`：

```go
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetConvCleared 写入/覆盖会话清空点（毫秒时间戳）。
func (m *Messages) SetConvCleared(convID string, at time.Time) error {
	return m.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
INSERT INTO conv_clears(conv_id, cleared_at) VALUES(?,?)
ON CONFLICT(conv_id) DO UPDATE SET cleared_at = excluded.cleared_at`,
			convID, at.UnixMilli())
		if err != nil {
			return fmt.Errorf("sqlite: set conv cleared: %w", err)
		}
		return nil
	})
}

// ConvClearedAt 读取清空点；从未清空时 ok=false（不是错误）。
func (m *Messages) ConvClearedAt(convID string) (time.Time, bool, error) {
	var ms int64
	err := m.db.QueryRow(`SELECT cleared_at FROM conv_clears WHERE conv_id = ?`, convID).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("sqlite: read conv cleared: %w", err)
	}
	return time.UnixMilli(ms), true, nil
}

// RemoveConvClear 撤销清空（幂等：没有记录也返回 nil）。
func (m *Messages) RemoveConvClear(convID string) error {
	return m.db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM conv_clears WHERE conv_id = ?`, convID); err != nil {
			return fmt.Errorf("sqlite: remove conv clear: %w", err)
		}
		return nil
	})
}
```

`mem.go`：结构体加 `cleared map[string]time.Time`，`NewMessages` 里初始化，并实现同样三个方法（用 `s.mu` 保护）。

`sqlite_test.go` 的表清单（`TestMigrationsAreReentrant` 附近）补上 `"conv_clears"`。

**Step 4: 跑测试确认通过**

```
go test ./internal/adapters/store/... -count=1
```
预期：全 PASS，且 `TestMigrationsAreReentrant` 仍通过（证明 v2 可重入）。

---

### Task 2: 应用层——三个方法

**Files:**
- Modify: `internal/app/chat_app.go`（`History` 方法附近，约 :124）
- Test: `internal/app/clear_conversation_test.go`（新建）

**Step 1: 写失败测试**

```go
// 清空点要能写入、读回、覆盖、撤销，并且【只影响它自己那个会话】。
func TestClearConversationRoundTrip(t *testing.T) {
	capp, _ := newTestChatApp(t)
	ctx := context.Background()

	if ms, err := capp.ClearedAt(ctx, "conv-a"); err != nil || ms != 0 {
		t.Fatalf("未清空时应返回 0，得到 %d err=%v", ms, err)
	}

	if err := capp.ClearConversation(ctx, "conv-a"); err != nil {
		t.Fatalf("ClearConversation: %v", err)
	}
	ms, err := capp.ClearedAt(ctx, "conv-a")
	if err != nil || ms <= 0 {
		t.Fatalf("清空点 = %d err=%v，期望正数毫秒", ms, err)
	}

	if other, _ := capp.ClearedAt(ctx, "conv-b"); other != 0 {
		t.Fatalf("清空 conv-a 影响了 conv-b：%d", other)
	}

	// 二次清空要往前推（覆盖）
	first := ms
	time.Sleep(2 * time.Millisecond)
	if err := capp.ClearConversation(ctx, "conv-a"); err != nil {
		t.Fatal(err)
	}
	again, _ := capp.ClearedAt(ctx, "conv-a")
	if again < first {
		t.Fatalf("二次清空把边界往回退了：%d -> %d", first, again)
	}

	if err := capp.RestoreConversation(ctx, "conv-a"); err != nil {
		t.Fatalf("RestoreConversation: %v", err)
	}
	if ms, _ := capp.ClearedAt(ctx, "conv-a"); ms != 0 {
		t.Fatalf("撤销后仍读到 %d", ms)
	}
}

// 空 convID 是调用方 bug，必须报错而不是写出一条垃圾记录。
func TestClearConversationRejectsEmptyID(t *testing.T) {
	capp, _ := newTestChatApp(t)
	if err := capp.ClearConversation(context.Background(), ""); err == nil {
		t.Fatal("空 convID 应当报错")
	}
}
```

**Step 2: 跑测试确认失败**

```
go test ./internal/app/ -run "TestClearConversation" -count=1
```
预期：编译失败（方法未定义）。

**Step 3: 实现**（加在 `History` 附近）

```go
// ClearConversation 记录会话的清空点：早于它的消息不再出现在聊天区。
//
// 【不删数据】：消息仍在库里，撤销清空（RestoreConversation）后原样回来。
// 时间取本机时钟 —— 清空点只用于本机视图，不需要与其他节点对齐。
func (a *ChatApp) ClearConversation(ctx context.Context, convID string) error {
	if convID == "" {
		return fmt.Errorf("chat: conv id 不能为空")
	}
	return a.store.SetConvCleared(convID, a.clk.Now())
}

// RestoreConversation 撤销清空点（幂等）。
func (a *ChatApp) RestoreConversation(ctx context.Context, convID string) error {
	if convID == "" {
		return fmt.Errorf("chat: conv id 不能为空")
	}
	return a.store.RemoveConvClear(convID)
}

// ClearedAt 返回会话清空点的毫秒时间戳；0 表示未清空。
//
// 读失败时返回 0：宁可多显示消息，也不要让用户的聊天记录凭空消失。
func (a *ChatApp) ClearedAt(ctx context.Context, convID string) (int64, error) {
	if convID == "" {
		return 0, nil
	}
	at, ok, err := a.store.ConvClearedAt(convID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	return at.UnixMilli(), nil
}
```

**Step 4: 跑测试确认通过**

```
go test ./internal/app/ -run "TestClearConversation" -count=1 -v
```

---

### Task 3: 服务层 + 绑定 + 前端 API 三处

**Files:**
- Modify: `internal/adapters/wails/services.go`（`ChatService`，加在 `History` 附近约 :520）
- Regenerate: `frontend/bindings/**`（`wails3 generate bindings`）
- Modify: `frontend/src/api/types.ts`（`SwarmApi` 接口）
- Modify: `frontend/src/api/bindings.ts`（`realApi`）
- Modify: `frontend/src/api/mock.ts`（`createMockApi`）

**Step 1: 服务方法**

```go
// ClearConversation 清空一个会话的聊天区（不删除消息数据）。
func (s *ChatService) ClearConversation(convID string) error {
	return s.d.Chat.ClearConversation(context.Background(), convID)
}

// RestoreConversation 撤销清空，让被隐藏的消息重新显示。
func (s *ChatService) RestoreConversation(convID string) error {
	return s.d.Chat.RestoreConversation(context.Background(), convID)
}

// ClearedAt 返回会话清空点的毫秒时间戳；0 表示未清空（前端据此决定按钮是否可点）。
func (s *ChatService) ClearedAt(convID string) (int64, error) {
	return s.d.Chat.ClearedAt(context.Background(), convID)
}
```

**Step 2: 重生成绑定**

```
cd d:/code/OpenSource/swarm-link
wails3 generate bindings -f "-tags wails" -clean=true -ts -i .
```
预期：`frontend/bindings/.../chatservice.ts` 出现 `ClearConversation` / `RestoreConversation` / `ClearedAt`。

**Step 3: 前端三处**

- `types.ts` 的 `SwarmApi`：
  ```ts
  clearConversation(convId: string): Promise<void>
  restoreConversation(convId: string): Promise<void>
  /** 返回毫秒时间戳；0 表示该会话未清空。 */
  clearedAt(convId: string): Promise<number>
  ```
- `bindings.ts` 的 `realApi`：
  ```ts
  clearConversation: async (convId) => { await ChatService.ClearConversation(convId) },
  restoreConversation: async (convId) => { await ChatService.RestoreConversation(convId) },
  clearedAt: async (convId) => ChatService.ClearedAt(convId),
  ```
- `mock.ts`：用一个 `mockCleared = new Map<string, number>()` 实现三个方法（让浏览器预览也能点着验证）。

**Step 4: 验证**

```
cd frontend && npm run typecheck   # 现有 6 条既有错误之外不应新增
```

---

### Task 4: 前端——两个按钮与渲染过滤

**Files:**
- Modify: `frontend/src/stores/chat.ts`（状态 + 两个 action + `activeMessages` 过滤）
- Modify: `frontend/src/components/Icon.vue`（新增 `trash`）
- Modify: `frontend/src/views/ChatView.vue`（两个按钮 + 空态文案）
- Create: `frontend/src/utils/clearView.ts`（纯函数 `canClear` / `canRestore`）
- Test: `frontend/tests/clearView.test.ts`

**Step 1: 写失败测试**

```ts
test('有消息才可清空', () => {
  assert.equal(canClear(0), false)
  assert.equal(canClear(3), true)
})

test('没有清空点时「查看历史」不可点', () => {
  assert.equal(canRestore(0), false)
  assert.equal(canRestore(1730000000000), true)
})
```

**Step 2: 跑测试确认失败**

```
cd frontend && npm test
```
预期：`canClear is not defined`。

**Step 3: 实现**

`utils/clearView.ts`：两个纯函数（`canClear(messageCount)` / `canRestore(clearedAt)`），供按钮 `:disabled` 与测试共用。

`stores/chat.ts`：

```ts
// 每个会话的清空点（毫秒；缺省/0 = 未清空）。视图规则，不参与后端查询。
const clearedAt = ref<Record<string, number>>({})

const activeMessages = computed(() =>
  (messagesByConv.value[activeConvId.value] ?? []).filter(
    (m) => m.sentAt > (clearedAt.value[activeConvId.value] ?? 0)
  )
)
```

外加：
- `loadClearedAt(conv)`：`open()` 时调用，取回清空点（失败当 0）；
- `clearConversation(conv)`：调服务成功后 `clearedAt[conv.convId] = Date.now()`（就地更新，立刻重渲染）；
- `restoreConversation(conv)`：调服务成功后删除该键；
- 导出 `isCleared(convId)`。

`Icon.vue` 新增 `trash`（描边风格，与现有图标一致）：

```ts
trash: ['M3 6h18', 'M8 6V4h8v2', 'M19 6l-1 14H6L5 6', 'M10 11v6', 'M14 11v6'],
```

`ChatView.vue` 工具栏（表情按钮之后）：

```vue
<button
  class="icon-btn"
  :disabled="!conv || !canClear(chat.activeMessages.length)"
  title="清空聊天区（不删除记录，可随时查看历史）"
  aria-label="清空聊天区"
  @click="clearView"
>
  <Icon name="trash" :size="17" />
</button>

<button
  class="icon-btn"
  :disabled="!conv || !canRestore(chat.clearedAtOf(conv.convId))"
  title="查看历史（恢复显示被清空的消息）"
  aria-label="查看历史"
  @click="restoreView"
>
  <Icon name="clock" :size="17" />
</button>
```

空态（`ChatView.vue:317` 那段）在"还没有消息"之前插入清空提示：

```vue
<div v-else-if="!chat.activeMessages.length" class="empty">
  <div v-if="chat.loading">正在加载历史…</div>
  <template v-else-if="conv && chat.isCleared(conv.convId)">
    <div class="empty-title">已清空</div>
    <div>消息没有被删除 · 点上方「查看历史」可恢复显示</div>
  </template>
  <template v-else>
    <div class="empty-title">还没有消息</div>
    <div>把文件直接拖到这里也能发送。</div>
  </template>
</div>
```

**Step 4: 验证**

```
cd frontend && npm test && npm run build
```

---

### Task 5: 验收

**Step 1: 后端全量**

```
cd d:/code/OpenSource/swarm-link
go test ./internal/... -count=1
go test -race ./internal/app/ ./internal/adapters/store/... -count=1
go vet ./...
```
预期：与本次改动前一致（既有红的仍是 `filesink` / `keystore` / `fsutil` 三个 Windows 问题）。

**Step 2: 出产物**

```
$env:EXTRA_TAGS='wails'; wails3 build DEV=true
```
注意：若已有实例在运行，新进程会被单实例机制立即弹出（不是崩溃）；冒烟前先关掉旧实例。

**Step 3: 手工清单**（见设计文档「验收」6 条，重点 3、4、6）

---

## 已知风险与对策

| 风险 | 对策 |
| --- | --- |
| 后端过滤会连带清空左栏预览 | 已在设计阶段改为前端过滤（见设计文档「后端设计 2」） |
| 对端时钟偏慢 → 其后发来的消息 `sentAt` 小于清空点，重启后落进隐藏区 | 实时消息走事件直插列表不受影响；该现象与"消息排序"是同一个既有时钟问题，非本功能引入 |
| 迁移改动历史版本导致 checksum 失配 | 只追加 version 2，绝不修改 version 1 |
