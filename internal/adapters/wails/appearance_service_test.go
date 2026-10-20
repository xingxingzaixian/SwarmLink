package wails

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
)

// 1x1 PNG：真实文件头，足以让 http.DetectContentType 认出 image/png。
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func newTestAppearance(t *testing.T) (*AppearanceService, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	return NewAppearanceService(Deps{Cfg: cfg, ConfigDir: dir}), dir
}

func TestSaveBackgroundWritesFileAndConfig(t *testing.T) {
	s, dir := newTestAppearance(t)

	got, err := s.Save(base64.StdEncoding.EncodeToString(tinyPNG), ".png")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.HasPrefix(got.DataURL, "data:image/png;base64,") {
		t.Fatalf("DataURL=%q", got.DataURL)
	}
	if got.FileName != "background.png" {
		t.Fatalf("FileName=%q want background.png", got.FileName)
	}

	// 文件必须落在数据目录下，而不是用户选图的原始位置
	if _, err := os.Stat(filepath.Join(dir, "backgrounds", "background.png")); err != nil {
		t.Fatalf("背景文件没有落盘：%v", err)
	}
	// 配置要落盘：重启后还能恢复
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Appearance.Background != "background.png" {
		t.Fatalf("config background=%q", cfg.Appearance.Background)
	}
}

// 关键防线：类型由文件头决定，不看客户端传的扩展名。
// 否则一个改名的可执行文件会以 image/png 的身份进入数据目录。
func TestSaveRejectsNonImage(t *testing.T) {
	s, _ := newTestAppearance(t)

	exe := []byte("MZ\x90\x00\x03\x00\x00\x00 this is not an image")
	if _, err := s.Save(base64.StdEncoding.EncodeToString(exe), ".png"); err == nil {
		t.Fatal("非图片必须被拒绝")
	} else if !strings.Contains(err.Error(), "PNG") {
		t.Fatalf("错误信息应说明支持的格式，got %v", err)
	}
}

func TestSaveRejectsTooLarge(t *testing.T) {
	s, _ := newTestAppearance(t)

	huge := bytes.Repeat([]byte{0x00}, maxBackgroundBytes+1)
	if _, err := s.Save(base64.StdEncoding.EncodeToString(huge), ".png"); err == nil {
		t.Fatal("超过上限的图片必须被拒绝")
	}
}

// 换图时旧扩展名的文件要删掉：否则「移除背景」之后还留着一份副本。
func TestSaveDropsOldExtension(t *testing.T) {
	s, dir := newTestAppearance(t)

	if _, err := s.Save(base64.StdEncoding.EncodeToString(tinyPNG), ".png"); err != nil {
		t.Fatal(err)
	}

	// 手工造一个旧 jpg，模拟「先设了 png，又换成 jpg」
	old := filepath.Join(dir, "backgrounds", "background.jpg")
	if err := os.WriteFile(old, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Save(base64.StdEncoding.EncodeToString(tinyPNG), ".png"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("旧扩展名的背景文件没有被清理")
	}
}

func TestClearBackground(t *testing.T) {
	s, dir := newTestAppearance(t)

	if _, err := s.Save(base64.StdEncoding.EncodeToString(tinyPNG), ".png"); err != nil {
		t.Fatal(err)
	}
	// 先调一次遮罩，确认 Clear 不会顺手把它重置（用户没动过的设置不该变）
	if _, err := s.SetLook(70, 12); err != nil {
		t.Fatal(err)
	}

	got, err := s.Clear()
	if err != nil {
		t.Fatal(err)
	}
	if got.DataURL != "" || got.FileName != "" {
		t.Fatalf("Clear 之后仍有背景：%+v", got)
	}
	if got.Dim != 70 || got.Blur != 12 {
		t.Fatalf("Clear 不该重置遮罩/模糊：dim=%d blur=%d", got.Dim, got.Blur)
	}
	if _, err := os.Stat(filepath.Join(dir, "backgrounds", "background.png")); !os.IsNotExist(err) {
		t.Fatal("背景文件没有被删除")
	}
}

// 滑杆越界只钳住，不报错 —— 前端每拖一帧都会发一次。
func TestSetLookClamps(t *testing.T) {
	s, _ := newTestAppearance(t)

	if _, err := s.SetLook(999, -5); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	if got.Dim != maxBackgroundDim || got.Blur != 0 {
		t.Fatalf("dim=%d blur=%d，want %d/0", got.Dim, got.Blur, maxBackgroundDim)
	}
}

// 配置被手工改成带路径穿越的名字时，只能落在 backgrounds/ 内。
func TestBackgroundPathStaysInsideDir(t *testing.T) {
	got := config.BackgroundPath("/data", "../../etc/passwd")
	if strings.Contains(got, "..") {
		t.Fatalf("BackgroundPath 允许了路径穿越：%s", got)
	}
	if !strings.HasPrefix(got, filepath.Join("/data", "backgrounds")) {
		t.Fatalf("BackgroundPath=%s", got)
	}
}
