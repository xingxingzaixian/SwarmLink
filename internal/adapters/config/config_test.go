package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 数据目录的可切换性：SWARMLINK_DATA_DIR 一旦设置就必须生效且原样使用。
//
// 这条能力是有实际来源的：程序装在只读目录、或运行在写不了用户配置目录的
// 受限环境里时，「把数据放到我指定的地方」是唯一的出路。默认仍是用户配置
// 目录 —— 见 Dir 的说明（装到 Program Files 之后程序旁边同样不可写）。
func TestDirEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom-data")
	t.Setenv(EnvDataDir, want)

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Dir() = %q，期望 %q", got, want)
	}
	// 数据库与配置文件都必须落在被覆盖的目录下，而不是各走各的。
	if db := DBPath(got); !filepath.IsAbs(db) || filepath.Dir(filepath.Dir(db)) != want {
		t.Fatalf("DBPath(%q) = %q，未落在覆盖目录下", got, db)
	}
	if p := Path(got); filepath.Dir(p) != want {
		t.Fatalf("Path(%q) = %q，未落在覆盖目录下", got, p)
	}
}

// 空白值不算设置：否则一个空的 SWARMLINK_DATA_DIR 会变成「当前工作目录」，
// 用户会莫名其妙发现数据跑到别处去了。
func TestDirEnvOverrideIgnoresBlank(t *testing.T) {
	t.Setenv(EnvDataDir, "   ")

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, AppName); got != want {
		t.Fatalf("空白覆盖值应当回落到用户配置目录：Dir() = %q，期望 %q", got, want)
	}
}

// 托管目录必须与数据目录【解耦】：它跟着环境变量走，不受数据目录影响。
func TestVaultDirEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "vault")
	t.Setenv(EnvVaultDir, want)

	if got := VaultDir(t.TempDir()); got != want {
		t.Fatalf("VaultDir = %q，期望 %q", got, want)
	}
}

// 空白值不算设置：空的 SWARMLINK_VAULT_DIR 会变成「当前目录」，比不设更糟。
func TestVaultDirEnvOverrideIgnoresBlank(t *testing.T) {
	t.Setenv(EnvVaultDir, "   ")

	base := machineDataDir()
	if base == "" {
		t.Skip("本平台没有机器级目录基座")
	}
	if got, want := VaultDir(t.TempDir()), filepath.Join(base, AppName); got != want {
		t.Fatalf("VaultDir = %q，期望回落到平台默认 %q", got, want)
	}
}

// 解析结果与数据目录重合时必须禁用：一旦相同，「删数据目录」就等于「删身份」。
func TestVaultDirRejectsDataDirOverlap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvVaultDir, dir)

	if got := VaultDir(dir); got != "" {
		t.Fatalf("与数据目录重合时必须返回空串（禁用托管），得到 %q", got)
	}
	// 尾部分隔符不该骗过守卫（Clean 之后是同一个目录）
	if got := VaultDir(dir + string(filepath.Separator)); got != "" {
		t.Fatalf("尾部分隔符应视为同一目录，得到 %q", got)
	}
}

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
