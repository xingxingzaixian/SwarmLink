# SwarmLink

无中心内网 P2P 聊天与文件分享工具。所有节点对等，**不依赖任何中心服务器或引导节点**。

- **技术栈**：Go + Wails v3 + Vue3 + Pinia + SQLite（纯 Go 驱动，无 CGO）
- **权威设计**：[`架构设计书.md`](架构设计书.md)（v1.2）；本仓库是该设计的实现
- **决策记录**：[`docs/adr/`](docs/adr/README.md)（11 条，多数带可执行验收测试）
- **实现规格**：[`docs/superpowers/specs/2026-09-20-swarmlink-v1-design.md`](docs/superpowers/specs/2026-09-20-swarmlink-v1-design.md)

---

## 1. 架构一览

```
L4 入站适配器   adapters/wails（5 个服务 + 事件桥）              ──┐
L3 应用层       app/{chat,transfer,discovery,peer,group}_app      ──┤ 只能向下调用
L2 领域核心     domain/{identity,peer,message,group,transfer,protocol,ports}（零 I/O）
L1 出站适配器   adapters/{net/{tcp,udp},store/{sqlite,mem},config,filesink,keystore}
横切            infra/{eventbus,clock,log,fsutil}
组合根          internal/bootstrap（唯一允许 new 具体适配器的地方）
```

**三条红线由 CI 强制**（`make lint-arch`），而不是靠自觉：

| 红线 | 内容 |
|---|---|
| R1 | `domain/**` 禁止 import `net` / `database/sql` / `os` / Wails |
| R2 | 适配器之间禁止互引；跨模块只走 `ports` 接口或事件总线 |
| R3 | 业务代码（`app` / `domain`）禁止 `new` 具体适配器 |

---

## 2. 快速开始

### 2.1 构建与测试

```bash
make build              # 编译后端
make test               # 全部测试
make lint-arch          # 架构红线
make test-e2e           # 端到端（含跨网段收敛）
make test-scale         # 20 节点规模验证
make test-fuzz          # 协议畸形输入
make ci                 # 等价于 CI 的一条命令
```

### 2.2 命令行前端（M1 交付物，也是最强调试工具）

```bash
make cli

# 终端 A
./bin/swarmlink-cli --name alice

# 终端 B（同网段可直接被发现；跨网段用 --seed 指向 A）
./bin/swarmlink-cli --name bob --seed 127.0.0.1:2425

# 命令：/peers /msg /send /history /group /seeds /diag /self-check
```

`--mem` 可完全内存运行（试验用）；不加则落盘 SQLite。

### 2.3 部署前置条件自检（**部署前必做**）

```bash
./bin/swarmlink-cli --self-check
```

它会探通每个种子、拉取目录，并报告 P-1 / P-2 是否成立（见第 4 节）。

### 2.4 桌面 GUI

```bash
# 1) 安装 wails3 CLI（见 Wails v3 官方文档）
# 2) 拉取框架依赖
go get github.com/wailsapp/wails/v3@v3.0.0-beta.23
# 3) 构建
wails3 build -tags wails
```

> GUI 装配被隔离在 `cmd/swarmlink-gui/main_wails.go`（build tag `wails`），
> 是**唯一**接触 Wails API 的文件。服务层与事件桥（`internal/adapters/wails`）
> 是零 Wails 依赖的纯 Go，因此 `go build ./...` 与 `go test ./...` 无需 Wails 工具链。

前端可独立开发与构建（不依赖 Go）：

```bash
cd frontend && npm install && npm run dev     # 无后端时自动进入降级模式
cd frontend && npm run typecheck && npm run build
```

---

## 3. 功能范围（v1.0）

| 能力 | 状态 |
|---|---|
| 节点自动发现（网段内 UDP 广播 + 签名 ANNOUNCE + 多网卡策略） | ✅ |
| 跨网段发现（每网段 ≥2 种子 + 单跳目录拉取，零特权零中继） | ✅ |
| 节点身份（Ed25519 持久化 + 三段握手认证 + 防重放） | ✅ |
| 单聊（`msg_id` + ACK + outbox 重发 + 幂等去重 + 送达状态） | ✅ |
| 群聊（全互联单播扇出，≤ 20 人，epoch 签名成员同步） | ✅ |
| 单文件断点续传（滑动窗口 + 周期性全量位图 + 坏块定位重传） | ✅ |
| 聊天记录（SQLite + WAL + 版本化迁移 + 单写队列） | ✅ |
| 桌面 GUI（Wails v3 + Vue3，含**调试面板**） | ✅ 代码就绪，需 Wails 工具链构建 |

**明确不做**：NAT 穿透 / 中心服务器 / 引导节点 / 文件夹传输 / **中继（永久不做）** /
跨设备身份合并 / 端到端加密（v1.1，接缝已留）。

---

## 4. 部署前置条件（**代码救不了，必须在部署前确认**）

| # | 前置条件 | 不满足的后果 | 自检 |
|---|---|---|---|
| **P-1** | 任意两网段的种子 IP 之间**可 TCP 直连**（三层路由可达，非 NAT 隔离） | 跨网段发现与消息全部失效，此时只能引入中继 —— 而中继是本项目刻意回避的设计 | `swarmlink-cli --self-check` |
| **P-2** | 各网段使用**不重叠的地址段**（不得都是 `192.168.1.0/24`） | 目录中出现 node_id 不同、IP 相同的条目 → **跨网段寻址崩溃，且现象隐蔽、极难排查** | `ANNOUNCE` 携带 `subnet`，CLI 与 GUI 均展示 |

> P-2 不是理论风险：「多个厂区/门店各自一个 `192.168.1.0/24`，再靠 VPN 互联」
> 是内网组网里最常见的形态之一。

**种子配置硬约束**：被选为种子的机器必须**固定 `udp_port`**（关闭端口自动回退），
否则跨网段节点拿着配置里的端口发第一个报文就石沉大海。
`tcp_port` 仍可回退 —— 对端从种子回传的 ANNOUNCE 学到实际端口。

另需在防火墙放行 TCP/UDP 端口，并在 VPN 场景把虚拟网卡加入 `deny_interfaces`（默认已含常见项）。

---

## 5. 配置

配置文件：`<用户配置目录>/SwarmLink/config.toml`（首次启动自动生成）。
数据库：`<用户配置目录>/SwarmLink/data/app.db`。

| 段 | 关键项 |
|---|---|
| `[general]` | `display_name` |
| `[download]` | `default_dir`、`auto_open`（接收不弹框，同名自动加 `(1)`） |
| `[network]` | `tcp_port=0`/`udp_port=0`（0 = 自动）、`port_fallback_range`、`interface_mode`、`allow/deny_interfaces` |
| `[discovery]` | `announce_interval`、`peer_ttl`、`max_peers`、`max_peer_list_size` |
| `[discovery.seeds]` | `refresh_interval`、`seeds_per_refresh=2`、`list`（格式 `IP:UDP_PORT`） |
| `[connection]` | `idle_conn_timeout`、`max_dial_concurrency`、`max_active_conns` |
| `[transfer]` | `max_concurrent`、`chunk_size`、`window_size`、`bitmap_flush_interval` |
| `[security]` | `require_auth=true`（**强烈建议保持开启**）、`encryption`（v1.1） |

---

## 6. 目录结构

```
internal/
  domain/        纯领域（零 I/O）: identity peer message group transfer protocol ports
  app/           用例编排: peer/chat/group/transfer/discovery App + Router
  adapters/      net/{tcp,udp} store/{sqlite,mem} config filesink keystore wails
  infra/         eventbus clock log fsutil
  bootstrap/     ★ 唯一组合根（CLI 与 GUI 共用装配路径）
cmd/
  swarmlink-cli/ 命令行前端 + P-1/P-2 自检
  swarmlink-gui/ Wails 桌面外壳（wails tag 隔离）
test/
  harness/       单进程多节点装配器
  e2e/           发现/握手/单聊/幂等/传输/续传/跨网段收敛
  scale/         20 节点全量目录 + 零常驻连接
frontend/        Vue3 + Pinia + TS（可独立构建）
docs/adr/        11 条架构决策记录
```

---

## 7. 测试矩阵

| 层级 | 范围 | 命令 | 目标 |
|---|---|---|---|
| 单元 | `domain/**` | `make test-domain-cover` | 覆盖率 ≥ 80% |
| 组件 | 协议编解码 | `make test-fuzz` | 无 panic |
| 组件 | SQLite 迁移/仓储 | `go test ./internal/adapters/store/sqlite/` | 迁移可重入 |
| 集成 | 2 节点握手 / 会话管理 | `go test ./internal/adapters/net/...` | 主链路通过 |
| E2E | 3~4 节点全链路 | `make test-e2e` | 通过 |
| E2E | **跨网段收敛** | `make test-e2e` | 通过（ADR-011 的验收物） |
| 规模 | 20 节点 | `make test-scale` | 目录同步正确 + 常驻连接 0 |
| 前端 | 类型 + 构建 | `make frontend-typecheck frontend-build` | 通过 |

`FakeClock` 是必需品：心跳超时（30 s）、TTL（300 s）、空闲回收（5 min）
若用真实时间测试，一个用例要跑几分钟；有了 `Clock` 接口全部变成微秒级。

---

## 8. 已知限制与后续

**当前限制**

- 原生文件选择对话框未接入 Wails 侧，传输页用路径输入框代替；
- 空闲回收后的「连接不再可用」不触发自动重连，下次发消息时才按需拨号（符合 ADR-009，但首条消息有 +1 RTT）；
- 群聊离线成员不暂存消息（诚实取舍，UI 明确标注）；
- 未接入混沌测试（随机杀连接）的自动化 CI 任务。

**v1.1 计划（接缝已留）**

- Noise 加密通道（`Flags.ENCRYPTED` + `KEY_EXCHANGE` 已预留）；
- 群聊上限提至 50 人（扇出改并行 + 连接复用）；
- 节点指纹校验 UI（首次连接让用户确认，防 MITM）。

**v1.2 / v2.0**：并发传输与带宽预算、目录传输、`sdk/` 插件接口（**无中继**）。

---

## 9. 相关文档

- [`架构设计书.md`](架构设计书.md) —— 架构评审 + 目标架构 + 协议 v2 + 数据模型 v2 + ADR + 演进路线图
- [`设计书.md`](设计书.md) —— 前置技术调研与原始方案
- [`docs/adr/README.md`](docs/adr/README.md) —— ADR 索引 + 实现偏差汇总
- [`docs/superpowers/specs/`](docs/superpowers/specs/) —— 实现规格
- [`docs/superpowers/plans/`](docs/superpowers/plans/) —— 实现计划
