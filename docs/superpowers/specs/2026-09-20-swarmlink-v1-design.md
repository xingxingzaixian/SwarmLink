# SwarmLink v1.0 — 实现规格（Implementation Spec）

**日期**：2026-09-20
**状态**：Approved（用户已确认范围与三项分叉）
**权威设计来源**：`架构设计书.md` v1.2（下称「架构书」）
**输入方案**：`设计书.md` v1.0

---

## 1. 目标与非目标

### 1.1 目标（v1.0 交付物）

无中心内网 P2P 聊天 + 单文件分享工具，覆盖架构书 M1–M4：

- 单聊（可靠投递、送达状态、历史记录）
- 群聊（全互联单播扇出，硬上限 20 人）
- 单文件断点续传（滑动窗口 + 周期性全量位图）
- 跨网段发现（网段内广播 + 网段间种子单播拉取）
- 节点身份认证（Ed25519 三段式握手）
- Wails v3 桌面壳 + Vue3 前端（含调试面板）

### 1.2 明确不做（Non-Goals）

NAT 穿透 / 公网打洞、中心服务器 / 云账号、引导节点 / 种子分发服务、文件夹传输、**中继与转发（永久不做）**、跨设备身份合并、Prometheus/OpenTelemetry。

---

## 2. 已确认决策

| 编号 | 决策 | 内容 |
|---|---|---|
| 范围 | 全量 v1.0 | 一次性实现 M1–M4（后端四层 + 协议 + 传输 + 群聊 + 可靠性 + Wails + Vue3） |
| 9.1 | **C** | v1.0 手写 Ed25519 挑战-应答认证；保留 `Flags.ENCRYPTED` + `KEY_EXCHANGE(0x40)` 接缝，v1.1 切 Noise |
| 9.2 | **A** | v1.0 就上群聊，全互联单播扇出，硬上限 20 人，不做离线暂存 |
| 9.3 | **B** | v1.0 直接上滑动窗口 + 周期性全量位图（ADR-005） |
| 规模 | 已确认 | 全局 ≤ 200 节点、3–5 网段、种子手动维护 |
| 拓扑 | 已确认 | 在线状态走 UDP announce；TCP 按需拨号 + 空闲回收（ADR-009） |

### 2.1 环境偏差（记录）

架构书要求 Go 1.23+，实际环境为 **Go 1.21.8**。本项目所有语言特性在 1.21 下均可用（`log/slog`、泛型、`slices`/`maps`、`min`/`max` 内建）。`go.mod` 定为 `go 1.21`。若后续需要 1.22+ 特性（如 loopvar 语义），作为独立任务评估。

---

## 3. 架构（照搬架构书 ADR-001）

### 3.1 四层与依赖方向

```
L4 入站适配器   adapters/wails（5 Service + eventbridge）        ──┐
L3 应用层       app/{chat,transfer,discovery,peer,group}_app      ──┤ 只能向下
L2 领域核心     domain/{identity,peer,message,group,transfer,ports}（零 I/O）
L1 出站适配器   adapters/{net/{protocol,tcp,udp},store/{sqlite,mem},config}
横切            infra/{eventbus,clock,log,fsutil}
组合根          main.go（唯一允许 new 具体适配器处）
```

### 3.2 三条依赖红线（CI 强制，见 `make lint-arch`）

- **R1**：`domain/**` 禁止 import `net`、`database/sql`、`os`、Wails（`io`/`time` 允许）
- **R2**：跨模块只走 `ports` 接口或事件总线；适配器之间禁止互引
- **R3**：业务代码（`app`/`domain`）禁止 `new` 具体适配器

### 3.3 端口接口（`domain/ports`）

`PeerDirectory`、`MessageRepo`、`TransferRepo`、`ConnManager`/`Session`、`DiscoveryStrategy`、`SeedRegistry`、`FileSink`、`EventBus`、`Clock`。签名以架构书 3.4 节为准。

---

## 4. 协议 v2 要点

- **TCP 帧头**：`Magic(0x53 0x4C) | Ver(0x02) | Flags | Type | Length(4B BE) | Payload`，固定 9 字节；`Length` 上限 16 MB；未知 Type 按 Length 跳过后丢弃，不断连；未知 Flags 位忽略。
- **UDP 独立编号空间**：4 字节魔数 `0x53 0x4C 0x41 0x4E`（"SLAN"）+ 独立 Type；`ANNOUNCE`/`PEER_LIST_REQ`/`PEER_LIST_RESP`/`SEED_PROBE` 承载于 UDP。**两套魔数与编号空间不得混用。**
- **握手**：三段 `HELLO / HELLO_ACK / AUTH`（+ `AUTH_OK`）。双方对 `H("swarmlink-hello" || nonce_A || nonce_B || pubKey_A || pubKey_B)` 签名；`nodeID == Fingerprint(pubKey)` 校验；握手期禁止业务帧。
- **NodeID**：`SHA-256("swarmlink-node-id-v1" || pubkey)[:8]`，hex 编码（16 字符）。
- **协商**：`proto_min/proto_max`、8 位 `caps`（CHAT_RELIABLE/GROUP_CHAT/RESUME_BITMAP/SLIDING_WINDOW/COMPRESSION/ENCRYPTED/FOLDER_TRANSFER/RELAY）。
- **消息类型**：0x01–0x21 保留 + 0x05–0x08、0x22 CHAT_ACK、0x30–0x33 GROUP_*、0x40–0x41 KEY_EXCHANGE*。

---

## 5. 数据模型 v2

按架构书第 5 章 SQL 落地：`schema_migrations`、`conversations(conv_id PK,kind)`、`messages(UNIQUE msg_id, state)`、`outbox`、`groups`、`group_members`、`peers`、`transfer_jobs(+chunk_size/total_chunks/window_size)`、`seed_nodes(addr PK)`。DSN 含 `_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_foreign_keys=on`；`SetMaxOpenConns(5)`；配一个**单写 goroutine 写队列**。

---

## 6. 关键机制

| 机制 | 规则 |
|---|---|
| 消息可靠性 | UUIDv7 `msg_id`；发送写 `messages(pending)` + `outbox`（单事务）；收 `CHAT_ACK` 后置 delivered 并删 outbox；退避 1s→2s→…→5min；`peer.online` 触发 flush；接收方 `AppendIfAbsent` 幂等，**重复消息也必须回 ACK** |
| 群聊 | `Group.ID = SHA-256(ownerID‖createdAt‖name)[:16]`；成员变更 `Epoch++` + owner 签名 `H(group_id|epoch|members)`；扇出并发上限 8；owner 离线 > 24h → 群只读 |
| 文件传输 | 窗口 W（初始 8，clamp(BDP/chunk,1,64)）；接收方每 500ms 或每 32 块回**全量位图**；位图每 2s 或每 256 块批量落盘；`FILE_META` 始终携带全量 `chunk_hashes`；`WriteAt(offset)` 乱序写；`.swarmlink.<job_id>.part` 临时文件；`os.Rename` 原子替换；坏块定位后仅重传坏块 |
| G3 路径清洗 | `filepath.Base` + 拒绝 空/`.`/`..`/含 `/\`及 `\x00` + Windows 保留名 + UTF-8 边界截断 255B + `Clean` 后前缀校验；收敛在 `FileSink` 一个实现里 |
| 连接拓扑 | 双向拨号胜负：`self.NodeID < peer.NodeID` 方负责拨号；HELLO 后检查同 nodeID 已有 ESTABLISHED，按同规则保留胜者、**关闭负者前不发任何业务帧** |
| 发现 | 广播 5s ± 20% 抖动；签名 ANNOUNCE（含 `subnet`）；TTL 300s，`last_seen` 为**本地到达时间**；`PEER_LIST_REQ.list_epoch` 相同则空响应；单响应 ≤256 条，本地 ≤512 条 LRU |
| 多网卡 | 排除 loopback/link-local/未 UP/虚拟网卡；按默认路由优先级排序；每接口独立子网广播地址 + 绑定；`allow_interfaces`/`deny_interfaces` 覆盖；三档模式 auto/manual/seed_only |
| 种子 | 只是「值得先问一声的地址」；UDP 探测 → 种子单播回 ANNOUNCE（含签名）→ 学得 tcp_port/node_id → TCP `PEER_LIST_REQ/RESP`；种子须固定 `udp_port`；每轮随机拉 2 颗；条目按 `node_id` 去重，TTL 用本地到达时间 |
| 端口回退 | TCP/UDP 2425 被占则试 2426…2434；实际端口写入 ANNOUNCE 供对端学习；种子机器必须关闭回退 |

---

## 7. 事件与主题

`peer.discovered`、`peer.online`、`peer.offline`、`chat.received`、`chat.delivered`、`group.updated`、`transfer.state`、`transfer.progress`（**节流 4 Hz**）、`transfer.done`、`transfer.error`、`net.error`、`config.changed`。事件总线为**同步**分发；需要返回值/事务的路径由 App 显式编排，事件只做通知。

---

## 8. 质量目标

| 属性 | 目标 |
|---|---|
| 可维护性 | 新增一种消息类型 ≤ 3 个文件改动 |
| 可测试性 | `domain/**` 单测覆盖率 ≥ 80%，无需真实网络 |
| 性能 | 千兆 LAN 单文件 ≥ 80 MB/s |
| 可靠性 | 消息 at-least-once + 幂等去重；断点恢复精度 = chunk |
| 安全 | 身份不可冒充；接收文件不逃逸目标目录 |
| 规模 | 200 节点目录同步正确；常驻连接 ≤ `max_active_conns`；无保活流量 |

---

## 9. 构建顺序（S0–S11）

| 阶段 | 内容 |
|---|---|
| S0 | 地基：`infra/{clock,log,eventbus}`、`domain/ports`、`protocol/const.go` |
| S1 | 领域：identity、peer、message、group、transfer（位图/状态机/窗口）+ 单测 |
| S2 | 协议：帧编解码 v2 + fuzz |
| S3 | 网络：tcp（握手/按需拨号/保活/空闲回收）、udp（广播/多网卡/签名 ANNOUNCE/seed） |
| S4 | 存储：sqlite + migrations + 单写 goroutine；mem 对等；FileSink |
| S5 | 应用：5 个 App 编排 + outbox 扫描器 + 滑动窗口 + 群扇出 |
| S6 | 可靠性：ACK/去重/群 epoch/端口回退 |
| S7 | 测试：e2e（含跨网段收敛）、scale（20~200）、chaos |
| S8 | CLI：`cmd/swarmlink-cli` |
| S9 | Wails 适配器 + 事件桥 + 节流 |
| S10 | Vue3 + Pinia 前端 + 调试面板 |
| S11 | CI `lint-arch`、ADR 落文件、用户文档（含 P-1/P-2 自检） |

---

## 10. 验收标准（v1.0）

1. `go test ./...` 全绿；`domain/**` 覆盖率 ≥ 80%
2. `test/e2e`：3 节点发现 + 握手 + 消息往返；**跨网段用例（两个隔离 UDP 域 + 共享种子，目录双向收敛）通过**
3. `test/scale`：20 节点常驻连接数 ≤ `max_active_conns`，无保活流量
4. `cmd/swarmlink-cli` 两节点互传 10 GB 文件，中途 `kill -9` 发送方 → 重启续传成功、哈希一致
5. CI `lint-arch` 通过（R1/R2/R3 无违例）
6. GUI 双机互传 + 聊天无卡顿

---

## 11. 部署前置条件（写进用户文档）

- **P-1** 任意两网段种子 IP 间可 TCP 直连（三层路由可达）——启动时对种子做 TCP 探测，全通即达标
- **P-2** 各网段地址段不重叠——ANNOUNCE 携带 `subnet`，同 subnet 来自两个网段即告警（M1 起即具备）
