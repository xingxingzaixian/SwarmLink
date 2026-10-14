# 群聊完善 · 设计

**日期**：2026-10-11
**范围**：群成员管理 + 群信息面板 / 群消息按成员投递跟踪（离线补投） / 群文件与文件夹 P2P 传输 / 消息体验增强
**不做**：退出群（成员主动退出，本次不做，见 §6）；群成员上限提升；群内多人同时建群冲突

---

## 1. 背景与现状

当前群聊已具备：建群（可选初始成员）、群文本/图片消息扇出、群历史、群元数据（owner 签名 + epoch）同步。
三处明确缺口：

1. **成员管理只有"加"的后端方法，没有任何 UI**；移除/改名/解散后端也没有服务方法。
2. **群消息不做离线补投**：`GroupApp.HandleGroupMsg` 用 `TypeChatAck` 回 ACK，而该帧统一由
   `ChatApp.HandleChatAck → store.Deliver(msgID)` 处理 —— **群里任意一人 ACK，消息就出队、停止重发**，
   离线成员永远收不到（`group_app.go:21` 的注释即源于此）。
3. **群文件传输前端主动禁用**（`services.go` SendFile 只发一条通知卡片）。

## 2. 关键设计洞察：群级变更如何被认证

`group.SigningBytes() = H(id | epoch | members)`，`commitGroup` 在每次变更时 `epoch++` 并让 owner 重新签名。
于是：

- **rename / dissolve / 成员变动**：只要走 `commitGroup`（epoch 递增 + owner 重签 + 广播），对端 `ApplyMeta`
  在 `epoch > cur.epoch` 且验签通过时才接受。**没有 owner 私钥就造不出 epoch+1 的合法签名**，
  因此这些变更天然不可伪造。
- **但群级"状态"（是否解散）必须进入签名域**：否则攻击者可拿一条合法的 `(id, epoch, members, sig)`
  把 state 篡改成 `dissolved` 再广播；对端首次见到该 epoch 时会接受（签名只覆盖了 id/epoch/members）。
  故本设计**把 `state` 纳入 `SigningBytes`**，签名布局改为 `id | epoch | state | members`。

代价：变更前后本地已有的群 signature 会失效（仅影响升级前的开发库）。本项目尚未发布，接受；
对端在收到新布局 meta 时会因验签失败而忽略，**最低成本的自愈是重建该群**（见 §7 风险）。

## 3. 数据模型

### 3.1 迁移 v5

```sql
ALTER TABLE groups ADD COLUMN state TEXT NOT NULL DEFAULT 'active';

CREATE TABLE IF NOT EXISTS group_msg_deliveries (
    msg_id    TEXT NOT NULL,
    group_id  TEXT NOT NULL,
    member_id TEXT NOT NULL,
    acked_at  INTEGER,               -- NULL = 未送达
    PRIMARY KEY (msg_id, member_id)
);
CREATE INDEX IF NOT EXISTS ix_gmd_pending ON group_msg_deliveries(msg_id, acked_at);
```

`state` 用纯追加（默认 active），零搬迁，符合 v3/v4 的既定做法。

### 3.2 领域

- `group.Group` 新增 `State GroupState`（`active` | `dissolved`）与 `IsDissolved()`；`Validate()` 增加状态合法性。
- `group.SigningBytes()` 纳入 state。
- 新端口 `ports.GroupDeliveryRepo`（见 §4.2）。

## 4. 后端改动

### 4.1 成员管理（`app.GroupApp` + `wails.GroupService`）

| 能力 | 语义 | 实现 |
|---|---|---|
| `Rename(groupID, name)` | 仅 owner；epoch++ 并广播 | 复用 `commitGroup` |
| `RemoveMember(groupID, memberID)` | 仅 owner；成员置 `kicked` | 复用 `SetMemberState` + 服务方法 |
| `Dissolve(groupID)` | 仅 owner；`State=dissolved`，epoch++ 广播 | 新增 |

`commitGroup` 保持不变（唯一收口）。`ApplyMeta` 增加：解散态的 epoch 必须**不低于**当前才接受（同现有 epoch 规则）。

### 4.2 按成员投递跟踪

新增端口：

```go
type GroupDeliveryRepo interface {
    EnsureDeliveries(msgID, groupID string, members []identity.NodeID) error
    AckDelivery(msgID string, memberID identity.NodeID) error
    PendingMembers(msgID string) ([]identity.NodeID, error)
    // StatsByConv 返回会话内每条群消息的 (已送达, 成员总数)。
    StatsByConv(convID string) (map[string]DeliveryStat, error)
    PurgeAcked(olderThan time.Time) (int, error)
}
type DeliveryStat struct{ Acked, Total int }
```

**发送**（`GroupApp.send`）：`AppendOutgoing` 后，对全体 `active` 成员（不含自己）`EnsureDeliveries`。
消息保持 `pending` 直到全员 ACK。

**扇出**（`fanout`）：目标 = `未 ACK 成员 ∩ 在线成员`（`PendingMembers` 过滤）。离线成员不当时发送，
留待 `SweepOutbox` 在其上线后补投。

**ACK 路由**：群接收方仍回 `TypeChatAck`。`ChatApp.HandleChatAck` 先按 `msgID` 查消息，若为群会话 →
委派 `GroupApp`：

```go
// ChatApp 增加注入点（nil 安全），由组合根接到 GroupApp.HandleGroupAck
GroupAck func(from identity.NodeID, msgID string) error
```

`GroupApp.HandleGroupAck`：`AckDelivery` → 若 `PendingMembers` 为空 → `store.Deliver(msgID)`（置 delivered + 出队）+ 发布 `chat.delivered`。

**补投**（`GroupApp.SweepOutbox`）：对到期群消息，`fanout` 给当前在线的未送达成员；无人待投则 `Deliver` 出队。
保留期沿用 outbox 退避；额外用 `PurgeAcked` 清理已完成的投递行（避免无限增长）。

### 4.3 群文件/文件夹传输

后端协议与传输层**已经具备**：文件传输始终是点对点（`TransferApp`）。群文件 = 发送端对**每个在线成员**
各发起一条既有 P2P 传输，群会话里只发**一条**聚合卡片消息（`SendGroupFile`，可携带 `batch_id`）。
故后端只需在前端放行后仍能正确回显卡片（复用现有 `MsgTypeFile` / `MsgTypeFolder`）。

## 5. 前端改动

- `stores/group.ts`：新增 `rename / removeMember / dissolve`。
- `components/GroupInfoPanel.vue`（新）：群名、群主标识、成员列表（角色 + 在线态）、
  owner 可见的操作（加人/移除/改名/解散）；解散前二次确认。
- `views/ChatView.vue`：群标题可点开面板；`dissolved` 时展示横幅并禁用输入。
- `components/ChatBubble.vue`：群消息显示**「N 人未送达」**（数据来自 `MessageDTO.pendingCount/memberCount`）；
  右键菜单：**复制**、**引用回复**。
- `composables/useFileSend.ts`：群会话中放行文件/文件夹，逐个在线成员发起传输。
- `@提及`：输入框 `@` 触发成员下拉；渲染时对 `@名字` 高亮。
- `events/index.ts`：`group:updated` 同时刷新 `groups` 列表（不只刷新会话）。

`MessageDTO` 增加 `pendingCount` / `memberCount`；`GroupDTO` 增加 `state`。

## 6. 不做（本次）

- **退出群**：由群主侧移除代替。成员主动退出需要新的 LEAVE_REQ 帧与"群主不在线"的失败路径，本次不做。
- 群成员上限提升（仍为 20）、群主离线自动选举（ADR-003 已明确不做）。

## 7. 风险

| 编号 | 风险 | 处置 |
|---|---|---|
| G-1 | `SigningBytes` 布局变更使旧签名失效 | 未发布，接受；文档明示重建群 |
| G-2 | 投递行随离线成员长期存在而膨胀 | `PurgeAcked` + outbox 退避上限；群消息仍受消息表保留策略约束 |
| G-3 | ACK 路由改动影响单聊路径 | `HandleChatAck` 只在"查到且为群会话"时分派；单聊路径零变化，配单测守住 |
| G-4 | 解散后组内成员各自处理时机不同 | 以 epoch 单调为准；解散 = 一次 epoch++ 广播，幂等 |

## 8. 验收（可执行）

1. `go test ./...` 全绿（含新增测试）。
2. 群成员：加人 → 移除 → 改名 → 解散，每次 epoch 单调递增，对端元数据一致。
3. 离线补投：3 人群，1 人离线发消息，该成员上线后收到且消息状态由 pending 转 delivered。
4. 未送达统计：群消息在有人未 ACK 时 `pendingCount>0`，全 ACK 后为 0。
5. 群文件：群会话发文件，在线成员各自收到 P2P 传输，群里只有一条卡片。
6. 单聊回归：单聊 ACK 仍走 `Deliver`，行为不变。
