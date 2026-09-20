# ADR-006：协议帧头一次性扩展为 Magic/Ver/Flags/Type/Length + 能力协商

**Status**：Accepted
**来源**：`架构设计书.md` §4.1 / §4.3 / §4.4

## Context

原帧头 `Magic(2) | Ver(1) | Type(1) | Length(4)` 没有扩展位：
后续加加密、压缩、分片都必须改版本号，而版本号一改就要做兼容分支。
同时协议无版本协商，加字段即破坏兼容。

## Decision

帧头一次性扩到固定 9 字节：

```
┌──────┬─────┬───────┬──────┬────────┬──────────┐
│Magic │ Ver │ Flags │ Type │ Length │ Payload  │
│ 2 B  │ 1 B │  1 B  │ 1 B  │  4 B   │  变长    │
│SL    │ 02  │位掩码 │      │ 大端   │          │
└──────┴─────┴───────┴──────┴────────┴──────────┘
```

- `Flags`：bit0 ENCRYPTED、bit1 COMPRESSED、bit2 FRAGMENT、bit3 FRAG_END、bit4-7 保留；
- `Length` 上限 16 MB（`max_frame_size`），超过即协议违规 → 关连接（防内存炸弹）；
- HELLO 协商 `proto_min/proto_max` + 8 位 `caps`；
- **两套独立的魔数与编号空间**：TCP `0x53 0x4C`（"SL"）+ 0x00–0xFF 类型区间；
  UDP `0x53 0x4C 0x41 0x4E`（"SLAN"）+ 独立 Type。

## 三条兼容规则（写进协议，避免以后吵架）

1. 接收方**必须先按 `Length` 读完载荷再判断能否处理**，未知 Type 直接丢弃并计数，**不得断连**；
2. 未知 Flags 位**忽略**而非报错；
3. 版本不匹配不在帧层拒绝，由 HELLO 阶段的协商提前解决。

## Consequences

**更好**

- 后续加加密（`Flags.ENCRYPTED`）、压缩、分片零协议破坏；
- 新老节点混网可用（能力降级到 `caps_common`）。

**更差**

- 头部从 8 B 增至 9 B（可忽略）；
- 所有解析代码必须严格遵守「先读长度、未知即跳过」。

## UDP 为什么需要独立魔数

UDP 报文可能落到任意一个 UDP 服务上，它必须能被**不认识它的服务**以最快速度丢弃；
TCP 帧不需要这层保护（连接已建立，对端必是本协议）。
**两套编号空间不得混用** —— 否则调整 UDP 魔数会牵动 TCP 的版本兼容。

## 协议 ABI 常量

`internal/domain/protocol/const.go` 集中定义全部常量，并在注释里标注
「这两个值属于协议 ABI，改即破坏兼容」。注意**集中 ≠ 相同**：
UDP 与 TCP 各用一套，`TestBytesConstantsAreStable` 锁定这些值。

## 验证

- 表驱动编解码测试：往返、未知 Type 返回而不报错、超大 Length 拒绝、未知 Flags 保留；
- `FuzzRead` 对畸形输入无 panic（CI 短跑 15 s，本地实测 50 万次执行无 panic）。
