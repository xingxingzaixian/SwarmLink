package wails

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// fakeOverlayHost 记录壳层收到的开窗请求。
//
// 截屏的「开窗」是外壳职责，服务层只该保证「算对了位置、传对了尺寸」——
// 用一个记录型的假实现，就能把这两件事钉死在测试里。
type fakeOverlayHost struct {
	overlays  int
	pins      []PinOptions
	closed    []string
	minimized bool
	restored  bool
}

func (f *fakeOverlayHost) OpenOverlay(OverlayOptions) error { f.overlays++; return nil }
func (f *fakeOverlayHost) ShowOverlay(string) error         { return nil }
func (f *fakeOverlayHost) FocusOverlay(string) error        { return nil }
func (f *fakeOverlayHost) CloseOverlays() error             { return nil }
func (f *fakeOverlayHost) MinimizeMain() error              { f.minimized = true; return nil }
func (f *fakeOverlayHost) RestoreMain() error               { f.restored = true; return nil }
func (f *fakeOverlayHost) OpenPin(opts PinOptions) error {
	f.pins = append(f.pins, opts)
	return nil
}
func (f *fakeOverlayHost) CloseWindow(name string) error {
	f.closed = append(f.closed, name)
	return nil
}
func (f *fakeOverlayHost) Emit(string, any) {}

func newScreenshotService(t *testing.T) (*ScreenshotService, *fakeOverlayHost) {
	t.Helper()
	host := &fakeOverlayHost{}
	return &ScreenshotService{d: Deps{Overlay: host}}, host
}

func readTestPNGB64(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(writeTestPNG(t))
	if err != nil {
		t.Fatalf("读测试图片: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// 另存为必须「原样写盘」：这是它和发送路径的分工 —— 发送要压到 460KB，
// 另存为的语义是「把我看到的存下来」，重新编码就变味了。
func TestSaveOverlayWritesRawBytes(t *testing.T) {
	svc, _ := newScreenshotService(t)
	raw, err := os.ReadFile(writeTestPNG(t))
	if err != nil {
		t.Fatalf("读测试图片: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "shot.png")
	if err := svc.SaveOverlay(base64.StdEncoding.EncodeToString(raw), dest); err != nil {
		t.Fatalf("SaveOverlay: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读取另存文件: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("另存的文件与原始字节不一致（%d vs %d 字节）", len(got), len(raw))
	}
}

// 对话框里的文件名可以编辑，用户很可能把扩展名删掉。
// 不补扩展名的话，存出来的文件双击打不开 —— 这是最容易被忽略的一种「保存失败」。
func TestSaveOverlayAppendsMissingExtension(t *testing.T) {
	svc, _ := newScreenshotService(t)
	base := filepath.Join(t.TempDir(), "无扩展名")

	if err := svc.SaveOverlay(readTestPNGB64(t), base); err != nil {
		t.Fatalf("SaveOverlay: %v", err)
	}
	if _, err := os.Stat(base + ".png"); err != nil {
		t.Fatalf("应当按真实格式补上 .png: %v", err)
	}
}

func TestSaveOverlayRejectsBadInput(t *testing.T) {
	svc, _ := newScreenshotService(t)
	dir := t.TempDir()

	cases := []struct {
		name string
		data string
		dest string
		want string
	}{
		{"空路径", readTestPNGB64(t), "", "请选择保存位置"},
		{"空数据", "", filepath.Join(dir, "a.png"), "图片内容为空"},
		{"非法 base64", "这不是 base64", filepath.Join(dir, "b.png"), "base64"},
		{"不是图片", base64.StdEncoding.EncodeToString([]byte("plain text")), filepath.Join(dir, "c.png"), "不是可识别"},
		{"目录不存在", readTestPNGB64(t), filepath.Join(dir, "没有这个目录", "d.png"), "保存位置不存在"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := svc.SaveOverlay(c.data, c.dest)
			if err == nil {
				t.Fatal("必须报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息 = %q，期望包含 %q", err.Error(), c.want)
			}
		})
	}
}

// 贴图的位置换算：覆盖层里的选区是物理像素，窗口定位是 DIP。
// 2 倍缩放的屏上把物理坐标直接当 DIP 用，贴图会跑到两倍远的地方。
func TestPinOverlayConvertsPhysicalToDIP(t *testing.T) {
	svc, host := newScreenshotService(t)
	svc.session = &overlaySession{displays: []ports.Display{{
		Index:          0,
		X:              100,
		Y:              50,
		Width:          960,
		Height:         540,
		ScaleFactor:    2,
		PhysicalWidth:  1920,
		PhysicalHeight: 1080,
	}}}

	id, err := svc.PinOverlay(0, readTestPNGB64(t), 400, 300, 200, 100)
	if err != nil {
		t.Fatalf("PinOverlay: %v", err)
	}
	if len(host.pins) != 1 {
		t.Fatalf("应当开 1 个贴图窗口，实得 %d", len(host.pins))
	}
	p := host.pins[0]
	if p.Width != 200 || p.Height != 150 {
		t.Errorf("窗口尺寸 = %d×%d，期望 200×150（物理 400×300 / 2）", p.Width, p.Height)
	}
	if p.X != 200 || p.Y != 100 {
		t.Errorf("窗口位置 = (%d,%d)，期望 (200,100)（屏原点 + 物理偏移 / 2）", p.X, p.Y)
	}
	if !strings.Contains(p.URL, fmt.Sprintf("mode=pin&id=%d", id)) {
		t.Errorf("窗口 URL = %q，期望带上贴图 id", p.URL)
	}

	img, err := svc.GetPinImage(id)
	if err != nil {
		t.Fatalf("GetPinImage: %v", err)
	}
	if img.MIME != "image/png" {
		t.Errorf("MIME = %q，期望按字节头嗅探出 image/png", img.MIME)
	}
	if img.Width != 200 || img.Height != 150 {
		t.Errorf("贴图基准尺寸 = %d×%d，期望 200×150", img.Width, img.Height)
	}
}

// 「隐藏本窗口」结束后必须把主窗口恢复回来 —— 否则用户截完图，
// 窗口不见了，图却躺在输入框里看不见也发不出去。
func TestCloseSessionRestoresMinimizedMain(t *testing.T) {
	svc, host := newScreenshotService(t)
	svc.session = &overlaySession{mainMinimized: true}

	if err := svc.closeOverlaySession(); err != nil {
		t.Fatalf("closeOverlaySession: %v", err)
	}
	if !host.restored {
		t.Fatal("隐藏模式结束后必须恢复主窗口")
	}
}

// 反过来：keep_window 模式没动过窗口，收尾也不该去 Show/Focus 它 ——
// 那会把用户正在看别的应用时窗口抢到前台。
func TestCloseSessionLeavesMainAloneWhenKept(t *testing.T) {
	svc, host := newScreenshotService(t)
	svc.session = &overlaySession{mainMinimized: false}

	if err := svc.closeOverlaySession(); err != nil {
		t.Fatalf("closeOverlaySession: %v", err)
	}
	if host.restored {
		t.Fatal("keep_window 模式不得动主窗口")
	}
}

func TestPinOverlayRequiresSession(t *testing.T) {
	svc, _ := newScreenshotService(t)
	_, err := svc.PinOverlay(0, readTestPNGB64(t), 10, 10, 0, 0)
	if err == nil {
		t.Fatal("没有截图会话时必须报错")
	}
	if !strings.Contains(err.Error(), "会话已结束") {
		t.Errorf("错误信息 = %q，期望点明会话已结束", err.Error())
	}
}

func TestPinOverlayRejectsBadImage(t *testing.T) {
	svc, _ := newScreenshotService(t)
	svc.session = &overlaySession{displays: []ports.Display{{ScaleFactor: 1}}}

	if _, err := svc.PinOverlay(0, base64.StdEncoding.EncodeToString([]byte("plain")), 10, 10, 0, 0); err == nil {
		t.Fatal("不是图片必须报错")
	}
	if _, err := svc.PinOverlay(0, readTestPNGB64(t), 0, 10, 0, 0); err == nil {
		t.Fatal("尺寸无效必须报错")
	}
	if _, err := svc.PinOverlay(3, readTestPNGB64(t), 10, 10, 0, 0); err == nil {
		t.Fatal("显示器序号越界必须报错")
	}
}

// 用户直接关窗口（Alt+F4 / 系统关机）时，图片字节也必须释放：
// 只在 ClosePin 里删会漏掉这条路，一张 4K 图就是好几 MB。
func TestPinReleasesImageWhenWindowCloses(t *testing.T) {
	svc, host := newScreenshotService(t)
	svc.session = &overlaySession{displays: []ports.Display{{ScaleFactor: 1}}}

	id, err := svc.PinOverlay(0, readTestPNGB64(t), 10, 10, 0, 0)
	if err != nil {
		t.Fatalf("PinOverlay: %v", err)
	}
	if len(host.pins) != 1 || host.pins[0].onClose == nil {
		t.Fatal("开窗时必须带上释放回调")
	}
	host.pins[0].onClose()
	if _, err := svc.GetPinImage(id); err == nil {
		t.Fatal("窗口关掉之后字节必须释放")
	}
}

// 关贴图是幂等的：用户已经关掉的那张，再关一次不该报错。
func TestClosePinIsIdempotent(t *testing.T) {
	svc, host := newScreenshotService(t)
	svc.session = &overlaySession{displays: []ports.Display{{ScaleFactor: 1}}}

	id, err := svc.PinOverlay(0, readTestPNGB64(t), 10, 10, 0, 0)
	if err != nil {
		t.Fatalf("PinOverlay: %v", err)
	}
	if err := svc.ClosePin(id); err != nil {
		t.Fatalf("第一次关: %v", err)
	}
	if err := svc.ClosePin(id); err != nil {
		t.Fatalf("重复关不该报错: %v", err)
	}
	if len(host.closed) != 1 {
		t.Fatalf("应当只关 1 次窗口，实得 %d", len(host.closed))
	}
	if _, err := svc.GetPinImage(id); err == nil {
		t.Fatal("关掉之后不应再取到内容")
	}
}
