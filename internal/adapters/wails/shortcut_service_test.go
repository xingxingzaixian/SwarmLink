package wails

import (
	"strings"
	"testing"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
)

func newTestShortcut(t *testing.T) (*ShortcutService, *config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	return &ShortcutService{d: Deps{Cfg: cfg, ConfigDir: dir}}, cfg, dir
}

// TestValidateAccelerator 覆盖「会被打字吞掉」那几类键：
// 规则错了的表现是用户绑完键之后在别处打不出字，极难自查。
func TestValidateAccelerator(t *testing.T) {
	cases := []struct {
		name    string
		accel   string
		global  bool
		wantErr string
	}{
		{name: "空串表示解绑", accel: ""},
		{name: "全局常规组合", accel: "Ctrl+Shift+A", global: true},
		{name: "全局 Alt 组合", accel: "Alt+Space", global: true},
		{name: "全局裸字母", accel: "a", global: true, wantErr: "必须包含"},
		{name: "全局只有 Shift", accel: "Shift+A", global: true, wantErr: "必须包含"},
		{name: "窗口内常规组合", accel: "Ctrl+K"},
		{name: "窗口内 Esc", accel: "Escape"},
		{name: "窗口内功能键", accel: "F5"},
		{name: "窗口内裸字母", accel: "k", wantErr: "必须包含"},
		{name: "窗口内只有 Shift", accel: "Shift+K", wantErr: "必须包含"},
		{name: "Modifier 写错", accel: "Controlx+K", wantErr: "不是合法的修饰键"},
		{name: "只有修饰键", accel: "Ctrl+Shift", wantErr: "缺少主键"},
		{name: "空分段", accel: "Ctrl++K", wantErr: "空的分段"},
		{name: "CmdOrCtrl 别名可用", accel: "CmdOrCtrl+K"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAccelerator(tc.accel, tc.global)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过，实际报错：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望报错（包含 %q），实际通过", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息应包含 %q，实际：%v", tc.wantErr, err)
			}
		})
	}
}

// TestApplyPersistsOnlyOverrides 守住「配置只存用户改过的值」这条：
// 一旦把默认值也写进文件，升级后新的默认键位就永远到不了老用户手里。
func TestApplyPersistsOnlyOverrides(t *testing.T) {
	s, cfg, dir := newTestShortcut(t)

	// 注入假的注册器：不碰系统，只记录被要求注册的全局键。
	var registered map[string]string
	SetGlobalShortcutApplier(func(keys map[string]string) map[string]error {
		registered = keys
		return nil
	})
	t.Cleanup(func() { SetGlobalShortcutApplier(nil) })

	snap, err := s.Apply(map[string]string{"search": "Ctrl+Shift+F"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if snap.Overrides["search"] != "Ctrl+Shift+F" {
		t.Fatalf("覆盖值应被保留，实际：%v", snap.Overrides)
	}
	if _, ok := snap.Overrides["screenshot"]; ok {
		t.Fatalf("没改过的全局键不该出现在覆盖表里：%v", snap.Overrides)
	}

	// 默认值仍然生效，并且被交给注册器。
	if got := registered["screenshot"]; got != config.GlobalShortcutDefaults["screenshot"] {
		t.Fatalf("全局键应用默认值注册，实际：%q", got)
	}

	// 重新 Load 一次，确认磁盘上也只有那一条覆盖。
	reloaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(reloaded.Shortcuts.Keys) != 1 || reloaded.Shortcuts.Keys["search"] != "Ctrl+Shift+F" {
		t.Fatalf("配置文件里只应有 search 一条覆盖，实际：%v", reloaded.Shortcuts.Keys)
	}
	if cfg.Shortcuts.Keys["search"] != "Ctrl+Shift+F" {
		t.Fatalf("内存中的配置应与磁盘一致")
	}
}

// TestApplyRejectsInvalid 非法键位必须【整体拒绝】：磁盘上写的与实际生效的
// 必须是同一件事，不能出现「一半写进去、一半没写」。
func TestApplyRejectsInvalid(t *testing.T) {
	s, cfg, _ := newTestShortcut(t)
	cfg.Shortcuts.Keys = map[string]string{"search": "Ctrl+K"}

	_, err := s.Apply(map[string]string{"screenshot": "a"})
	if err == nil {
		t.Fatal("裸字母作为全局键应被拒绝")
	}
	if cfg.Shortcuts.Keys["search"] != "Ctrl+K" {
		t.Fatalf("拒绝时不该改动已有配置：%v", cfg.Shortcuts.Keys)
	}
}

// TestApplyUnbind Stored空串 = 主动解绑，必须落盘，否则重启后默认键又回来了。
func TestApplyUnbind(t *testing.T) {
	s, _, dir := newTestShortcut(t)
	var registered map[string]string
	SetGlobalShortcutApplier(func(keys map[string]string) map[string]error {
		registered = keys
		return nil
	})
	t.Cleanup(func() { SetGlobalShortcutApplier(nil) })

	snap, err := s.Apply(map[string]string{"screenshot": ""})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, ok := snap.Overrides["screenshot"]; !ok || got != "" {
		t.Fatalf("解绑应作为空串覆盖值保存，实际：%v", snap.Overrides)
	}
	if registered["screenshot"] != "" {
		t.Fatalf("解绑后不该再注册，实际：%q", registered["screenshot"])
	}
	reloaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v, ok := reloaded.Shortcuts.Keys["screenshot"]; !ok || v != "" {
		t.Fatalf("解绑状态必须落盘，实际：%v", reloaded.Shortcuts.Keys)
	}

	// Reset 之后回到出厂键位。
	if _, err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if registered["screenshot"] != config.GlobalShortcutDefaults["screenshot"] {
		t.Fatalf("Reset 后应回到默认键位，实际：%q", registered["screenshot"])
	}
}

// TestGlobalResultsReportFailure 注册失败要能被设置页看到（“已被其他程序占用”）。
func TestGlobalResultsReportFailure(t *testing.T) {
	s, _, _ := newTestShortcut(t)
	SetGlobalShortcutApplier(func(map[string]string) map[string]error {
		return map[string]error{"screenshot": errStub("hotkey taken")}
	})
	t.Cleanup(func() { SetGlobalShortcutApplier(nil) })

	snap, err := s.Apply(nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var found bool
	for _, g := range snap.Global {
		if g.ID != "screenshot" {
			continue
		}
		found = true
		if g.Error == "" {
			t.Fatal("注册失败应体现在结果里")
		}
		if g.Accelerator == "" {
			t.Fatal("结果里要带上实际键位，否则设置页无法回显")
		}
	}
	if !found {
		t.Fatalf("结果里应包含 screenshot：%v", snap.Global)
	}
}

// TestToSystemAccelerator 守住「Ctrl 表示主修饰键」这条约定：
// macOS 上不换名，用户会看到设置页写着 ⌘⇧A 却怎么按都没反应。
func TestToSystemAccelerator(t *testing.T) {
	cases := []struct {
		goos, in, want string
	}{
		{goos: "windows", in: "Ctrl+Shift+A", want: "Ctrl+Shift+A"},
		{goos: "linux", in: "Ctrl+Alt+Space", want: "Ctrl+Alt+Space"},
		{goos: "darwin", in: "Ctrl+Shift+A", want: "Cmd+Shift+A"},
		{goos: "darwin", in: "Ctrl+K", want: "Cmd+K"},
		{goos: "darwin", in: "Alt+Space", want: "Alt+Space"},
		{goos: "darwin", in: "", want: ""},
	}
	for _, tc := range cases {
		if got := toSystemAccelerator(tc.goos, tc.in); got != tc.want {
			t.Fatalf("%s：%q → %q，期望 %q", tc.goos, tc.in, got, tc.want)
		}
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
