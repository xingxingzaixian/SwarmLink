package wails

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
)

// GlobalShortcutIDs 是必须在 Go 侧注册的命令 id（应用失焦也要能按）。
//
// 前端命令表（frontend/src/hotkeys/commands.ts）里同样把这些命令标成
// scope: 'global' —— 两处必须一致。加一条全局热键时两边都要改：
// Go 侧负责真的注册，前端只负责在设置页画出来。
var GlobalShortcutIDs = []string{"screenshot", "toggle_window"}

// ShortcutResultDTO 是某个全局热键的注册现状。
//
// Error 非空表示「这个键没绑上」，最常见的原因是别的程序已经占着它
// （QQ / 微信 / 输入法 / 显卡驱动都会抢）。此时用户的设置【不清空】：
// 换一台机器、或者关掉那个程序之后，同一个键位就会生效。
type ShortcutResultDTO struct {
	ID          string `json:"id"`
	Accelerator string `json:"accelerator"`
	Error       string `json:"error"`
}

// ShortcutsDTO 是快捷键配置的完整快照。
//
// 只有 Overrides（用户覆盖）与 Global（全局键注册现状）两项：
// 窗口内命令的生效值由前端用「覆盖 ?? 命令表默认值」算出来，
// 后端不需要、也不应该知道那些默认值。
type ShortcutsDTO struct {
	Overrides map[string]string   `json:"overrides"`
	Global    []ShortcutResultDTO `json:"global"`
}

// globalShortcutApplier 由组合根注入（只有它碰 Wails API）。
//
// 入参是「命令 id → 生效加速键字符串」（只含全局命令，空串 = 不绑定），
// 返回值是「命令 id → 注册失败原因」。成功的不出现在返回值里。
var globalShortcutApplier struct {
	mu    sync.RWMutex
	apply func(map[string]string) map[string]error
}

// SetGlobalShortcutApplier 注入全局热键的注册能力（组合根在 app 创建后调用）。
func SetGlobalShortcutApplier(fn func(map[string]string) map[string]error) {
	globalShortcutApplier.mu.Lock()
	defer globalShortcutApplier.mu.Unlock()
	globalShortcutApplier.apply = fn
}

// applyGlobal 调一次注入的注册器。未注入时（例如没有 GUI 的构建）返回空结果：
// 快捷键本身不是启动前提，不能因为它没接上就让应用起不来。
func applyGlobal(keys map[string]string) map[string]error {
	globalShortcutApplier.mu.RLock()
	fn := globalShortcutApplier.apply
	globalShortcutApplier.mu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn(keys)
}

// ShortcutService 读写快捷键配置，并把全局热键同步给系统。
//
// 它独立于 SettingsService 而不是挂在那上面：SettingsService.Save 的语义是
// 「整份设置表单全量覆盖」，而快捷键在另一个页签、且是即时生效的 ——
// 共用一条保存链路会变成「点保存把对方页签的清空」。
type ShortcutService struct {
	mu sync.Mutex
	d  Deps
	// results 是最近一次全局注册的结果（启动时算一次，之后每次 Apply/Reset 刷新）。
	// Get 要返回它：用户打开设置页时看到的「已占用」来自这里，而不是重新注册一次
	// —— 重注册会短暂解绑再绑，按一次正好落在窗口期的用户就会以为快捷键坏了。
	results []ShortcutResultDTO
}

// Get 返回当前快捷键快照（不触发任何注册动作）。
func (s *ShortcutService) Get() ShortcutsDTO {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// Apply 整体替换用户的覆盖表，落盘后重新注册全局热键。
//
// 语义是「整体替换」而不是「增量合并」：前端每次提交的是它手上的完整覆盖表，
// 合并语义会让「把某个键改回默认」变成永远删不掉的一条脏数据。
func (s *ShortcutService) Apply(overrides map[string]string) (ShortcutsDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	clean := make(map[string]string, len(overrides))
	for id, accel := range overrides {
		id = strings.TrimSpace(id)
		accel = strings.TrimSpace(accel)
		if id == "" {
			continue
		}
		if err := validateAccelerator(accel, isGlobalID(id)); err != nil {
			return s.snapshotLocked(), fmt.Errorf("快捷键 %s 无效：%w", id, err)
		}
		if accel == "" {
			// 解绑也是一条「设置」，必须落盘，否则下次启动又会用默认键。
			clean[id] = ""
			continue
		}
		clean[id] = accel
	}

	cfg := s.d.Cfg
	prev := cfg.Shortcuts.Keys
	cfg.Shortcuts.Keys = clean
	// 落盘失败就【不】改注册，并把内存里的表一并还原：
	// 磁盘上写的与实际生效的必须是同一件事，否则用户会看到
	// 「设置页显示改成功了、重启后变回去」，而日志里什么都没有。
	if err := config.Save(s.d.ConfigDir, cfg); err != nil {
		cfg.Shortcuts.Keys = prev
		return s.snapshotLocked(), fmt.Errorf("保存快捷键设置失败：%w", err)
	}

	s.registerLocked()
	return s.snapshotLocked(), nil
}

// Reset 清空全部覆盖值，回到出厂键位。
func (s *ShortcutService) Reset() (ShortcutsDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.d.Cfg
	prev := cfg.Shortcuts.Keys
	cfg.Shortcuts.Keys = nil
	if err := config.Save(s.d.ConfigDir, cfg); err != nil {
		cfg.Shortcuts.Keys = prev
		return s.snapshotLocked(), fmt.Errorf("恢复默认快捷键失败：%w", err)
	}
	s.registerLocked()
	return s.snapshotLocked(), nil
}

// ReloadGlobal 按当前配置注册一次全局热键（组合根在应用启动完成后调用）。
//
// 为什么不在 Run() 之前注册：Wails 会把启动前的注册延后到此时统一绑定，
// 而绑定失败只会进全局 error handler —— 调用方拿不到原因，设置页也就
// 没法显示「被哪个程序占了」。等应用真正起来之后再注册，错误是同步返回的。
func (s *ShortcutService) ReloadGlobal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registerLocked()
}

// registerLocked 把全局键同步给系统并记下结果。调用方必须持有 s.mu。
func (s *ShortcutService) registerLocked() {
	effective := make(map[string]string, len(GlobalShortcutIDs))
	system := make(map[string]string, len(GlobalShortcutIDs))
	for _, id := range GlobalShortcutIDs {
		// DTO 里返回【可移植写法】（Ctrl 表示“主修饰键”），交给系统的则是
		// 该平台的真实键名 —— 两者不是一回事，详见 toSystemAccelerator。
		effective[id] = s.d.Cfg.Shortcut(id)
		system[id] = toSystemAccelerator(runtime.GOOS, effective[id])
	}
	failed := applyGlobal(system)

	s.results = make([]ShortcutResultDTO, 0, len(GlobalShortcutIDs))
	for _, id := range GlobalShortcutIDs {
		r := ShortcutResultDTO{ID: id, Accelerator: effective[id]}
		if err := failed[id]; err != nil {
			r.Error = err.Error()
		}
		s.results = append(s.results, r)
	}
}

// toSystemAccelerator 把配置里的可移植写法翻成当前平台真实的加速键。
//
// 约定是「Ctrl = 主修饰键」：Windows / Linux 上它就是 Control，macOS 上是 ⌘
// （前端的展示、匹配、冲突检测全部按这个约定）。但 Wails 的 `Ctrl` 是字面意义的
// Control —— 直接交给它，macOS 用户会看到设置页写着 ⌘⇧A、手上按 Ctrl+Shift+A
// 才生效。所以只在交给系统的那一步换成 `Cmd`，配置与 DTO 里仍保持可移植写法。
func toSystemAccelerator(goos, accel string) string {
	if goos != "darwin" || accel == "" {
		return accel
	}
	parts := strings.Split(accel, "+")
	for i := 0; i < len(parts)-1; i++ {
		switch strings.ToLower(strings.TrimSpace(parts[i])) {
		case "ctrl", "control":
			parts[i] = "Cmd"
		}
	}
	return strings.Join(parts, "+")
}

// snapshotLocked 组装快照。调用方必须持有 s.mu。
func (s *ShortcutService) snapshotLocked() ShortcutsDTO {
	overrides := make(map[string]string, len(s.d.Cfg.Shortcuts.Keys))
	for k, v := range s.d.Cfg.Shortcuts.Keys {
		overrides[k] = v
	}
	global := make([]ShortcutResultDTO, len(s.results))
	copy(global, s.results)
	// 排序只为一件事：设置页的行顺序稳定，不随 map 遍历顺序跳动。
	sort.Slice(global, func(i, j int) bool { return global[i].ID < global[j].ID })
	return ShortcutsDTO{Overrides: overrides, Global: global}
}

// isGlobalID 判断一个命令 id 是否由 Go 侧注册。
func isGlobalID(id string) bool {
	for _, g := range GlobalShortcutIDs {
		if g == id {
			return true
		}
	}
	return false
}

// 修饰键白名单：与 Wails 的加速键语法对齐（keys.go 的 modifierMap）。
// CmdOrCtrl 与 Cmd 统一按「主修饰键」处理 —— 前端存储只写 Ctrl/Alt/Shift/Cmd，
// 但这里放宽一点：手改过配置文件的用户写 CmdOrCtrl 也不该被判成非法。
var shortcutModifiers = map[string]bool{
	"ctrl": true, "control": true,
	"alt": true, "option": true,
	"shift": true,
	"cmd": true, "command": true, "cmdorctrl": true,
	"win": true, "super": true, "meta": true,
}

// primaryModifiers 里至少有一个，才算「不是一个会被打字吞掉的键」。
var primaryModifiers = map[string]bool{
	"ctrl": true, "control": true,
	"alt": true, "option": true,
	"cmd": true, "command": true, "cmdorctrl": true,
	"win": true, "super": true, "meta": true,
}

// validateAccelerator 校验一个加速键字符串。
//
// 空串是合法值（表示解绑）。规则与前端录制控件一致，这里只是【兜底】
// （配置文件可能被手改、也可能来自旧版本）：
//   - 全局键必须含 Ctrl / Alt / Win —— Wails 自己不校验（parseAccelerator
//     允许裸 "a"），真绑上一个裸字母会让用户整块键盘失灵，且他根本不知道
//     是谁干的；
//   - 窗口内命令必须含 Ctrl / Alt / Cmd，除非主键是 Esc 或 F1-F12
//     （功能键不会与打字冲突，而单独的字母/数字键会被输入框吃掉）。
func validateAccelerator(accel string, needPrimary bool) error {
	if accel == "" {
		return nil
	}
	parts := strings.Split(accel, "+")
	hasPrimary := false
	key := ""
	for i, p := range parts {
		token := strings.ToLower(strings.TrimSpace(p))
		if token == "" {
			return fmt.Errorf("存在空的分段")
		}
		if i == len(parts)-1 {
			if shortcutModifiers[token] {
				return fmt.Errorf("缺少主键（只写了修饰键）")
			}
			key = token
			continue
		}
		if !shortcutModifiers[token] {
			return fmt.Errorf("%q 不是合法的修饰键", p)
		}
		if primaryModifiers[token] {
			hasPrimary = true
		}
	}
	if key == "" {
		return fmt.Errorf("缺少主键")
	}
	if needPrimary {
		if !hasPrimary {
			return fmt.Errorf("全局快捷键必须包含 Ctrl / Alt / Win（只有 Shift 等于裸键）")
		}
		return nil
	}
	if hasPrimary || key == "escape" || isFunctionKey(key) {
		return nil
	}
	return fmt.Errorf("必须包含 Ctrl / Alt / Cmd 修饰键（或改用 Esc / F1-F12）")
}

// isFunctionKey 判断主键是否是 F1-F24。
func isFunctionKey(key string) bool {
	if !strings.HasPrefix(key, "f") {
		return false
	}
	n, err := strconv.Atoi(key[1:])
	return err == nil && n >= 1 && n <= 24
}
