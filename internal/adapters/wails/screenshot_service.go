package wails

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"sync"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/infra/imagecodec"
)

const (
	// screenshotMaxEdge / screenshotMaxBytes 是截屏专用的压缩上限。
	//
	// 为什么比 imagecodec 的默认值（1280 / 300KB）大：那两个取值针对的是
	// 「用户自己选的图片」（聊天窗口里放大后清晰即可），而截屏的内容常常
	// 是要给人看细节的（报错信息、代码、表格）。1920 是一张 1080p 屏的
	// 原生宽度，再小就真的会看不清。
	//
	// 上限必须留在接收侧之内（message.MaxInlineImageBytes = 512KB）——
	// 那个上限是协议面的约束，本次不动它。
	screenshotMaxEdge  = 1920
	screenshotMaxBytes = 480 << 10

	// screenshotJPEGQuality 是第一次编码的质量。
	//
	// 先编一次 JPEG 是为了绕开 imagecodec 的一个行为：它对 PNG 是
	// 「体积达标就原样保留」，只看书节数不看尺寸 —— 一张 400KB 的
	// 2560×1440 PNG 会被原样保留，绕过尺寸上限。自己编成 JPEG 之后，
	// imagecodec 的行为就确定了：合规则复用，不合规则缩放 + 降质。
	screenshotJPEGQuality = 92
)

// ScreenshotService 抓本机屏幕。
//
// 它是纯本地的：没有远端触发路径，也就不需要授权与确认。
// 这一点与「远程控制 / 屏幕共享」有本质区别（那两项本仓明确不做）。
type ScreenshotService struct {
	d Deps

	// mu 保护 session / pins —— 覆盖层窗口、贴图窗口与主窗口是多个
	// WebView，它们的调用会落在不同的 goroutine 上。
	mu sync.Mutex
	// session 是当前这一次交互式截图的状态（底图等）。
	session *overlaySession
	// pins 是当前贴在桌面上的图（id → 贴图会话）。
	//
	// 它与 session 的生命周期【不同】：贴图活到自己被关掉为止，
	// 而 session 在覆盖层收起时就没了。
	pins map[int]*pinSession
	// pinSeq 是下一个贴图 id（只增不减，避免同一窗口名被复用）。
	pinSeq int
}

// Capture 抓一张屏幕截图。
//
// mode 取 ports.ScreenshotModeKeepWindow（保留本窗口）或
// ports.ScreenshotModeHideWindow（隐藏本窗口）；传空串表示「用上次的模式」。
func (s *ScreenshotService) Capture(mode string) (ScreenshotDTO, error) {
	if s.d.Capturer == nil {
		return ScreenshotDTO{}, fmt.Errorf("截屏功能不可用：本机没有可用的屏幕抓取实现")
	}
	if s.d.Cfg == nil || !s.d.Cfg.Screenshot.Enabled {
		return ScreenshotDTO{}, fmt.Errorf("截屏功能已在设置中关闭")
	}

	// 模式归一：空串 → 上次的模式；非法值 → 交互式（兜底）。
	m := ports.NormalizeScreenshotMode(mode, s.d.Cfg.Screenshot.LastMode)
	if m == ports.ScreenshotModeInteractive {
		// 交互式要开覆盖层窗口、要在前端做选区与标注，
		// 这条「一张整屏」的路径做不到。
		return ScreenshotDTO{}, fmt.Errorf("交互式截图要走覆盖层，不能用整屏抓取")
	}

	img, err := s.d.Capturer.Capture(context.Background(), ports.ScreenshotExcludesSelf(m))
	if err != nil {
		return ScreenshotDTO{}, err
	}

	raw, err := encodeScreenshot(img)
	if err != nil {
		return ScreenshotDTO{}, err
	}

	inline, err := imagecodec.Prepare(raw, screenshotMaxEdge, screenshotMaxBytes)
	switch {
	case errors.Is(err, imagecodec.ErrNotImage):
		return ScreenshotDTO{}, fmt.Errorf("截屏结果不是可识别的图片")
	case errors.Is(err, imagecodec.ErrTooLarge):
		return ScreenshotDTO{}, fmt.Errorf("截屏结果过大，压缩后仍超出上限")
	case err != nil:
		return ScreenshotDTO{}, fmt.Errorf("处理截屏结果失败: %w", err)
	}

	return ScreenshotDTO{
		MIME:       inline.MIME,
		Width:      inline.W,
		Height:     inline.H,
		DataBase64: base64.StdEncoding.EncodeToString(inline.Data),
	}, nil
}

// RememberMode 记住用户显式选择的模式（工具栏下拉菜单选的那一次）。
//
// 只写 last_mode，【不碰】enabled —— enabled 归设置页管
// （SettingsService.Save）。两个写者会互相覆盖：设置页保存时会把用户
// 刚选的模式抹成配置里的旧值。
func (s *ScreenshotService) RememberMode(mode string) error {
	if s.d.Cfg == nil {
		return fmt.Errorf("配置尚未就绪")
	}
	m := ports.NormalizeScreenshotMode(mode, s.d.Cfg.Screenshot.LastMode)
	if m == s.d.Cfg.Screenshot.LastMode {
		return nil
	}
	s.d.Cfg.Screenshot.LastMode = m
	if err := config.Save(s.d.ConfigDir, s.d.Cfg); err != nil {
		return fmt.Errorf("保存截屏模式失败: %w", err)
	}
	return nil
}

// SendDraft 把输入框里的截图草稿发出去。
//
// 为什么单独一条路，而不是复用 ChatService.SendImageBytes：
// 那条路会走 imagecodec 的默认上限（1280px / 300KB）重新压一次 ——
// 而截屏在抓屏那一刻已经按 1920px / 480KB 压过了，再压一次就把那次
// 压缩白做了（4K 屏截图会糊成 1280 宽）。
//
// 仍然保留的两道校验与通用路径完全一致：
//   - 魔数嗅探（不信调用方给的 mime —— 它只是提示，真实格式按字节头判）
//   - 体积不超过接收侧上限（message.MaxInlineImageBytes）
//
// kind 是会话类型（"group" 走群发，其余走单聊），convID 在群聊时是群 id、
// 在单聊时是对端 node id —— 与前端会话对象的字段含义一致。
func (s *ScreenshotService) SendDraft(
	kind, convID, name, mime string,
	width, height int,
	dataB64 string,
) (MessageDTO, error) {
	if s.d.Cfg == nil || !s.d.Cfg.Screenshot.Enabled {
		return MessageDTO{}, fmt.Errorf("截屏功能已在设置中关闭")
	}
	data, err := decodeImageBytes(dataB64)
	if err != nil {
		return MessageDTO{}, err
	}
	content, err := preparePreparedImage(data, width, height)
	if err != nil {
		return MessageDTO{}, withImageSource(err, name)
	}

	if kind == "group" {
		if convID == "" {
			return MessageDTO{}, fmt.Errorf("缺少群 id")
		}
		msg, err := s.d.Group.SendGroupImage(context.Background(), convID, content)
		if err != nil {
			return MessageDTO{}, err
		}
		return fillDeliveryStat(ToMessageDTO(msg), statsOf(s.d, convID)), nil
	}

	id, err := identity.ParseNodeID(convID)
	if err != nil {
		return MessageDTO{}, fmt.Errorf("无效的 NodeID: %w", err)
	}
	msg, err := s.d.Chat.SendImage(context.Background(), id, content)
	if err != nil {
		return MessageDTO{}, err
	}
	return ToMessageDTO(msg), nil
}

// preparePreparedImage 把「已经压缩好」的图片字节内联成消息内容。
//
// 与 prepareImageData 的唯一区别是【不重新压缩】。其余校验（体积上限、
// 格式嗅探）保持一致，否则这条路就成了绕过约束的后门。
func preparePreparedImage(data []byte, width, height int) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("图片内容为空")
	}
	if len(data) > message.MaxInlineImageBytes {
		return "", fmt.Errorf("图片过大（%d 字节），超出单条图片消息上限", len(data))
	}
	if width <= 0 || height <= 0 {
		return "", fmt.Errorf("图片尺寸无效（%d×%d）", width, height)
	}
	// 魔数嗅探：不信任调用方给的 mime，只认字节头。
	format, err := imagecodec.Sniff(data)
	if err != nil {
		return "", fmt.Errorf("不是可识别的图片格式")
	}
	content, err := message.EncodeInlineImage(message.InlineImage{
		MIME: "image/" + format,
		W:    width,
		H:    height,
		B64:  base64.StdEncoding.EncodeToString(data),
	})
	if err != nil {
		return "", fmt.Errorf("图片内容编码失败: %w", err)
	}
	return content, nil
}

// encodeScreenshot 把抓到的像素编成 JPEG 字节。
func encodeScreenshot(img ports.ScreenImage) ([]byte, error) {
	if img.Width <= 0 || img.Height <= 0 {
		return nil, fmt.Errorf("截屏结果尺寸无效（%d×%d）", img.Width, img.Height)
	}
	want := img.Width * img.Height * 4
	if len(img.Data) < want {
		return nil, fmt.Errorf("截屏数据不完整（期望 %d 字节，实得 %d 字节）", want, len(img.Data))
	}
	// ScreenImage.Data 是紧密排列的 RGBA8888，与 image.RGBA 的 Pix 布局
	// 完全一致（RGBA 的 stride 就是 4*w，没有额外 padding），所以可以直接拷。
	rgba := image.NewRGBA(image.Rect(0, 0, img.Width, img.Height))
	copy(rgba.Pix, img.Data)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: screenshotJPEGQuality}); err != nil {
		return nil, fmt.Errorf("截屏编码失败: %w", err)
	}
	return buf.Bytes(), nil
}
