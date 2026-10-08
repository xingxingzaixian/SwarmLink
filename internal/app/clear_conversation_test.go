package app

import (
	"context"
	"testing"
	"time"
)

/*
「清空聊天视图」是纯本机的视图偏好：消息不删除，只记一个清空点。
它的失效方式很刺眼 —— 要么清空了没反应，要么消息凭空消失 —— 所以
写入、读回、覆盖、撤销、互不干扰这五件事各钉一条。
*/

// 清空点的完整往返：0（未清空）→ 写入 → 覆盖 → 撤销 → 回到 0。
func TestClearConversationRoundTrip(t *testing.T) {
	capp, store := newTestChatApp(t)
	ctx := context.Background()

	if ms, err := capp.ClearedAt(ctx, "conv-a"); err != nil || ms != 0 {
		t.Fatalf("未清空时应返回 0，得到 %d（err=%v）", ms, err)
	}

	if err := capp.ClearConversation(ctx, "conv-a"); err != nil {
		t.Fatalf("ClearConversation: %v", err)
	}
	first, err := capp.ClearedAt(ctx, "conv-a")
	if err != nil || first <= 0 {
		t.Fatalf("清空点 = %d（err=%v），期望正的毫秒时间戳", first, err)
	}

	// 覆盖：先人为写一个很早的清空点，再清空一次，边界必须往前推。
	// （直接写存储而不是 sleep 等时钟走，是为了让断言与真实时间无关。）
	old := time.UnixMilli(1_000)
	if err := store.SetConvCleared("conv-a", old); err != nil {
		t.Fatal(err)
	}
	if err := capp.ClearConversation(ctx, "conv-a"); err != nil {
		t.Fatal(err)
	}
	again, _ := capp.ClearedAt(ctx, "conv-a")
	if again <= old.UnixMilli() {
		t.Fatalf("二次清空没有覆盖旧边界：old=%d new=%d", old.UnixMilli(), again)
	}

	if err := capp.RestoreConversation(ctx, "conv-a"); err != nil {
		t.Fatalf("RestoreConversation: %v", err)
	}
	if ms, _ := capp.ClearedAt(ctx, "conv-a"); ms != 0 {
		t.Fatalf("撤销后仍读到清空点：%d", ms)
	}
}

// 清空一个会话绝不能影响另一个：这是「只清当前会话」这条需求的底线。
func TestClearConversationIsPerConversation(t *testing.T) {
	capp, _ := newTestChatApp(t)
	ctx := context.Background()

	if err := capp.ClearConversation(ctx, "conv-a"); err != nil {
		t.Fatal(err)
	}
	if ms, _ := capp.ClearedAt(ctx, "conv-b"); ms != 0 {
		t.Fatalf("清空 conv-a 影响到了 conv-b：%d", ms)
	}
}

// 撤销是幂等的：没清空过也能点（前端按钮虽然会禁用，服务端不能依赖这一点）。
func TestRestoreConversationIsIdempotent(t *testing.T) {
	capp, _ := newTestChatApp(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := capp.RestoreConversation(ctx, "conv-a"); err != nil {
			t.Fatalf("第 %d 次撤销报错: %v", i+1, err)
		}
	}
}

// 空 convID 是调用方 bug：必须报错，而不是往库里写一行垃圾记录
// （那种记录永远不会被读到，却会一直躺在数据库里）。
func TestClearConversationRejectsEmptyID(t *testing.T) {
	capp, _ := newTestChatApp(t)
	ctx := context.Background()

	if err := capp.ClearConversation(ctx, ""); err == nil {
		t.Error("ClearConversation 对空 convID 应当报错")
	}
	if err := capp.RestoreConversation(ctx, ""); err == nil {
		t.Error("RestoreConversation 对空 convID 应当报错")
	}
	// 读用空 id 只是查不到，不算错误（前端在没打开会话时会传空）
	if ms, err := capp.ClearedAt(ctx, ""); err != nil || ms != 0 {
		t.Errorf("ClearedAt(\"\") = %d err=%v，期望 0/nil", ms, err)
	}
}
