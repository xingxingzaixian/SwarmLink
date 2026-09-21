# 架构决策记录（ADR）

> 每一条都对应 `架构设计书.md` 的一节。**Status = Accepted** 表示已落地到代码，
> 且多数带有可执行的验收测试（见每条末尾的「验证」）。

| # | 决策 | 对应章节 | 验收测试 |
|---|---|---|---|
| [ADR-001](ADR-001-hexagonal-event-driven.md) | 六边形架构 + 事件驱动核心 | §3.1 §3.2 | `make lint-arch`（R1/R2/R3） |
| [ADR-002](ADR-002-ed25519-handshake-auth.md) | 握手引入 Ed25519 三段挑战-应答认证 | §4.2 | `tcp.TestHandshakeHappyPath` / `TestInitiatorRejectsImpersonation` |
| [ADR-003](ADR-003-group-chat-fanout.md) | 群聊全互联单播扇出 + epoch 签名 | §4.6 | `group.*` + `e2e` 群用例 |
| [ADR-004](ADR-004-message-reliability.md) | 消息可靠性 `msg_id + ACK + outbox` | §4.7 | `e2e.TestDuplicateChatIsDedupedButStillAcked` |
| [ADR-005](ADR-005-sliding-window-bitmap.md) | 滑动窗口 + 周期性全量位图回传 | §4.8 | `e2e.TestFileTransferResumesAfterInterruption` |
| [ADR-006](ADR-006-frame-v2-capability-negotiation.md) | 帧头 v2 + 能力协商 | §4.1 §4.3 §4.4 | `protocol` 表驱动 + `FuzzRead` |
| [ADR-007](ADR-007-dependency-inversion-ports.md) | 依赖倒置：domain 零 I/O，ports 集中定义 | §3.4 | `test/scale` 用内存适配器跑 20 节点 |
| [ADR-008](ADR-008-port-and-interface-policy.md) | 端口与网卡策略交给用户显式选择 | §4.5 §7.4 | `udp` 接口策略 6 项测试 |
| [ADR-009](ADR-009-online-state-vs-connections.md) | 在线状态与传输连接解耦 | §3.7 | `test/scale` 常驻连接数 = 0 |
| [ADR-010](ADR-010-full-directory-sync.md) | 全量节点目录同步，不做增量 | §4.5 | `test/scale` 目录 19/19 条 |
| [ADR-011](ADR-011-cross-subnet-seeds.md) | 每网段多种子 + 单跳目录拉取 | §4.5.1 | `e2e.TestCrossSubnetConvergenceViaSeed` |

## 本仓库相对架构书的实现偏差

以下三处是**为实现约束（CI 红线、Go 包依赖方向）不得不做的调整**，均已记录在对应 ADR 中：

1. **`protocol` 包位置**：从 `adapters/net/protocol` 移到 `internal/domain/protocol`。
   原因：`domain/ports.ConnManager` 需要 `protocol.Frame`，若留在 adapters 下会让
   domain 反向依赖 adapter，违反「依赖单向向内」。帧编解码是纯函数零 I/O，放 domain 合法。
   （见 ADR-001）

2. **密钥文件 I/O 下沉**：`domain/identity` 只保留纯值类型，文件读写移到
   `adapters/keystore`。原因：红线 R1 禁止 `domain/**` import `os`。（见 ADR-007）

3. **UDP 接口绑定用可移植等价实现**：以「发送 socket 源地址绑定到接口 IP」替代
   `SO_BINDTODEVICE` / `IP_BOUND_IF` 的平台特定代码。（见 ADR-008）

## 未采纳的替代（永久移除）

| 原列入的风险 | 移除原因 |
|---|---|
| 分层 / 分区发现协议 | 200 节点用广播 + 全量目录交换即可覆盖 |
| 软中继 / 弱中心 | 群上限 = 全局节点数 = 200，Gossip 即闭环 |
| 目录增量同步 / 分片拉取 | 全量目录仅 12.8 KB |
| 种子分发机制 / 只读引导节点 | 3–5 网段手工配 6–10 条 IP 即可闭环 |
| 跨网段中继 / 转发节点 | P-1 成立时直接按需拨号，**零跳数** |

## 工程化注记（不属于架构决策，但影响日常操作）

1. **Wails 入口在仓库根目录**（`main_wails.go`，build tag `wails`）。
   两个原因叠加：wails3 的构建任务在根目录执行**不带包路径**的 `go build`；
   而架构书 §3.1 第 4 点本来就把 `main.go` 定义为唯一组合根。
   未启用该 tag 时根目录没有可编译文件，`go build ./...` / `go test ./...`
   会跳过它，因此在没有 Wails 工具链的环境下依然可用；
   反过来，`wails3 build` 忘传 tag 会【直接失败】，不会再悄悄产出占位程序。

2. **`frontend/bindings/` 提交入库**。它是 `wails3 generate bindings` 的产物，
   入库后前端可脱离 Go 工具链构建（CI 的 frontend job 依赖这一点）。
   改动服务导出方法后重新执行 `make bindings`。

3. **`build/` 目录提交入库**（wails3 的平台构建配置与图标资源），
   因此 `.gitignore` **不**忽略它；被忽略的是 `.task/`（Task 运行器缓存）与 `bin/`。

4. **Go 版本下限实际是 1.25**，由 Wails v3 beta.23 的 `go.mod` 决定
   （架构书写的是 1.23+，该约束已被依赖覆盖）。

5. **wails3 不使用 `wails.json`**（那是 v2 的配置）。v3 的配置在 `build/config.yml`。

