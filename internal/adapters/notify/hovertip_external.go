//go:build darwin || linux

package notify

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// 为什么 macOS/Linux 不用自绘窗口（照搬 Windows 那套做法）：
//
// 建一个「贴着托盘图标的气泡窗口」在 macOS 要 NSPanel（Cocoa）、在 Linux 要
// GTK，都是 cgo + 系统开发包；而本包是纯 Go，且明确不认识 Wails（红线 R2 ——
// 也不能借 Wails 的 webview 自己画一个）。系统通知是唯一「零依赖但真的能被
// 看到」的通道：macOS 用系统自带的 osascript 的 display notification，
// Linux 用 libnotify 的 notify-send。
//
// 与 Windows 实现的差异（刻意接受，不是缺陷）：
//   - 位置由系统通知中心决定（macOS 右上角、Linux 随桌面环境），做不到
//     「贴在托盘图标上方」；
//   - Hide 无法收回：通知一旦发出就归系统管，只能等它自己超时或用户划掉。
//     组合根的 4 秒自动收起因此在两个平台上退化为「不做动作」（不会报错）；
//   - LastSize 恒为 0,0：我们拿不到通知的尺寸。Windows 那份之所以存在，
//     是为了断言「气泡塌成 6×4」这种静默失效，这里没有对应的失败形态。
//
// 已知限制：悬停提示与「新消息气泡」共用这一个出口。Wails 的 macOS 托盘在
// 当前版本不发 mouse-enter 事件，因此 mac 上实际触发的主要是收到消息时的
// 主动弹出；Linux 的托盘会转发进入事件，悬停时会出现一条系统通知。
type HoverTip struct {
	mu   sync.Mutex
	last string
	at   time.Time
}

// bubbleDedupe 是「同一段文案在多久内不再重复弹」的窗口。
//
// 系统通知没有「替换上一条」的保证（notify-send 默认每条都新建一个），而托盘
// 的进入事件会因为光标在图标边缘抖动而连着来好几次（Windows 侧实测过 29ms
// 内 ENTER 之后紧跟 LEAVE）。不去重的话，用户只是把鼠标划过托盘，通知中心
// 就会堆起同一条消息。文案变了（真有新消息）则立刻放行，不受这个窗口限制。
const bubbleDedupe = 5 * time.Second

// errNoBubble 表示当前系统没有可用的气泡通知命令。
//
// 它是【可预期】的错误（不是 bug）：组合根只打一次日志（见 showTip 的
// tipWarn），之后静默降级，不去反复尝试。
var errNoBubble = errors.New("notify: 当前系统没有可用的气泡通知命令")

// NewHoverTip 构造一个用系统通知承载气泡的提示器（没有需要关闭的资源）。
func NewHoverTip() *HoverTip { return &HoverTip{} }

// Show 在鼠标当前位置弹出提示（鼠标悬停用）。
//
// 两个平台都只能交给系统通知中心，位置不由我们决定 —— 因此与
// ShowNearNotificationArea 等价。
func (t *HoverTip) Show(text string) error { return t.show(text) }

// ShowNearNotificationArea 弹出「新消息气泡」。
func (t *HoverTip) ShowNearNotificationArea(text string) error { return t.show(text) }

// show 去重后把文案交给系统通知。
func (t *HoverTip) show(text string) error {
	text = strings.TrimSpace(text)

	t.mu.Lock()
	if text == t.last && time.Since(t.at) < bubbleDedupe {
		t.mu.Unlock()
		return nil // 同一段文案：视作「已经弹着」，不叠加
	}
	t.last, t.at = text, time.Now()
	t.mu.Unlock()

	return bubble(text)
}

// Hide 在两个平台上没有对应动作：系统通知无法被发送方收回。
//
// 保留这个方法是为了与 Windows 实现同形（组合根不按平台分支），
// 也为了明确记录「这里不是漏写」。
func (t *HoverTip) Hide() {}

// Close 无资源可释放；可重复调用。
func (t *HoverTip) Close() {}

// LastSize 恒为 0,0（我们拿不到通知的尺寸，见类型注释）。
func (t *HoverTip) LastSize() (int, int) { return 0, 0 }

// bubble 把文案交给系统通知。
func bubble(text string) error {
	name := bubbleCommand()
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%w：找不到 %s", errNoBubble, name)
	}

	args := bubbleArgs(text)
	// 异步：Show 由托盘回调（主线程）调用，起一个子进程约 50–100ms，
	// 同步等待会把 UI 卡住。失败也不再上报 —— 与提示音一致：
	// 宁可不显示，也不要阻塞调用方或弹错误。
	go func() { _ = exec.Command(name, args...).Run() }()
	return nil
}
