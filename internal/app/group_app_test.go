package app

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

func newTestGroupApp(t *testing.T) (*GroupApp, *mem.Messages) {
	t.Helper()
	clk := clock.New()
	store := mem.NewMessages(clk)
	groups := mem.NewGroups()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	if err := groups.Upsert(group.Group{
		ID:      "g1",
		Name:    "测试群",
		OwnerID: self,
		Epoch:   1,
		Members: []group.Member{
			{NodeID: self, Role: group.RoleOwner, State: group.MemberActive},
			{NodeID: peerID, Role: group.RoleMember, State: group.MemberActive},
		},
	}); err != nil {
		t.Fatalf("建群: %v", err)
	}

	gapp := NewGroupApp(self, nil, groups, store, noConns{}, mem.NewPeers(clk), eventbus.New(), clk, nil)
	return gapp, store
}

// 群消息必须把 msg_type 原样带到线上：否则群里的图片到了对方会被当成文本，
// 渲染成一个空白的文字气泡。
func TestGroupMessageCarriesMsgType(t *testing.T) {
	gapp, store := newTestGroupApp(t)
	peerID := mustNodeID(t, testPeerHex)

	content := `{"v":1,"mime":"image/jpeg","w":1,"h":1,"b64":"aGk="}`
	msg, err := gapp.SendGroupImage(context.Background(), "g1", content)
	if err != nil {
		t.Fatalf("SendGroupImage: %v", err)
	}
	if msg.MsgType != message.MsgTypeImage {
		t.Errorf("落库的 MsgType = %q，期望 %q", msg.MsgType, message.MsgTypeImage)
	}

	// 入站：把对方发来的群图片消息解出来，类型必须保持
	frame := mustGroupFrame(t, protocol.GroupMsg{
		MsgID:    "m-in",
		GroupID:  "g1",
		SenderID: peerID.String(),
		Content:  content,
		MsgType:  string(message.MsgTypeImage),
		SentAt:   clock.New().Now().UnixMilli(),
	})
	if err := gapp.HandleGroupMsg(stubSession{peer: peerID}, frame); err != nil {
		t.Fatalf("HandleGroupMsg: %v", err)
	}

	got, ok := store.Get("m-in")
	if !ok {
		t.Fatal("入站群图片消息未落库")
	}
	if got.MsgType != message.MsgTypeImage {
		t.Errorf("入站消息 MsgType = %q，期望 %q", got.MsgType, message.MsgTypeImage)
	}
}

// 群里的坏图片消息同样必须「丢弃 + 仍回 ACK」：单聊与群聊是两条独立实现
// （HandleChat / HandleGroupMsg），只测一条等于赌另一条不会写错。
func TestHandleGroupMsgDropsBrokenImageButStillAcks(t *testing.T) {
	gapp, store := newTestGroupApp(t)
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
			frame := mustGroupFrame(t, protocol.GroupMsg{
				MsgID:    "gm-" + name,
				GroupID:  "g1",
				SenderID: peerID.String(),
				Content:  content,
				MsgType:  string(message.MsgTypeImage),
			})
			if err := gapp.HandleGroupMsg(sess, frame); err != nil {
				t.Fatalf("HandleGroupMsg 不应把坏消息变成连接级错误: %v", err)
			}
			if _, ok := store.Get("gm-" + name); ok {
				t.Error("畸形群图片消息不得落库")
			}
			if !sess.acked {
				t.Error("必须回 ACK，否则发送方会无限重发")
			}
		})
	}
}

// 群文本消息不受影响（回归）：缺失 msg_type 的旧客户端消息仍按 text 处理。
func TestGroupTextMessageDefaultsToText(t *testing.T) {
	gapp, store := newTestGroupApp(t)
	peerID := mustNodeID(t, testPeerHex)

	frame := mustGroupFrame(t, protocol.GroupMsg{
		MsgID:    "m-old",
		GroupID:  "g1",
		SenderID: peerID.String(),
		Content:  "老客户端发来的消息",
	})
	if err := gapp.HandleGroupMsg(stubSession{peer: peerID}, frame); err != nil {
		t.Fatalf("HandleGroupMsg: %v", err)
	}
	got, ok := store.Get("m-old")
	if !ok {
		t.Fatal("群消息未落库")
	}
	if got.MsgType != message.MsgTypeText {
		t.Errorf("MsgType = %q，期望 text（缺失 msg_type 时应回落到文本）", got.MsgType)
	}
}
