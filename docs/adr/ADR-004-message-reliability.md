# ADR-004：消息可靠性采用「msg_id + ACK + outbox」

**Status**：Accepted
**来源**：`架构设计书.md` §4.7

## Context

原设计 CHAT 是 fire-and-forget：连接断开期间对方发的消息全部丢失，
重连后无任何补齐机制；且 `messages` 表没有唯一键，重复到达会重复入库。
TCP 只保证**当前连接内**不丢不重不乱序。

## Decision

发送侧：

1. `msg_id = UUIDv7()`（时间有序，便于 UI 排序与游标分页）；
2. **单事务**写入 `messages(state='pending')` + `outbox`；
3. 发送后等 `CHAT_ACK`，收到即 `state='delivered'` 并删除 outbox 行；
4. 未收到由 outbox 扫描器重发（退避 1s → 2s → … → 5min）；
5. 对端离线时 outbox 保留，收到 `peer.online` 立即 flush。

接收侧：

1. `AppendIfAbsent` 幂等写入；只有首次插入才通知 UI；
2. **无论是否重复，都必须回 ACK**。

语义：at-least-once 投递 + 接收端幂等去重 ≈ 对用户等效 exactly-once。

## Consequences

**更好**

- 断线不丢消息；重复到达被吸收；送达状态可展示；
- 顺带解决群聊去重（群消息共用同一张表与同一个 `UNIQUE INDEX(msg_id)`）。

**更差**

- 多一张表与一个后台扫描器。

## 实现要点（最容易写错的点）

**重复消息也必须回 ACK。** 很多实现去重后直接 return，导致发送方永远收不到确认、
无限重发。本仓库对此有专门测试：
`test/e2e.TestDuplicateChatIsDedupedButStillAcked`
（断言「不重复入库、UI 只通知一次、但仍回 ACK」）。

## 为什么用 outbox 表而不是内存队列

进程崩溃后未送达消息不能丢 —— 这是「聊天工具」最基本的承诺。

## 另一处必须做对的细节：conv_id 必须对称

单聊 `conv_id` 若取「对端 NodeID」，则 A 存成 B、B 存成 A，
同一段会话在两端**分裂成两条**（历史、未读、去重全部错乱）。
因此取两个 NodeID 排序后拼接：`DirectConvID(a,b) = min+":"+max`。
`domain/message` 提供 `DirectConvID` / `DirectPeer` / `IsDirectConv` 三个函数，
CLI、App 与前端都使用同一规则。
