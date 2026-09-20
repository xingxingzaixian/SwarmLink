# ADR-001：采用六边形架构 + 事件驱动核心

**Status**：Accepted
**来源**：`架构设计书.md` §3.1 / §3.2

## Context

原设计按技术分层（service / transport / store）平铺，`transport.Manager.onChatReceived()`
直接调用 `store.SaveMessage()` —— 把存储层焊进了网络层。这个方向一旦固定，
后续每加一个功能（群聊、加密、历史检索、统计）都会继续往网线上挂逻辑，
且状态机无法在没有网络和数据库的情况下测试。

## Decision

采用四层结构（入站适配器 / 应用层 / 领域核心 / 出站适配器）+

- `domain/**` 零外部 I/O 依赖；
- 跨模块通信只走 `domain/ports` 接口或 `infra/eventbus`；
- `internal/bootstrap` 是唯一组合根（所有 `new` 具体适配器都在那里）。

## Consequences

**更好**

- `domain/transfer` 状态机可 100% 单测（本项目最需要测试的部分）；
- 换存储 / 换传输不动业务代码：`mem` 与 `sqlite` 是对等实现，`test/e2e`
  用内存适配器在单进程跑多节点；
- 事件总线让「调试面板」几乎零成本（见 ADR-001 的副产品）。

**更差**

- 多约 20% 胶水代码（接口定义 + 依赖注入）；
- 事件驱动让调用链在 IDE 里不能直接跳转，靠「主题常量 + 订阅点」导航；
- 需要团队纪律：一旦有人在 transport 里直接 import store，整套约束就退化。
  因此红线由 CI 强制（`make lint-arch`）。

## 实现注记（本仓库的偏差）

架构书把帧编解码放在 `adapters/net/protocol`，但 `domain/ports.ConnManager`
需要引用 `protocol.Frame`。若将其留在 adapters 下，会让 domain 反向依赖 adapter。
由于帧编解码是**纯函数零 I/O**，本仓库把它放在 `internal/domain/protocol`，
使 ports 与 adapters 都向内依赖它。

## 不采纳的替代

- **微服务**：本项目是单机进程，共享内存、生命周期一致。拆进程只会把函数调用
  变成 IPC、把编译期错误变成运行时错误；
- **纯分层**：无法解决测试与横穿依赖两个问题。
