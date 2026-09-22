package app

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

func newTestChatApp(t *testing.T) (*ChatApp, *mem.Messages) {
	t.Helper()
	clk := clock.New()
	store := mem.NewMessages(clk)
	self := mustNodeID(t, testSelfHex)
	return NewChatApp(self, store, noConns{}, mem.NewPeers(clk), eventbus.New(), clk, nil), store
}

// 图片消息必须带着 msg_type=image 落库：这是前端决定渲染成图还是文字的唯一依据。
func TestSendImagePersistsImageMessage(t *testing.T) {
	capp, store := newTestChatApp(t)
	peerID := mustNodeID(t, testPeerHex)

	content, err := message.EncodeInlineImage(message.InlineImage{
		MIME: "image/jpeg",
		W:    4,
		H:    4,
		B64:  base64.StdEncoding.EncodeToString([]byte("jpegdata")),
	})
	if err != nil {
		t.Fatalf("编码内联图: %v", err)
	}

	msg, err := capp.SendImage(context.Background(), peerID, content)
	if err != nil {
		t.Fatalf("SendImage: %v", err)
	}
	if msg.MsgType != message.MsgTypeImage {
		t.Errorf("MsgType = %q，期望 %q", msg.MsgType, message.MsgTypeImage)
	}

	got, ok := store.Get(msg.MsgID)
	if !ok {
		t.Fatal("图片消息未落库")
	}
	img, err := message.DecodeInlineImage(got.Content)
	if err != nil {
		t.Fatalf("落库内容不是合法的内联图片: %v", err)
	}
	if img.W != 4 || img.H != 4 {
		t.Errorf("落库尺寸 = %dx%d，期望 4x4", img.W, img.H)
	}
}

// 文本发送路径必须保持不变（SendMessage 被抽成公共实现后的回归）。
func TestSendMessageStillTextType(t *testing.T) {
	capp, store := newTestChatApp(t)
	peerID := mustNodeID(t, testPeerHex)

	msg, err := capp.SendMessage(context.Background(), peerID, "你好")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if msg.MsgType != message.MsgTypeText {
		t.Errorf("MsgType = %q，期望 text", msg.MsgType)
	}
	if _, ok := store.Get(msg.MsgID); !ok {
		t.Fatal("文本消息未落库")
	}
}

// 入站图片消息要能进历史并触发 UI 事件。
func TestHandleChatAcceptsImageMessage(t *testing.T) {
	clk := clock.New()
	store := mem.NewMessages(clk)
	bus := eventbus.New()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	var received []message.Message
	bus.Subscribe(eventbus.TopicChatReceived, func(payload any) {
		if m, ok := payload.(message.Message); ok {
			received = append(received, m)
		}
	})

	capp := NewChatApp(self, store, noConns{}, mem.NewPeers(clk), bus, clk, nil)

	content := `{"v":1,"mime":"image/jpeg","w":2,"h":2,"b64":"` +
		base64.StdEncoding.EncodeToString([]byte("img")) + `"}`
	payload, err := protocol.EncodeJSON(protocol.Chat{
		MsgID:    "m-img",
		SenderID: peerID.String(),
		Content:  content,
		MsgType:  string(message.MsgTypeImage),
		SentAt:   clk.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("编码: %v", err)
	}

	if err := capp.HandleChat(stubSession{peer: peerID}, protocol.New(protocol.TypeChat, payload)); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}
	if _, ok := store.Get("m-img"); !ok {
		t.Fatal("合法图片消息未落库")
	}
	if len(received) != 1 {
		t.Fatalf("chat 事件数 = %d，期望 1", len(received))
	}
}

// 送达事件可能早于「发送调用返回」—— 这是前端的顺序前提，不是假设。
//
// 本机测试两个实例时，对端在几百微秒内就回了 ACK，而发送调用还要经过
// DTO 序列化 + Wails IPC 才回到前端。因此界面有可能先收到 chat:delivered、
// 后拿到 SendMessage 的返回值（那个返回值里的 state 永远是 pending 快照）。
//
// 这条契约一旦被后端改掉（例如把事件延后到 RPC 返回之后再发），
// 下面的断言会失败 —— 但更重要的是它记录了前端【必须】容忍两种顺序。
func TestDeliveredEventMayArriveBeforeSendReturns(t *testing.T) {
	clk := clock.New()
	store := mem.NewMessages(clk)
	bus := eventbus.New()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	delivered := make([]string, 0, 1)
	bus.Subscribe(eventbus.TopicChatDelivered, func(payload any) {
		if ack, ok := payload.(protocol.ChatAck); ok {
			delivered = append(delivered, ack.MsgID)
		}
	})

	conns := newAckEagerConns(peerID)
	capp := NewChatApp(self, store, conns, mem.NewPeers(clk), bus, clk, nil)
	conns.sess.chat = capp

	msg, err := capp.SendMessage(context.Background(), peerID, "在吗")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if len(delivered) != 1 {
		t.Fatalf("送达事件数 = %d，期望 1（对端已确认）", len(delivered))
	}
	if delivered[0] != msg.MsgID {
		t.Errorf("事件 msg_id = %q，期望 %q", delivered[0], msg.MsgID)
	}
	// 关键：此刻发送调用刚返回，而事件【已经】发过了 ——
	// 界面若等到返回值才把消息写进列表，这条 delivered 就永远追不上了。
	if msg.State != message.StatePending {
		t.Errorf("返回的 DTO state = %q，期望 pending 快照（这正是前端不能只依赖返回值的原因）", msg.State)
	}

	stored, ok := store.Get(msg.MsgID)
	if !ok {
		t.Fatal("消息未落库")
	}
	if stored.State != message.StateDelivered {
		t.Errorf("落库 state = %q，期望 delivered", stored.State)
	}
}

// 畸形/超限的图片消息必须被丢弃，且【仍然回 ACK】——
// 不回 ACK 会让发送方按 outbox 策略无限重发同一条垃圾消息。
func TestHandleChatDropsBrokenImageButStillAcks(t *testing.T) {
	capp, store := newTestChatApp(t)
	peerID := mustNodeID(t, testPeerHex)

	huge := base64.StdEncoding.EncodeToString(make([]byte, message.MaxInlineImageBytes+1))
	cases := map[string]string{
		"不是 JSON":   "这是一段普通文字",
		"base64 非法": `{"v":1,"mime":"image/jpeg","w":1,"h":1,"b64":"!!!bad!!!"}`,
		"超出体积上限":  `{"v":1,"mime":"image/jpeg","w":1,"h":1,"b64":"` + huge + `"}`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			sess := &recordingSession{peer: peerID}
			payload, err := protocol.EncodeJSON(protocol.Chat{
				MsgID:    "m-" + name,
				SenderID: peerID.String(),
				Content:  content,
				MsgType:  string(message.MsgTypeImage),
			})
			if err != nil {
				t.Fatalf("编码: %v", err)
			}
			if err := capp.HandleChat(sess, protocol.New(protocol.TypeChat, payload)); err != nil {
				t.Fatalf("HandleChat 不应把坏消息变成连接级错误: %v", err)
			}
			if _, ok := store.Get("m-" + name); ok {
				t.Error("畸形图片消息不得落库")
			}
			if !sess.acked {
				t.Error("必须回 ACK，否则发送方会无限重发")
			}
		})
	}
}

// 文本消息不受新校验影响（回归）。
func TestHandleChatStillAcceptsPlainText(t *testing.T) {
	capp, store := newTestChatApp(t)
	peerID := mustNodeID(t, testPeerHex)

	payload, err := protocol.EncodeJSON(protocol.Chat{
		MsgID: "m-text", SenderID: peerID.String(), Content: "你好",
	})
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	if err := capp.HandleChat(stubSession{peer: peerID}, protocol.New(protocol.TypeChat, payload)); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}
	got, ok := store.Get("m-text")
	if !ok {
		t.Fatal("文本消息未落库")
	}
	if got.MsgType != message.MsgTypeText {
		t.Errorf("MsgType = %q，期望 text", got.MsgType)
	}
}

// 尺寸字段来自网络，必须钳制：否则伪造的 w/h 能让前端按天文数字撑开占位框。
func TestHandleChatClampsImageDimensions(t *testing.T) {
	capp, store := newTestChatApp(t)
	peerID := mustNodeID(t, testPeerHex)

	content := `{"v":1,"mime":"image/jpeg","w":9999999,"h":-5,"b64":"` +
		base64.StdEncoding.EncodeToString([]byte("img")) + `"}`
	payload, err := protocol.EncodeJSON(protocol.Chat{
		MsgID: "m-clamp", SenderID: peerID.String(), Content: content,
		MsgType: string(message.MsgTypeImage),
	})
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	if err := capp.HandleChat(stubSession{peer: peerID}, protocol.New(protocol.TypeChat, payload)); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}
	got, ok := store.Get("m-clamp")
	if !ok {
		t.Fatal("消息未落库")
	}
	img, err := message.DecodeInlineImage(got.Content)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if img.W > maxImageEdge || img.H < 0 || img.H > maxImageEdge {
		t.Errorf("尺寸未钳制: %dx%d", img.W, img.H)
	}
}
