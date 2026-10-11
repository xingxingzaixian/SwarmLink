// Package systemnotify 提供「系统通知」（Windows 通知中心 / macOS 通知中心 /
// Linux D-Bus）这一个平台能力。
//
// 它不放进 adapters/notify：那个包的约定是「完全不认识 Wails」（见其包注释），
// 而系统通知只能经由 Wails 的服务获得，两者的依赖方向相反。
//
// 决策（什么时候提示、提示什么）仍然在 app.NotifyApp 里，本包只负责
// 「怎么发」以及【发不出去时怎么不拖累应用】。
// 对外暴露的是基本类型（Message / convID），Wails 的选项结构体不外泄，
// 因此组合根不必认识这个 beta 期 API。
package systemnotify

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// Notifier 把 Wails 的系统通知服务包一层，只为了解决两件事。
//
// 【一】不能让通知功能把应用拖死。
//
// Wails 在 app.Run() 里按注册顺序启动服务，任一 ServiceStartup 返回错误都会让
// Run() 直接失败（application.go: startupService → "error starting service"），
// 而组合根对 Run() 失败的处置是 os.Exit(1)。系统通知却是典型的「锦上添花」能力：
// 没有桌面会话的 Linux（连不上 D-Bus）、没打包签名的 macOS 都可能起不来。
// 为一个提示功能把整个应用变成起不来，代价完全不对等 —— 这里把启动错误吞掉，
// 收敛成 Available()==false，由决策层回落到原有的托盘气泡。
//
// 【二】给决策层一个明确的可用性标志。
//
// NotifyEnv.SystemNotifyOK 需要的只是「现在能不能发」这一个 bool，
// 而不是一堆平台差异。启动失败与发送失败都会把它翻成 false 且不可逆：
// 一次发不出去，之后每条消息都重试只会重复失败并刷满日志，
// 不如稳定退回气泡 —— 用户至少还能看到东西。
type Notifier struct {
	// svc 用【未导出字段】而不是内嵌：内嵌会把 Wails 那一整套通知 API
	// （SendNotification / UpdateNotification / …）提升成 Notifier 的方法，
	// 进而被绑定生成器搬到前端 —— 那不是我们想给前端的接口面。
	// 前端永远不该发系统通知：发什么、什么时候发，是决策层的事。
	svc *notifications.NotificationService

	ok atomic.Bool
}

// New 创建系统通知适配器。
func New() *Notifier {
	n := &Notifier{svc: notifications.New()}
	// 乐观置为可用：真正的失败时刻只有两个 —— 服务启动、第一次发送，
	// 两处出错都会立刻翻成 false。
	n.ok.Store(true)
	return n
}

// Message 是一条待发的系统通知。
type Message struct {
	// ID 是系统侧的身份：同一会话复用同一个 ID，
	// macOS 会去重、Linux 会替换掉上一条（Windows 目前仍会新增一条）。
	ID string
	// Title 是通知标题（发送者名字）。
	Title string
	// Body 是通知正文（内容预览，可能带未读总数）。
	Body string
	// ConvID 随通知一起交给系统，点击时原样回传。
	//
	// 这是跨进程、甚至跨重启还原「这条通知说的是哪个会话」的唯一通道：
	// 点击回调发生在系统/COM 侧，本进程当时的状态完全不可依赖。
	ConvID string
	// Silent 为真时不发声（用户在设置里关了提示音）。
	Silent bool
}

// Available 报告系统通知当前是否可用（供组合根填进 NotifyEnv）。
//
// 前端不需要它，因此不生成绑定：本服务对前端没有任何用途。
//
//wails:ignore
func (n *Notifier) Available() bool { return n.ok.Load() }

// ServiceName 是本服务在 Wails 里的名字。
//
// 不直接沿用 Wails 那个实现的名字：它返回自己的包路径
// （"github.com/wailsapp/wails/v3/services/notifications"），
// 而这个名字会被绑定生成器当成前端侧的服务名 —— 别人的包路径不该出现在
// 我们的前端约定里。
func (n *Notifier) ServiceName() string { return "SystemNotify" }

// ServiceStartup 由 Wails 在 app.Run() 时调用；启动失败只降级、不致命。
func (n *Notifier) ServiceStartup(ctx context.Context, options application.ServiceOptions) error {
	if err := n.svc.ServiceStartup(ctx, options); err != nil {
		n.ok.Store(false)
		slog.Warn("系统通知不可用，回落到托盘气泡", "err", err)
	}
	return nil
}

// ServiceShutdown 由 Wails 在退出时调用：关闭失败不该影响退出流程。
func (n *Notifier) ServiceShutdown() error {
	if err := n.svc.ServiceShutdown(); err != nil {
		slog.Warn("关闭系统通知服务失败", "err", err)
	}
	return nil
}

// Send 发出一条系统通知。
//
// 失败时把可用性翻成 false（组合根本次会用托盘气泡补上，此后不再尝试系统通知），
// 错误原样返回给调用方，由它决定怎么降级。
//
//wails:ignore
func (n *Notifier) Send(m Message) error {
	err := n.svc.SendNotification(msgOptions(m))
	if err != nil {
		n.ok.Store(false)
	}
	return err
}

// msgOptions 把本包的消息翻译成 Wails 的通知选项。
//
// 抽成独立函数只为一个理由：下面那条【不能违反】的约束必须能被测试钉住
// （见 systemnotify_test.go），而不是靠注释提醒下一个人。
func msgOptions(m Message) notifications.NotificationOptions {
	return notifications.NotificationOptions{
		ID:    m.ID,
		Title: m.Title,
		Body:  m.Body,
		// 【刻意不设 ThreadID —— 这一条是「通知上只有『新通知』四个字」的正面修复】
		//
		// Wails beta.28 只要看到非空 ThreadID，就会往 toast XML 里塞一个
		// <header>（notifications_windows.go:511），而它的 toastHeader.Arguments
		// 带 `omitempty`（:425）且【从不赋值】：
		//
		//	<header id="direct:…" title="direct:…"/>   ← 少了 arguments
		//
		// 而 header 的 id / title / arguments 三个属性在 Windows 的 toast schema 里
		// 【全是必需】（learn.microsoft.com/uwp/schemas/tiles/toastschema/element-header，
		// 明确写着 arguments "不能为 null"）。少一个属性的后果不是「少显示一行」，
		// 而是整条 toast 不合规：Windows 退化成一条通用通知，标题与正文【全部丢失】，
		// 用户看到的就是光秃秃的「新通知」。
		//
		// 代价是放弃「按会话在通知中心分组」。取舍很清楚：分组只是好看，
		// 正文才是这条通知存在的理由；何况即便渲染出来，header 标题也只是那串会话 id。
		// 点击回跳不受影响 —— 它靠的是下面的 Data.convId。
		//
		// Sound 是「按需静音」：用户关了提示音时通知也别响，
		// 否则设置里那个开关在 Windows 上会看起来失效。
		Sound: &notifications.NotificationSound{Silent: m.Silent},
		Data:  map[string]any{"convId": m.ConvID},
	}
}

// OnActivated 注册「用户点了通知」的回调，参数是通知里带的会话 ID（取不到时为空串）。
//
// 界面动作（把窗口叫到前台、切会话）由调用方决定：本包只管把点击翻译成
// 一个业务上说得通的值，不碰窗口，也不碰前端事件。
//
//wails:ignore
func (n *Notifier) OnActivated(fn func(convID string)) {
	n.svc.OnNotificationResponse(func(r notifications.NotificationResult) {
		if r.Error != nil {
			// 激活失败只记日志：用户点了一下通知，最坏的结果就是什么都没发生，
			// 没有可补救的动作。
			slog.Warn("处理系统通知点击失败", "err", r.Error)
			return
		}
		convID, _ := r.Response.UserInfo["convId"].(string)
		fn(convID)
	})
}
