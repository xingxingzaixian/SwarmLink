package wails

import (
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/swarmlink/swarmlink/internal/infra/imagecodec"
)

// pinWindowPrefix 是贴图窗口名的前缀。
//
// 与覆盖层前缀（screenshot-overlay-）刻意不同：关掉覆盖层时是按前缀
// 批量关窗口的，贴图窗口必须活下来 —— 用户刚贴上去的图不该因为
// 截屏会话结束而消失。
const pinWindowPrefix = "screenshot-pin-"

// maxHandoffImageBytes 是「另存为 / 贴图」允许的单张图上限。
//
// 这两个动作都不进消息流（不占协议面的 512KB 预算），因此上限比
// message.MaxInlineImageBytes 宽松得多；但仍然要有一个上限：
// 无上限意味着一次误调用就能把几 GB 的字符串解进内存。
const maxHandoffImageBytes = 32 << 20

// pinSession 是一张已经贴到桌面上的图。
//
// 图片字节存在 Go 侧、由窗口按 id 取，而不是塞进窗口 URL：
// 一张 4K 截图的 base64 有几 MB，URL 装不下（WebView2 对 URL 长度
// 有硬限制），而塞进 query 还会让它进历史记录与日志。
type pinSession struct {
	// name 是窗口名（关窗时用）。
	name string
	// mime 是按字节头嗅探出来的真实类型。
	mime string
	// dataB64 是图片原始字节的 base64。
	dataB64 string
	// width / height 是 1:1 显示时的窗口尺寸（DIP），
	// 前端按它做缩放：窗口尺寸 = 基准尺寸 × 缩放比。
	width, height int
}

// SaveOverlay 把截图另存到用户选定的路径。
//
// 与 SendDraft 的区别是它【不】进消息流：用户只是想把这张图存下来。
// 字节按原样写盘（前端交上来的是 PNG），不再重新压缩 —— 另存为的
// 语义就是「把我看到的原样存下来」。
func (s *ScreenshotService) SaveOverlay(dataB64, destPath string) error {
	if destPath == "" {
		return fmt.Errorf("请选择保存位置")
	}
	data, err := handoffImageBytes(dataB64)
	if err != nil {
		return err
	}
	format, err := imagecodec.Sniff(data)
	if err != nil {
		return fmt.Errorf("不是可识别的图片格式")
	}
	// 先看目录：目标目录不存在是「用户选错了位置」，与「写盘失败」
	// 不是一回事，分开报错才能给出可行动的信息。
	if dir := filepath.Dir(destPath); dir != "" {
		if st, statErr := os.Stat(dir); statErr != nil || !st.IsDir() {
			return fmt.Errorf("保存位置不存在：%s", dir)
		}
	}
	// 对话框里的文件名可以编辑，用户很可能把扩展名删掉。按真实格式补上，
	// 否则存出来的文件在资源管理器里没有图标、双击也打不开。
	if strings.TrimSpace(filepath.Ext(destPath)) == "" {
		destPath += "." + format
	}
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		return fmt.Errorf("保存图片失败: %w", err)
	}
	return nil
}

// PinOverlay 把选中的一块图贴到桌面上（置顶的独立窗口）。
//
// displayIndex / offX / offY 是这块图在【哪块屏、屏内什么位置】：
// 覆盖层里的选区是物理像素，这里换算成 DIP 之后再加上显示器的原点，
// 贴图窗口才会正好盖住用户刚才框的地方（否则会「贴到别处去」）。
//
// 完成后覆盖层由前端收起（CancelOverlay）—— 贴图窗口是另一个窗口，
// 不受关覆盖层影响。
func (s *ScreenshotService) PinOverlay(displayIndex int, dataB64 string, width, height, offX, offY int) (int, error) {
	if s.d.Overlay == nil {
		return 0, fmt.Errorf("贴图不可用：本机没有可用的窗口实现")
	}
	if width <= 0 || height <= 0 {
		return 0, fmt.Errorf("贴图尺寸无效（%d×%d）", width, height)
	}
	data, err := handoffImageBytes(dataB64)
	if err != nil {
		return 0, err
	}
	format, err := imagecodec.Sniff(data)
	if err != nil {
		return 0, fmt.Errorf("不是可识别的图片格式")
	}

	s.mu.Lock()
	sess := s.session
	if sess == nil {
		s.mu.Unlock()
		return 0, fmt.Errorf("截图会话已结束，无法贴图")
	}
	if displayIndex < 0 || displayIndex >= len(sess.displays) {
		s.mu.Unlock()
		return 0, fmt.Errorf("显示器序号越界（要第 %d 块）", displayIndex)
	}
	d := sess.displays[displayIndex]
	scale := d.ScaleFactor
	if scale <= 0 {
		scale = 1
	}

	dipW := int(math.Round(float64(width) / scale))
	dipH := int(math.Round(float64(height) / scale))
	if dipW < 1 {
		dipW = 1
	}
	if dipH < 1 {
		dipH = 1
	}
	x := d.X + int(math.Round(float64(offX)/scale))
	y := d.Y + int(math.Round(float64(offY)/scale))

	id := s.pinSeq
	s.pinSeq++
	name := fmt.Sprintf("%s%d", pinWindowPrefix, id)
	if s.pins == nil {
		s.pins = make(map[int]*pinSession)
	}
	s.pins[id] = &pinSession{
		name:    name,
		mime:    "image/" + format,
		dataB64: base64.StdEncoding.EncodeToString(data),
		width:   dipW,
		height:  dipH,
	}
	s.mu.Unlock()

	opts := PinOptions{
		Name:   name,
		URL:    fmt.Sprintf("/?mode=pin&id=%d", id),
		X:      x,
		Y:      y,
		Width:  dipW,
		Height: dipH,
		// 窗口没了就把字节放掉：ClosePin 与「用户直接关窗口」两条路
		// 都要落到这里，否则图片会一直留在内存里。
		OnClose: func() {
			s.mu.Lock()
			delete(s.pins, id)
			s.mu.Unlock()
		},
	}
	if err := s.d.Overlay.OpenPin(opts); err != nil {
		s.mu.Lock()
		delete(s.pins, id)
		s.mu.Unlock()
		return 0, fmt.Errorf("打开贴图窗口失败: %w", err)
	}
	return id, nil
}

// GetPinImage 取一张贴图的内容（贴图窗口自己在加载完成后调用）。
func (s *ScreenshotService) GetPinImage(id int) (PinImageDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pins[id]
	if !ok {
		return PinImageDTO{}, fmt.Errorf("贴图已关闭")
	}
	return PinImageDTO{
		MIME:       p.mime,
		Width:      p.width,
		Height:     p.height,
		DataBase64: p.dataB64,
	}, nil
}

// ClosePin 关掉一张贴图（幂等：已经关掉的再关一次不算错）。
func (s *ScreenshotService) ClosePin(id int) error {
	s.mu.Lock()
	p, ok := s.pins[id]
	delete(s.pins, id)
	s.mu.Unlock()

	if !ok || s.d.Overlay == nil {
		return nil
	}
	return s.d.Overlay.CloseWindow(p.name)
}

// handoffImageBytes 解码「另存为 / 贴图」交上来的图片字节。
//
// 允许带 data URL 前缀（前端两种传法都行），并先按 base64 长度粗判
// 体积 —— 免得把几十 MB 的字符串完整解码进内存再拒绝。
func handoffImageBytes(dataB64 string) ([]byte, error) {
	raw := strings.TrimSpace(stripDataURLPrefix(dataB64))
	if raw == "" {
		return nil, fmt.Errorf("图片内容为空")
	}
	if int64(len(raw))/4*3 > maxHandoffImageBytes {
		return nil, fmt.Errorf("图片过大（超过 %d MB）", maxHandoffImageBytes>>20)
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("图片数据不是合法的 base64")
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("图片内容为空")
	}
	if len(data) > maxHandoffImageBytes {
		return nil, fmt.Errorf("图片过大（超过 %d MB）", maxHandoffImageBytes>>20)
	}
	return data, nil
}
