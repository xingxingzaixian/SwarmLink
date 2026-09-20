// Package eventbus 提供进程内同步发布/订阅。
//
// 设计约束（架构书 3.1 第 2、3 点）：
//   - 同步分发：让「消息已入库」这类顺序假设可推理；需要异步的消费者自行开 goroutine。
//   - 仅用于「通知」。需要返回值或事务边界的路径由 app 层显式编排，不走总线。
package eventbus

import "sync"

// Topic 常量集中定义，避免主题字符串散落（架构书 3.5）。
const (
	TopicPeerDiscovered   = "peer.discovered"
	TopicPeerOnline       = "peer.online"
	TopicPeerOffline      = "peer.offline"
	TopicChatReceived     = "chat.received"
	TopicChatDelivered    = "chat.delivered"
	TopicGroupUpdated     = "group.updated"
	TopicTransferState    = "transfer.state"
	TopicTransferProgress = "transfer.progress"
	TopicTransferDone     = "transfer.done"
	TopicTransferError    = "transfer.error"
	TopicNetError         = "net.error"
	TopicConfigChanged    = "config.changed"
)

type handler struct {
	id uint64
	fn func(any)
}

// Bus 是同步事件总线。零值不可用，请用 New 构造。
type Bus struct {
	mu     sync.RWMutex
	nextID uint64
	subs   map[string][]handler
}

// New 创建事件总线。
func New() *Bus { return &Bus{subs: make(map[string][]handler)} }

// Publish 同步调用该主题的全部订阅者。
// 快照订阅列表后在锁外调用，避免 handler 内部再次 Publish/Subscribe 造成死锁。
func (b *Bus) Publish(topic string, payload any) {
	b.mu.RLock()
	hs := make([]func(any), len(b.subs[topic]))
	for i, h := range b.subs[topic] {
		hs[i] = h.fn
	}
	b.mu.RUnlock()

	for _, fn := range hs {
		fn(payload)
	}
}

// Subscribe 注册订阅者，返回退订函数（幂等）。
func (b *Bus) Subscribe(topic string, fn func(any)) (unsubscribe func()) {
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.subs[topic] = append(b.subs[topic], handler{id: id, fn: fn})
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			hs := b.subs[topic]
			for i, h := range hs {
				if h.id == id {
					b.subs[topic] = append(hs[:i:i], hs[i+1:]...)
					break
				}
			}
		})
	}
}
