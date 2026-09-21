# SwarmLink v1.0 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 按 `架构设计书.md` v1.2 实现无中心内网 P2P 聊天与文件分享工具的 v1.0（六边形架构 + 事件驱动核心，含单聊/群聊/断点续传/跨网段发现/Ed25519 认证/Wails+Vue3 前端）。

**Architecture:** 四层——L4 入站适配器（wails）→ L3 应用层（app）→ L2 纯领域核心（domain，零 I/O）→ L1 出站适配器（net/store/config）；横切 `infra/{eventbus,clock,log,fsutil}`；`main.go` 为唯一组合根。跨模块只走 `domain/ports` 接口或事件总线。

**Tech Stack:** Go 1.21（架构书要求 1.23+，见 spec §2.1）、Wails v3.0.0-beta.24、Vue3 + Pinia + TypeScript、`modernc.org/sqlite`（纯 Go）、`google/uuid`（UUIDv7）、`BurntSushi/toml`、`log/slog`。

**Spec:** `docs/superpowers/specs/2026-09-20-swarmlink-v1-design.md`
**权威契约来源:** `架构设计书.md`（协议表 4.4、SQL 第 5 章、端口定义 3.4、主题 3.5）

---

## File Structure

```
main.go                                  # 组合根（Wails 装配 + 启动）
cmd/swarmlink-cli/main.go                # CLI 原型/调试器
internal/
  domain/
    identity/identity.go                 # KeyPair, NodeID, Sign, Verify, Fingerprint
    peer/peer.go                         # Peer 实体, State, SeedAddr
    message/message.go                   # Message, MsgID(UUIDv7), 排序/去重规则
    group/group.go                       # Group 聚合, Member, Epoch, FanoutPlan
    transfer/{job.go,bitmap.go,window.go,state.go}  # Job, ChunkBitmap, StateMachine, Window
    ports/ports.go                       # 全部接口
  app/{chat_app,transfer_app,discovery_app,peer_app,group_app}.go
  adapters/
    net/protocol/{const.go,frame.go,codec.go}
    net/tcp/{session.go,handshake.go,manager.go,lazy.go}
    net/udp/{broadcast.go,iface.go,iface_darwin.go,iface_linux.go,iface_windows.go,seed.go,announce.go}
    store/sqlite/{db.go,migrations.go,writer.go,chat.go,transfer.go,peer.go,group.go,seed.go}
    store/mem/{mem.go}
    config/config.go
    wails/{services.go,eventbridge.go}
  infra/
    eventbus/eventbus.go
    clock/clock.go
    log/log.go
    fsutil/{safename.go,atomic.go}
test/e2e, test/scale, test/chaos
frontend/ (Vue3 + Pinia)
docs/adr/ADR-00X-*.md
Makefile (lint-arch)
```

---

## Phase S0 — 地基

### Task 0.1: 模块与工具链

**Files:** Modify `go.mod`

- [ ] **Step 1:** 设置模块与依赖基线

Run:

```bash
cd /Users/small_bud/Desktop/OpenCode/SwarmLink
go mod edit -go=1.21
go get github.com/google/uuid@latest github.com/BurntSushi/toml@latest
```

Expected: `go.mod` 含上述 require。

- [ ] **Step 2:** 建 `Makefile`（lint-arch 红线条）

```makefile
.PHONY: test lint-arch
test:
	go test ./...
lint-arch:
	@! grep -rn '"net"\|"database/sql"\|"os"\|wails' internal/domain/ --include=*.go | grep -v '_test.go' | grep -v 'domain/ports' || (echo "R1 VIOLATION" && exit 1)
	@! grep -rn 'internal/adapters/store' internal/adapters/net/ --include=*.go || (echo "R2 VIOLATION" && exit 1)
	@! grep -rn 'sqlite.New\|tcp.NewManager' internal/app/ internal/domain/ --include=*.go || (echo "R3 VIOLATION" && exit 1)
	@echo "arch OK"
```

- [ ] **Step 3:** Commit

```bash
git add -A && git commit -m "chore: init module, deps and arch lint"
```

### Task 0.2: infra/clock

**Files:** Create `internal/infra/clock/clock.go`, `internal/infra/clock/clock_test.go`

- [ ] **Step 1:** 写失败测试

```go
package clock

import (
	"testing"
	"time"
)

func TestFakeClockAdvances(t *testing.T) {
	fc := NewFake(time.Unix(0, 0))
	base := fc.Now()
	fc.Advance(5 * time.Second)
	if d := fc.Now().Sub(base); d != 5*time.Second {
		t.Fatalf("want 5s got %v", d)
	}
}
```

- [ ] **Step 2:** 运行确认失败 `go test ./internal/infra/clock/` → FAIL(undefined)
- [ ] **Step 3:** 实现

```go
package clock

import (
	"sync"
	"time"
)

type Ticker interface{ C() <-chan time.Time; Stop() }

type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
}

type realClock struct{}

func New() Clock { return realClock{} }

func (realClock) Now() time.Time { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) NewTicker(d time.Duration) Ticker { return &realTicker{t: time.NewTicker(d)} }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }

// ---- Fake ----

type Fake struct {
	mu    sync.Mutex
	now   time.Time
	waits []*fakeWait
}

type fakeWait struct {
	at time.Time
	ch chan time.Time
}

func NewFake(start time.Time) *Fake { return &Fake{now: start} }

func (f *Fake) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }

func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	var fire []*fakeWait
	rest := f.waits[:0]
	for _, w := range f.waits {
		if !w.at.After(now) {
			fire = append(fire, w)
		} else {
			rest = append(rest, w)
		}
	}
	f.waits = rest
	f.mu.Unlock()
	for _, w := range fire {
		w.ch <- now
	}
}

func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	f.waits = append(f.waits, &fakeWait{at: f.now.Add(d), ch: ch})
	return ch
}

func (f *Fake) NewTicker(d time.Duration) Ticker { return &fakeTicker{c: f.After(d)} }

type fakeTicker struct{ c <-chan time.Time }

func (f *fakeTicker) C() <-chan time.Time { return f.c }
func (f *fakeTicker) Stop()               {}
```

- [ ] **Step 4:** `go test ./internal/infra/clock/` → PASS
- [ ] **Step 5:** Commit `feat(infra): clock with fake`

### Task 0.3: infra/log + infra/eventbus

**Files:** Create `internal/infra/log/log.go`, `internal/infra/eventbus/eventbus.go`, `internal/infra/eventbus/eventbus_test.go`

- [ ] **Step 1:** 事件总线测试（同步分发 + 退订）

```go
package eventbus

import "testing"

func TestPublishSubscribeSync(t *testing.T) {
	b := New()
	var got any
	un := b.Subscribe("t", func(p any) { got = p })
	b.Publish("t", 42)
	if got != 42 { t.Fatalf("want 42 got %v", got) }
	un()
	b.Publish("t", 43)
	if got != 42 { t.Fatalf("unsubscribe failed") }
}
```

- [ ] **Step 2:** 运行确认失败
- [ ] **Step 3:** 实现（mutex + map[string][]func(any)；订阅返回 unsubscribe；Publish 在锁外调用 handler 防重入死锁）

```go
package eventbus

import "sync"

type Bus struct {
	mu   sync.RWMutex
	subs map[string][]func(any)
}

func New() *Bus { return &Bus{subs: make(map[string][]func(any))} }

func (b *Bus) Publish(topic string, payload any) {
	b.mu.RLock()
	hs := append([]func(any)(nil), b.subs[topic]...)
	b.mu.RUnlock()
	for _, h := range hs { h(payload) }
}

func (b *Bus) Subscribe(topic string, fn func(any)) (unsubscribe func()) {
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], fn)
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		hs := b.subs[topic]
		for i, h := range hs {
			if &hs[i] == &fn { continue }
			_ = h
		}
		// 简化：按值比较不可行，改用索引
		_ = hs
	}
}
```

> 实现注意：函数不可比较。用 **slice 元素计数** 或给订阅分配 id。推荐：`type sub struct{ id uint64; fn func(any) }`，`map[string][]sub`，unsubscribe 按 id 删除。

- [ ] **Step 4:** `go test ./internal/infra/...` → PASS
- [ ] **Step 5:** `log.go`：`log/slog` JSON handler，提供 `WithNodeID/WithConn/WithJob` 上下文注入
- [ ] **Step 6:** Commit `feat(infra): eventbus and slog logger`

### Task 0.4: domain/ports

**Files:** Create `internal/domain/ports/ports.go`

- [ ] **Step 1:** 按架构书 3.4 节**逐字**落地接口：`PeerDirectory`、`MessageRepo`、`TransferRepo`、`ConnManager`、`Session`、`DiscoveryStrategy`、`SeedRegistry`、`FileSink`、`EventBus`、`Clock`。
- [ ] **Step 2:** `go build ./...` → 编译通过（会因引用 domain 包而报缺包 → 先做 S1）
- [ ] **Step 3:** Commit `feat(domain): ports contracts`

---

## Phase S1 — 领域核心（零 I/O）

### Task 1.1: domain/identity

**Files:** Create `internal/domain/identity/identity.go`, `identity_test.go`

- [ ] **Step 1:** 测试

```go
package identity

import (
	"path/filepath"
	"testing"
)

func TestNodeIDStableAndDomainSeparated(t *testing.T) {
	dir := t.TempDir()
	id1, err := LoadOrCreate(dir)
	if err != nil { t.Fatal(err) }
	id2, err := LoadOrCreate(dir)
	if err != nil { t.Fatal(err) }
	if id1.NodeID() != id2.NodeID() { t.Fatal("node id not stable") }
	if len(id1.NodeID().String()) != 16 { t.Fatalf("want 16 hex chars got %d", len(id1.NodeID().String())) }
	if _, err := os.Stat(filepath.Join(dir, "identity", "node.key")); err != nil { t.Fatal(err) }
}

func TestSignVerifyAndTamperDetect(t *testing.T) {
	id, _ := LoadOrCreate(t.TempDir())
	msg := []byte("swarmlink-hello")
	sig := id.Sign(msg)
	if !Verify(id.PublicKey(), msg, sig) { t.Fatal("valid sig rejected") }
	if Verify(id.PublicKey(), append(msg, 1), sig) { t.Fatal("tampered msg accepted") }
	if Fingerprint(id.PublicKey()) != id.NodeID() { t.Fatal("fingerprint mismatch") }
}
```

- [ ] **Step 2:** 运行确认失败
- [ ] **Step 3:** 实现：Ed25519、PKCS#8 PEM `PRIVATE KEY`、0600/0700；`Fingerprint = SHA-256("swarmlink-node-id-v1" || pub)[:8]`；`NodeID` 为 16 字符 hex；提供 `Sign/Verify/MarshalPublic/UnmarshalPublic`。
- [ ] **Step 4:** `go test ./internal/domain/identity/` → PASS
- [ ] **Step 5:** Commit

### Task 1.2: domain/peer

**Files:** Create `internal/domain/peer/peer.go`, `peer_test.go`

- [ ] **Step 1:** 定义 `State`（unknown/discovered/online/offline/blocked）、`Peer{NodeID, DisplayName, PubKey, LastAddr, Caps, ProtoVer, FirstSeen, LastSeen, Subnet, State, Source}`、`Announcement{NodeID, DisplayName, PubKey, TCPPort, UDPPort, Subnet, Nonce, Timestamp, Epoch, Sig}`、`SeedAddr{IP, UDPPort}`。
- [ ] **Step 2:** 测试 TTL 判定用本地到达时间：`Peer.Expired(now, ttl)`；`Announcement.SigningBytes()` 拼接顺序固定（magic||ver||nodeID||tcpPort||nonce||timestamp||epoch）。
- [ ] **Step 3:** Commit

### Task 1.3: domain/message

**Files:** Create `internal/domain/message/message.go`, `message_test.go`

- [ ] **Step 1:** `Message{MsgID, ConvID, SenderID, Direction, Content, MsgType, FileID, SentAt, RecvAt, State}`；`NewID()` 用 `google/uuid` V7；`SortRules`（sent_at 升序，同 sent_at 按 msg_id）。
- [ ] **Step 2:** 测试 UUIDv7 单调 + 排序稳定
- [ ] **Step 3:** Commit

### Task 1.4: domain/group

**Files:** Create `internal/domain/group/group.go`, `group_test.go`

- [ ] **Step 1:** `Group{ID, Name, OwnerID, Epoch, StateSig, Members, CreatedAt}`、`Member{NodeID, DisplayName, Role, JoinedAt, State}`、`GroupID(owner, createdAt, name)`、`SigningBytes()` = `H(group_id|epoch|members)`（成员按 NodeID 排序保证确定性）。`FanoutPlan(members, online) []NodeID` 仅 active+在线，上限 20。
- [ ] **Step 2:** 测试：成员顺序不影响签名；上限拒绝 >20 人。
- [ ] **Step 3:** Commit

### Task 1.5: domain/transfer（最关键，覆盖率重点）

**Files:** Create `internal/domain/transfer/{bitmap.go,job.go,state.go,window.go}` + 测试

- [ ] **Step 1:** `ChunkBitmap` 测试（Set/IsSet/CompletedCount/Marshal/Unmarshal/Len）

```go
func TestBitmapRoundTrip(t *testing.T) {
	bm := NewChunkBitmap(10)
	bm.Set(0); bm.Set(3); bm.Set(9)
	b, err := bm.MarshalBinary()
	if err != nil { t.Fatal(err) }
	var out ChunkBitmap
	if err := out.UnmarshalBinary(b); err != nil { t.Fatal(err) }
	if out.CompletedCount() != 3 || !out.IsSet(3) || out.IsSet(1) { t.Fatal("roundtrip broken") }
}
```

- [ ] **Step 2:** `StateMachine` 测试：`IDLE→META_EXCHANGE→TRANSFERRING⇄PAUSED→VERIFYING→DONE`；`PAUSED→TRANSFERRING` 需重走 META_EXCHANGE；`VERIFYING` 失败**不清空位图**、标记坏块后回 TRANSFERRING。
- [ ] **Step 3:** `Window` 测试：`clamp(BDP/chunk,1,64)`，初始 8，每 2 RTT 调整。
- [ ] **Step 4:** `Job` 聚合：`MissingChunks(bm, total)`、`BadChunks(hashes, computed)`。
- [ ] **Step 5:** `go test ./internal/domain/... -cover` → 覆盖率 ≥ 80%
- [ ] **Step 6:** Commit

---

## Phase S2 — 协议编解码

### Task 2.1: protocol/const + frame + codec

**Files:** Create `internal/adapters/net/protocol/{const.go,frame.go,codec.go}` + `codec_test.go`, `fuzz_test.go`

- [ ] **Step 1:** `const.go`：TCP `Magic = 0x53 0x4C`、`Ver = 0x02`、9 字节头、`MaxFrameSize = 16<<20`；Flags 位常量；Type 常量（含 0x05–0x08/0x22/0x30–0x33/0x40–0x41）与分组区间注释；UDP 魔数 `"SLAN"` + 独立 Type。
- [ ] **Step 2:** 表驱动测试：编解码往返、未知 Type 按 Length 跳过后丢弃、`Length > MaxFrameSize` 报错、未知 Flags 位忽略。
- [ ] **Step 3:** `go test -fuzz=FuzzDecode ./internal/adapters/net/protocol/ -fuzztime=10s` → 无 panic
- [ ] **Step 4:** Commit

---

## Phase S3 — 网络适配器

### Task 3.1: tcp 三段握手（ADR-002）

**Files:** Create `internal/adapters/net/tcp/{handshake.go,session.go}` + 测试（`net.Pipe()`）

- [ ] **Step 1:** 测试双节点握手：A/B 交换 nonce 与 pubkey，验签通过进入 ESTABLISHED；篡改 pubkey 或 nonce → 验签失败 → CLOSED(冒充告警)
- [ ] **Step 2:** 实现 `HELLO/HELLO_ACK/AUTH/AUTH_OK`；握手期拒绝业务帧；`proto_chosen`、`caps_common`
- [ ] **Step 3:** Commit

### Task 3.2: tcp ConnManager（按需拨号/保活/空闲回收/胜负规则）

**Files:** Create `internal/adapters/net/tcp/{manager.go,lazy.go}` + 测试（内存 ConnManager + FakeClock）

- [ ] **Step 1:** 测试：`self.NodeID < peer.NodeID` 才拨号；重复连接按规则保留胜者、关闭负者且不发业务帧；`idle_conn_timeout` 触发 DRAINING→CLOSED；`max_dial_concurrency` 生效
- [ ] **Step 2:** 实现 `Dial/Accept/SessionOf/Broadcast/Close`；会话级心跳 + `RTT()`
- [ ] **Step 3:** Commit

### Task 3.3: udp 广播发现 + 多网卡策略 + seed 单播

**Files:** Create `internal/adapters/net/udp/{broadcast.go,iface.go,iface_darwin.go,iface_linux.go,iface_windows.go,announce.go,seed.go}` + 测试

- [ ] **Step 1:** 测试：子网广播地址由 IP+netmask 计算（非 255.255.255.255）；排除 loopback/link-local/虚拟网卡；收到 `nodeID==self` 丢弃；`list_epoch` 相同回空响应；条目按 node_id 去重 + 本地到达时间 TTL
- [ ] **Step 2:** 实现签名 ANNOUNCE（含 `subnet`）、PEER_LIST_REQ/RESP、SEED_PROBE→单播回 ANNOUNCE→学 tcp_port
- [ ] **Step 3:** 平台特定接口绑定（build tags）
- [ ] **Step 4:** Commit

---

## Phase S4 — 存储与落盘

### Task 4.1: sqlite + migrations + 单写 goroutine

**Files:** Create `internal/adapters/store/sqlite/{db.go,migrations.go,writer.go}` + 测试

- [ ] **Step 1:** `schema_migrations(version, applied_at, checksum)`；所有迁移在启动事务中执行；迁移可重入测试（跑两次不报错）
- [ ] **Step 2:** 按架构书第 5 章建全部表；DSN 含 `_foreign_keys=on`
- [ ] **Step 3:** 单写 goroutine 写队列（channel + worker），所有写操作串行化
- [ ] **Step 4:** Commit

### Task 4.2: mem store（对等实现）

**Files:** Create `internal/adapters/store/mem/mem.go` + 测试。实现 `MessageRepo`/`TransferRepo`/`PeerDirectory`，供单测与 e2e 用。

### Task 4.3: FileSink + fsutil（G3）

**Files:** Create `internal/infra/fsutil/{safename.go,atomic.go}`、`internal/adapters/store/sqlite` 外的 `FileSink` 实现 + 测试

- [ ] **Step 1:** 测试逃逸用例：`../../.ssh/authorized_keys`、`/etc/x`、`a\x00b`、`CON`、`..\\evil.exe`、超 255B 多字节名
- [ ] **Step 2:** 实现清洗 + `.swarmlink.<job_id>.part` 临时名 + `os.Rename` 原子替换
- [ ] **Step 3:** Commit

---

## Phase S5 — 应用层

### Task 5.1–5.5: discover/peer/chat/transfer/group App

- [ ] `peer_app.go`：订阅 `peer.discovered`，Upsert 到 PeerDirectory，发 `peer.online/offline`
- [ ] `discovery_app.go`：驱动广播 + 种子刷新循环（5s/300s 定时器，用 Clock）
- [ ] `chat_app.go`：`SendMessage` 编排（写 messages+outbox 单事务 → 拨号 → 发 CHAT）；订阅 `chat.received`→`AppendIfAbsent`→**无条件回 ACK**；outbox 扫描器（退避）+ `peer.online` flush
- [ ] `transfer_app.go`：滑动窗口发送、位图批量落盘、进度节流 4Hz、并发上限 3
- [ ] `group_app.go`：成员变更 epoch+owner 签名、`GROUP_META` 广播、扇出（并发 8）、送达统计
- [ ] 每项配单测 → Commit

---

## Phase S6–S7 — 可靠性与测试

- [ ] `test/e2e`：3 节点发现+握手+消息往返
- [ ] `test/e2e`（跨网段）：两个隔离 UDP 端口域 + 共享种子清单 → 目录双向收敛
- [ ] `test/scale`：20 节点常驻连接 ≤ `max_active_conns`
- [ ] `test/chaos`：随机 kill 连接 → 位图恢复正确
- [ ] 端口回退 2425→2434 测试
- [ ] Commit

---

## Phase S8 — CLI

**Files:** Create `cmd/swarmlink-cli/main.go`

- [ ] `--name alice --seed ip:port`；打印在线节点、可发消息、可发文件；等价 M1 交付物
- [ ] 手工双进程验证发现 + 聊天 + 传输 → Commit

---

## Phase S9–S10 — Wails + Vue3

### Task 9.1: adapters/wails

- [ ] `services.go`：`SettingsService/ChatService/TransferService/PeerService/GroupService`（仅参数校验 + 调 App）
- [ ] `eventbridge.go`：订阅总线主题 → `app.Event.Emit`，`transfer.progress` 节流 4Hz
- [ ] `wails.json`、`main.go` 装配

### Task 10.1: Vue3 前端

- [ ] `frontend/`：Vite + Vue3 + Pinia + TS；`src/api/` 包 binding；`src/events/` 集中订阅+节流；视图：节点列表/聊天/传输/设置/**调试面板**
- [ ] `npm install && npm run build` 通过

---

## Phase S11 — 工程化

- [ ] `make lint-arch` 接入 CI（GitHub Actions）
- [ ] `docs/adr/ADR-001..011` 落文件
- [ ] `README.md` / 用户文档：防火墙例外、种子清单配置、**P-1/P-2 自检步骤**、VPN 说明
- [ ] Commit

---

## Self-Review Notes

- **Spec 覆盖**：S0–S11 对 spec §9 构建顺序逐条对应；验收标准 §10 由 S6/S7/S8/S11 覆盖。
- **类型一致性**：`Session.Recv()` 阻塞式；`FileSink.Create(jobID, rawName)`；`Clock.NewTicker` 返回 `Ticker`（`C()/Stop()`）——所有实现须与此一致。
- **风险**：Go 1.21.8 与架构书 1.23+ 的偏差已记录；Wails v3 beta 可能无法在本机 `wails3 build`（缺 CLI/平台工具链），前端与适配器代码仍按约定编写，构建验证降级为 `go build`（backend）+ `npm run build`（frontend）。
