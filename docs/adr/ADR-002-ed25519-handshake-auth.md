# ADR-002：握手引入 Ed25519 三段挑战-应答认证

**Status**：Accepted
**来源**：`架构设计书.md` §4.2

## Context

项目已投入 Ed25519 密钥对做节点身份（P0），但原握手仅明文交换 NodeID。
任何人读取广播包拿到你的 NodeID，就能以你的身份连上第三方。
E2E 加密列 P1，但**没有认证时加密无意义** —— 那只是「和一个自称是张三的陌生人加密聊天」。

## Decision

三段式握手：

```
HELLO      → proto_min/max, caps, nodeID, pubKey, nonce_A, displayName, tcpPort
HELLO_ACK  ← proto_chosen, caps_common, nodeID, pubKey, nonce_B, sig_B
AUTH       → sig_A
AUTH_OK    ←
```

双方对 `H("swarmlink-hello" || nonce_A || nonce_B || pubKey_A || pubKey_B)` 签名，
并校验 `nodeID == Fingerprint(pubKey)`。验签通过方进入 ESTABLISHED；
**握手期不允许业务帧**。

## Consequences

**更好**

- 身份不可冒充；
- 防重放：`nonce_A` 由 A 生成、`nonce_B` 由 B 生成，任一方都无法单方面构造可重放的转录
  （只签自己发的 nonce 是不够的，攻击者可原样重放整个握手）；
- 域分隔哈希前缀防止签名被挪用到其他协议上下文；
- 三段而非两段：`HELLO_ACK` 先给 `sig_B`，让 A 立刻能拒绝冒充者。

**更差**

- 握手增加 1 个 RTT（LAN < 1 ms，可忽略）；
- 必须维护「握手未完成即拒绝业务帧」的状态机，否则会出现竞态。

## 加密演进（v1.0 取方案 C）

v1.0 只做认证，并在协议里留好接缝：`Flags.ENCRYPTED` + `KEY_EXCHANGE(0x40)`。
v1.1 切换 `flynn/noise` 时只加一个适配器，不改协议形状。

理由：威胁模型是内网可路由环境，同网段攻击者本就能嗅探，前向保密价值有限；
而 Noise 会引入新的调试面（握手失败难定位）。先把认证做对，符合「可逆性优先」。

## 不采纳的替代

- **直接上 Noise**：推迟 v1.0 交付、调试成本高；
- **不做认证**：身份体系失去意义，E2E 加密也失去前提。
