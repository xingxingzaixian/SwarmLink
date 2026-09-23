# 托盘常驻与新消息提示 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 点关闭按钮时把窗口收进系统托盘而不是退出；新消息与传输事件按 QQ 式规则闪烁 + 提示音 + 托盘未读数。

**Architecture:** 把「何时提示」从 Wails 里剥出来做成零依赖的应用层决策器 `NotifyApp`（事件 + 环境快照 + 设置 → 动作列表），规则表可表驱动单测；Wails 侧（托盘、闪烁、关闭拦截）与声音播放（`internal/adapters/notify`）各由组合根 `main_wails.go` 接线，两者互不引用（红线 R2）。

**Tech Stack:** Go 1.25 / Wails v3 beta.24（`SystemTray`、`WebviewWindow.Flash`、`RegisterHook`）/ Windows `winmm.PlaySound` / Vue 3 设置页。

**设计文档：** `docs/plans/2026-09-23-tray-notification-design.md`

**本计划的约定**

- 本机没有 `make` / `task`，命令一律用 `go` 与 `wails3` 原生形式。
- 按你的要求：不提交 git，每个 Task 末尾只给建议的提交信息。
- 架构红线：`domain/**` 不碰 `os`/`net`/Wails；适配器之间不互引；`app`/`domain` 不 `new` 具体适配器。
- **唯一 import Wails 的文件仍是 `main_wails.go`（含根目录下按构建标签拆分的 `*_dev.go`）**。
- 已核实的 API：`app.SystemTray.New()`、`tray.SetIcon([]byte)`（PNG 或 ICO）、`menu.AddCheckbox(label, bool)`、`MenuItem.SetChecked/Checked/OnClick(func(*application.Context))`、`win.Flash(bool)`、`win.RegisterHook(events.Common.WindowClosing/WindowFocus, …)`、`WindowEvent.Cancel()`、`app.Quit()`。

---

## Task 1: 决策器（纯逻辑 + 表驱动单测）

**Files:**
- Create: `internal/app/notify_app.go`
- Test: `internal/app/notify_app_test.go`

**Step 1: 写失败的测试**

```go
package app

import (
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

// 环境与设置都是快照函数：测试里直接改闭包捕获的变量，模拟「窗口状态变了」。
func newNotifyFixture(t *testing.T, env *NotifyEnv, set *NotifySettings) (*NotifyApp, *clock.Fake) {
	t.Helper()
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	return NewNotifyApp(fc, func() NotifyEnv { return *env }, func() NotifySettings { return *set }), fc
}

func kinds(acts []Act) []ActKind {
	out := make([]ActKind, 0, len(acts))
	for _, a := range acts {
		out = append(out, a.Kind)
	}
	return out
}

func has(acts []Act, k ActKind) bool {
	for _, a := range acts {
		if a.Kind == k {
			return true
		}
	}
	return false
}

func inbound(convID string) message.Message {
	return message.Message{MsgID: "m1", ConvID: convID, Direction: message.DirectionIn}
}

// 规则表：新消息 × 四种环境
func TestChatMessageRules(t *testing.T) {
	cases := []struct {
		name      string
		env       NotifyEnv
		wantSound bool
		wantFlash bool
		wantUnread int
	}{
		{"隐藏到托盘 → 响+闪", NotifyEnv{WindowVisible: false}, true, true, 1},
		{"可见未聚焦 → 响+闪", NotifyEnv{WindowVisible: true, WindowFocused: false}, true, true, 1},
		{"聚焦且正是该会话 → 静默", NotifyEnv{WindowVisible: true, WindowFocused: true, ActiveConvID: "c1"}, false, false, 0},
		{"聚焦但别的会话 → 响+闪", NotifyEnv{WindowVisible: true, WindowFocused: true, ActiveConvID: "c2"}, true, true, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, set := tc.env, NotifySettings{Sound: true, MinimizeToTray: true}
			n, _ := newNotifyFixture(t, &env, &set)
			acts := n.OnChatMessage(inbound("c1"))

			if got := has(acts, ActPlaySound); got != tc.wantSound {
				t.Errorf("发声 = %v，期望 %v（动作=%v）", got, tc.wantSound, kinds(acts))
			}
			if got := has(acts, ActFlashOn) || has(acts, ActTrayAlert); got != tc.wantFlash {
				t.Errorf("闪烁 = %v，期望 %v", got, tc.wantFlash)
			}
			if n.Unread() != tc.wantUnread {
				t.Errorf("未读 = %d，期望 %d", n.Unread(), tc.wantUnread)
			}
		})
	}
}

// 自己发的消息永不提示（回显不该响）
func TestOutgoingMessageNeverNotifies(t *testing.T) {
	env, set := NotifyEnv{}, NotifySettings{Sound: true}
	n, _ := newNotifyFixture(t, &env, &set)
	m := inbound("c1")
	m.Direction = message.DirectionOut
	if acts := n.OnChatMessage(m); len(acts) != 0 {
		t.Fatalf("自己发的消息产生了动作：%v", kinds(acts))
	}
	if n.Unread() != 0 {
		t.Fatalf("未读 = %d，期望 0", n.Unread())
	}
}

// 传输事件只在窗口不可见时提示：用户可能正盯着传输列表看进度
func TestTransferEventsOnlyWhenHidden(t *testing.T) {
	for _, tc := range []struct {
		name    string
		visible bool
		want    bool
	}{
		{"隐藏 → 提示", false, true},
		{"可见（未聚焦也算）→ 静默", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, set := NotifyEnv{WindowVisible: tc.visible}, NotifySettings{Sound: true}
			n, _ := newNotifyFixture(t, &env, &set)

			for _, acts := range [][]Act{n.OnTransferDone(), n.OnTransferError()} {
				if has(acts, ActPlaySound) != tc.want {
					t.Errorf("发声 = %v，期望 %v", has(acts, ActPlaySound), tc.want)
				}
			}
		})
	}
}

// 节流：连发多条只响一声，但未读仍按条累加
func TestSoundThrottledButUnreadAccumulates(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: true}
	n, fc := newNotifyFixture(t, &env, &set)

	first := n.OnChatMessage(inbound("c1"))
	if !has(first, ActPlaySound) {
		t.Fatal("第一条应当发声")
	}

	fc.Advance(time.Second)
	second := n.OnChatMessage(inbound("c1"))
	if has(second, ActPlaySound) {
		t.Error("1 秒内的第二条不应再发声")
	}
	if n.Unread() != 2 {
		t.Errorf("未读 = %d，期望 2（节流只影响声音，不影响未读）", n.Unread())
	}

	fc.Advance(4 * time.Second)
	third := n.OnChatMessage(inbound("c1"))
	if !has(third, ActPlaySound) {
		t.Error("超过节流窗口后应恢复发声")
	}
}

// 静音开关：不发声，其余（未读、闪烁）照旧
func TestSoundDisabledStillCountsUnread(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: false}
	n, _ := newNotifyFixture(t, &env, &set)
	acts := n.OnChatMessage(inbound("c1"))
	if has(acts, ActPlaySound) {
		t.Error("静音时不应发声")
	}
	if !has(acts, ActTrayAlert) {
		t.Error("静音只关声音，闪烁与未读仍要工作")
	}
	if n.Unread() != 1 {
		t.Errorf("未读 = %d，期望 1", n.Unread())
	}
}

// 焦点回到窗口：停闪、未读归零、tooltip 复位
func TestWindowFocusClearsEverything(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: true}
	n, _ := newNotifyFixture(t, &env, &set)
	n.OnChatMessage(inbound("c1"))

	acts := n.OnWindowFocus()
	if !has(acts, ActFlashOff) || !has(acts, ActTrayNormal) {
		t.Errorf("焦点恢复应停闪并切回正常图标：%v", kinds(acts))
	}
	if n.Unread() != 0 {
		t.Errorf("未读 = %d，期望 0", n.Unread())
	}
	var tip string
	for _, a := range acts {
		if a.Kind == ActTooltip {
			tip = a.Text
		}
	}
	if tip != "SwarmLink" {
		t.Errorf("tooltip = %q，期望复位为 SwarmLink", tip)
	}
}

// 隐藏时靠交替托盘图标来「闪」：Tick 必须真的在两种图标间切换
func TestTickAlternatesTrayIconWhenHidden(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: true}
	n, _ := newNotifyFixture(t, &env, &set)
	n.OnChatMessage(inbound("c1"))

	seen := map[ActKind]int{}
	for i := 0; i < 4; i++ {
		for _, a := range n.Tick() {
			seen[a.Kind]++
		}
	}
	if seen[ActTrayAlert] == 0 || seen[ActTrayNormal] == 0 {
		t.Fatalf("托盘图标没有交替：%v（窗口隐藏时这是唯一的闪烁手段）", seen)
	}

	// 窗口恢复可见后不再换图标（有任务栏按钮，用 FlashWindow）
	env.WindowVisible = true
	if acts := n.Tick(); len(acts) != 0 {
		t.Errorf("可见时不该再换托盘图标：%v", kinds(acts))
	}
}

// 关闭请求：开关关闭 → 放行退出
func TestCloseAllowsQuitWhenDisabled(t *testing.T) {
	env, set := NotifyEnv{TrayOK: true}, NotifySettings{MinimizeToTray: false}
	n, _ := newNotifyFixture(t, &env, &set)
	if !has(n.OnWindowClose(), ActAllowQuit) {
		t.Fatal("开关关闭时必须放行退出")
	}
}

// 关闭请求：开关开启、托盘可用 → 隐藏到托盘
func TestCloseHidesToTrayWhenEnabled(t *testing.T) {
	env, set := NotifyEnv{TrayOK: true}, NotifySettings{MinimizeToTray: true}
	n, _ := newNotifyFixture(t, &env, &set)
	if !has(n.OnWindowClose(), ActHideToTray) {
		t.Fatal("开关开启且托盘可用时应隐藏到托盘")
	}
}

// 关键失败模式：托盘不可用时必须退化为真正退出，
// 否则用户会遇到「窗口消失、右下角没图标、彻底关不掉」。
func TestCloseFallsBackToQuitWhenTrayUnavailable(t *testing.T) {
	env, set := NotifyEnv{TrayOK: false}, NotifySettings{MinimizeToTray: true}
	n, _ := newNotifyFixture(t, &env, &set)
	if !has(n.OnWindowClose(), ActAllowQuit) {
		t.Fatal("托盘不可用时必须放行退出，不能让用户关不掉应用")
	}
}
```

**Step 2: 跑测试确认失败**

Run: `go test ./internal/app/ -run 'Notify|ChatMessageRules|TransferEvents|Close|Tick' -v`
Expected: FAIL —— `undefined: NotifyApp / Act / ActPlaySound …`

**Step 3: 实现**

创建 `internal/app/notify_app.go`：

```go
package app

import (
	"fmt"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// ActKind 是提示动作的类型。
//
// 用「值」而不是回调接口：测试只需断言「产生了哪些动作」，
// 不必 mock 一个 Wails 适配器；Wails 相关的执行全部留在组合根。
type ActKind int

const (
	ActPlaySound ActKind = iota
	ActFlashOn
	ActFlashOff
	ActTrayAlert  // 托盘图标切成带红点版
	ActTrayNormal // 切回正常版
	ActTooltip    // Text 为新 tooltip
	ActHideToTray // 关闭请求 → 隐藏窗口、保持进程
	ActAllowQuit  // 关闭请求 → 放行退出
)

// Act 是一个待执行的动作。
type Act struct {
	Kind ActKind
	Text string
}

// NotifyEnv 是决策所需的环境快照（窗口状态是 Wails 的事，这里只读值）。
type NotifyEnv struct {
	WindowVisible bool
	WindowFocused bool
	ActiveConvID  string
	// TrayOK 表示托盘图标是否真的建起来了：它是「关闭 = 收进托盘」的前提。
	TrayOK bool
}

// NotifySettings 是决策所需的设置快照。
type NotifySettings struct {
	Sound          bool
	MinimizeToTray bool
}

const (
	defaultSoundThrottle = 3 * time.Second
	trayTooltipIdle      = "SwarmLink"
)

// NotifyApp 决定「什么时候提示、提示成什么样」。
//
// 零 Wails 依赖、零 goroutine：环境与设置都由组合根以快照函数注入，
// 时间来自 ports.Clock，因此整张规则表都能用 FakeClock 表驱动测试。
type NotifyApp struct {
	clk ports.Clock
	env func() NotifyEnv
	set func() NotifySettings

	soundThrottle time.Duration
	lastSoundAt   time.Time
	soundPlayed   bool

	unread   int
	flashing bool
	alertOn  bool
}

// NewNotifyApp 构造决策器。
func NewNotifyApp(clk ports.Clock, env func() NotifyEnv, set func() NotifySettings) *NotifyApp {
	return &NotifyApp{clk: clk, env: env, set: set, soundThrottle: defaultSoundThrottle}
}

// Unread 返回 Go 侧自算的未读数（供组合根写 tooltip、也方便测试断言）。
func (n *NotifyApp) Unread() int { return n.unread }

// OnChatMessage 处理一条入站消息。
func (n *NotifyApp) OnChatMessage(m message.Message) []Act {
	if m.Direction != message.DirectionIn {
		return nil // 自己发的消息永不提示
	}
	e, s := n.env(), n.set()

	// 正看着这个会话：不打扰，也不计入未读（前端会把它标成已读）
	if e.WindowVisible && e.WindowFocused && e.ActiveConvID == m.ConvID {
		return nil
	}

	n.unread++
	acts := n.startFlash(e)
	acts = append(acts, n.soundActs(s)...)
	return append(acts, Act{Kind: ActTooltip, Text: n.tooltip()})
}

// OnTransferDone / OnTransferError：传输类提示只在窗口不可见时给。
//
// 理由：用户很可能正盯着右栏传输列表看进度，那时再响一声是纯噪声；
// 但窗口收在托盘时，传输失败现在完全无从得知 —— 那才是最该提醒的场景。
func (n *NotifyApp) OnTransferDone() []Act  { return n.onTransferEvent() }
func (n *NotifyApp) OnTransferError() []Act { return n.onTransferEvent() }

func (n *NotifyApp) onTransferEvent() []Act {
	e, s := n.env(), n.set()
	if e.WindowVisible {
		return nil
	}
	n.unread++
	acts := n.startFlash(e)
	acts = append(acts, n.soundActs(s)...)
	return append(acts, Act{Kind: ActTooltip, Text: n.tooltip()})
}

// OnWindowFocus 窗口重新获得焦点：停闪、未读归零、tooltip 复位。
func (n *NotifyApp) OnWindowFocus() []Act {
	n.flashing = false
	n.alertOn = false
	n.unread = 0
	return []Act{
		{Kind: ActFlashOff},
		{Kind: ActTrayNormal},
		{Kind: ActTooltip, Text: trayTooltipIdle},
	}
}

// OnWindowClose 回应窗口关闭请求。
func (n *NotifyApp) OnWindowClose() []Act {
	e, s := n.env(), n.set()
	if !s.MinimizeToTray || !e.TrayOK {
		// 开关关掉，或托盘根本没建起来。后者必须退化为真正退出：
		// 否则用户会遇到「窗口消失、右下角没图标、彻底关不掉」。
		return []Act{{Kind: ActAllowQuit}}
	}
	return []Act{{Kind: ActHideToTray}}
}

// Tick 由组合根的计时器驱动（约 600ms）：窗口隐藏期间交替托盘图标来「闪」。
//
// Wails 的托盘没有闪烁 API，换图标是唯一办法；有任务栏按钮时不用它（那边用 Flash）。
func (n *NotifyApp) Tick() []Act {
	if !n.flashing {
		return nil
	}
	if e := n.env(); e.WindowVisible {
		return nil
	}
	n.alertOn = !n.alertOn
	if n.alertOn {
		return []Act{{Kind: ActTrayAlert}}
	}
	return []Act{{Kind: ActTrayNormal}}
}

func (n *NotifyApp) startFlash(e NotifyEnv) []Act {
	n.flashing = true
	if e.WindowVisible {
		// 有任务栏按钮（含最小化）→ 闪按钮
		return []Act{{Kind: ActFlashOn}}
	}
	n.alertOn = true
	return []Act{{Kind: ActTrayAlert}}
}

// soundActs 施加节流：连续多条消息只响一次，避免「对方连发十条 = 十声」。
func (n *NotifyApp) soundActs(s NotifySettings) []Act {
	if !s.Sound {
		return nil
	}
	now := n.clk.Now()
	if n.soundPlayed && now.Sub(n.lastSoundAt) < n.soundThrottle {
		return nil
	}
	n.lastSoundAt, n.soundPlayed = now, true
	return []Act{{Kind: ActPlaySound}}
}

func (n *NotifyApp) tooltip() string {
	if n.unread <= 0 {
		return trayTooltipIdle
	}
	return fmt.Sprintf("%s · %d 条未读", trayTooltipIdle, n.unread)
}
```

**Step 4: 跑测试确认通过**

Run: `go test ./internal/app/ -run 'Notify|ChatMessageRules|TransferEvents|Close|Tick|Sound|Outgoing' -v`
Expected: PASS（全部用例）

**建议提交信息：** `feat(notify): 提示决策器（规则表/节流/未读/关闭退化）`

---

## Task 2: 合成提示音并入库

**Files:**
- Create: `internal/adapters/notify/assets/notify.wav`（生成物）
- Create: `internal/adapters/notify/assets_test.go`
- 临时工具：`tmp_wav/main.go`（用完删除）

**Step 1: 写素材自检测试**

```go
package notify

import (
	"encoding/binary"
	"testing"
)

// 提示音必须是合法的 RIFF/WAVE，时长在合理区间 —— 防的是「文件放错/生成截断，
// 表现为永远没声音」这种静默失效。
func TestSoundAssetIsValidWav(t *testing.T) {
	data := Sound()
	if len(data) < 44 {
		t.Fatalf("提示音只有 %d 字节，连 WAV 头都不够", len(data))
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatal("不是 RIFF/WAVE 文件")
	}

	byteRate := binary.LittleEndian.Uint32(data[28:32])
	dataSize := binary.LittleEndian.Uint32(data[40:44])
	if byteRate == 0 {
		t.Fatal("byteRate 为 0，音频参数不合法")
	}
	seconds := float64(dataSize) / float64(byteRate)
	if seconds < 0.3 || seconds > 1.0 {
		t.Fatalf("时长 %.2fs，期望 0.3~1.0s（太短听不清、太长会烦）", seconds)
	}
}
```

**Step 2: 跑测试确认失败**

Run: `go test ./internal/adapters/notify/ -v`
Expected: FAIL —— `undefined: Sound`

**Step 3: 写生成工具并产出资产**

`tmp_wav/main.go` 要点（一次性工具，生成后删除）：

- 格式：44100Hz / 16-bit / 单声道（PCM）。
- 音型：第一音 1318Hz（E6）60ms、第二音 880Hz（A5）180ms，每音 `exp(-t/τ)` 衰减，τ 分别约 45ms / 90ms；
- 尾巴：把第二音的尾音叠一个 0.25 幅度、延迟 90ms 的衰减副本（轻微回响，让声音不干瘪）；
- 峰值归一化到 **-6dBFS**（0.5 满量程），避免比系统提示音更响、也不爆音；
- 总长约 0.55s，尾部 30ms 淡出，避免截断爆音。

Run: `go run ./tmp_wav internal/adapters/notify/assets/notify.wav`
Expected: 生成 `notify.wav`（约 48KB）

同时导一份给人工试听：
```powershell
Copy-Item internal/adapters/notify/assets/notify.wav "$env:TEMP\swarmlink-notify.wav"
```
（我会把路径告诉你，双击即可试听；不满意就调参数重生成。）

**Step 4: 让 `Sound()` 可用**

创建 `internal/adapters/notify/notify.go`：

```go
// Package notify 提供与提示相关的平台能力：提示音播放 + 托盘图标素材。
//
// 它刻意不认识 Wails、也不认识事件桥 —— 两者都由组合根接线（红线 R2）。
package notify

import _ "embed"

//go:embed assets/notify.wav
var soundWav []byte

//go:embed assets/tray.png
var trayIcon []byte

//go:embed assets/tray-alert.png
var trayAlertIcon []byte

// Sound 返回内嵌提示音的 WAV 字节。
func Sound() []byte { return soundWav }

// TrayIcon 返回托盘正常状态图标（PNG）。
func TrayIcon() []byte { return trayIcon }

// TrayAlertIcon 返回托盘「有未读」图标（PNG，带红点角标）。
func TrayAlertIcon() []byte { return trayAlertIcon }
```

> Task 3 会产出两张 PNG；在此之前先放占位 PNG 让本 Task 的测试通过，Task 3 结束时会覆盖为正式素材。

**Step 5: 跑测试确认通过**

Run: `go test ./internal/adapters/notify/ -v`
Expected: PASS

**Step 6: 删除临时工具**

Run: `Remove-Item -Recurse -Force tmp_wav`

**建议提交信息：** `feat(notify): 内嵌合成提示音与托盘图标素材`

---

## Task 3: 托盘图标素材（无文字版 + 红点版）

**Files:**
- Create: `internal/adapters/notify/assets/tray.png`、`tray-alert.png`（生成物）
- Modify: `internal/adapters/notify/assets_test.go`（加 PNG 自检）
- 临时工具：`tmp_trayicon/main.go`（用完删除）

**Step 1: 写失败的测试**

```go
func TestTrayIcons(t *testing.T) {
	normal, alert := TrayIcon(), TrayAlertIcon()
	for name, data := range map[string][]byte{"tray.png": normal, "tray-alert.png": alert} {
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s 解码失败: %v", name, err)
		}
		if img.Bounds().Dx() != 32 || img.Bounds().Dy() != 32 {
			t.Errorf("%s 尺寸 %v，期望 32x32（Wails 会缩到系统小图标 16x16）", name, img.Bounds())
		}
	}

	// 两版必须真的不同，否则「闪烁」看起来毫无变化 —— 静默失效
	diff := 0
	a, _ := png.Decode(bytes.NewReader(normal))
	b, _ := png.Decode(bytes.NewReader(alert))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			if a.At(x, y) != b.At(x, y) {
				diff++
			}
		}
	}
	if diff < 20 {
		t.Fatalf("两版图标只有 %d 个像素不同，闪烁会看不出来", diff)
	}
}
```

**Step 2: 跑测试确认失败**

Run: `go test ./internal/adapters/notify/ -v`
Expected: FAIL —— 尺寸不是 32×32 或两版相同（占位素材）

**Step 3: 生成正式素材**

`tmp_trayicon/main.go` 要点：

1. 读 `build/appicon.png`（1024×1024，已加工过）；
2. **抹掉产品名文字**：文字区域在源图 500px 坐标系里是 `x 99..400, y 366..404`，换算到 1024 画布（源图裁剪区为 466px、含 8% 留白）后按比例定位；用该区域**上下两侧的背景色做垂直线性插值**填充（该区域是深蓝底渐变，16px 下看不出修补痕迹）；
3. 缩放为 32×32（CatmullRom），输出 `tray.png`；
4. `tray-alert.png` = 同图 + 右下角实心红点（直径 11px，圆心距右下角各 6px，外圈 1px 深色描边保证在浅色任务栏上也可辨）；
5. 自检：两图差异像素数 > 20，否则报错退出。

Run: `go run ./tmp_trayicon internal/adapters/notify/assets/tray.png internal/adapters/notify/assets/tray-alert.png`

同时导一份放大预览到 `%TEMP%\swarmlink-tray-preview.png`（32×32 与 16×16 并排，深/浅两种底），供人工确认 16px 下是否可辨。

**Step 4: 跑测试确认通过**

Run: `go test ./internal/adapters/notify/ -v`
Expected: PASS

**Step 5: 删除临时工具**

Run: `Remove-Item -Recurse -Force tmp_trayicon`

**建议提交信息：** `feat(notify): 托盘图标（无文字版 + 红点版）`

---

## Task 4: 声音适配器（Windows 实现 + 其它平台空实现）

**Files:**
- Create: `internal/adapters/notify/play_windows.go`
- Create: `internal/adapters/notify/play_other.go`

**Step 1: Windows 实现**

```go
//go:build windows

package notify

import (
	"sync"
	"syscall"
	"unsafe"
)

var (
	winmm         = syscall.NewLazyDLL("winmm.dll")
	procPlaySound = winmm.NewProc("PlaySoundW")
)

const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndMemory    = 0x0004
)

var playMu sync.Mutex // PlaySound 是进程级的，串行化避免互相打断

// Play 异步播放内嵌提示音。
//
// 失败一律静默：提示音是锦上添花，绝不能因为它弹错误或阻塞调用方。
// SND_MEMORY 要求缓冲区在播放期间保持有效 —— soundWav 是包级变量，天然满足。
func Play() {
	if len(soundWav) == 0 {
		return
	}
	playMu.Lock()
	defer playMu.Unlock()
	procPlaySound.Call(
		uintptr(unsafe.Pointer(&soundWav[0])),
		0,
		uintptr(sndAsync|sndMemory|sndNoDefault),
	)
}
```

**Step 2: 其它平台空实现**

```go
//go:build !windows

package notify

// Play 在非 Windows 平台是空实现：不响、不崩。
//
// 要做需要各自平台的原生播放（macOS afplay / Linux paplay 或 GStreamer），
// 本次不做 —— 宁可不响，也不要引入半成品的音频栈。
func Play() {}
```

**Step 3: 编译验证（两个平台都要过编译这关）**

Run: `go build ./internal/adapters/notify/ && $env:GOOS='linux'; go build ./internal/adapters/notify/; $env:GOOS=''
Expected: 两次都无输出

**建议提交信息：** `feat(notify): 声音适配器（winmm PlaySound + 非 Windows 空实现）`

---

## Task 5: 配置 [notify] 段 + DTO/绑定 + NotifyService

**Files:**
- Modify: `internal/adapters/config/config.go`（`Notify` 段 + 默认值）
- Modify: `internal/adapters/wails/dto.go`、`internal/adapters/wails/services.go`（SettingsDTO 两字段 + `NotifyService`）
- Test: `internal/adapters/config/config_test.go`（若存在则追加）、`internal/adapters/wails/services_test.go`（追加）

**Step 1: 写失败的测试**

config 侧：

```go
func TestNotifyDefaultsAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Notify.Sound || !cfg.Notify.MinimizeToTray {
		t.Fatalf("默认应两项全开，得到 %+v", cfg.Notify)
	}

	cfg.Notify.Sound = false
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	back, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back.Notify.Sound {
		t.Fatal("sound=false 没有落盘")
	}
	if !back.Notify.MinimizeToTray {
		t.Fatal("未改动的字段被写坏了")
	}
}
```

services 侧（NotificationService 只做「把当前会话告诉 Go」，因此测试它真的落到了决策器的环境快照里）：

```go
func TestNotifyServiceTracksActiveConversation(t *testing.T) {
	got := ""
	svc := &NotifyService{d: Deps{SetActiveConv: func(id string) { got = id }}}
	if err := svc.SetActiveConversation("conv-1"); err != nil {
		t.Fatal(err)
	}
	if got != "conv-1" {
		t.Fatalf("当前会话 = %q，期望 conv-1", got)
	}
}
```

**Step 2: 跑测试确认失败**

Run: `go test ./internal/adapters/config/ ./internal/adapters/wails/ -run 'Notify' -v`
Expected: FAIL —— `cfg.Notify undefined`、`undefined: NotifyService`

**Step 3: 实现**

`config.go`：加结构体与默认值

```go
// Notify 是提示行为设置。
type Notify struct {
	// Sound 控制新消息 / 传输事件是否发声。
	Sound bool `toml:"sound"`
	// MinimizeToTray 为真时，点关闭按钮把窗口收进托盘而不是退出。
	MinimizeToTray bool `toml:"minimize_to_tray"`
}

// 在 Default() 里：
c.Notify.Sound = true
c.Notify.MinimizeToTray = true
```

`services.go`：`SettingsDTO` 增加 `NotifySound bool` / `NotifyMinimizeToTray bool`（`ToSettingsDTO` 与 `Save` 双向映射），并新增薄服务：

```go
// NotifyService 只承载「前端告诉 Go 当前在看哪个会话」这一件事。
//
// 不塞进 ChatService：它和聊天读写无关，只是提示决策的一个环境输入。
type NotifyService struct{ d Deps }

// SetActiveConversation 记录前端当前打开的会话 id（窗口焦点/可见性由 Go 侧直接读）。
func (s *NotifyService) SetActiveConversation(convID string) error {
	if s.d.SetActiveConv == nil {
		return nil
	}
	s.d.SetActiveConv(convID)
	return nil
}
```

**Step 4: 注册服务并重新生成绑定**

- `main_wails.go` 的 `Services:` 列表加入 `application.NewService(services.Notify)`。
- Run: `wails3 generate bindings -f "-tags wails" -clean=true -ts -i .`
- Expected: 生成 `notifyservice.ts`；`settingsservice` 的模型里出现两个新字段。

**Step 5: 跑测试确认通过**

Run: `go test ./internal/adapters/config/ ./internal/adapters/wails/ -v`
Expected: PASS

**建议提交信息：** `feat(config,notify): [notify] 设置段、DTO 与服务`

---

## Task 6: 组合根接线（托盘 / 关闭拦截 / 焦点 / 动作执行 / DEV 测试项）

**Files:**
- Modify: `main_wails.go`
- Create: `dev_notify_menu.go`（`//go:build !production`，DEV 专用的测试提示菜单项）
- Test: 无自动化（Wails 侧），靠 Task 8 的手工走查

**Step 1: 环境快照与动作执行**

`main_wails.go` 中（服务构造之后、`app.Run()` 之前）：

```go
// ---------------------------------------------------------------- 提示决策
// 环境快照：窗口状态由 Wails 直接读，当前会话由前端经 NotifyService 上报。
var (
	activeConvMu sync.RWMutex
	activeConvID string
)
envSnapshot := func() app.NotifyEnv {
	activeConvMu.RLock()
	conv := activeConvID
	activeConvMu.RUnlock()
	return app.NotifyEnv{
		WindowVisible: win.IsVisible(),
		WindowFocused: win.IsFocused(),
		ActiveConvID:  conv,
		TrayOK:        trayOK.Load(),
	}
}
settingsSnapshot := func() app.NotifySettings {
	// 每次实时读配置：设置页或托盘菜单改完立即生效，不需要重启
	cfg := cfgRef.Load()
	return app.NotifySettings{Sound: cfg.Notify.Sound, MinimizeToTray: cfg.Notify.MinimizeToTray}
}
notifier := app.NewNotifyApp(node.Clk, envSnapshot, settingsSnapshot)

runActs := func(acts []app.Act) {
	for _, a := range acts {
		switch a.Kind {
		case app.ActPlaySound:
			notify.Play()
		case app.ActFlashOn:
			win.Flash(true)
		case app.ActFlashOff:
			win.Flash(false)
		case app.ActTrayAlert:
			tray.SetIcon(notify.TrayAlertIcon())
		case app.ActTrayNormal:
			tray.SetIcon(notify.TrayIcon())
		case app.ActTooltip:
			tray.SetTooltip(a.Text)
		}
	}
}
```

**Step 2: 托盘 + 菜单（含两个开关）**

```go
tray := app.SystemTray.New()
tray.SetIcon(notify.TrayIcon())
tray.SetTooltip("SwarmLink")

showWindow := func() {
	win.Show()
	win.Restore()
	win.Focus()
}

menu := app.NewMenu()
menu.Add("显示主窗口").OnClick(func(*application.Context) { showWindow() })
soundItem := menu.AddCheckbox("新消息提示音", true).SetChecked(cfg.Notify.Sound)
soundItem.OnClick(func(*application.Context) { setNotifyOption(notifyOptSound, soundItem.Checked()) })
trayItem := menu.AddCheckbox("关闭时最小化到托盘", true).SetChecked(cfg.Notify.MinimizeToTray)
trayItem.OnClick(func(*application.Context) { setNotifyOption(notifyOptTray, trayItem.Checked()) })
addDevMenuItems(menu) // 仅 DEV 构建：见 dev_notify_menu.go
menu.AddSeparator()
menu.Add("退出").OnClick(func(*application.Context) { app.Quit() })
tray.SetMenu(menu)
tray.OnClick(showWindow)
trayOK.Store(true)
```

`setNotifyOption` 写配置并落盘，然后复用已有事件通知前端：

```go
// 托盘菜单与设置页共用同一份状态：改完立即落盘 + 推 config:changed，
// 前端据此刷新设置页开关，因此两个入口不会出现状态不一致。
func setNotifyOption(opt notifyOption, on bool) {
	cfg := cfgRef.Load()
	switch opt {
	case notifyOptSound:
		cfg.Notify.Sound = on
	case notifyOptTray:
		cfg.Notify.MinimizeToTray = on
	}
	cfgRef.Store(cfg)
	if err := config.Save(node.ConfigDir, &cfg); err != nil {
		slog.Warn("保存提示设置失败", "err", err)
	}
	node.Bus.Publish(eventbus.TopicConfigChanged, nil)
}
```

**Step 3: 关闭拦截与焦点钩子**

```go
win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
	for _, a := range notifier.OnWindowClose() {
		if a.Kind == app.ActHideToTray {
			e.Cancel()
			win.Hide()
			// 「我在这儿」：隐藏后闪两下托盘图标，否则用户会以为程序退出了
			go blinkTrayTwice()
			return
		}
	}
	// 走到这里即 ActAllowQuit：不取消，正常退出
})

win.RegisterHook(events.Common.WindowFocus, func(*application.WindowEvent) {
	runActs(notifier.OnWindowFocus())
})
```

**Step 4: 事件订阅与托盘闪烁计时器**

```go
node.Bus.Subscribe(eventbus.TopicChatReceived, func(payload any) {
	if m, ok := payload.(message.Message); ok {
		runActs(notifier.OnChatMessage(m))
	}
})
// 传输事件的载荷类型以 internal/adapters/wails/eventbridge.go 里既有映射为准
node.Bus.Subscribe(eventbus.TopicTransferDone, func(any) { runActs(notifier.OnTransferDone()) })
node.Bus.Subscribe(eventbus.TopicTransferError, func(any) { runActs(notifier.OnTransferError()) })

// 托盘图标的闪烁只能靠换图标：600ms 交替一次，窗口恢复后 Tick 自己会停。
go func() {
	t := time.NewTicker(600 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runActs(notifier.Tick())
		}
	}
}()
```

**Step 5: DEV 专用「测试提示」菜单项**

创建 `dev_notify_menu.go`：

```go
//go:build !production

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// addDevMenuItems 只在开发构建里加一项「测试提示」。
//
// 目的：托盘、闪烁、提示音这些行为无法单测，而「等真实消息」既慢又不确定性。
// 一个能手动触发的假提示，让一台机器就能走完整个手工验收清单。
func addDevMenuItems(menu *application.Menu) {
	menu.AddSeparator()
	menu.Add("测试提示（仅开发）").OnClick(func(*application.Context) {
		fake := message.Message{MsgID: "dev-test", ConvID: "dev-test-conv", Direction: message.DirectionIn}
		runActs(notifier.OnChatMessage(fake))
	})
}
```

> 注意：`runActs` / `notifier` 需为包级变量，`dev_notify_menu.go` 才能直接引用（否则改成把回调传进来）。生产文件里提供空的 `addDevMenuItems`（`//go:build production`）保持接口一致。

**Step 6: 编译两种构建**

Run: `go build -tags wails -o bin/SwarmLink.exe .` 然后 `$env:EXTRA_TAGS='wails'; wails3 build DEV=true`
Expected: 两次都成功；DEV 构建的托盘菜单里有「测试提示（仅开发）」，生产构建里没有

**建议提交信息：** `feat(tray): 组合根接线（托盘/关闭拦截/焦点/动作执行）与 DEV 测试提示项`

---

## Task 7: 前端两处改动

**Files:**
- Modify: `frontend/src/api/types.ts`（`Settings` 加两个字段、`SwarmApi` 加 `setActiveConversation`）
- Modify: `frontend/src/api/bindings.ts`、`frontend/src/api/mock.ts`
- Modify: `frontend/src/stores/settings.ts`（或对应 store）
- Modify: `frontend/src/views/SettingsDialog.vue`（两个开关）
- Modify: `frontend/src/stores/chat.ts` 或 `App.vue`（watch activeConvId → 上报）

**Step 1: 上报当前会话（一处 watch）**

在 `App.vue` 的 `onMounted` 里（store 已就绪之后）：

```ts
// 把「用户正在看哪个会话」告诉 Go：提示决策需要它来区分
// 「正看着这个会话（不打扰）」与「别的会话来消息（要提示）」。
watch(
  () => chat.activeConvId,
  async (id) => {
    try {
      await (await getApi()).setActiveConversation(id ?? '')
    } catch {
      /* 提示是附加能力，上报失败不影响聊天 */
    }
  },
  { immediate: true }
)
```

**Step 2: 设置页两个开关**

按现有设置项样式加：

```vue
<label class="switch">
  <input v-model="form.notifySound" type="checkbox" />
  <span>新消息提示音</span>
</label>
<label class="switch">
  <input v-model="form.notifyMinimizeToTray" type="checkbox" />
  <span>关闭时最小化到托盘</span>
</label>
```

（字段名与 Task 5 的 DTO 对齐；保存走既有的 `settings.save`。）

**Step 3: 类型、绑定映射、mock 同步**

- `types.ts`：`Settings` 加 `notifySound: boolean` / `notifyMinimizeToTray: boolean`；`SwarmApi` 加 `setActiveConversation(convId: string): Promise<void>`。
- `bindings.ts`：`SettingsService.Get/Save` 的映射带上新字段；`setActiveConversation: (id) => NotifyService.SetActiveConversation(id)`（注释掉返回值即可）。
- `mock.ts`：新字段默认 true；`setActiveConversation` 空实现。
- 兼容旧配置：后端 DTO 已有默认值（true），前端不必兜底。

**Step 4: 验证**

Run: `cd frontend; npm run typecheck && npm run test && npm run build`
Expected: 全部通过

**建议提交信息：** `feat(web): 提示设置开关与当前会话上报`

---

## Task 8: 验收

**Step 1: 全量 Go 测试**

Run: `go test ./... -count=1`
Expected: 本次涉及的包全绿（`filesink`/`keystore`/`fsutil` 三个包在 Windows 上因 POSIX 语义失败是既有问题，与本次无关）

**Step 2: 构建与启动**

Run: `$env:EXTRA_TAGS='wails'; wails3 build DEV=true`
Expected: 产出 `bin/SwarmLink.exe`，托盘常驻

**Step 3: 手工走查（14 条，用 DEV 的「测试提示」触发）**

1. 点关闭 → 窗口消失、托盘出现图标、进程仍在（任务管理器可见）；
2. 点托盘图标 → 窗口恢复并聚焦；
3. 点最小化 → 留在任务栏（不收进托盘）；
4. 隐藏状态下触发测试提示 → 托盘图标交替闪 + 响一声 + tooltip 显示 `SwarmLink · 1 条未读`；
5. 恢复窗口 → 停闪、tooltip 复位为 `SwarmLink`；
6. 最小化后触发 → 任务栏按钮闪 + 响；
7. 窗口可见但被别的窗口盖住时触发 → 闪 + 响；
8. 正看着测试会话（`dev-test-conv` 不可达，改用真实会话验证）时来消息 → 不响不闪；
9. 正看 A 会话、B 来消息 → 响 + 闪；
10. 设置页关掉提示音 → 不再响；托盘菜单里的勾选同步变化（反向再试一次）；
11. `关闭时最小化到托盘` 关掉后点关闭 → 真的退出；托盘菜单「退出」永远能退出；
12. 重启应用 → 两个开关状态保持；
13. 隐藏状态下让一个传输失败（对端退出/断网）→ 有提示音 + 闪；
14. **开发模式回归**：`start_dev.bat` 启动后 Ctrl+C 仍能正常退出。

**Step 4: 跨平台编译检查**

Run: `$env:GOOS='darwin'; go build ./...; $env:GOOS='linux'; go build ./...; $env:GOOS=''`
Expected: 均通过（声音为空实现、托盘用 Wails 原生支持）

---

## 风险与已知取舍

1. **托盘创建时机**：Wails 的托盘 setter 在应用未运行时是「缓存」语义（`systemtray.go` 里 `impl == nil` 时只存值）。若在 `app.Run()` 之前配置未生效，改为在 `app.PostShutdown` 之外的生命周期点创建 —— 实施时先跑一遍确认，不靠猜。
2. **托盘图标的 16px 可读性**：无文字版已按此优化，但仍需人工在真实托盘里确认（Task 3 的预览图 + Task 8 第 4 条）。
3. **未读数与前端未读可能短暂不一致**：以「窗口获得焦点」为归零点，这是刻意的简化（设计文档 §9）。
4. **声音只有 Windows 实现**：其它平台静默不响，不影响功能。

---

## 实施差异记录（执行期核实，覆盖本计划中的对应描述）

按批次执行时发现以下几处计划与实际不符，均已按「实际」实现。**后来者请以本节为准。**

1. **`app` 包名被遮蔽**：`main_wails.go` 里 `app := application.New(...)` 已经是 Wails 的 `*application.App`，因此 `internal/app` 必须以别名 `capp` 导入。计划里 `app.NotifyEnv` / `app.NewNotifyApp` 的写法原样编译不过。
2. **不存在 `cfgRef`**：`SettingsService.Save` 是就地修改 `*config.Config`（与 `node.Cfg` 同一个指针），所以设置快照直接读 `node.Cfg.Notify` 本身就是实时的，不需要额外的原子引用层。
3. **`TopicConfigChanged` 不能用于通知前端**：`eventbus.TopicConfigChanged` 只有常量定义 —— 全仓库没有任何发布者，事件桥也没订阅它。托盘菜单改完设置后要刷新设置页，只能由组合根直接 `app.Event.Emit(wadapter.EventConfigChanged, nil)`。（Task 5 Step 3 里的 `node.Bus.Publish(eventbus.TopicConfigChanged, nil)` 发出去不会有任何效果。）
4. **DEV 菜单用参数传递**：`addDevMenuItems(menu, notifier, runActs)`，不为一个调试菜单项引入包级可变状态；生产空实现在 `dev_notify_menu_prod.go`（`//go:build production`）。二进制字符串比对已证实该菜单项只出现在开发构建里。
5. **步骤 5-2 的 `prepareImageContent` 与步骤 6-3 的 `setNotifyOption` 写法**：前者属图片功能（已按实际拆成 `prepareImageFile` / `prepareImageData`），后者按第 3 条改为直接发前端事件。
6. **计划漏掉一个真实缺陷（已修）**：`NotifyApp` 会被事件总线（多个 goroutine）与闪烁计时器同时调用，原设计没有任何同步。`go test -race` 实测报 `DATA RACE`（`notify_app.go` 的 `unread` 与 tooltip 读取）。现已在内部状态上加锁，并遵守「锁内绝不调用 `env()`/`set()`」的纪律（那两个快照会经 `InvokeSync` 阻塞等主线程，持锁调用与主线程上的焦点钩子构成死锁）。
7. **Task 8 Step 4 的跨平台编译预期不成立**：`GOOS=darwin|linux go build ./...` 会失败，但 11 条错误**全部**落在 `github.com/wailsapp/wails/v3/pkg/application` 内部（平台文件需要 cgo 与目标平台工具链），指向本仓库的错误为 0 条。这是 Wails 的交叉编译限制，不是本次改动引入的；本次新增的 `internal/adapters/notify` 单独在三个平台都能编译通过。
8. **设置页文件是 `views/SettingsView.vue`**（不是计划里写的 `SettingsDialog.vue`，后者只是承载弹层与分页）；前端也**没有** `stores/settings`，设置项由该视图自己持有。
9. **托盘 → 设置页的刷新**：新增 `ui.configRevision` 计数器，`config:changed` 时自增，设置页 watch 后**只回读那两个提示开关**（整份重载会冲掉用户没保存的种子编辑）。
10. **未读数从托盘 tooltip 改到窗口标题**（`ActTooltip` → `ActWindowTitle`）：Windows 下 Wails 启用了 `NOTIFYICON_VERSION_4`，该系统模式会抑制标准 tooltip，实测悬停毫无反应（连初始的 `SwarmLink` 都不显示）。因此删掉 `tray.SetTooltip` 相关代码，改由 `win.SetTitle` 承载 `SwarmLink · N 条未读`；决策层的规则表与测试不变，只换了渲染位置。详见设计文档 §9。
11. **本轮踩到的工具坑（写给后来者）**：不要用 PowerShell 的 `Get-Content`/`-replace`/`WriteAllText` 去改含中文的 Go 文件 —— 它按 ANSI 读、按 UTF-8 写，会把中文双重编码成乱码（连 `fmt.Sprintf` 的格式串都会被破坏），而新文件不在 git 里、无法回滚。改代码一律用编辑工具。
12. **悬停提示改为自建原生实现**（`internal/adapters/notify/hovertip_{windows,other}.go`）：Wails 的 `SetTooltip` 在 Windows 上无效（原因见设计文档 §9），而需求是「QQ 式：悬停看到是谁发来的」，只能自己做。用系统的 `tooltips_class32` 控件而非自绘窗口。落地时间踩到两个 Win32 坑，**都已写进代码注释**：
    - **`TOOLINFOW` 的 `cbSize` 必须是 64**：x64 下加上 `lpReserved` 就是 72，控件只接受它「认识」的尺寸，`TTM_ADDTOOLW` 返回 0 且**不给任何错误码**，表现为「悬停毫无反应」。用一次性探针遍历 6 种 cbSize 才定位到（72 拒绝 / 64 及更小接受）。
    - **窗口属于创建它的线程**：Go 的 goroutine 会在 OS 线程间迁移，实测「同一个 goroutine 里先 `CreateWindowEx` 再 `SendMessage`」会**永久阻塞**（测试挂了 10 分钟）。因此 `HoverTip` 自建专用线程（`LockOSThread` + `GetMessage` 泵 + 自动重置事件唤醒），所有窗口操作都在那条线程上，外部调用者不必关心线程。
    - 决策层新增 `HoverText()`（拉取语义）与「最近发送者」状态；`ActTooltip` 早已改名为 `ActWindowTitle`（见第 10 条），两者并存：标题给计数，悬停给「谁+什么内容」。
    - 另附：`write_to_file` 在 Windows 上会写成 CRLF，而本仓库既有文件是 LF → 新建文件会被 `gofmt -l` 标记。已把本次新建文件按字节归一为 LF（不是文本替换，是纯字节 CRLF→LF），并修掉了 `services_test.go` 一处真实的 gofmt 对齐问题。
