// Package imagecodec 把用户选中的图片规范化为「可以内联进聊天消息」的小图。
//
// 为什么独立成包：缩放与降质是纯计算，输入输出都是 []byte，
// 因此可以脱离文件系统、脱离应用层做边界测试（超大图、透明通道、动图、伪装成图片的文本）。
//
// 为什么放在 infra 而不是 domain：domain 层禁止 import os / net（红线 R1），
// 而这类「格式适配」本来就不属于业务规则。
//
// 为什么按魔数显式分发解码器，而不用 image.Decode：
// 通用解码函数依赖各 image/* 包在 init() 里自注册格式，注册漏了就会静默退化成
// 「不是可识别的图片格式」；更隐蔽的是，只要【测试文件】恰好 import 了那个包，
// 测试就会通过而线上依旧失败。显式分发把这个类别的缺陷从根上消掉。
package imagecodec

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"

	"golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

const (
	// DefaultMaxEdge 是输出图的最长边。1280 是聊天窗口里放大后仍然清晰的甜点：
	// 再大对阅读没有增益，只是白送流量。
	DefaultMaxEdge = 1280

	// DefaultMaxBytes 是压缩目标（300KB）。内联消息的代价是「一条消息 = 一张图」，
	// 300KB 让一万张图约占 3GB，对本地 SQLite 是可接受的量级。
	DefaultMaxBytes = 300 << 10
)

var (
	// ErrEmpty 输入为空。
	ErrEmpty = errors.New("imagecodec: 输入为空")
	// ErrNotImage 输入不是可识别的图片格式（按魔数判断，不信扩展名）。
	ErrNotImage = errors.New("imagecodec: 不是可识别的图片格式")
	// ErrTooLarge 已按最大压缩力度处理，仍然超出发送上限。
	ErrTooLarge = errors.New("imagecodec: 图片过大，压缩后仍超出上限")
)

// qualityLadder 是 JPEG 质量降级阶梯：从「几乎无损」逐级退到「能看」。
var qualityLadder = []int{85, 80, 75, 70, 65, 60}

// 支持的格式名（同时用作 Inline.MIME 的后缀部分）。
const (
	formatJPEG = "jpeg"
	formatPNG  = "png"
	formatGIF  = "gif"
	formatWebP = "webp"
	formatBMP  = "bmp"
)

// Inline 是规范化后的图片。
type Inline struct {
	MIME string // image/jpeg | image/png | image/gif | image/webp（BMP 一律转 JPEG）
	W, H int
	Data []byte
	// Downgraded 表示动图被降级成了静态图。
	//
	// 目前只由 Go 侧标记、尚未被 UI 消费（「动图过大，将按静态图发送」的提示
	// 留到后续批次），保留它是为了不丢掉这个已经算出来的事实。
	Downgraded bool
}

// Prepare 规范化一张图片：长边超过 maxEdge 则等比缩小，
// JPEG 质量沿阶梯逐级下调直到不超过 maxBytes。
//
// 保留原样的两种情况（无损且省事）：
//   - PNG / GIF / WebP 在【原字节数已达标】时原样输出：透明通道与动画一旦被
//     JPEG 化就永久丢失，而尺寸过大只影响显示 —— 显示尺寸本就由前端钳制，
//     为此牺牲透明度是划不来的；
//   - 尺寸与体积本就合规的 JPEG 不做重新编码（重编码只会损失画质）。
func Prepare(raw []byte, maxEdge, maxBytes int) (Inline, error) {
	if len(raw) == 0 {
		return Inline{}, ErrEmpty
	}
	if maxEdge <= 0 {
		maxEdge = DefaultMaxEdge
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	format, err := sniff(raw)
	if err != nil {
		return Inline{}, err
	}
	// 先只读文件头拿尺寸：原样保留的那条路径不该为了 W/H 就把整张位图解码进内存
	// （小体积、超大像素的图会让发送侧无谓地吃一大块内存）。
	w, h, err := decodeConfig(raw, format)
	if err != nil {
		return Inline{}, ErrNotImage
	}

	withinSize := len(raw) <= maxBytes
	mime := "image/" + format

	// 无损格式：体积达标就原样保留动画与透明通道。
	switch format {
	case formatPNG, formatGIF, formatWebP:
		if withinSize {
			return Inline{MIME: mime, W: w, H: h, Data: raw}, nil
		}
	}

	// JPEG 原尺寸已合规时直接复用原始字节，不重新编码。
	if format == formatJPEG && withinSize && max(w, h) <= maxEdge {
		return Inline{MIME: "image/jpeg", W: w, H: h, Data: raw}, nil
	}

	// 走到这里才真的需要像素数据（缩放 + 重编码）。
	img, err := decode(raw, format)
	if err != nil {
		return Inline{}, ErrNotImage
	}

	downgraded := format == formatGIF

	scaled := scaleDown(img, maxEdge)
	if hasAlpha(scaled) {
		// JPEG 不存 alpha：不先合成白底，透明区域会变成黑色。
		scaled = flattenOnWhite(scaled)
	}

	for _, q := range qualityLadder {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: q}); err != nil {
			return Inline{}, fmt.Errorf("imagecodec: JPEG 编码失败: %w", err)
		}
		if buf.Len() <= maxBytes {
			return Inline{
				MIME:       "image/jpeg",
				W:          scaled.Bounds().Dx(),
				H:          scaled.Bounds().Dy(),
				Data:       buf.Bytes(),
				Downgraded: downgraded,
			}, nil
		}
	}
	return Inline{}, ErrTooLarge
}

// sniff 按魔数判断格式。扩展名一律不信 —— 用户可以把任何文件改名成 .png。
func sniff(raw []byte) (string, error) {
	switch {
	case len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF:
		return formatJPEG, nil
	case len(raw) >= 8 && bytes.Equal(raw[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return formatPNG, nil
	case len(raw) >= 6 && (bytes.Equal(raw[:6], []byte("GIF87a")) || bytes.Equal(raw[:6], []byte("GIF89a"))):
		return formatGIF, nil
	case len(raw) >= 12 && bytes.Equal(raw[:4], []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")):
		return formatWebP, nil
	case len(raw) >= 2 && raw[0] == 'B' && raw[1] == 'M':
		return formatBMP, nil
	default:
		return "", ErrNotImage
	}
}

// decode 按格式调用对应的解码器。
func decode(raw []byte, format string) (image.Image, error) {
	r := bytes.NewReader(raw)
	switch format {
	case formatJPEG:
		return jpeg.Decode(r)
	case formatPNG:
		return png.Decode(r)
	case formatGIF:
		return gif.Decode(r)
	case formatWebP:
		return webp.Decode(r)
	case formatBMP:
		return bmp.Decode(r)
	default:
		return nil, ErrNotImage
	}
}

// decodeConfig 只读文件头拿尺寸，不解码像素。
func decodeConfig(raw []byte, format string) (int, int, error) {
	r := bytes.NewReader(raw)
	var (
		cfg image.Config
		err error
	)
	switch format {
	case formatJPEG:
		cfg, err = jpeg.DecodeConfig(r)
	case formatPNG:
		cfg, err = png.DecodeConfig(r)
	case formatGIF:
		cfg, err = gif.DecodeConfig(r)
	case formatWebP:
		cfg, err = webp.DecodeConfig(r)
	case formatBMP:
		cfg, err = bmp.DecodeConfig(r)
	default:
		return 0, 0, ErrNotImage
	}
	if err != nil {
		return 0, 0, ErrNotImage
	}
	return cfg.Width, cfg.Height, nil
}

// scaleDown 等比缩小到最长边不超过 maxEdge；已合规则原样返回（绝不放大）。
func scaleDown(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	longest := max(w, h)
	if longest <= maxEdge || longest == 0 {
		return src
	}

	ratio := float64(maxEdge) / float64(longest)
	nw := max(1, int(math.Round(float64(w)*ratio)))
	nh := max(1, int(math.Round(float64(h)*ratio)))

	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

// hasAlpha 判断图片是否存在非不透明像素。
//
// 优先用标准库类型自带的 Opaque()（O(1)），未知类型才退化为逐像素扫描。
func hasAlpha(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return !o.Opaque()
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0xffff {
				return true
			}
		}
	}
	return false
}

// flattenOnWhite 把带透明通道的图合成到白底上。
func flattenOnWhite(src image.Image) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}
