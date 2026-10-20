package wails

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
)

// maxBackgroundBytes 是自定义背景图的大小上限（8 MiB）。
//
// 为什么要有上限：图片是以 data URL 的形式经 JS 桥送进 WebView 的，
// base64 会膨胀 33%。一张 20MB 的图会变成 27MB 的字符串，启动时要序列化
// 一次、解析一次，肉眼可见地卡顿 —— 而背景图在这个界面里只是纹理，
// 8MB 的 JPEG 已经远超需要。
const maxBackgroundBytes = 8 << 20

// maxBackgroundDim / maxBackgroundBlur 是外观滑杆的上限。
// 模糊上限刻意不大：再大就等于给整窗加毛玻璃，老机器上会掉帧。
const (
	maxBackgroundDim  = 100
	maxBackgroundBlur = 40
)

// AppearanceService 负责「外观」这一类可写设置（目前只有背景图）。
//
// 单独开一个服务而不是塞进 SettingsService：背景图是二进制载荷，
// 放进设置表单会让每次保存设置都带上几 MB 的字符串；而它又必须落盘，
// 不能只存在 localStorage（换机器、清缓存就丢，且 5MB 配额不够）。
type AppearanceService struct{ d Deps }

// NewAppearanceService 构造外观服务。
func NewAppearanceService(d Deps) *AppearanceService { return &AppearanceService{d: d} }

// Get 返回当前背景（data URL）与遮罩参数。
//
// 没有背景时 DataURL 为空串 —— 前端据此决定是否加 body.has-app-bg，
// 而不是靠猜文件名。
func (s *AppearanceService) Get() (BackgroundDTO, error) {
	return s.snapshot()
}

// Save 保存一张新背景。
//
// dataBase64 是图片原始字节的 base64（允许带 data: 前缀，前端两种传法都行）。
// ext 只是提示：真正的类型由服务端按文件头嗅探决定 —— 客户端说的不算，
// 否则一个改了后缀的 .exe 会以 image/png 的名义被写进数据目录。
func (s *AppearanceService) Save(dataBase64 string, ext string) (BackgroundDTO, error) {
	raw := stripDataURLPrefix(dataBase64)

	// 先按 base64 长度粗判，避免把 30MB 的字符串完整解码进内存再拒绝。
	if int64(len(raw))/4*3 > maxBackgroundBytes+1024 {
		return BackgroundDTO{}, fmt.Errorf("图片太大（上限 %d MB），请先压缩", maxBackgroundBytes>>20)
	}

	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return BackgroundDTO{}, fmt.Errorf("图片数据无法解析：%w", err)
	}
	if len(data) == 0 {
		return BackgroundDTO{}, fmt.Errorf("图片是空的")
	}
	if len(data) > maxBackgroundBytes {
		return BackgroundDTO{}, fmt.Errorf("图片太大（上限 %d MB），请先压缩", maxBackgroundBytes>>20)
	}

	mime := sniffImageMime(data)
	if mime == "" {
		return BackgroundDTO{}, fmt.Errorf("只支持 PNG / JPEG / WebP / GIF 图片")
	}
	ext2, ok := imageExt[mime]
	if !ok {
		return BackgroundDTO{}, fmt.Errorf("只支持 PNG / JPEG / WebP / GIF 图片")
	}

	dir := config.BackgroundsDir(s.d.ConfigDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return BackgroundDTO{}, fmt.Errorf("创建背景目录失败：%w", err)
	}

	name := "background" + ext2
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		return BackgroundDTO{}, fmt.Errorf("保存背景失败：%w", err)
	}
	// 清掉旧扩展名的文件：否则从 png 换成 jpg 后会留一份永远用不到的副本，
	// 用户「移除背景」时也会以为没删干净。
	s.removeOtherExtensions(name)

	s.d.Cfg.Appearance.Background = name
	if err := config.Save(s.d.ConfigDir, s.d.Cfg); err != nil {
		return BackgroundDTO{}, fmt.Errorf("保存配置失败：%w", err)
	}
	return s.snapshot()
}

// Clear 移除背景图（删文件 + 清配置）。
//
// 保留遮罩/模糊参数：用户很可能只是想换一张图，先移除再选新的；
// 把滑杆一起重置成默认值等于顺手改了他没动过的设置。
func (s *AppearanceService) Clear() (BackgroundDTO, error) {
	dir := config.BackgroundsDir(s.d.ConfigDir)
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "background.") {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	s.d.Cfg.Appearance.Background = ""
	if err := config.Save(s.d.ConfigDir, s.d.Cfg); err != nil {
		return BackgroundDTO{}, fmt.Errorf("保存配置失败：%w", err)
	}
	return s.snapshot()
}

// SetLook 调整遮罩强度与模糊半径（0-100 / 0-40，超界钳住而不是报错）。
//
// 钳住而不是报错：这两个值是滑杆在拖，前端每一帧都可能发过来，
// 为「拖过头了」弹一个错误框毫无意义。
func (s *AppearanceService) SetLook(dim int, blur int) (BackgroundDTO, error) {
	s.d.Cfg.Appearance.BackgroundDim = clampInt(dim, 0, maxBackgroundDim)
	s.d.Cfg.Appearance.BackgroundBlur = clampInt(blur, 0, maxBackgroundBlur)
	if err := config.Save(s.d.ConfigDir, s.d.Cfg); err != nil {
		return BackgroundDTO{}, fmt.Errorf("保存配置失败：%w", err)
	}
	return s.snapshot()
}

// snapshot 组装返回给前端的当前状态。
func (s *AppearanceService) snapshot() (BackgroundDTO, error) {
	out := BackgroundDTO{
		Dim:      clampInt(s.d.Cfg.Appearance.BackgroundDim, 0, maxBackgroundDim),
		Blur:     clampInt(s.d.Cfg.Appearance.BackgroundBlur, 0, maxBackgroundBlur),
		MaxBytes: maxBackgroundBytes,
		FileName: s.d.Cfg.Appearance.Background,
	}
	if out.FileName == "" {
		return out, nil
	}

	path := config.BackgroundPath(s.d.ConfigDir, out.FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 配置文件说有背景、文件却没了（用户手动删了数据目录）：
			// 当成「没有背景」而不是报错，否则每次启动都会弹一个无法处理的失败。
			out.FileName = ""
			return out, nil
		}
		return BackgroundDTO{}, fmt.Errorf("读取背景失败：%w", err)
	}

	mime := sniffImageMime(data)
	if mime == "" {
		out.FileName = ""
		return out, nil
	}
	out.DataURL = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	return out, nil
}

// removeOtherExtensions 删除 backgrounds/ 下除 keep 之外的 background.* 文件。
func (s *AppearanceService) removeOtherExtensions(keep string) {
	entries, err := os.ReadDir(config.BackgroundsDir(s.d.ConfigDir))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "background.") || e.Name() == keep {
			continue
		}
		_ = os.Remove(filepath.Join(config.BackgroundsDir(s.d.ConfigDir), e.Name()))
	}
}

// imageExt 是允许的图片类型与落盘扩展名。
var imageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// sniffImageMime 按文件头判断图片类型（空串 = 不是支持的图片）。
//
// http.DetectContentType 认 jpeg/png/gif/webp，且不依赖扩展名。
func sniffImageMime(data []byte) string {
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	mime := http.DetectContentType(head)
	if mime == "image/jpg" { // 不会发生，但多一层防御不吃亏
		mime = "image/jpeg"
	}
	if _, ok := imageExt[mime]; !ok {
		return ""
	}
	return mime
}

// stripDataURLPrefix 允许前端直接甩一个完整的 data URL 过来。
func stripDataURLPrefix(v string) string {
	if i := strings.Index(v, ","); i >= 0 && strings.HasPrefix(v, "data:") {
		return v[i+1:]
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
