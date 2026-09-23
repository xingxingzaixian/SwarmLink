package config

import (
	"os"
	"testing"
)

// 提示设置的默认值与落盘往返。
//
// 重点在「默认即开启」：Load 是先 Default() 再 unmarshal 到同一个结构，
// 因此老配置文件（里面根本没有 [notify] 段）会保留 true —— 这正是我们要的，
// 用户升级后应当默认就有关闭收托盘与提示音。
// 反过来，bool 的 false 是合法值，绝不能在 applyDefaults 里用
// `if x == 0` 那种手法回填，否则用户关掉开关重启后会被强行打开。
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
		t.Fatal("sound=false 没有落盘（重启后提示音会自己打开，用户会认为开关是坏的）")
	}
	if !back.Notify.MinimizeToTray {
		t.Fatal("未改动的字段被写坏了")
	}
}

// 老配置文件里没有 [notify] 段时必须补默认值，而不是退化成 false。
func TestNotifyDefaultsOnLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	legacy := []byte("[general]\ndisplay_name = \"老配置\"\n")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dir), legacy, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Notify.Sound || !cfg.Notify.MinimizeToTray {
		t.Fatalf("老配置应补上默认值 true，得到 %+v", cfg.Notify)
	}
	if cfg.General.DisplayName != "老配置" {
		t.Fatalf("既有字段被覆盖：%q", cfg.General.DisplayName)
	}
}
