# 清空聊天视图（软清空）设计

状态：已与用户确认，待实施
日期：2026-09-29

## 背景与目标

用户需要两个动作：

1. **清空**：把当前会话的聊天气泡藏起来 —— **不删除任何数据**，只是视图上看不到；
2. **查看历史**：把被藏起来的重新展示出来。

两个动作各占一个图标，位置在聊天输入区工具栏那一行（与「发送文件 / 发送图片 / 表情」同一行）。

四个已确认的决策（本次设计中不可再动摇的前提）：

| # | 问题 | 决策 |
| --- | --- | --- |
| 1 | 作用范围 | **只清当前打开的会话**，其它会话互不影响 |
| 2 | 是否持久 | **跨重启保持**（因此必须存到后端，而不是内存） |
| 3 | 「查看历史」语义 | **等于撤销清空**：点一次之后一直全显示，不存在"再点收起" |
| 4 | 存储位置 | **SQLite 新表 + 服务接口 + 重新生成绑定** |

## 非目标（明确不做，避免范围蔓延）

- **不删除消息数据**：数据库里的消息一条不少，"清空"只是一个视图边界；
- **不清理传输任务**：右栏传输列表不受影响，"清空"只作用于聊天气泡；
- **不动未读数与左栏会话预览**（用户明确选择的范围）；
- **不做"清空所有会话"**；
- 不做"按时间段清理"、"清理文件缓存"之类的延伸功能。

## 数据模型

每个会话一行，记录一个「清空点」：

```sql
-- migrations.go 追加 version 2（已发布的 v1 永不修改：checksum 校验会拒绝）
CREATE TABLE IF NOT EXISTS conv_clears (
  conv_id    TEXT PRIMARY KEY,
  cleared_at INTEGER NOT NULL   -- 毫秒时间戳：早于它的消息不再显示
);
```

语义：**`sent_at <= cleared_at` 的消息属于"被清空的那段"**，不予展示；`sent_at > cleared_at` 的消息照常展示。

为什么用「时间戳」而不是「消息列表快照」：消息的排序键就是 `sent_at`（`Latest` 按 `sent_at DESC, id DESC`），用一个与排序键同源的边界来切分，语义才自洽 —— 被藏起来的正是"清空那一刻排在列表里的那些"。

## 后端设计

### 1. 存储接口（`internal/domain/ports/ports.go`）

在 `ChatStore` 上追加三个方法（而不是新建一个依赖）：`ChatApp` 已经持有 `ChatStore`，追加方法可以让构造函数与组合根**零改动**。

```go
SetConvCleared(convID string, at time.Time) error          // 写入/覆盖清空点
ConvClearedAt(convID string) (time.Time, bool, error)      // 读取；未清空时 ok=false
RemoveConvClear(convID string) error                       // 撤销清空（幂等）
```

两个对等实现各写一遍（否则 `var _ ports.ChatStore` 断言编译失败）：

- `internal/adapters/store/sqlite/`：迁移 v2 建表 + 三个方法的 SQL（用 `INSERT ... ON CONFLICT(conv_id) DO UPDATE` 实现"重复清空取更晚时刻"）；
- `internal/adapters/store/mem/mem.go`：map 实现，同一张 map 供 app 层测试使用。

### 2. 过滤放在前端渲染层（实施前核实后的一处修正）

**修正来由**：原本打算在应用层（`ChatApp.History`）过滤。实施前核实发现：`ChatService.Conversations()`（左栏会话列表与预览）内部也用 `ChatApp.History(convID, 1, 0)` 取"最后一条消息"（`services.go:530`、`:552`），`GroupService.History` 同样调用它（`services.go:886`）。若在那里过滤，**清空一个会话会让左栏预览一起变空** —— 与用户选定的范围（只清聊天区、不动左栏预览）直接冲突。

因此：**后端只负责存/取清空点，过滤发生在前端渲染层。**

- 前端把清空点（`clearedAt` 毫秒；`0` 表示未清空）与该会话的消息放在一起；
- `messagesByConv` 仍然保存**全部**拉取到的消息 —— 点「查看历史」时无需重新请求，消息立刻可见；
- 过滤放在 store 的 `activeMessages` 计算属性里：聊天区列表与"还没有消息"空态读的是同一个值（`ChatView.vue:317`），过滤一处即可保证两者一致。

分页语义：一次拉最新 100 条再在渲染层过滤，被滤掉的一定是更早的那些；清空之后到达的新消息在最新的 100 条里，照常显示。当前没有"加载更多历史"，因此不存在跨页过滤问题。

### 3. 应用层方法

```go
// ChatApp（单聊与群聊共用同一张表：单聊 conv_id 含冒号、群 ID 是纯 hex）
func (a *ChatApp) ClearConversation(ctx context.Context, convID string) error   // 写入 now()
func (a *ChatApp) RestoreConversation(ctx context.Context, convID string) error // 删除清空点（幂等）
func (a *ChatApp) ClearedAt(ctx context.Context, convID string) (int64, error)  // 毫秒；0 = 未清空
```

### 4. 服务层与绑定（`internal/adapters/wails/services.go`）

```go
ChatService.ClearConversation(convID string) error
ChatService.RestoreConversation(convID string) error
ChatService.ClearedAt(convID string) (int64, error)
```

用 `convID` 而不是 `peerID`：清空点是会话级概念，单聊与群聊无差别，而前端手里就是 convId。之后执行 `wails3 generate bindings` 重生成 `frontend/bindings/...`。

## 前端设计

### 1. 工具栏（`views/ChatView.vue`）

在现有三个按钮（回形针 / 图片 / 表情）之后追加：

| 图标 | 动作 | 可用条件 |
| --- | --- | --- |
| `trash`（需新增 SVG path） | 清空 | 当前会话有消息 |
| `clock`（Icon.vue 已有） | 查看历史 | 当前会话**存在清空点** |

「查看历史」在无清空点时**禁用**，因此不存在"点了却没东西可恢复"的路径。

### 2. 交互链路

- 「清空」→ `chat.clearConversation(conv)` → 写清空点 → 重新 `loadHistory` → 消息消失；空态文案改为「已清空 · 点上方『查看历史』可恢复显示」；
- 「查看历史」→ `chat.restoreConversation(conv)` → 删清空点 → 重新 `loadHistory` → 全部回来。

两处都复用现有 toast 给一句反馈（不做确认弹窗：不删数据、且一键可恢复）。

### 3. 状态与 mock

- `stores/chat.ts`：`clearedAt: Record<convId, number>`（毫秒，`0`/缺省 = 未清空）。`open()` 时拉一次 `ClearedAt`；两个 action 调服务成功后**就地更新**本地值，界面立刻重渲染（清空的那一刻消息消失、查看历史的那一刻回来）；`activeMessages` 据此过滤；
- 按钮可用性抽成纯函数（`canClear` / `canRestore`），用 `frontend/tests/*.test.ts` 的 `node:test` 覆盖；
- `api/{types,bindings,mock}.ts` 三处同步新增的三个方法。

## 边界与错误处理

| # | 情形 | 行为 |
| --- | --- | --- |
| 1 | 清空时正在传输的文件卡片 | 一起隐藏；传输任务不受影响，右栏照常跑完 |
| 2 | 清空后对方发来消息 | 立刻显示（实时消息由 `chat:new-message` 直插列表，不经过历史过滤） |
| 3 | 未读数 / 左栏预览 | 不动。清空点只作用于渲染层，`Conversations()` 与预览走的是未过滤的查询（见"后端设计 2"的修正来由） |
| 4 | 老数据库没有这张表 | 迁移自动建表，用户无需任何操作 |
| 5 | 重复点「清空」 | 覆盖成更晚的时刻（幂等） |
| 6 | 群聊与单聊共用同一张表 | 单聊 `conv_id` 含冒号、群 ID 是纯 hex，形态不冲突 |
| 7 | 存储读失败 | 当作"未清空"处理（宁可多显示，也不要出现"消息凭空消失"） |

### 一处如实说明的边界

若对端时钟比本机慢，他"清空之后"发来的消息其 `sentAt` 可能小于清空点 → **重启后**重新拉历史时它会被排进隐藏区。实时消息不受影响（走事件直插列表）。局域网时钟一般由 NTP 同步，这个窗口很小；且该现象与"消息被排进历史中间"是同一个既有时钟偏差问题，并非本功能引入。

## 测试策略

**后端**

- SQLite：迁移 v2 生效（`sqlite_test.go` 的表清单补 `conv_clears`）；清空点读写往返；重复清空取更晚时刻；
- 同一组断言同时跑 mem 与 sqlite，防止两个实现对等性漂移；
- `ChatApp` / `GroupApp`：清空后 `History` 不返回旧消息、但返回其后的新消息；`RestoreConversation` 后全部回来；**未清空的会话不受影响**；清空后无新消息时返回空列表而非报错；存储读失败时退化为"显示全部"。

**前端**

- `canClear` / `canRestore` 纯函数测试；
- `npm run build` + `npm run typecheck`（typecheck 现有 6 条既有错误，本次不新增）。

## 验收（手工）

1. 打开有历史的会话 → 点「清空」→ 气泡消失、空态提示出现、左栏预览与未读不变；
2. 点「查看历史」→ 全部消息回来，「查看历史」按钮变灰；
3. 重启应用 → 该会话仍是清空的；
4. 清空后让对方发一条消息 → 立刻出现；
5. 切到另一个会话 → 它完全不受影响；
6. 无消息的会话 / 无清空点的会话 → 对应按钮为禁用态。
