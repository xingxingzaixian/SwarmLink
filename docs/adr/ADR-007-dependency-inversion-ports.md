# ADR-007：依赖倒置 —— domain 零 I/O，ports 集中定义

**Status**：Accepted
**来源**：`架构设计书.md` §3.4 / §3.2

## Context

原设计中 transfer 状态机直接依赖 SQLite，导致无法单测；
`Store` 是上帝对象（chat / transfer / seed 全塞在一起）。

## Decision

- `domain/ports` **独立包**集中定义全部接口；
- `adapters/store/sqlite` 与 `adapters/store/mem` 是**对等实现**；
- 引入 `Clock` 接口，使心跳超时、TTL 过期、重传退避全部可测；
- 三条红线由 CI 强制（`make lint-arch`）：
  - **R1** `domain/**` 禁止 import `net` / `database/sql` / `os` / Wails；
  - **R2** 适配器之间禁止互引，跨模块只走 ports 或事件；
  - **R3** 业务代码禁止 `new` 具体适配器。

## Consequences

**更好**

- 状态机可脱离 I/O 测试；
- `test/e2e` 用内存适配器 + 真实 loopback TCP/UDP 在单进程跑多节点；
- 时间相关逻辑确定性测试（`infra/clock.Fake`，微秒级而非分钟级）。

**更差**

- 接口数量增加；需要防止「为每个方法建一个接口」的过度设计。

## 实现注记（本仓库的两处必要调整）

1. **密钥文件 I/O 下沉到 `adapters/keystore`。**
   `domain/identity` 保持纯值类型（KeyPair / NodeID / Sign / Verify / Fingerprint /
   PEM 编解码），文件读写由 `adapters/keystore.LoadOrCreate` 承担 ——
   否则 R1（domain 禁止 import os）无法成立。

2. **`Clock` / `Ticker` 接口定义在 `ports`，实现放在 `infra/clock`。**
   这样依赖方向是 `infra → ports`（向内），而不是 `domain → infra`。
   注意 `ports.Ticker.C()` 是**方法**，而标准库 `time.Ticker.C` 是**字段** ——
   混用会编译失败。

## 测试红利（可直接验证）

- `test/scale.TestTwentyNodesHaveFullDirectoryAndNoResidentConnections`：
  20 节点全量目录同步 + **常驻连接数为 0**（ADR-009 / ADR-010 的验证物）；
- `internal/adapters/net/tcp.TestIdleTimeoutReclaimsSession`：
  用 `FakeClock` 推进 200 ms 完成「5 分钟空闲回收」的验证。
