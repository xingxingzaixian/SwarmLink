# SwarmLink — 项目长期约定

**项目**：无中心内网 P2P 聊天与文件分享工具（类飞秋/内网通/飞鸽传书）
**技术栈**：Go 1.23+ · Wails v3.0.0-beta.23（锁定版本）· Vue3 + Pinia · modernc.org/sqlite
**命名**：项目名 `SwarmLink`；设计文档中的占位名 `MyApp` / `myapp` 一律替换
**文档**：`设计书.md`（技术方案 v1.0，用户侧）· `架构设计书.md`（架构评审与目标架构 v1.2，本助手产出）

## 硬约束（不要重新讨论，除非用户明确变更）

| 约束 | 值 |
|---|---|
| **全局节点数** | **≤ 200**（已确认 2026-09-20） |
| **网段数** | **3–5 个，种子清单手动维护**（已确认 2026-09-20；不考虑运维负担） |
| 网络环境 | 可路由内网，可 ping 通；**不做 NAT 穿透** |
| 中心节点 | **不存在**。**不设引导节点、不设中继** —— 3–5 网段手工配 6–10 条种子 IP 即闭环（ADR-011） |
| 节点间连接 | **不建立全互联持久连接**（ADR-009） |
| v1.0 文件传输 | 仅单文件，不支持文件夹 |

### 部署前置条件（写进安装文档；不满足则跨网段方案失效）

- **P-1**：任意两网段的种子 IP 之间**可 TCP 直连**（三层可达，非 NAT 隔离）。不满足 → 只能引入中继（架构级返工）
- **P-2**：各网段**地址段不得重叠**（不能都是 `192.168.1.0/24`）。自检靠 ANNOUNCE 的 `subnet` 字段，**M1 就要加**

## 架构约定

- **风格**：六边形架构（Ports & Adapters）+ 进程内同步事件驱动核心
- **四层**：入站适配器（`adapters/wails`）→ 应用层（`app/`）→ 领域核心（`domain/`，零 I/O）→ 出站适配器（`adapters/`）；横切在 `infra/`
- **三条红线**（CI 强制）：
  - R1 `domain/**` 禁止 import `net` / `database/sql` / `os` / Wails
  - R2 跨模块只走 `ports` 接口或事件总线，禁止跨包直接 import 兄弟模块
  - R3 适配器之间禁止互相依赖；`main.go` 是唯一组合根
- **全部接口集中在 `domain/ports`**，`adapters/store/mem` 与 `sqlite` 是对等实现
- 命名规范：`domain/` 纯领域 · `app/` 用例 · `adapters/` I/O · `infra/` 基础设施

## 协议约定（属于协议 ABI，改动即破坏兼容）

- 帧头固定 **9 字节**：`Magic(2) | Ver(1) | Flags(1) | Type(1) | Length(4,大端)`；帧上限 16 MB
- Magic：TCP `0x53 0x4C`("SL")；UDP `0x53 0x4C 0x41 0x4E`("SLAN")；常量集中在 `adapters/net/protocol/const.go`
- Type 空间分段：`0x00–0x0F` 会话控制 · `0x10–0x1F` 文件传输 · `0x20–0x2F` 消息可靠性 · `0x30–0x3F` 群组 · `0x40–0x4F` 安全 · `0x50–0x7F` 预留 · `0x80–0xFF` 插件
- **UDP 发现报文（`ANNOUNCE`/`PEER_LIST_REQ`/`PEER_LIST_RESP`/`SEED_PROBE`）使用独立魔数与独立 Type 编号空间**，不与上表 TCP 帧争用（UDP 需强魔数防误触，TCP 不需要）—— 集中管理 ≠ 编号相同
- **未知 Type 必须按 Length 跳过后丢弃，不得断连**
- NodeID = `SHA-256("swarmlink-node-id-v1" || pubkey)[:8]`（域分隔哈希）
- 握手：三段式 `HELLO / HELLO_ACK / AUTH`，签名覆盖 `H("swarmlink-hello" || nonce_A || nonce_B || pubKey_A || pubKey_B)`
- 拨号胜负规则：`self.NodeID < peer.NodeID` 的一方负责拨号
- **跨网段（ADR-011 / 4.5.1）**：网段内广播 + **网段间种子单播拉取**；每网段 ≥2 颗种子，节点每轮**随机拉 2 颗**；种子**零特权**，与普通节点跑同一套 `PEER_LIST_REQ/RESP`
  - **种子必须固定 UDP 端口**（关掉端口回退）。第一跳 UDP 探测（`SEED_PROBE`）→ 种子单播回签名 ANNOUNCE → 学到 `tcp_port`/`node_id`/`pub_key` → 第二跳 TCP 拉目录
  - **防环无需 `via`/`hop_count`**：① 条目按 `node_id` 去重；② **TTL 用本地到达时间计，绝不用对端 timestamp**（跨网段时钟偏差不影响正确性）

## 数据模型关键约定

- `peers` 含 **`subnet`** 字段（P-2 地址重叠检测）；`peers.last_seen` 语义是**本机收到该条目的时刻**（本地时钟），不是对端时间戳
- `seed_nodes` 主键是 **`addr`**（`ip:udp_port`），`node_id` 可空（探测前未知），另有 `tcp_port` / `last_epoch` / `fail_cnt`
- `conversations` 主键是 **`conv_id`**（不是 `peer_id`）：单聊 = peer_id，群聊 = group_id
- `messages` 有 **`UNIQUE(msg_id)`**（UUIDv7）；**重复消息也必须回 CHAT_ACK**
- 传输位图：**每 500 ms 或每 32 块回传全量位图**（不用累积 ACK/SACK）；内存维护，每 2 s 或每 256 块批量落盘
- 所有 SQLite 写操作走**单写 goroutine**（避免多连接争锁）；DSN 需带 `_foreign_keys=on`
- 接收文件名必须经 `FileSink` 统一清洗（base / 拒绝 `..` 与绝对路径 / Windows 保留名 / 长度 / 前缀校验）

## 明确不做

微服务 · 消息队列 · ORM · Prometheus/OpenTelemetry · **软中继 / 任何形式的中继转发（永久不做）** · 分层发现 · 目录增量同步 · 中心服务器 · **引导节点 / 种子分发服务** · v1.0 文件夹传输

## 工程习惯

- Wails 版本锁死，升级作为独立任务 + 回归测试
- 新依赖需说明"标准库为何不够"；预期依赖白名单见 `架构设计书.md` 7.5
- `FakeClock` 与内存适配器是测试必需品（心跳超时/TTL/退避必须可确定性测试）
- 前端监听 Wails 事件必须节流（进度类 4~10 Hz）
