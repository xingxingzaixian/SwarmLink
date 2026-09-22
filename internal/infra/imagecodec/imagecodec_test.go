package imagecodec

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
)

// noisyJPEG 造一张压缩后不会太小的图：纯色图会被 JPEG 压到几百字节，
// 无法用来验证「缩到 1280 / 降质到 300KB 以内」这类边界。
func noisyJPEG(t *testing.T, w, h, quality int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(42))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(rnd.Intn(256)), G: uint8(rnd.Intn(256)), B: uint8(rnd.Intn(256)), A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("构造测试图: %v", err)
	}
	return buf.Bytes()
}

// smoothJPEG 造一张【压缩后远小于预算】的图：渐变图 JPEG 编码效率很高。
// 用它验证「本来就合规的图不做无意义的重编码」——
// 若用噪声图，它本身就会超出 300KB 预算，测的就不是这条分支了。
func smoothJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 128, A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("构造测试图: %v", err)
	}
	if buf.Len() > DefaultMaxBytes {
		t.Fatalf("测试前提不成立：构造出的渐变图 %d 字节已超出预算", buf.Len())
	}
	return buf.Bytes()
}

func smallPNGWithAlpha(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 40, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 40; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 200, G: 30, B: 90, A: uint8(x * 6)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("构造 PNG: %v", err)
	}
	return buf.Bytes()
}

func smallGIF(t *testing.T) []byte {
	t.Helper()
	pal := color.Palette{color.White, color.Black}
	img := image.NewPaletted(image.Rect(0, 0, 16, 16), pal)
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 2)
	}
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("构造 GIF: %v", err)
	}
	return buf.Bytes()
}

func TestPrepareScalesDownAndFitsBudget(t *testing.T) {
	raw := noisyJPEG(t, 3000, 2000, 95)

	got, err := Prepare(raw, DefaultMaxEdge, DefaultMaxBytes)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if longest := max(got.W, got.H); longest != DefaultMaxEdge {
		t.Errorf("长边 = %d，期望缩到 %d", longest, DefaultMaxEdge)
	}
	if len(got.Data) > DefaultMaxBytes {
		t.Errorf("压缩后 %d 字节，仍超出 %d", len(got.Data), DefaultMaxBytes)
	}
	if got.MIME != "image/jpeg" {
		t.Errorf("MIME = %q，期望 image/jpeg", got.MIME)
	}

	// 声明的尺寸必须与真实解码结果一致，否则前端占位比例会错
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatalf("解码结果: %v", err)
	}
	if cfg.Width != got.W || cfg.Height != got.H {
		t.Errorf("声明 %dx%d，实际 %dx%d", got.W, got.H, cfg.Width, cfg.Height)
	}
}

func TestPrepareKeepsSmallImageAsIs(t *testing.T) {
	raw := smoothJPEG(t, 800, 600)

	got, err := Prepare(raw, DefaultMaxEdge, DefaultMaxBytes)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.W != 800 || got.H != 600 {
		t.Errorf("小图不得放大: %dx%d", got.W, got.H)
	}
	if !bytes.Equal(got.Data, raw) {
		t.Error("原尺寸已满足条件时应原样输出，不做无意义的重新编码")
	}
}

func TestPrepareKeepsSmallPNGWithAlpha(t *testing.T) {
	got, err := Prepare(smallPNGWithAlpha(t), DefaultMaxEdge, DefaultMaxBytes)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.MIME != "image/png" {
		t.Errorf("MIME = %q，期望 image/png（转 JPEG 会丢掉透明通道）", got.MIME)
	}
}

// 尺寸超限但体积达标的透明 PNG 同样要原样保留：
// 透明图（图标、UI 稿）的 PNG 压缩率极高，3000px 宽也可能只有一两百 KB，
// 若因为「超过 1280px」就把它 JPEG 化，透明通道会被静默丢成白底。
func TestPrepareKeepsOversizedButSmallPNGWithAlpha(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3000, 2000)) // 默认全透明
	img.SetNRGBA(10, 10, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("构造 PNG: %v", err)
	}
	if buf.Len() > DefaultMaxBytes {
		t.Fatalf("测试前提不成立：构造出的透明 PNG 已超出预算（%d 字节）", buf.Len())
	}

	got, err := Prepare(buf.Bytes(), DefaultMaxEdge, DefaultMaxBytes)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.MIME != "image/png" {
		t.Errorf("MIME = %q，期望 image/png（体积达标就该保留，尺寸不影响透明通道的取舍）", got.MIME)
	}
	if got.W != 3000 || got.H != 2000 {
		t.Errorf("尺寸 = %dx%d，期望原样 3000x2000", got.W, got.H)
	}
}

// 「生产代码忘了注册 GIF 解码器」这类缺陷在本实现下不可能发生：
// 格式是按魔数显式分发、直接调用 gif.Decode 的，删掉 import 会直接编译不过 ——
// 编译期兜底比任何测试都可靠。本文件与 wails 层都 import 了 image/gif 造夹具，
// 那只是为了造测试数据，不再是（也不可能成为）对注册机制的验证。
func TestPrepareKeepsSmallGIF(t *testing.T) {
	got, err := Prepare(smallGIF(t), DefaultMaxEdge, DefaultMaxBytes)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.MIME != "image/gif" {
		t.Errorf("MIME = %q，期望 image/gif（转 JPEG 会丢动画）", got.MIME)
	}
}

func TestPrepareRejectsNotAnImage(t *testing.T) {
	if _, err := Prepare([]byte("这只是一个文本文件，只是扩展名叫 .png"), DefaultMaxEdge, DefaultMaxBytes); !errors.Is(err, ErrNotImage) {
		t.Fatalf("err = %v，期望 ErrNotImage", err)
	}
}

func TestPrepareRejectsEmpty(t *testing.T) {
	if _, err := Prepare(nil, DefaultMaxEdge, DefaultMaxBytes); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v，期望 ErrEmpty", err)
	}
}

func TestPrepareRejectsWhenStillTooLarge(t *testing.T) {
	raw := noisyJPEG(t, 3000, 2000, 95)
	// 把预算压到 1KB：所有质量档位都装不下，必须明确报错而不是悄悄发一张超标的图
	if _, err := Prepare(raw, DefaultMaxEdge, 1<<10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v，期望 ErrTooLarge", err)
	}
}

// 透明的图转 JPEG 之前必须合成到白底，否则透明区域会变成黑色。
func TestPrepareFlattensAlphaOnWhite(t *testing.T) {
	// 填随机噪声：纯透明的图 PNG 压缩率极高（会走「原样保留」分支），
	// 这里要的是「必然超出预算、必须走 JPEG 重编码」的那种输入。
	img := image.NewNRGBA(image.Rect(0, 0, 2000, 1200))
	rnd := rand.New(rand.NewSource(7))
	for y := 0; y < 1200; y++ {
		for x := 0; x < 2000; x++ {
			// 四周全透明，中心不透明
			a := uint8(0)
			if x > 500 && x < 1500 && y > 300 && y < 900 {
				a = 255
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(rnd.Intn(256)), G: uint8(rnd.Intn(256)), B: uint8(rnd.Intn(256)), A: a,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("构造 PNG: %v", err)
	}

	// 预算用默认值：噪声区的 PNG 有 MB 级，必然超预算而走 JPEG 分支
	got, err := Prepare(buf.Bytes(), DefaultMaxEdge, DefaultMaxBytes)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.MIME != "image/jpeg" {
		t.Fatalf("MIME = %q，期望 image/jpeg", got.MIME)
	}

	decoded, _, err := image.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatalf("解码: %v", err)
	}
	r, g, b, _ := decoded.At(2, 2).RGBA() // 左上角原本是透明区
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Errorf("透明区像素 = (%d,%d,%d)，期望接近白色（透明必须合成到白底）", r>>8, g>>8, b>>8)
	}
}
