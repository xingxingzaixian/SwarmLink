# 群成员模型改造：发票 / 撕票（Token-Tombstone）设计

**日期**：2026-10-11
**状态**：草案（待评审）
**替代**：`ADR-003` 中「群状态仅由 owner 以 epoch 全量签名传播」的**成员同步部分**（群级字段与扇出策略保留）
**关联**：`docs/plans/2026-10-11-group-chat-enhancement-design.md`（群离线补投）、`ADR-003`、`ADR-010`

---

## 一、要解决的问题

当前成员一致性只有**一条**路径：群主变更 → `epoch+1` → 群主重签全表 → **单播给「active ∩ 在线」**。由此产生三个已被代码核实的缺口：

| # | 缺口 | 代码事实 |
|---|---|---|
| **G-1** | **主动退出无处安放**。`active/left/kicked` 三个状态里，「退出」的权威是**本人**，其余是**群主**；现在整表压在**一个** owner 签名下，因此只有群主能改 | `SigningBytes` 覆盖完整成员表；`SetMemberState` 要求 `OwnerID == self` |
| **G-2** | **离线成员错过即永久错过**。广播目标只含「在线」；`GroupApp` 无 `Start()`、不订阅 `peer.online`；`GROUP_MEMBERS_REQ` 有 handler **无 sender**；成员收到 meta 后不转发 | `broadcastMeta` 仅由 `commitGroup` 调用；全仓无 `TypeGroupMembersReq` 的发送点 |
| **G-3** | **入站群消息不校验成员资格**，被移出者仍能收发 | `HandleGroupMsg` 只检查 `msg_id`/`group_id` 非空 |

**G-1 是本次改造的动因**；G-2/G-3 是同一处的连带缺口，一并修。

## 二、目标与非目标

**目标**

1. 本人可**主动退出**，且**不需要群主在线**、离线也能立即生效；
2. 任意成员的上线都能**双向对齐**（别人能告诉我群存在；我也能把新状态推给别人）；
3. 合并规则**不依赖时钟**、不依赖全局全序，任意顺序 / 任意同步源都收敛到同一结果；
4. **群主仍保留**「加人 / 踢人 / 改名 / 解散」的权力（`ADR-003` 的权威模型不推翻）；
5. 存储不随操作次数增长（可压缩）。

**非目标（本期明确不做）**

- 防篡改 / 抗恶意节点 / 分裂视图防护：沿用 `ADR-003` 的「内网可信」立场；
- 群主自动选举、多群主 / 管理员分级授权；
- 「被踢者自助回群」（踢人仍不可自撤销，见 §11 否决方案 B）。

## 三、术语

| 术语 | 含义 |
|---|---|
| **票（Token）** | 一次「准入」的凭据。群主**加人 / 重新拉回**时发一张**全新的**票。票的存在 ⇔ 该成员被准入过 |
| **撕票（Tombstone）** | 把票作废。**本人退出**由本人撕（`left`）；**群主踢人**由群主撕（`kicked`） |
| **票号（seq）** | 票的唯一标识，取自**群主的群级单调计数器** `MintSeq`。**这是本设计的核心约束**（见 §4.2） |
| **名册摘要（RosterDigest）** | 对规范化后的完整成员状态求哈希。两个节点摘要相同 ⇔ 双方视图一致 |

## 四、模型

### 4.1 概念模型（先说清语义）

```
active(成员 m)  ⟺  m 存在至少一张票，且该票未被任何撕票作废
```

- 群主加人 / 拉回 → **发新票**（一张从没出现过的票）；
- 本人退出 → **撕掉自己当前那张票**（自签，本地立即生效）；
- 群主踢人 → **撕掉该成员当前那张票**（群主签）；
- 并发「退出 + 踢人」→ 两人撕的是**同一张票**，结果相同，天然幂等。

**这个规则没有任何「取最大」**：合并是集合求并，因此不需要时钟、不需要全序、不需要版本比大小。拉回一定生效，因为新票「没被任何人撕过」这个事实与对方撕过什么无关。

### 4.2 核心约束：票号必须是「群级、群主单一写者」的单调计数器

> **票号只能来自群主的一个群级计数器 `MintSeq`，绝不能是「每用户各自的计数器」。**

理由（这是把「每用户版本号」方案推翻的那条，见 §11 否决方案 A 的完整反例）：
「加人是否生效」必须**不依赖群主是否知道对方的计数器**。群级计数器保证「新发的票号恒大于此前发过的所有票号」，于是群主**无需读取任何对端状态**就能发一张必胜的票。

### 4.3 紧凑表示（为什么不需要存票集合）

因为每次准入只发**一张**票、且撕票总是撕掉「当前那张」，所以任一成员**任一时刻最多只有一张活票**。于是：

- 活票 → 只需记它的票号：`AdmitSeq`
- 撕票集合 → 两个「已撕到的最大票号」：`KickSeq`（群主撕）、`LeftSeq`（本人撕）

判定式：

```
active(m)  ⟺  m.AdmitSeq > m.KickSeq  且  m.AdmitSeq > m.LeftSeq
```

**每成员三个整数，无集合增长，压缩成本为零。** 这正是「发票/撕票」的等价紧凑形式，不是另一种模型。

领域类型：

```go
type Member struct {
    NodeID      identity.NodeID
    DisplayName string
    Role        MemberRole
    JoinedAt    time.Time

    AdmitSeq int64  // 最近一次「发票」的票号；0 = 从未被准入
    KickSeq  int64  // 群主最近一次「撕票」的票号；0 = 未踢过
    LeftSeq  int64  // 本人最近一次「撕票」的票号；0 = 未退过
    LeftSig  []byte // 本人对 (groupID|nodeID|LeftSeq) 的签名
}

func (m Member) Active() bool { return m.AdmitSeq > m.KickSeq && m.AdmitSeq > m.LeftSeq }
```

群级：

```go
type Group struct {
    ID, Name   string
    OwnerID    identity.NodeID
    CreatedAt  time.Time
    State      GroupState // active | dissolved
    MintSeq    int64      // 群主的群级发票计数器，单调递增
    RosterSig  []byte     // 群主对「名册快照」的签名，见 §6
}
```

## 五、合并规则（收敛性）

**关键约束：名册分两块存储、两块合并，绝不把两块揉成一份再当作群主签名转发。**

### 5.1 两块状态

| 块 | 内容 | 写者 | 合并方式 |
|---|---|---|---|
| **owner 快照** | `Name`/`State`/`Epoch`/`MintSeq` + 每成员的 `AdmitSeq`/`KickSeq` | 群主 | **原子替换**（`Epoch` 更高时整块替换，不逐字段 merge） |
| **left 覆盖** | 每成员的 `LeftSeq` + `LeftSig` | 各自本人 | **逐成员取 max** |

```
active(m) ⟺ owner快照.AdmitSeq(m) > owner快照.KickSeq(m)  且  owner快照.AdmitSeq(m) > left覆盖.LeftSeq(m)
```

### 5.2 为什么 `AdmitSeq`/`KickSeq` **不能**逐字段取 max

群主的 `RosterSig` 覆盖「群级字段 + 全部成员的 `AdmitSeq`/`KickSeq`」，是**一份整体**。
若把两份不同快照的成员 seq 各取 max 拼起来，得到的组合**不对应任何一份合法签名**。
而中继节点会把「自己当前的名册」转发给别人 —— 于是它会以群主之名转发一份**没人签过**的名册。
（这正是 `ADR-003` 当初把成员表压成「一个签名 + 一个 epoch」要避免的事。）

因此：**owner 快照按 `Epoch` 原子替换**；成员 seq 不参与跨快照合并。

### 5.3 中继必须转发「收到的那份签名快照」，而不是「本地合并后的视图」

- `RosterSig` 对应的字节原样保留（`RosterBlob`），转发时原样送出；
- `left` 覆盖是**自签**的，逐条原样转发；
- 两者的并集才是「我当前的名册」，但它**永远不会被当作群主签名送出**。

### 5.4 性质

- **交换律**：`merge(a,b) == merge(b,a)`（整块替换比 `Epoch`；left 取 max）；
- **幂等**：`merge(a,a) == a`；
- **单调**：`Epoch` 与三个 seq 只增不减。

因为每块都只有**一个写者**且该写者单调递增，「取 max / 取更高 epoch」与「取最新」等价，
**不需要时间戳**参与仲裁。
（对比：本项目消息排序「沿用对端时钟」已是记录在案的已知问题，那份时钟不足以承担成员表的仲裁。）

### 5.5 落库形态

`group_members` 同时存 `admit_seq`/`kick_seq`（来自 owner 快照）与 `left_seq`/`left_sig`（本地合并结果）；
`groups` 存 `roster_sig`（当前生效的 owner 快照签名）。判定 `Active()` 在读取时按 5.1 的式子算，
**不再依赖存储的 `state` 列**（该列改为派生缓存，见 §9）。

### 合并前的校验

1. `RosterSig` 用 `OwnerID` 的公钥验签（公钥来自节点目录，即 `ANNOUNCE`/握手已认证的身份）；
2. 每个成员的 `LeftSeq > 0` 时，`LeftSig` 必须能由其 `NodeID` 的公钥验证；
3. `AdmitSeq < KickSeq` 之类的组合是**合法**的（就是「被踢」），不做拒绝。

## 六、权威划分（谁签什么）

| 内容 | 签名者 | 覆盖字节 |
|---|---|---|
| 群级字段 + 名册（含 `AdmitSeq`/`KickSeq`） | **群主** | `H(groupID \| name \| state \| epoch \| mintSeq \| Σ(memberID, admitSeq, kickSeq))` |
| 本人退出 | **本人** | `H(groupID \| nodeID \| leftSeq)` |

即：**群主签「名册」，本人签「我的退出」**。两块各自单调且互不覆盖，因此可以**独立传播、独立合并**；
线上也按两块独立携带（§7.1），中继只转发收到的签名块，不重签、不拼装。

`DisplayName` **不进入任何签名域**：它由各成员的 `ANNOUNCE` 维护，纳入会让改昵称触发全表重签。
它作为**非权威提示**随名册携带，取值冲突时以本地节点目录为准。

（`LeftSeq` 取值落在群主的票号域内，所以 `AdmitSeq > LeftSeq` 的比较是有意义的。）

## 七、协议改造

### 7.1 复用

| 帧 | 用途 | 变化 |
|---|---|---|
| `GROUP_META 0x30` | 全量名册快照（推送 / 广播 / 应答） | 载荷改为**两块独立认证的内容**：① 群主签名块（`name`/`state`/`epoch`/`mint_seq`/每成员 `admit_seq`+`kick_seq`/`roster_sig`）；② `left` 覆盖列表（每项 `{node_id, left_seq, left_sig}`）。另加 `roster_digest` |
| `GROUP_MEMBERS_REQ 0x31` | **终于接上发送端**：摘要不一致时拉全量 | 增加 `known_digest`（摘要一致则回空，省流量）；`known_epoch` 保留 |
| `GROUP_MEMBERS_RESP 0x32` | 返回全量快照 | 载荷同 `GROUP_META` |
| `GROUP_MSG 0x33` | 群消息 | **新增成员资格校验**（见 §8.3） |

### 7.2 新增一帧

| 帧 | 值 | 载荷 | 说明 |
|---|---|---|---|
| `GROUP_LEAVE` | `0x34` | `{group_id, node_id, left_seq, sig}` | 本人退出的独立传播 |

**为什么不复用 `GROUP_META`**：meta 是**群主签名**的全量快照；退出是**本人签名**的单点增量。两者权威不同、合并维度不同，混在一个帧里会让「谁能改这一帧」再次变得说不清 —— 那正是 G-1 的病根。

`GROUP_LEAVE` 的接受条件（三条全满足）：本机认识该群 → `sig` 用 `node_id` 的公钥验过 → `left_seq` 大于本地该成员的 `LeftSeq`。

### 7.3 摘要

```
RosterDigest = H(groupID ‖ state ‖ sorted( memberID, admitSeq, kickSeq, leftSeq ) )
```

- 不含 `Name`/`DisplayName`（改名/改名后的显示名不该触发名册同步）；
- 摘要相同 ⇒ 收到 meta 后**不落库、不触发 UI 刷新**，省掉无意义的事件风暴；
- 摘要不同 ⇒ 触发一次 `GROUP_MEMBERS_REQ`。

## 八、同步触发点（修 G-2）

新增 `GroupApp.Start(ctx)`（当前该类型**没有任何 Start**），订阅三类事件 + 一个低频抽查：

### 8.0 变更时的广播受众必须含「刚被移出的人」

新增 `Group.MetaAudience`，与 `FanoutPlan` 并列但语义不同：

| 方法 | 受众 | 用途 |
|---|---|---|
| `FanoutPlan` | 在群 ∩ 在线（不含自己） | **群消息**：只发给还在群里的人 |
| `MetaAudience` | **名册里出现过的所有人** ∩ 在线（不含自己） | **名册变更**：必须也发给刚被移出的人 |

这不是理论问题。实测（e2e）发现：只用 `FanoutPlan` 广播会得到一个极难解释的不一致 ——
**在线被踢的人收不到自己被踢**（他已不在 active 集合里，而广播目标正是 active），
而**离线被踢的人反而能靠上线推送得知**。同一个动作的结果取决于当时在不在线。

> 这也是 T9 的 e2e 用例把一个真实缺陷暴露出来的地方：三条「离线对齐」用例全过、
> 唯独「在线被踢后不能再发消息」那条卡在「c 迟迟不认为自己被移出」。
> 修法就是本节的 `MetaAudience`。

### 8.1 有人上线 → **主动推**（覆盖「加离线成员入群」与「踢人通知」）

```
订阅 TopicPeerOnline(peerID)：
  对每个「peerID ∈ 我的名册且 active」或「peerID 曾出现在我名册里」的群
      → 单播一次 GROUP_META（全量快照）
```

**任一成员都能推送**，不依赖群主在线 —— 这正是「新成员本地压根没有这个群、无从发起拉取」的解法：他不需要发起，别人会推给他。
（代价：在内网可信前提下，任一成员都能「告诉」别人他属于某个群。已在 §12 记为接受的取舍。）

### 8.2 拉取 → 对齐（覆盖「我在线但落后了」）

```
订阅 TopicPeerDiscovered(新增节点)：
  若本机有群且该节点与我有共同群 → 发 GROUP_MEMBERS_REQ{group_id, known_digest}
订阅 TopicPeerOnline：
  同上，并额外对「摘要未知」的群拉一次
```

### 8.3 周期抽查（收敛保险）

复用 `bootstrap` 的 1 秒节拍，**低频**（默认 60s，可配）轮转：对每个群挑一个在线成员比对摘要，不一致则拉取。用于兜住「推送丢了、`GROUP_LEAVE` 丢了」这类静默丢失。

### 8.4 入站群消息的成员校验（修 G-3）

`HandleGroupMsg` 在落库前增加一条判据：`Active(senderID) == false` ⇒ **丢弃但仍回 ACK**。
- 为什么仍回 ACK：与现有「坏图片消息也回 ACK」同策略 —— 不回 ACK 会让发送方按 outbox 策略无限重发；
- 为什么用「当前名册」而不是「发送时的名册」：成员资格是**现在**的事实。

## 九、数据模型与迁移

### 9.1 迁移 v6

```sql
ALTER TABLE groups        ADD COLUMN mint_seq INTEGER NOT NULL DEFAULT 1;
ALTER TABLE group_members ADD COLUMN admit_seq INTEGER NOT NULL DEFAULT 1;
ALTER TABLE group_members ADD COLUMN kick_seq  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE group_members ADD COLUMN left_seq  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE group_members ADD COLUMN left_sig  TEXT;
```

默认值刻意取「`admit_seq = mint_seq = 1`，其余 0」，即**每个已有成员视作拿到过 1 号票**：语义上等价于「升级前的一次历史准入」，存量数据不需要人工修补。

### 9.2 签名兼容性（本次唯一的一次性代价）

`RosterSig` 覆盖的字节里新增了 seq 字段，因此**升级前已存在的群，其旧签名在新代码下验不过**。处置：

- 选项 1（推荐）：**群主打开一次群信息面板即触发一次重签与广播**（`GroupApp` 在 `Start` 时对「本机为群主且 `RosterSig` 为空」的群自动重签一次，无需用户操作）；
- 选项 2：`ApplyMeta` 同时接受旧格式签名并就地升级 —— 兼容代码只为一版存在，收益低。

**开发库无存量用户**，采用选项 1，并在 `bootstrap` 启动时一次性完成。

## 十、与既有模块的衔接

| 模块 | 所需改动 |
|---|---|
| `domain/group` | `Member` 加三个 seq + `LeftSig`；`Group` 加 `State`/`MintSeq`/`RosterSig`；`Active()`/`ActiveMembers()`/`FanoutPlan()` 改用 `Active()`；新增 `Merge()`（纯函数，可单测） |
| `app/group_app.go` | 新增 `Start(ctx)`、`HandleGroupLeave`、`Leave`（本地撕票 + 广播）、`requestSync`、`maybePushMeta`；`commitGroup` 改为「发票/撕票」语义并递增 `MintSeq`；`EnsureDeliveries` 与 `pendingActive` 改用 `Active()` |
| `app/chat_app.go` | 群消息 ACK 委派不变 |
| `app/router.go` | 注册 `TypeGroupLeave` |
| `adapters/store/{sqlite,mem}` | 迁移 v6 + 三 seq/签名的读写 |
| `adapters/wails` | `GroupDTO` 增 `state`（已加）/`Active()` 计算结果；`GroupService` 增 `Leave`；`History` 的未送达统计改用 `Active()` |
| `frontend` | 群信息面板增加「退出群」（非群主可见）；成员行显示「已退出/已移出」；解散横幅（已加） |
| `bootstrap` | 调用 `GroupApp.Start(ctx)`；群主重签一次（§9.2） |

**与群离线补投的协同**：补投的「未送达」判据若是「pending ∩ 当前 active」，则成员一旦退出/被踢会**自动不再阻塞该消息完成**，两条改动天然互补（现有 `pendingActive` 只需把 `State == MemberActive` 换成 `Active()`）。

## 十一、已否决的替代方案

### 方案 A：每用户一个递增版本号 + 取最新（用户最初提案）

- **否决理由（与安全无关）**：`active` 与 `left` 由**两个不同的写者**写同一个计数器，导致「同一操作是否生效」取决于群主是否知道对方的计数值。

  **反例**：群 `{A(群主), B, C, D}`。
  1. D 离线期间自己退出：`D.v = 5, left`；
  2. A 不知道 D 已自增到 5，按加人写 `D.v = 3, active`；
  3. D 上线，从 B、C 拉全量并**逐用户取 max(v)** → `v=5, left`。

  **群主明明把 D 拉回了，却永久不生效**；若 A 恰好知道 D 是 5 而写 `v=6`，则生效。→ 「值写对」依赖于「先读到对方」，这不是可在离线 / 乱序下收敛的值。

  另：并发时 A 写 `v=5,kicked` 与 D 写 `v=5,left` 撞同版号，「取最新」对同版号无定义；而 `mtime` 不可信（见 §5）。

- **保留的部分**：**「每人一条记录」这个形状被采纳**，只是把「一个版本号」换成「一个票号 + 两个撕票号」。

### 方案 B：完全去中心 gossip（去掉群主权威）

- 允许被踢者自助回群；
- 需额外引入 tiebreak 与集合摘要；
- 与 `ADR-003` 的「owner 签名仲裁」直接冲突。
- **否决理由**：收益（少一次群主操作）远小于代价（踢人不可撤销 + 需新仲裁规则）。

### 方案 C：退出 = 向群主发请求（不引入 CRDT）

- 零模型改动，最小；
- **否决理由**：**群主离线即无法退出**。`ADR-003` 已接受「群主长期离线则群只读」，但把「退出」也绑进这条会显著放大抱怨面。

### 方案 D：保留 epoch 全量快照，仅叠加自签 left 标记

- 折中：名册仍由 epoch 全序，退出作为自签增量叠加。
- **保留为降级选项**：若实施中发现 seq 模型对现有代码的面太宽，可退化到此方案 —— 它同样能解决 G-1，只是「拉回」仍需群主递增 epoch（因为 epoch 域与 leftSeq 域不同源）。**这也是本设计选择「群级票号」而非「epoch」的原因：同一域内比较才免去「谁大」的歧义。**

## 十二、风险与已接受的取舍

| # | 风险 / 取舍 | 等级 | 说明与缓解 |
|---|---|---|---|
| R-1 | 任一成员都可向他人推送「你属于某群」 | 中 | 内网可信前提下的接受项（`ADR-003` 同立场）。后续 v1.1 引入端到端加密与设备鉴权后可收紧 |
| R-2 | 群主永久离线则**无法加人/踢人/解散** | 中 | 与现状一致（`ADR-003` 已接受），本设计**不使其更差**；但退出不再受此限制，属净改善 |
| R-3 | `RosterSig` 布局变更导致存量群需重签一次 | 低 | §9.2：启动时群主自动重签；开发阶段无存量用户 |
| R-4 | 摘要比对 + 拉取带来额外往返 | 低 | 摘要一致即不落库；抽查默认 60s 且轮转；20 人群快照约 2 KB |
| R-5 | 票号只增不减，极端长期使用后 `int64` 溢出 | 极低 | `int64` 每秒一票可用 2.9×10¹¹ 年 |

## 十三、测试计划

**单元（领域，纯函数，表驱动）**

1. `Merge` 的交换律 / 幂等 / 单调（随机对生成，交叉断言）；
2. `Active()` 的边界：**踢人 / 退出之后 `KickSeq`（或 `LeftSeq`）恰好等于 `AdmitSeq`**（撕的就是当前那张活票），
   因此判据必须是**严格大于**：`admit > kick && admit > left`。若误写成 `>=`，**踢人当场失效** —— 这是本设计最容易写错的一行；
3. 退出后拉回：`admit=4 → left=4 → admit=13` ⇒ active（**直接覆盖否决方案 A 的反例**）；
4. 并发「踢人 + 退出」两序合并结果一致；
5. 旧规约 `left_seq=0` 时退化为「只有群主权威」的当前语义（回归）。

**集成（应用层）**

6. 群主踢离线成员 → 成员上线 → 收到推送 → 本地状态变 `kicked`；
7. 群主加离线成员 → 该成员上线 → 收到推送 → **本地新建该群且名册正确**（当前完全缺失的能力）；
8. 成员离线自行退出 → 上线 → 其 `left` 不被他人旧快照覆盖；
9. `GROUP_LEAVE` 丢包 + 周期抽查 → 最终一致；
10. 非 active 成员发来的群消息被丢弃**但仍回 ACK**（G-3）；
11. 与离线补投联动：成员退出后不再阻塞该消息「全员送达」。

**回归**：20 人上限、扇出、解散广播、单聊不受影响。

## 十四、实施拆分

逐任务的详细步骤（含测试代码与命令）见 **[`2026-10-11-group-membership-token.md`](2026-10-11-group-membership-token.md)**。概览：

| # | 内容 | 量 |
|---|---|---|
| T1 | 领域：`Member` 票号字段 + `Active()`（严格大于 + 零值兼容回退） | M |
| T2 | 领域：`Group` 群级字段 + `RosterSigningBytes` + `Validate` | M |
| T3 | 领域：`Admit`/`Kick`/`MergeLeft`/`MergeOwnerSnapshot`/`RosterDigest` + 收敛性测试 | L |
| T4 | 迁移 v6 + `sqlite`/`mem` 读写 | M |
| T5 | 协议：两块载荷 + `GROUP_LEAVE 0x34` + 路由 | M |
| T6 | `GroupApp`：`Leave`/`HandleGroupLeave`/`Start`（推送+拉取+抽查）/`Resign`/`HandleGroupMsg` 校验/`ApplyMeta` 改造 | L |
| T7 | 服务层 + DTO + `bootstrap` 装配（含群主自动重签）+ 绑定重生成 | M |
| T8 | 前端：退出群入口、成员票号状态、摘要驱动的静默刷新 | M |
| T9 | e2e：离线入群/被踢/自退跨上线对齐 + 非成员消息 | M |

---

## 附 A：ADR

已按仓库约定（「新协议动作必须先立 ADR」）拆成独立文件：

- [`docs/adr/ADR-012-group-membership-token-crdt.md`](../adr/ADR-012-group-membership-token-crdt.md)

其中 `ADR-003` 被**部分替代**：成员同步改为本设计，**扇出策略 / 群级字段 / 20 人上限 / 群主权威全部保留**。
若日后要执行既有计划里的「发现层两处修正」，那条顺延为 ADR-013。

## 附 B：已确认的决策（2026-10-11）

| # | 问题 | 结论 |
|---|---|---|
| 1 | `Name` / `DisplayName` 是否纳入 `RosterSig` | **`Name` 纳入**（改名是群主权力）；**`DisplayName` 不纳入**（由各成员 `ANNOUNCE` 维护，纳入会让改昵称触发全表重签） |
| 2 | 票号域 | **使用独立的 `int64`**，与 `epoch` 解耦；`epoch` 继续承担发现层目录版本的角色 |
| 3 | 周期抽查间隔 | **默认 60s**（可配）。只在已有群上跑；摘要一致时不落库、不产生事件 |
| 4 | ADR 是否独立成文件 | **是**，见附 A |
