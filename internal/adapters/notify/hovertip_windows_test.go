//go:build windows

package notify

import (
	"errors"
	"strings"
	"testing"
)

// 悬停提示的 Win32 管线冒烟测试。
//
// 它验证的是【调用链能被系统接受】：建窗口 → TTM_ADDTOOLW → 定位 → TRACKACTIVATE。
// 它验证不了「画出来的框长什么样」（那需要人眼），但能挡住最要命的那几类失败：
// 结构体布局写错、消息号记错、TOOLINFO 被控件拒绝、跨线程 SendMessage 卡死 ——
// 这些都会让功能静默失效（或者挂起），而界面上只表现为「悬停没反应」。
//
// 顺带说明它为什么跑得起来：窗口属于创建它的线程，而 Go 的 goroutine 会在
// 线程间迁移，直接在本 goroutine 里建窗再 SendMessage 会永久阻塞。
// 本类型自建专用线程 + 消息泵，因此这里【不需要】LockOSThread 也不会卡住 ——
// 这正是这个测试要守住的性质。
func TestHoverTipPlumbing(t *testing.T) {
	tip := NewHoverTip()
	defer tip.Close()

	// 多行 + 中文 + 代理对（emoji）：编码与 CRLF 换行都要走一遍
	if err := tip.Show("张三：晚上一起吃饭吗\n李四：[图片] 🎉\n共 3 条未读"); err != nil {
		if errors.Is(err, errNoDesktop) {
			t.Skipf("当前环境没有交互式桌面，跳过：%v", err)
		}
		t.Fatalf("Show: %v", err)
	}
	tip.Hide()

	// 【最要命的一条】必须断言提示框真的有可见尺寸。
	//
	// 这套 API 最阴的失败方式：所有调用都返回成功、窗口 visible=1，
	// 但控件把窗口排成了 6×4 的一条缝 —— 界面上什么都看不到，日志里也没有错误。
	// 历史事故：多设了一个 TTM_SETMAXTIPWIDTH（换行宽度取自退化的 tool 矩形），
	// 结果就是这个下场。只断言「没报错」的测试抓不住它。
	if w, h := tip.LastSize(); w < 40 || h < 10 {
		t.Fatalf("提示框尺寸 = %dx%d，小到看不见（这正是「调用全成功但什么都不显示」的失败方式）", w, h)
	}

	// 反复显示/隐藏：缓冲被复用，不能出现越界或残留
	for i := 0; i < 3; i++ {
		if err := tip.Show("第二条消息，稍长一些的内容用于验证复用缓冲：" + strings.Repeat("测", 100)); err != nil {
			t.Fatalf("第 %d 次 Show: %v", i+2, err)
		}
		tip.Hide()
	}

	// 超长文本必须被截断在缓冲容量内（本用例主要防越界 panic）
	if err := tip.Show(strings.Repeat("超长内容", 5000)); err != nil {
		t.Fatalf("超长文本 Show: %v", err)
	}
	tip.Hide()
}

// 隐藏与关闭必须可重复调用、且顺序无关：托盘回调与退出路径都可能重复触发。
func TestHoverTipHideAndCloseAreIdempotent(t *testing.T) {
	tip := NewHoverTip()
	tip.Hide() // 还没显示过就 hide
	if err := tip.Show("第一条"); err != nil {
		if errors.Is(err, errNoDesktop) {
			t.Skipf("当前环境没有交互式桌面，跳过：%v", err)
		}
		t.Fatalf("Show: %v", err)
	}
	tip.Hide()
	tip.Hide()
	tip.Close()
	tip.Close() // 重复 Close 不能 panic

	// 关闭之后仍调用 Show：静默忽略，不 panic、不阻塞
	if err := tip.Show("关闭后"); err != nil {
		t.Fatalf("Close 之后 Show 不应报错: %v", err)
	}
}

// 文本编码是纯函数，不需要 Win32：单独测它才能覆盖「换行是否规范成 CRLF」，
// 而工具提示控件只按 CRLF 断行 —— 只给 \n 的话多行文案会挤成一行。
func TestHoverWindowTextEncoding(t *testing.T) {
	w := hoverWindow{text: make([]uint16, hoverTextCap)}
	w.writeText("甲：你好\n乙：在吗")

	var got []rune
	for _, u := range w.text {
		if u == 0 {
			break
		}
		got = append(got, rune(u))
	}
	if want := "甲：你好\r\n乙：在吗"; string(got) != want {
		t.Errorf("缓冲内容 = %q，期望 %q", string(got), want)
	}
}

// 缓冲是固定大小的：超长输入必须被截断到容量内并保留 NUL 终止符，
// 否则 TTM_ADDTOOLW 之后的读取会跑出缓冲末尾。
func TestHoverWindowTextTruncatesSafely(t *testing.T) {
	w := hoverWindow{text: make([]uint16, 16)}
	w.writeText(strings.Repeat("abcdefg", 100))

	nul := -1
	for i, u := range w.text {
		if u == 0 {
			nul = i
			break
		}
	}
	if nul < 0 {
		t.Fatal("超长文本把 NUL 终止符挤掉了")
	}
	if nul != len(w.text)-1 {
		t.Errorf("应填满到倒数第一个单元（%d），实际 %d", len(w.text)-1, nul)
	}
}
