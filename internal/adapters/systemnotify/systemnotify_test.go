package systemnotify

import "testing"

// ThreadID 必须【永远】为空 —— 这是「通知上只有『新通知』四个字」那条缺陷的唯一开关。
//
// 机理见 msgOptions 的注释：非空 ThreadID 会让 Wails 生成一个缺必填 arguments 的
// <header>，整个 toast 因此不合规，Windows 只显示通用占位文案，
// 标题（发送者）与正文（消息内容）全部丢失。
//
// 这条用例的意义不是「断言一个空值」，而是：谁再把 ThreadID 加回来，
// 会立刻看到它为什么不能加 —— 那个现象在 Go 侧完全不可观测（发送成功、无日志）。
func TestMsgOptionsNeverSetsThreadID(t *testing.T) {
	got := msgOptions(Message{
		ID:     "direct:aabbccdd00112233:1122334455667788",
		Title:  "张三",
		Body:   "晚上一起吃饭吗",
		ConvID: "direct:aabbccdd00112233:1122334455667788",
	})
	if got.ThreadID != "" {
		t.Fatalf("ThreadID 必须为空，得到 %q：非空会让 Windows 丢掉整条通知的标题与正文，"+
			"用户只会看到「新通知」", got.ThreadID)
	}
}

// 标题与正文必须原样透传：正文就是用户要看的消息内容。
func TestMsgOptionsCarriesTitleAndBody(t *testing.T) {
	got := msgOptions(Message{
		ID: "c1", Title: "张三", Body: "[图片]", ConvID: "c1", Silent: true,
	})
	if got.Title != "张三" {
		t.Errorf("标题 = %q，期望发送者名字", got.Title)
	}
	if got.Body != "[图片]" {
		t.Errorf("正文 = %q，期望消息预览", got.Body)
	}
	if got.Sound == nil || !got.Sound.Silent {
		t.Error("Silent 没有透传：用户关掉提示音后通知仍会响，设置页看起来失效")
	}
}

// 去掉 ThreadID 之后，点击回跳仍然只能靠 Data.convId —— 它不能跟着一起丢。
func TestMsgOptionsKeepsConvIDForClickThrough(t *testing.T) {
	got := msgOptions(Message{ID: "c9", Title: "李四", Body: "在吗", ConvID: "group:1234"})
	convID, _ := got.Data["convId"].(string)
	if convID != "group:1234" {
		t.Fatalf("Data.convId = %q，期望原样的会话 id（点击通知要打开的就是它）", convID)
	}
}
