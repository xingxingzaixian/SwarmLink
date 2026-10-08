package app

import (
	"context"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

/*
文件消息这条链路曾经整体失效，用户可见现象是：
  - 接收方聊天框里堆满裸 JSON {"JobID":"","FileName":"","Reason":"文件已被删除或移动"}
  - 两端都看不到文件卡片

根因是「消息内容的契约两端不一致」：
  ChatApp.SendFile 写的是 {"job_id","file_name"}（且两个结构体都没有 json tag），
  而 trySend 把 file 消息转给 TransferApp.HandleFileMessage，后者只认 FilePath。
  于是每次发送、以及 outbox 的每次重试，都会走一遍「解不出字段 → os.Stat("") 失败」
  → 凭空产生一条 file_failed；这条假失败又是普通消息，会被反复投递给对端。

下面三条各自锁住契约的一个面，任何一条红都说明文件消息又断了。
*/

// 生产端写出的 content 必须能被 FileMessageContent 原样解回来。
//
// 这条看着像废话，但正是它决定了整条链路的生死：字段名对不上时
// json.Unmarshal 不报错、只是留下一个空结构，失败会以「文件已被删除」
// 这种完全无关的名义冒出来。
func TestSendFileContentIsDecodable(t *testing.T) {
	capp, _ := newTestChatApp(t)
	peerID := mustNodeID(t, testPeerHex)

	msg, err := capp.SendFile(context.Background(), peerID, "job-1", "季度报告.pdf")
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	var content message.FileMessageContent
	if err := protocol.DecodeJSON([]byte(msg.Content), &content); err != nil {
		t.Fatalf("content 不是合法 JSON: %v（content=%s）", err, msg.Content)
	}
	if content.JobID != "job-1" {
		t.Errorf("JobID = %q，期望 job-1（content=%s）", content.JobID, msg.Content)
	}
	if content.FileName != "季度报告.pdf" {
		t.Errorf("FileName = %q，期望 季度报告.pdf（content=%s）", content.FileName, msg.Content)
	}
}

// 文件名里的特殊字符不能把 content 拼成非法 JSON。
//
// 手写 fmt.Sprintf 拼 JSON 时，一个英文引号就能让整条消息变成坏 JSON，
// 而接收方解析失败只会表现为「文件名不见了」——很难被发现。
func TestSendFileContentSurvivesAwkwardFileName(t *testing.T) {
	capp, _ := newTestChatApp(t)
	peerID := mustNodeID(t, testPeerHex)

	name := `他说"这个"很重要\报告.pdf`
	msg, err := capp.SendFile(context.Background(), peerID, "job-2", name)
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	var content message.FileMessageContent
	if err := protocol.DecodeJSON([]byte(msg.Content), &content); err != nil {
		t.Fatalf("content 不是合法 JSON: %v（content=%s）", err, msg.Content)
	}
	if content.FileName != name {
		t.Errorf("FileName = %q，期望 %q", content.FileName, name)
	}
}

// 文件消息必须真的投递给对端，且送达后从 outbox 出队。
//
// 修复前 file 消息在 trySend 里被转给本机 handler、从不 sess.Send，
// 于是它永远处于 pending，outbox 会一直重试 —— 每次重试都再产生一条假失败。
func TestSendFileMessageIsDeliveredToPeer(t *testing.T) {
	clk := clock.New()
	store := mem.NewMessages(clk)
	bus := eventbus.New()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	conns := newAckEagerConns(peerID)
	capp := NewChatApp(self, store, conns, mem.NewPeers(clk), bus, clk, nil)
	conns.sess.chat = capp // 让对端在 Send 返回前就回 ACK

	msg, err := capp.SendFile(context.Background(), peerID, "job-3", "设计稿.pdf")
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	waitFor(t, func() bool {
		got, ok := store.Get(msg.MsgID)
		return ok && got.State == message.StateDelivered
	}, "file 消息始终没有变成 delivered（说明它根本没发出去）")

	if n := store.OutboxLen(); n != 0 {
		t.Errorf("送达后 outbox 仍有 %d 条待确认（会无限重试并反复制造假失败）", n)
	}
}

// 发送文件不能在聊天记录里留下「文件已被删除或移动」这类假失败。
//
// 即使用户的文件安安静静躺在磁盘上，只要握手逻辑还依赖本地路径，
// 这条假消息就会一直冒出来 —— 它比「没有卡片」更难排查。
func TestSendFileDoesNotCreateFakeFailureMessage(t *testing.T) {
	clk := clock.New()
	store := mem.NewMessages(clk)
	bus := eventbus.New()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	capp := NewChatApp(self, store, noConns{}, mem.NewPeers(clk), bus, clk, nil)
	capp.Start(context.Background())

	if _, err := capp.SendFile(context.Background(), peerID, "job-4", "照片.zip"); err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	// 给它充分的时间去犯错（发送是异步的）
	time.Sleep(200 * time.Millisecond)

	convID := message.DirectConvID(self, peerID)
	list, _ := store.Latest(convID, 10, 0)
	for _, m := range list {
		if m.MsgType == message.MsgTypeFileFailed {
			t.Errorf("凭空出现失败消息：content=%s", m.Content)
		}
	}
	if len(list) != 1 {
		t.Errorf("该会话应有且仅有 1 条 file 消息，实际 %d 条：", len(list))
		for _, m := range list {
			t.Errorf("  type=%s content=%s", m.MsgType, m.Content)
		}
	}
}

// 失败消息的字段名必须与前端渲染对齐。
//
// 前端按 jobId/fileName/reason 读；Go 结构体没有 json tag 时会输出
// JobID/FileName/Reason，前端只能显示成裸 JSON —— 这正是用户看到的东西。
func TestFileFailedContentIsFrontendReadable(t *testing.T) {
	raw, err := protocol.EncodeJSON(message.FileFailedContent{
		JobID:    "job-5",
		FileName: "丢了.pdf",
		Reason:   "文件已被删除或移动",
	})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}

	var parsed map[string]any
	if err := protocol.DecodeJSON(raw, &parsed); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	// 用 snake_case：消息 content 是「落库 + 上网」的数据，全仓库统一 snake_case
	// （protocol.Chat 的 msg_type/file_id、FileMessageContent 的 job_id 都是），
	// 而 camelCase 只属于对外 DTO。混用会让下一个读这段 JSON 的人猜错一半。
	for _, key := range []string{"job_id", "file_name", "reason"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("缺少前端要读的字段 %q：%s", key, raw)
		}
	}
}

// waitFor 轮询等待条件成立（异步发送路径无法用固定 sleep 断言）。
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}
