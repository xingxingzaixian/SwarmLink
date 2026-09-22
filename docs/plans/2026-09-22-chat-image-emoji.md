# 聊天图片与表情 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 让聊天支持发送图片（气泡内直接显示、不入传输列表）与 emoji 表情输入。

**Architecture:** 图片经 Go 侧压缩后以 base64 内联进消息 `content`（`msg_type='image'`，JSON 小信封），完全复用既有单聊/群聊消息链路，不触碰 transfer 模块，因此传输列表天然不受影响。emoji 是普通字符，走既有文本消息。

**Tech Stack:** Go 1.25 / Wails v3 beta.24 / Vue 3 + Pinia + Vite 5 / 标准库 `image` + `golang.org/x/image/draw`。

**设计文档：** `docs/plans/2026-09-22-chat-image-emoji-design.md`

**本计划的约定**

- 本机没有 `make` / `task` CLI，所有命令用 `go` 与 `wails3` 原生命令。
- 按用户要求：不创建工作区（worktree）、不代为提交 git。每个 Task 末尾给出建议的提交信息，由你自己决定何时提交。
- 架构红线（必须遵守）：`domain/**` 不得 import `net` / `os` / `database/sql` / Wails；适配器之间不得互引；`app` / `domain` 不得 `new` 具体适配器。
- 改动导出方法后必须重新生成绑定：`wails3 generate bindings -f "-tags wails" -clean=true -ts -i .`

---

## Task 1: 内联图片消息格式（domain 层）

**Files:**

- Create: `internal/domain/message/inline_image.go`
- Test: `internal/domain/message/inline_image_test.go`

**Step 1: 写失败的测试**

```go
package message

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestInlineImageRoundTrip(t *testing.T) {
	raw := []byte("fake-image-bytes")
	want := InlineImage{MIME: "image/jpeg", W: 1280, H: 720, B64: base64.StdEncoding.EncodeToString(raw)}

	content, err := EncodeInlineImage(want)
	if err != nil {
		t.Fatalf("EncodeInlineImage: %v", err)
	}

	got, err := DecodeInlineImage(content)
	if err != nil {
		t.Fatalf("DecodeInlineImage: %v", err)
	}
	if got.MIME != want.MIME || got.W != want.W || got.H != want.H || got.B64 != want.B64 {
		t.Fatalf("往返后内容不一致: %+v", got)
	}
	if got.V != 1 {
		t.Errorf("V = %d，期望 1（未显式设置时应填默认版本）", got.V)
	}

	decoded, err := got.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(decoded) != string(raw) {
		t.Errorf("Bytes = %q，期望 %q", decoded, raw)
	}
}

func TestDecodeInlineImageRejectsGarbage(t *testing.T) {
	if _, err := DecodeInlineImage("这不是 JSON"); err == nil {
		t.Fatal("非 JSON 内容必须报错")
	}
}

func TestEncodeInlineImageRejectsEmptyPayload(t *testing.T) {
	if _, err := EncodeInlineImage(InlineImage{MIME: "image/jpeg"}); err == nil {
		t.Fatal("空图片内容必须报错")
	}
}

// 上限校验放在 Bytes 里：接收侧必须在【解码之后】判断真实字节数，
// 否则一条 base64 炸弹就能把消息表撑爆。
func TestInlineImageBytesRejectsOversize(t *testing.T) {
	huge := base64.StdEncoding.EncodeToString(make([]byte, MaxInlineImageBytes+1))
	if _, err := (InlineImage{B64: huge}).Bytes(); err == nil {
		t.Fatal("超过 MaxInlineImageBytes 必须报错")
	}
}

func TestDecodeInlineImageRejectsBrokenBase64(t *testing.T) {
	content := `{"v":1,"mime":"image/jpeg","w":1,"h":1,"b64":"!!!not base64!!!"}`
	img, err := DecodeInlineImage(content)
	if err != nil {
		t.Fatalf("解析信封本身不应失败: %v", err)
	}
	if _, err := img.Bytes(); err == nil {
		t.Fatal("非法 base64 必须在 Bytes 阶段报错")
	}
}

func TestMsgTypeImageIsStable(t *testing.T) {
	if MsgTypeImage != "image" {
		t.Fatalf("MsgTypeImage = %q，改成别的值会让已入库的历史消息解析不出来", MsgTypeImage)
	}
	if !strings.Contains(string(MsgTypeImage), "image") {
		t.Fatal("类型名必须自解释")
	}
}
```

**Step 2: 运行测试，确认失败**

Run: `go test ./internal/domain/message/ -run 'InlineImage|MsgTypeImage' -v`
Expected: FAIL —— `undefined: InlineImage` / `undefined: EncodeInlineImage`

**Step 3: 实现**

创建 `internal/domain/message/inline_image.go`：

```go
package message

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// MsgTypeImage 是图片消息类型。
//
// 取值必须稳定：已入库的历史消息靠它决定渲染分支，改名等于让旧消息变成空白气泡。
const MsgTypeImage MsgType = "image"

// MaxInlineImageBytes 是接收侧允许的最大【base64 解码后】字节数。
//
// 发送侧的目标是 300KB（infra/imagecodec），这里留出余量到 512KB 是刻意的：
// 上限的作用是挡住畸形/恶意消息，而不是复刻发送侧的策略 ——
// 若两处取同一个值，任何一次发送侧策略微调都会让旧客户端拒收新客户端的图。
const MaxInlineImageBytes = 512 << 10

// inlineImageVersion 当前内联信封版本。
const inlineImageVersion = 1

// InlineImage 是图片消息的 content 结构。
//
// 为什么用一个 JSON 信封而不是裸 base64：
//   - w/h 让前端在图片解码完成前就能按原始宽高比占位，消息列表不会跳；
//   - mime 用来区分 PNG（透明）与 GIF（动图），裸 base64 无从判断；
//   - v 为后续加字段（缩略图、原始文件名）留出演进空间。
type InlineImage struct {
	V    int    `json:"v"`
	MIME string `json:"mime"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	B64  string `json:"b64"`
}

// EncodeInlineImage 把内联图片编码成消息 content。
func EncodeInlineImage(img InlineImage) (string, error) {
	if img.B64 == "" {
		return "", fmt.Errorf("message: 内联图片内容为空")
	}
	if img.V == 0 {
		img.V = inlineImageVersion
	}
	b, err := json.Marshal(img)
	if err != nil {
		return "", fmt.Errorf("message: 编码内联图片: %w", err)
	}
	return string(b), nil
}

// DecodeInlineImage 解析图片消息 content。
//
// 只校验信封本身；base64 与体积校验交给 Bytes()，
// 这样调用方可以先把 w/h 用于占位，再决定要不要真的解码图片。
func DecodeInlineImage(content string) (InlineImage, error) {
	var img InlineImage
	if err := json.Unmarshal([]byte(content), &img); err != nil {
		return InlineImage{}, fmt.Errorf("message: 解析内联图片: %w", err)
	}
	return img, nil
}

// Bytes 解码图片字节并执行上限校验。
func (i InlineImage) Bytes() ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(i.B64)
	if err != nil {
		return nil, fmt.Errorf("message: 内联图片 base64 无效: %w", err)
	}
	if len(raw) > MaxInlineImageBytes {
		return nil, fmt.Errorf("message: 内联图片超出上限（%d > %d 字节）", len(raw), MaxInlineImageBytes)
	}
	return raw, nil
}
```

**Step 4: 运行测试，确认通过**

Run: `go test ./internal/domain/message/ -v`
Expected: PASS（全部用例）

**建议提交信息：** `feat(message): 内联图片消息格式（msg_type=image 的 JSON 信封）`

---

## Task 2: 图片压缩包 infra/imagecodec

**Files:**

- Create: `internal/infra/imagecodec/imagecodec.go`
- Test: `internal/infra/imagecodec/imagecodec_test.go`
- Modify: `go.mod`（新增 `golang.org/x/image`）

**Step 1: 加入依赖**

Run: `go get golang.org/x/image@latest && go mod tidy`
Expected: `go.mod` 出现 `golang.org/x/image vX.Y.Z`。

> 为什么允许新增这个依赖：标准库没有图像缩放算法，自己写双线性不仅慢而且容易在图上有明显伪影。`golang.org/x/image` 是纯 Go（无 CGO），只用到 `draw` 一个子包。

**Step 2: 写失败的测试**

创建 `internal/infra/imagecodec/imagecodec_test.go`：

```go
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
	raw := noisyJPEG(t, 800, 600, 80)

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
	img := image.NewNRGBA(image.Rect(0, 0, 2000, 1200))
	for y := 0; y < 1200; y++ {
		for x := 0; x < 2000; x++ {
			// 只在中心画不透明像素，四周全透明
			if x > 500 && x < 1500 && y > 300 && y < 900 {
				img.SetNRGBA(x, y, color.NRGBA{R: 20, G: 120, B: 220, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("构造 PNG: %v", err)
	}

	got, err := Prepare(buf.Bytes(), 1280, 20<<10) // 预算很小，逼它走 JPEG 分支
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
```

**Step 3: 运行测试，确认失败**

Run: `go test ./internal/infra/imagecodec/ -v`
Expected: FAIL —— `undefined: Prepare` / `undefined: ErrNotImage`

**Step 4: 实现**

创建 `internal/infra/imagecodec/imagecodec.go`：

```go
// Package imagecodec 把用户选中的图片规范化为「可以内联进聊天消息」的小图。
//
// 为什么独立成包：缩放与降质是纯计算，输入输出都是 []byte，
// 因此可以脱离文件系统、脱离应用层做边界测试（超大图、透明通道、动图、伪装成图片的文本）。
//
// 为什么放在 infra 而不是 domain：domain 层禁止 import os / net（红线 R1），
// 而这类「格式适配」本来就不属于业务规则。
package imagecodec

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"

	xdraw "golang.org/x/image/draw"
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

// Inline 是规范化后的图片。
type Inline struct {
	MIME string // image/jpeg | image/png | image/gif
	W, H int
	Data []byte
	// Downgraded 表示动图被降级成了静态图（UI 可据此提示用户）。
	Downgraded bool
}

// Prepare 规范化一张图片：长边超过 maxEdge 则等比缩小，
// JPEG 质量沿阶梯逐级下调直到不超过 maxBytes。
//
// 保留原样的两种情况（无损且省事）：
//   - PNG 与 GIF 在【原尺寸合规且原字节数已达标】时原样输出，以保住透明通道与动画；
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

	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return Inline{}, ErrNotImage
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Inline{}, ErrNotImage
	}

	longest := max(cfg.Width, cfg.Height)
	withinEdge := longest <= maxEdge
	withinSize := len(raw) <= maxBytes

	switch format {
	case "png", "gif":
		if withinEdge && withinSize {
			mime := "image/png"
			if format == "gif" {
				mime = "image/gif"
			}
			return Inline{MIME: mime, W: cfg.Width, H: cfg.Height, Data: raw}, nil
		}
	}

	downgraded := format == "gif"

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
```

**Step 5: 运行测试，确认通过**

Run: `go test ./internal/infra/imagecodec/ -v`
Expected: PASS

**Step 6: 跑一次全量单测，确认没影响既有包**

Run: `go test ./internal/...`
Expected: PASS

**建议提交信息：** `feat(imagecodec): 图片规范化（缩放 + 逐级降质 + 白底合成）`

---

## Task 3: 群聊协议补 msg_type / file_id

**Files:**

- Modify: `internal/domain/protocol/wire.go`（`GroupMsg` 结构体）
- Modify: `internal/app/group_app.go`（`sendGroupMsgTo`、`HandleGroupMsg`、`SendGroupMessage`）
- Test: `internal/app/group_app_test.go`（若已存在则追加，不存在则新建）

**Step 1: 先看现有定义**

Run: `go doc ./internal/domain/protocol GroupMsg`（或直接读 `internal/domain/protocol/wire.go` 的 `GroupMsg`）
确认当前字段为 `MsgID / GroupID / SenderID / Content / SentAt`。

**Step 2: 写失败的测试**

在 `internal/app/group_app_test.go` 追加（文件不存在的则新建，package 为 `app`）：

```go
// 群消息必须把 msg_type 原样带到线上：否则群里的图片到了对方会被当成文本，
// 渲染成一个空白的文字气泡。
func TestGroupMessageCarriesMsgType(t *testing.T) {
	clk := clock.New()
	store := mem.NewMessages(clk)
	peers := mem.NewPeers(clk)
	groups := mem.NewGroups()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	if err := groups.Upsert(group.Group{
		ID:      "g1",
		Name:    "测试群",
		OwnerID: self,
		Epoch:   1,
		Members: []group.Member{{NodeID: self, Role: group.RoleOwner}, {NodeID: peerID, Role: group.RoleMember}},
	}); err != nil {
		t.Fatalf("建群: %v", err)
	}

	gapp := NewGroupApp(self, store, groups, peers, noConns{}, eventbus.New(), clk, nil)
	gapp.MaxDialConcurrency = 1

	msg, err := gapp.SendGroupImage(context.Background(), "g1", `{"v":1,"b64":"aGk=","w":1,"h":1,"mime":"image/jpeg"}`)
	if err != nil {
		t.Fatalf("SendGroupImage: %v", err)
	}
	if msg.MsgType != message.MsgTypeImage {
		t.Errorf("落库的 MsgType = %q，期望 %q", msg.MsgType, message.MsgTypeImage)
	}

	// 入站：把对方发来的群图片消息解出来，类型必须保持
	frame := mustGroupFrame(t, protocol.GroupMsg{
		MsgID:    "m-in",
		GroupID:  "g1",
		SenderID: peerID.String(),
		Content:  `{"v":1,"b64":"aGk=","w":1,"h":1,"mime":"image/jpeg"}`,
		MsgType:  string(message.MsgTypeImage),
		SentAt:   clk.Now().UnixMilli(),
	})
	if err := gapp.HandleGroupMsg(stubSession{peer: peerID}, frame); err != nil {
		t.Fatalf("HandleGroupMsg: %v", err)
	}

	got, ok := store.Get("m-in")
	if !ok {
		t.Fatal("入站群图片消息未落库")
	}
	if got.MsgType != message.MsgTypeImage {
		t.Errorf("入站消息 MsgType = %q，期望 %q", got.MsgType, message.MsgTypeImage)
	}
}
```

同时补齐三个小 fake（若文件内已存在同名 helper 则复用）：

```go
type noConns struct{}

func (noConns) Dial(context.Context, identity.NodeID, string) (ports.Session, error) {
	return nil, errors.New("test: 无会话")
}
func (noConns) Accept(context.Context) (ports.Session, error)   { return nil, errors.New("test: 无会话") }
func (noConns) SessionOf(identity.NodeID) (ports.Session, bool) { return nil, false }
func (noConns) Broadcast(protocol.Frame, ...identity.NodeID) error { return nil }
func (noConns) Close(identity.NodeID) error                      { return nil }

// stubSession 只需要能回 ACK。
type stubSession struct{ peer identity.NodeID }

func (s stubSession) PeerID() identity.NodeID                       { return s.peer }
func (s stubSession) Send(protocol.Frame) error                     { return nil }
func (s stubSession) Close() error                                  { return nil }
func (s stubSession) RemoteAddr() string                            { return "" }

func mustNodeID(t *testing.T, hex string) identity.NodeID {
	t.Helper()
	id, err := identity.ParseNodeID(hex)
	if err != nil {
		t.Fatalf("解析 NodeID: %v", err)
	}
	return id
}

func mustGroupFrame(t *testing.T, gm protocol.GroupMsg) protocol.Frame {
	t.Helper()
	payload, err := protocol.EncodeJSON(gm)
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	return protocol.New(protocol.TypeGroupMsg, payload)
}
```

> 若 `ports.Session` 的方法集与上面不一致，直接 `go doc ./internal/domain/ports Session` 对齐签名（只保留接口里真实存在的方法）。

**Step 3: 运行测试，确认失败**

Run: `go test ./internal/app/ -run TestGroupMessageCarriesMsgType -v`
Expected: FAIL —— `gm.MsgType undefined` / `undefined: SendGroupImage`

**Step 4: 实现**

`internal/domain/protocol/wire.go` 的 `GroupMsg`：

```go
// GroupMsg 群聊消息。
//
// MsgType / FileID 与单聊的 Chat 对齐：群里的图片消息同样需要声明类型，
// 否则接收端只能当纯文本渲染。新增字段对旧版本是「忽略未知字段」，向后兼容。
type GroupMsg struct {
	MsgID    string `json:"msg_id"`
	GroupID  string `json:"group_id"`
	SenderID string `json:"sender_id"`
	Content  string `json:"content"`
	MsgType  string `json:"msg_type,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	SentAt   int64  `json:"sent_at"`
}
```

`internal/app/group_app.go`：

1. `sendGroupMsgTo` 补两个字段：

```go
	payload, err := protocol.EncodeJSON(protocol.GroupMsg{
		MsgID:    msg.MsgID,
		GroupID:  msg.ConvID,
		SenderID: msg.SenderID.String(),
		Content:  msg.Content,
		MsgType:  string(msg.MsgType), // 群里也要声明类型，否则图片到达后变成空白文字气泡
		FileID:   msg.FileID,
		SentAt:   msg.SentAt.UnixMilli(),
	})
```

2. `HandleGroupMsg` 解出类型（放在构造 `m` 之前）：

```go
	msgType := message.MsgType(gm.MsgType)
	if msgType == "" {
		msgType = message.MsgTypeText
	}
```

并在 `m` 中把 `MsgType: message.MsgTypeText` 改为 `MsgType: msgType`，同时补 `FileID: gm.FileID`。

3. `SendGroupMessage` 抽出通用构造函数并新增图片入口（与 Task 4 的单聊实现保持同样的形状）：

```go
// SendGroupMessage 持久化并扇出到所有在线成员。
func (a *GroupApp) SendGroupMessage(ctx context.Context, groupID, text string) (message.Message, error) {
	return a.send(ctx, groupID, text, message.MsgTypeText)
}

// SendGroupImage 发送群聊图片消息（content 为内联图片 JSON 信封）。
func (a *GroupApp) SendGroupImage(ctx context.Context, groupID, content string) (message.Message, error) {
	return a.send(ctx, groupID, content, message.MsgTypeImage)
}

func (a *GroupApp) send(ctx context.Context, groupID, content string, msgType message.MsgType) (message.Message, error) {
	g, ok := a.groups.Get(groupID)
	if !ok {
		return message.Message{}, fmt.Errorf("group: unknown group %s", groupID)
	}
	now := a.clk.Now()
	msg := message.Message{
		MsgID:     message.NewID(),
		ConvID:    message.GroupConvID(groupID),
		SenderID:  a.self,
		Direction: message.DirectionOut,
		Content:   content,
		MsgType:   msgType,
		SentAt:    now,
		State:     message.StatePending,
	}
	if err := a.store.AppendOutgoing(msg, now); err != nil {
		return msg, err
	}
	go a.fanout(ctx, g, msg)
	return msg, nil
}
```

**Step 5: 运行测试，确认通过**

Run: `go test ./internal/app/ -v`
Expected: PASS

**建议提交信息：** `feat(group): GroupMsg 支持 msg_type/file_id 并新增 SendGroupImage`

---

## Task 4: 单聊图片发送与接收校验

**Files:**

- Modify: `internal/app/chat_app.go`
- Create: `internal/app/chat_app_test.go`

**Step 1: 写失败的测试**

创建 `internal/app/chat_app_test.go`。测试用 `internal/adapters/store/mem`（它实现了 `ports.ChatStore`），不写 stub —— mem 与 sqlite 是对等实现，这正是它的用途。

```go
package app

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

const (
	testSelfHex = "1122334455667788"
	testPeerHex = "aabbccddeeff0011"
)

// 图片消息必须带着 msg_type=image 落库：这是前端决定渲染成图还是文字的唯一依据。
func TestSendImagePersistsImageMessage(t *testing.T) {
	clk := clock.New()
	store := mem.NewMessages(clk)
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	capp := NewChatApp(self, store, noConns{}, mem.NewPeers(clk), eventbus.New(), clk, nil)

	content, err := message.EncodeInlineImage(message.InlineImage{
		MIME: "image/jpeg",
		W:    4,
		H:    4,
		B64:  base64.StdEncoding.EncodeToString([]byte("jpegdata")),
	})
	if err != nil {
		t.Fatalf("编码内联图: %v", err)
	}

	msg, err := capp.SendImage(context.Background(), peerID, content)
	if err != nil {
		t.Fatalf("SendImage: %v", err)
	}
	if msg.MsgType != message.MsgTypeImage {
		t.Errorf("MsgType = %q，期望 %q", msg.MsgType, message.MsgTypeImage)
	}

	got, ok := store.Get(msg.MsgID)
	if !ok {
		t.Fatal("图片消息未落库")
	}
	img, err := message.DecodeInlineImage(got.Content)
	if err != nil {
		t.Fatalf("落库内容不是合法的内联图片: %v", err)
	}
	if img.W != 4 || img.H != 4 {
		t.Errorf("落库尺寸 = %dx%d，期望 4x4", img.W, img.H)
	}
}

// 入站图片消息要能进历史并触发 UI 事件。
func TestHandleChatAcceptsImageMessage(t *testing.T) {
	clk := clock.New()
	store := mem.NewMessages(clk)
	bus := eventbus.New()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	var received []message.Message
	bus.Subscribe(eventbus.TopicChatReceived, func(payload any) {
		if m, ok := payload.(message.Message); ok {
			received = append(received, m)
		}
	})

	capp := NewChatApp(self, store, noConns{}, mem.NewPeers(clk), bus, clk, nil)

	content := `{"v":1,"mime":"image/jpeg","w":2,"h":2,"b64":"` +
		base64.StdEncoding.EncodeToString([]byte("img")) + `"}`
	payload, err := protocol.EncodeJSON(protocol.Chat{
		MsgID:    "m-img",
		SenderID: peerID.String(),
		Content:  content,
		MsgType:  string(message.MsgTypeImage),
		SentAt:   clk.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("编码: %v", err)
	}

	if err := capp.HandleChat(stubSession{peer: peerID}, protocol.New(protocol.TypeChat, payload)); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}
	if _, ok := store.Get("m-img"); !ok {
		t.Fatal("合法图片消息未落库")
	}
	if len(received) != 1 {
		t.Fatalf("chat 事件数 = %d，期望 1", len(received))
	}
}

// 畸形/超限的图片消息必须被丢弃，且【仍然回 ACK】——
// 不回 ACK 会让发送方按 outbox 策略无限重发同一条垃圾消息。
func TestHandleChatDropsBrokenImageButStillAcks(t *testing.T) {
	clk := clock.New()
	store := mem.NewMessages(clk)
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	capp := NewChatApp(self, store, noConns{}, mem.NewPeers(clk), eventbus.New(), clk, nil)

	huge := base64.StdEncoding.EncodeToString(make([]byte, message.MaxInlineImageBytes+1))
	cases := map[string]string{
		"不是 JSON":  "这是一段普通文字",
		"base64 非法": `{"v":1,"mime":"image/jpeg","w":1,"h":1,"b64":"!!!bad!!!"}`,
		"超出体积上限": `{"v":1,"mime":"image/jpeg","w":1,"h":1,"b64":"` + huge + `"}`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			sess := &recordingSession{peer: peerID}
			payload, err := protocol.EncodeJSON(protocol.Chat{
				MsgID:    "m-" + name,
				SenderID: peerID.String(),
				Content:  content,
				MsgType:  string(message.MsgTypeImage),
			})
			if err != nil {
				t.Fatalf("编码: %v", err)
			}
			if err := capp.HandleChat(sess, protocol.New(protocol.TypeChat, payload)); err != nil {
				t.Fatalf("HandleChat 不应把坏消息变成连接级错误: %v", err)
			}
			if _, ok := store.Get("m-" + name); ok {
				t.Error("畸形图片消息不得落库")
			}
			if !sess.acked {
				t.Error("必须回 ACK，否则发送方会无限重发")
			}
		})
	}
}

// 文本消息不受新校验影响（回归）。
func TestHandleChatStillAcceptsPlainText(t *testing.T) {
	clk := clock.New()
	store := mem.NewMessages(clk)
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	capp := NewChatApp(self, store, noConns{}, mem.NewPeers(clk), eventbus.New(), clk, nil)
	payload, _ := protocol.EncodeJSON(protocol.Chat{
		MsgID: "m-text", SenderID: peerID.String(), Content: "你好", SentAt: clk.Now().UnixMilli(),
	})
	if err := capp.HandleChat(stubSession{peer: peerID}, protocol.New(protocol.TypeChat, payload)); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}
	got, ok := store.Get("m-text")
	if !ok {
		t.Fatal("文本消息未落库")
	}
	if got.MsgType != message.MsgTypeText {
		t.Errorf("MsgType = %q，期望 text", got.MsgType)
	}
}

// 尺寸字段来自网络，必须钳制：否则伪造的 w/h 能让前端按天文数字撑开占位框。
func TestHandleChatClampsImageDimensions(t *testing.T) {
	clk := clock.New()
	store := mem.NewMessages(clk)
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	capp := NewChatApp(self, store, noConns{}, mem.NewPeers(clk), eventbus.New(), clk, nil)
	content := `{"v":1,"mime":"image/jpeg","w":9999999,"h":-5,"b64":"` +
		base64.StdEncoding.EncodeToString([]byte("img")) + `"}`
	payload, _ := protocol.EncodeJSON(protocol.Chat{
		MsgID: "m-clamp", SenderID: peerID.String(), Content: content, MsgType: string(message.MsgTypeImage),
	})
	if err := capp.HandleChat(stubSession{peer: peerID}, protocol.New(protocol.TypeChat, payload)); err != nil {
		t.Fatalf("HandleChat: %v", err)
	}
	got, ok := store.Get("m-clamp")
	if !ok {
		t.Fatal("消息未落库")
	}
	img, err := message.DecodeInlineImage(got.Content)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if img.W > 20000 || img.H < 0 || img.H > 20000 {
		t.Errorf("尺寸未钳制: %dx%d", img.W, img.H)
	}
}

// recordingSession 记录是否回过 ACK。
type recordingSession struct {
	peer  identity.NodeID
	acked bool
}

func (s *recordingSession) PeerID() identity.NodeID { return s.peer }
func (s *recordingSession) Send(f protocol.Frame) error {
	if f.Type == protocol.TypeChatAck {
		s.acked = true
	}
	return nil
}
func (s *recordingSession) Close() error { return nil }

var _ = strings.TrimSpace
```

`noConns` / `stubSession` / `mustNodeID` 已在 Task 3 的 `group_app_test.go` 中定义（同一个 package），这里直接复用。

**Step 2: 运行测试，确认失败**

Run: `go test ./internal/app/ -run 'TestSendImagePersists|TestHandleChat' -v`
Expected: FAIL —— `undefined: SendImage`

**Step 3: 实现**

`internal/app/chat_app.go`：

1. 把 `SendMessage` 改为薄壳 + 新增图片入口：

```go
// SendMessage 持久化并尽力发送一条单聊文本消息。
func (a *ChatApp) SendMessage(ctx context.Context, peerID identity.NodeID, text string) (message.Message, error) {
	return a.send(ctx, peerID, text, message.MsgTypeText)
}

// SendImage 发送单聊图片消息。
//
// content 是 message.EncodeInlineImage 产出的 JSON 信封（不是裸 base64）：
// 图片在发送前已被压到 300KB 量级，因此直接内联进消息体，
// 走的是与文本完全相同的「入库 → trySend → outbox 重发 → ACK」链路。
func (a *ChatApp) SendImage(ctx context.Context, peerID identity.NodeID, content string) (message.Message, error) {
	return a.send(ctx, peerID, content, message.MsgTypeImage)
}

func (a *ChatApp) send(ctx context.Context, peerID identity.NodeID, content string, msgType message.MsgType) (message.Message, error) {
	now := a.clk.Now()
	msg := message.Message{
		MsgID:     message.NewID(),
		ConvID:    message.DirectConvID(a.self, peerID),
		SenderID:  a.self,
		Direction: message.DirectionOut,
		Content:   content,
		MsgType:   msgType,
		SentAt:    now,
		State:     message.StatePending,
	}
	if err := a.store.AppendOutgoing(msg, now); err != nil {
		return msg, fmt.Errorf("chat: persist outgoing: %w", err)
	}
	if err := a.trySend(ctx, peerID, msg); err != nil && a.lg != nil {
		a.lg.Info("chat: initial send deferred to outbox", "msg_id", msg.MsgID, "err", err)
	}
	return msg, nil
}
```

2. `HandleChat` 在构造 `m` 之前加入图片校验，并按需钳制尺寸：

```go
	content := c.Content
	if msgType == message.MsgTypeImage {
		sanitized, ok := sanitizeImageContent(content)
		if !ok {
			// 坏消息的处置：不落库、不通知 UI，但【照常回 ACK】。
			// 不回 ACK 会让发送方按 outbox 策略无限重发同一条垃圾消息。
			return a.sendAck(sess, c.MsgID)
		}
		content = sanitized
	}
```

3. 新增钳制函数：

```go
// maxImageEdge 是接收侧允许声明的最大边长。
//
// 尺寸字段来自网络，只用于前端占位；钳制它的意义是防止伪造的
// w/h 让接收端按天文数字撑开占位框。真实的图片尺寸由浏览器的解码结果决定。
const maxImageEdge = 20000

// sanitizeImageContent 校验并规范化入站的内联图片 content。
func sanitizeImageContent(content string) (string, bool) {
	img, err := message.DecodeInlineImage(content)
	if err != nil {
		return "", false
	}
	if _, err := img.Bytes(); err != nil { // base64 合法性 + 体积上限
		return "", false
	}
	if img.W < 0 || img.H < 0 || img.W > maxImageEdge || img.H > maxImageEdge {
		img.W = clampEdge(img.W)
		img.H = clampEdge(img.H)
		out, err := message.EncodeInlineImage(img)
		if err != nil {
			return "", false
		}
		return out, true
	}
	return content, true
}

func clampEdge(v int) int {
	if v < 0 {
		return 0
	}
	if v > maxImageEdge {
		return maxImageEdge
	}
	return v
}
```

并把 `m` 的 `Content: c.Content` 改成 `Content: content`。

**Step 4: 运行测试，确认通过**

Run: `go test ./internal/app/ -v`
Expected: PASS

**建议提交信息：** `feat(chat): 单聊图片发送与接收校验（坏消息丢弃但仍回 ACK）`

---

## Task 5: Wails 服务层与绑定

**Files:**

- Modify: `internal/adapters/wails/services.go`（`ChatService`、`GroupService`）
- Test: `internal/adapters/wails/services_test.go`（追加）

**Step 1: 写失败的测试**

在 `internal/adapters/wails/services_test.go` 追加（该文件已有 `stubConns` / `emptyDir` / `selfID` 可复用）：

```go
func newChatService(t *testing.T) *ChatService {
	t.Helper()
	clk := clock.New()
	store := mem.NewMessages(clk)
	capp := app.NewChatApp(selfID(t), store, stubConns{}, mem.NewPeers(clk), eventbus.New(), clk, nil)
	return &ChatService{d: Deps{Self: SelfInfo{NodeID: selfID(t)}, Chat: capp}}
}

func writeTestPNG(t *testing.T) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 32, 24))
	p := filepath.Join(t.TempDir(), "shot.png")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建文件: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("写 PNG: %v", err)
	}
	return p
}

// 服务层的职责：路径校验 → 读文件 → 压缩 → 编码 → 落库，并回一个图片类型的 DTO。
func TestSendImagePersistsImageDTO(t *testing.T) {
	svc := newChatService(t)

	dto, err := svc.SendImage(testPeerHex, writeTestPNG(t))
	if err != nil {
		t.Fatalf("SendImage: %v", err)
	}
	if dto.MsgType != string(message.MsgTypeImage) {
		t.Errorf("MsgType = %q，期望 image", dto.MsgType)
	}
	if _, err := message.DecodeInlineImage(dto.Content); err != nil {
		t.Fatalf("DTO.content 不是合法的内联图片: %v", err)
	}
}

// 伪装成图片的文本必须在服务层同步报错，而不是发出去让对方显示一个坏气泡。
func TestSendImageRejectsNonImage(t *testing.T) {
	svc := newChatService(t)
	p := filepath.Join(t.TempDir(), "fake.png")
	if err := os.WriteFile(p, []byte("not an image at all"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}

	if _, err := svc.SendImage(testPeerHex, p); err == nil {
		t.Fatal("非图片必须报错")
	}
}

// 粘贴路径只有字节没有路径，走 SendImageBytes。
func TestSendImageBytesWorks(t *testing.T) {
	svc := newChatService(t)

	raw, err := os.ReadFile(writeTestPNG(t))
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	dto, err := svc.SendImageBytes(testPeerHex, "clipboard.png", raw)
	if err != nil {
		t.Fatalf("SendImageBytes: %v", err)
	}
	if dto.MsgType != string(message.MsgTypeImage) {
		t.Errorf("MsgType = %q，期望 image", dto.MsgType)
	}
}

// 另存为：把已入库的图片写回用户选定的路径。
func TestSaveImageWritesDecodedBytes(t *testing.T) {
	svc := newChatService(t)
	sent, err := svc.SendImage(testPeerHex, writeTestPNG(t))
	if err != nil {
		t.Fatalf("SendImage: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "saved.png")
	if err := svc.SaveImage(sent.MsgID, dest); err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读取另存文件: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("另存文件为空")
	}
	if _, _, err := image.Decode(bytes.NewReader(got)); err != nil {
		t.Fatalf("另存的文件不是图片: %v", err)
	}
}
```

新增 import：`bytes`、`image`、`image/png`、`internal/adapters/store/mem`、`internal/domain/message`、`internal/infra/eventbus`。

**Step 2: 运行测试，确认失败**

Run: `go test ./internal/adapters/wails/ -run 'TestSendImage|TestSaveImage' -v`
Expected: FAIL —— `svc.SendImage undefined`

**Step 3: 实现**

`ChatApp` 需要暴露按 id 取消息（`SaveImage` 用），在 `internal/app/chat_app.go` 加：

```go
// Message 按 msg_id 取一条消息（另存为等按 id 操作的入口用）。
func (a *ChatApp) Message(msgID string) (message.Message, bool) {
	if a.store == nil {
		return message.Message{}, false
	}
	return a.store.Get(msgID)
}
```

在 `internal/adapters/wails/services.go` 的 `ChatService` 下新增：

```go
// SendImage 发送一张本地图片：读文件 → 压缩 → 内联进消息。
//
// 为什么在这里（而不是前端）压缩：前端拿到的是路径或 Blob，压缩需要解码 + 缩放 + 体积控制，
// 放在 Go 侧只需实现一次，且不会把大图搬进 WebView 内存。
func (s *ChatService) SendImage(peerID, path string) (MessageDTO, error) {
	id, err := identity.ParseNodeID(peerID)
	if err != nil {
		return MessageDTO{}, fmt.Errorf("无效的 NodeID: %w", err)
	}
	if err := checkSendableFile(path); err != nil {
		return MessageDTO{}, err
	}
	content, err := prepareImageContent(path, nil)
	if err != nil {
		return MessageDTO{}, err
	}
	msg, err := s.d.Chat.SendImage(context.Background(), id, content)
	if err != nil {
		return MessageDTO{}, err
	}
	return ToMessageDTO(msg), nil
}

// SendImageBytes 发送剪贴板里的图片：浏览器只能拿到 Blob，拿不到路径。
func (s *ChatService) SendImageBytes(peerID, name string, data []byte) (MessageDTO, error) {
	id, err := identity.ParseNodeID(peerID)
	if err != nil {
		return MessageDTO{}, fmt.Errorf("无效的 NodeID: %w", err)
	}
	if len(data) == 0 {
		return MessageDTO{}, fmt.Errorf("剪贴板里没有图片数据")
	}
	content, err := prepareImageContent(name, data)
	if err != nil {
		return MessageDTO{}, err
	}
	msg, err := s.d.Chat.SendImage(context.Background(), id, content)
	if err != nil {
		return MessageDTO{}, err
	}
	return ToMessageDTO(msg), nil
}

// SaveImage 把一条图片消息另存到指定路径。
func (s *ChatService) SaveImage(msgID, destPath string) error {
	if msgID == "" {
		return fmt.Errorf("缺少消息 id")
	}
	if destPath == "" {
		return fmt.Errorf("请选择保存位置")
	}
	msg, ok := s.d.Chat.Message(msgID)
	if !ok {
		return fmt.Errorf("找不到该图片消息")
	}
	if msg.MsgType != message.MsgTypeImage {
		return fmt.Errorf("这条消息不是图片")
	}
	img, err := message.DecodeInlineImage(msg.Content)
	if err != nil {
		return fmt.Errorf("图片内容已损坏: %w", err)
	}
	raw, err := img.Bytes()
	if err != nil {
		return err
	}
	if err := os.WriteFile(destPath, raw, 0o644); err != nil {
		return fmt.Errorf("保存图片失败: %w", err)
	}
	return nil
}

// prepareImageContent 把「一张图（路径或字节）」变成可内联的消息 content。
//
// 错误信息刻意做成用户能读懂并据此行动的中文：图片发送失败的原因
// 只可能是「不是图片 / 太大 / 读不了」这三种，让用户猜是最差的选择。
func prepareImageContent(name string, data []byte) (string, error) {
	if data == nil {
		raw, err := os.ReadFile(name)
		if err != nil {
			return "", fmt.Errorf("无法读取该图片: %w", err)
		}
		data = raw
	}
	img, err := imagecodec.Prepare(data, imagecodec.DefaultMaxEdge, imagecodec.DefaultMaxBytes)
	switch {
	case errors.Is(err, imagecodec.ErrNotImage):
		return "", fmt.Errorf("不是可识别的图片格式")
	case errors.Is(err, imagecodec.ErrTooLarge):
		return "", fmt.Errorf("图片过大，压缩后仍超出上限，请改用「发送文件」")
	case errors.Is(err, imagecodec.ErrEmpty):
		return "", fmt.Errorf("图片内容为空")
	case err != nil:
		return "", fmt.Errorf("处理图片失败: %w", err)
	}
	content, err := message.EncodeInlineImage(message.InlineImage{
		MIME: img.MIME,
		W:    img.W,
		H:    img.H,
		B64:  base64.StdEncoding.EncodeToString(img.Data),
	})
	if err != nil {
		return "", err
	}
	return content, nil
}
```

`GroupService` 下新增同形的 `SendImage(peerID, path)` / `SendImageBytes(peerID, name, data)`，内部把 `s.d.Chat.SendImage` 换成 `s.d.Group.SendGroupImage(ctx, groupID, content)`（参数名改为 `groupID`）。

新增 import：`encoding/base64`、`errors`、`os`、`internal/infra/imagecodec`、`internal/domain/message`。

**Step 4: 运行测试，确认通过**

Run: `go test ./internal/adapters/wails/ -v`
Expected: PASS

**Step 5: 编译整个桌面应用**

Run: `go build -tags wails -o bin/SwarmLink.exe .`
Expected: 无输出、退出码 0

**Step 6: 重新生成前端绑定**

Run: `wails3 generate bindings -f "-tags wails" -clean=true -ts -i .`
Expected: `frontend/bindings/.../chatservice.ts` 出现 `SendImage` / `SendImageBytes` / `SaveImage`；`groupservice.ts` 出现 `SendImage` / `SendImageBytes`。

Run: `cd frontend; npm run typecheck`
Expected: 暂无变化（前端还没引用新方法），退出码 0

**Step 7: 全量 Go 测试回归**

Run: `go test ./...`
Expected: PASS

**建议提交信息：** `feat(wails): 图片发送/另存为服务与重新生成的绑定`

---

## Task 6: 前端 API 层与 chat store

**Files:**

- Modify: `frontend/src/api/types.ts`（`ImageContent`、`parseImageContent`、`SwarmApi` 新方法）
- Modify: `frontend/src/api/bindings.ts`（`realApi` 映射）
- Modify: `frontend/src/api/mock.ts`（降级实现）
- Modify: `frontend/src/stores/chat.ts`（`sendImage` / `sendImageBytes`）
- Create: `frontend/src/utils/image.ts`（路径判图 + 内联图片解析）

**Step 1: 先写类型与解析工具**

创建 `frontend/src/utils/image.ts`：

```ts
/**
 * 图片相关的前端工具。
 *
 * 内联图片的 content 是 Go 侧写下的 JSON 信封，解析失败【不能抛错】：
 * 历史消息里可能存在旧版本或损坏的数据，一条坏消息不该让整个聊天区崩掉。
 */
export interface ImageContent {
  v: number
  mime: string
  w: number
  h: number
  b64: string
}

const IMAGE_EXT = ['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp']

/** 按扩展名粗判是否图片。仅用于 UX 分流，真实格式判定以后端魔数嗅探为准。 */
export function isImagePath(path: string): boolean {
  const ext = path.split('.').pop()?.toLowerCase() ?? ''
  return IMAGE_EXT.includes(ext)
}

export function parseImageContent(content: string): ImageContent | null {
  try {
    const obj = JSON.parse(content) as Partial<ImageContent>
    if (!obj?.b64 || typeof obj.b64 !== 'string') return null
    return {
      v: obj.v ?? 1,
      mime: obj.mime || 'image/jpeg',
      w: Number(obj.w) || 0,
      h: Number(obj.h) || 0,
      b64: obj.b64
    }
  } catch {
    return null
  }
}

/** 拼成 <img src> 可直接用的 data URL。 */
export function imageSrc(img: ImageContent): string {
  return `data:${img.mime};base64,${img.b64}`
}

/** 占位比例用的宽高；缺省或异常时给一个方形，避免 0 导致元素塌陷。 */
export function imageBox(img: ImageContent, max: number): { width: number; height: number } {
  const w = img.w > 0 ? img.w : 1
  const h = img.h > 0 ? img.h : 1
  const ratio = w / h
  return ratio >= 1 ? { width: max, height: Math.round(max / ratio) } : { width: Math.round(max * ratio), height: max }
}
```

**Step 2: 扩展 `SwarmApi` 接口**

在 `frontend/src/api/types.ts` 的 `SwarmApi` 接口里，紧邻 `sendMessage` 添加：

```ts
  sendImage(peerId: string, path: string): Promise<Message>
  sendImageBytes(peerId: string, name: string, data: Uint8Array): Promise<Message>
  saveImage(msgId: string, destPath: string): Promise<void>
  groupSendImage(groupId: string, path: string): Promise<Message>
  groupSendImageBytes(groupId: string, name: string, data: Uint8Array): Promise<Message>
```

**Step 3: `realApi` 映射**

在 `frontend/src/api/bindings.ts` 的 `realApi` 里，紧邻 `sendMessage` 添加：

```ts
  sendImage: async (peerId, path) => ChatService.SendImage(peerId, path),
  sendImageBytes: async (peerId, name, data) => ChatService.SendImageBytes(peerId, name, data),
  saveImage: async (msgId, destPath) => {
    await ChatService.SaveImage(msgId, destPath)
  },
  groupSendImage: async (groupId, path) => GroupService.SendImage(groupId, path),
  groupSendImageBytes: async (groupId, name, data) =>
    GroupService.SendImageBytes(groupId, name, data),
```

**Step 4: mock 降级实现**

在 `frontend/src/api/mock.ts` 里加：

```ts
/** 用 canvas 现画一张小图，让浏览器预览也有真实的图片消息可看。 */
function mockImageContent(seedText: string): string {
  const canvas = document.createElement('canvas')
  canvas.width = 320
  canvas.height = 200
  const g = canvas.getContext('2d')!
  const grad = g.createLinearGradient(0, 0, 320, 200)
  grad.addColorStop(0, '#7c3aed')
  grad.addColorStop(1, '#ec4899')
  g.fillStyle = grad
  g.fillRect(0, 0, 320, 200)
  g.fillStyle = '#fff'
  g.font = '16px sans-serif'
  g.fillText(seedText.slice(0, 18) || '预览图片', 16, 108)
  const b64 = canvas.toDataURL('image/jpeg', 0.8).split(',')[1] ?? ''
  return JSON.stringify({ v: 1, mime: 'image/jpeg', w: 320, h: 200, b64 })
}

function mockImageMessage(convId: string, senderId: string, direction: 'in' | 'out'): Message {
  return {
    msgId: `mock-img-${Date.now()}`,
    convId,
    senderId,
    direction,
    content: mockImageContent(direction === 'out' ? '发出的图片' : '收到的图片'),
    msgType: 'image',
    sentAt: Date.now(),
    state: 'delivered'
  }
}
```

并在返回的 API 对象里实现五个新方法（`sendImage` 与 `sendImageBytes` 都返回 `mockImageMessage`；`saveImage` 在浏览器里退化为「触发下载」，因为原生保存对话框不存在）：

```ts
    async sendImage(peerId, _path) {
      await delay(120)
      return mockImageMessage(directConvId(self.nodeId, peerId), self.nodeId, 'out')
    },
    async sendImageBytes(peerId, _name, _data) {
      await delay(120)
      return mockImageMessage(directConvId(self.nodeId, peerId), self.nodeId, 'out')
    },
    async saveImage(_msgId, _destPath) {
      // 浏览器预览没有原生保存对话框，由调用方改为 <a download>
    },
    async groupSendImage(groupId, _path) {
      await delay(120)
      return mockImageMessage(groupId, self.nodeId, 'out')
    },
    async groupSendImageBytes(groupId, _name, _data) {
      await delay(120)
      return mockImageMessage(groupId, self.nodeId, 'out')
    },
```

另外在 mock 的初始消息列表（`jobs` 附近的 `messages`）里塞一条入站图片消息，方便一打开就能看到渲染效果。

**Step 5: chat store 补发送方法**

在 `frontend/src/stores/chat.ts` 的 `send` 之后添加：

```ts
  /**
   * 发送图片消息。
   *
   * 与文本发送共用 upsert/bump：图片同样会有 chat:new-message 与 chat:delivered
   * 事件回来，走的是同一条回填路径 —— 若各写一套，送达状态迟早会分叉。
   */
  async function sendImage(conv: Conversation, path: string): Promise<void> {
    const api = await getApi()
    const m =
      conv.kind === 'group'
        ? await api.groupSendImage(conv.convId, path)
        : await api.sendImage(conv.peerId ?? '', path)
    upsert(conv.convId, m)
    previews.value = { ...previews.value, [conv.convId]: m }
    bump(conv.convId, m.sentAt)
  }

  /** 发送剪贴板图片（只有字节，没有路径）。 */
  async function sendImageBytes(conv: Conversation, name: string, data: Uint8Array): Promise<void> {
    const api = await getApi()
    const m =
      conv.kind === 'group'
        ? await api.groupSendImageBytes(conv.convId, name, data)
        : await api.sendImageBytes(conv.peerId ?? '', name, data)
    upsert(conv.convId, m)
    previews.value = { ...previews.value, [conv.convId]: m }
    bump(conv.convId, m.sentAt)
  }
```

并在 `return { ... }` 里导出 `sendImage`、`sendImageBytes`。

**Step 6: 类型检查**

Run: `cd frontend; npm run typecheck`
Expected: PASS

**建议提交信息：** `feat(web): API 层与 chat store 支持图片消息`

---

## Task 7: 气泡渲染图片 + 查看器 + 另存为

**Files:**

- Modify: `frontend/src/components/ChatBubble.vue`
- Create: `frontend/src/components/ImageLightbox.vue`
- Modify: `frontend/src/views/ChatView.vue`（接住 `zoom` 事件）

**Step 1: 气泡渲染图片**

在 `ChatBubble.vue` 的 `<script setup>` 里加：

```ts
import { imageBox, imageSrc, parseImageContent } from '../utils/image'

/** 图片消息：解析失败（旧数据/损坏）时给占位而不是崩掉整块聊天区。 */
const image = computed(() =>
  props.message.msgType === 'image' ? parseImageContent(props.message.content) : null
)
const box = computed(() => (image.value ? imageBox(image.value, 220) : { width: 220, height: 140 }))
const broken = computed(() => props.message.msgType === 'image' && !image.value)
```

模板里把气泡体改为分支（保留原有 `.meta`）：

```vue
      <div class="bubble" :class="{ 'bubble-image': message.msgType === 'image' }">
        <template v-if="message.msgType === 'image'">
          <img
            v-if="image"
            class="shot"
            :src="imageSrc(image)"
            :width="box.width"
            :height="box.height"
            alt="图片消息"
            @click="$emit('zoom', message)"
          />
          <div v-else class="shot-broken">图片显示失败</div>
        </template>
        <div v-else class="text">{{ message.content }}</div>
        <div class="meta">
          <span class="time">{{ time }}</span>
          <Icon v-if="stateIcon" :name="stateIcon" :size="12" :class="['state', message.state]" />
        </div>
      </div>
```

在 `defineProps` 之后（`withDefaults` 所在处）补 `emit`：

```ts
const emit = defineEmits<{ (e: 'zoom', m: Message): void }>()
```

样式追加：

```css
/* 图片气泡：padding 收紧到 4px，图片自带留白；圆角由气泡给，图片只需裁圆。 */
.bubble-image {
  padding: 4px 4px 2px;
}

.shot {
  display: block;
  max-width: 220px;
  border-radius: var(--r-md);
  cursor: zoom-in;
  object-fit: contain;
  background: rgba(0, 0, 0, 0.08);
}

.shot-broken {
  width: 220px;
  height: 140px;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: var(--r-md);
  background: var(--surface-sunken);
  color: var(--t3);
  font-size: var(--fs-xs);
}
```

`ChatBubble` 里直接引用 `$emit` 是可行的（模板作用域内可用），但更稳妥的写法是 `@click="emit('zoom', message)"` —— 二者等价，选其一即可，注意与已有 `emit` 常量名保持一致。

**Step 2: 查看器**

创建 `frontend/src/components/ImageLightbox.vue`：

```vue
<script setup lang="ts">
/**
 * 图片查看器。
 *
 * 复用 Modal 的遮罩与 Esc 行为，不自己实现键盘/焦点管理 ——
 * 这类细节重复实现一次就会漏掉一次。
 */
import { computed } from 'vue'

import Icon from './Icon.vue'
import Modal from './Modal.vue'
import type { Message } from '../api/types'
import { isMockMode } from '../api'
import { getApi } from '../api'
import { imageSrc, parseImageContent } from '../utils/image'
import { saveFile } from '../api/dialog'
import { useUiStore } from '../stores/ui'

const props = defineProps<{ message: Message }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const ui = useUiStore()
const img = computed(() => parseImageContent(props.message.content))

async function saveAs(): Promise<void> {
  const m = img.value
  if (!m) return
  const suggested = props.message.fileNameHint || 'swarmlink-image.jpg'

  // 浏览器预览里没有原生保存对话框，退化成浏览器下载
  if (isMockMode()) {
    const a = document.createElement('a')
    a.href = imageSrc(m)
    a.download = suggested
    a.click()
    return
  }

  const dest = await saveFile(suggested)
  if (!dest) return
  try {
    await (await getApi()).saveImage(props.message.msgId, dest)
    ui.notify('已保存')
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || '保存失败')
  }
}
</script>

<template>
  <Modal @close="emit('close')">
    <div class="lb">
      <img v-if="img" :src="imageSrc(img)" alt="图片" />
      <div v-else class="faint">图片显示失败</div>
      <div class="bar">
        <button class="btn btn-soft" @click="saveAs">
          <Icon name="download" :size="14" />另存为
        </button>
        <button class="btn btn-soft" @click="emit('close')">关闭</button>
      </div>
    </div>
  </Modal>
</template>

<style scoped>
.lb {
  display: flex;
  flex-direction: column;
  gap: 10px;
  max-width: min(88vw, 1100px);
}
.lb img {
  max-width: 100%;
  max-height: 76vh;
  object-fit: contain;
  border-radius: var(--r-lg);
}
.bar {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
</style>
```

> `props.message.fileNameHint` 不存在于 `Message` 类型 —— 直接删掉这一行，改为固定 `'swarmlink-image.jpg'`，或按 `m.mime` 推导扩展名。**按 mime 推导扩展名**是更正确的做法，例如 `m.mime === 'image/png' ? '.png' : '.jpg'`。

在 `frontend/src/api/dialog.ts` 追加保存对话框：

```ts
/**
 * 原生保存对话框，返回用户选择的完整路径（取消时返回空串）。
 *
 * 与 pickFiles 同理：直接用 Wails 运行时，不在 Go 侧再注册一个服务。
 */
export async function saveFile(defaultName: string): Promise<string> {
  try {
    const res = await Dialogs.SaveFile({
      Title: '保存图片',
      DefaultFilename: defaultName,
      ButtonText: '保存'
    })
    return typeof res === 'string' ? res : ''
  } catch {
    return ''
  }
}
```

> `Dialogs.SaveFile` 的参数名以当前 `@wailsio/runtime` 版本为准：写完后在浏览器 `npm run dev` 里点一次「另存为」验证；若报参数错误，用 `Object.keys(Dialogs)` 与包内类型定义核对字段名即可。

**Step 3: ChatView 接住 zoom**

在 `ChatView.vue`：

```ts
import ImageLightbox from '../components/ImageLightbox.vue'

const zoomed = ref<Message | null>(null)
```

模板中：

```vue
        <ChatBubble
          v-else
          ...
          @zoom="(m) => (zoomed = m)"
        />
```

并在 `</main>` 之前加：

```vue
    <ImageLightbox v-if="zoomed" :message="zoomed" @close="zoomed = null" />
```

**Step 4: 验证**

Run: `cd frontend; npm run typecheck && npm run dev`
Expected: typecheck 通过；浏览器里（mock 模式）列表中的图片消息渲染成缩略图，点击弹出查看器。

**建议提交信息：** `feat(web): 图片气泡渲染、查看器与另存为`

---

## Task 8: 图片的三个入口

**Files:**

- Modify: `frontend/src/views/ChatView.vue`（图片按钮 + 粘贴）
- Modify: `frontend/src/composables/useFileSend.ts`（新增 `sendImage` / `attachImage`）
- Modify: `frontend/src/events/index.ts`（拖拽分流）

**Step 1: composable 增加图片发送**

在 `useFileSend.ts` 里加：

```ts
import { pickImages } from '../api/dialog'
import { isImagePath } from '../utils/image'

  /** 发送单个图片文件：不切传输面板 —— 图片不进传输列表，切过去只会让人困惑。 */
  async function sendImage(path: string): Promise<boolean> {
    const conv = chat.activeConversation
    if (!conv) {
      ui.notify('先打开一个会话')
      return false
    }
    try {
      await chat.sendImage(conv, path)
      return true
    } catch (e: any) {
      ui.notify(String(e?.message ?? e) || '图片发送失败')
      return false
    }
  }

  /** 图片按钮：选图片文件后立即发送。 */
  async function attachImage(): Promise<void> {
    if (!chat.activeConversation) {
      ui.notify('先打开一个会话')
      return
    }
    const res = await pickImages()
    if (!res.available) {
      ui.notify('当前环境无法打开文件对话框')
      return
    }
    if (!res.picked) return
    for (const p of res.paths) await sendImage(p)
  }
```

导出加上 `sendImage`、`attachImage`。

在 `frontend/src/api/dialog.ts` 追加：

```ts
/** 只选图片文件（对话框层面过滤，减少选错文件的概率）。 */
export async function pickImages(): Promise<PickOutcome> {
  try {
    const res = await Dialogs.OpenFile({
      Title: '选择要发送的图片',
      CanChooseFiles: true,
      CanChooseDirectories: false,
      AllowsMultipleSelection: true,
      Filters: [{ DisplayName: '图片', Pattern: '*.png;*.jpg;*.jpeg;*.gif;*.webp;*.bmp' }],
      ButtonText: '发送'
    })
    const paths = (Array.isArray(res) ? res : [res]).filter((p): p is string => !!p)
    return { picked: paths.length > 0, paths, available: true }
  } catch {
    return { picked: false, paths: [], available: false }
  }
}
```

**Step 2: 拖拽分流**

`frontend/src/events/index.ts` 的 `handleFilesDropped` 改为：

```ts
async function handleFilesDropped(data: FilesDroppedEvent): Promise<void> {
  if (data?.target !== 'chat' || !data.paths?.length) return

  const chat = useChatStore()
  const transfers = useTransferStore()
  const ui = useUiStore()

  const conv = chat.activeConversation
  if (!conv || !chat.activePeerId) {
    ui.notify('先打开与某个联系人的会话，再拖入文件')
    return
  }

  // 图片走消息链路（气泡内显示，不进传输列表），其余走文件传输；
  // 这里按扩展名分流只是为了少一次往返，真实格式仍由后端魔数嗅探判定。
  const images = data.paths.filter(isImagePath)
  const files = data.paths.filter((p) => !isImagePath(p))

  for (const p of images) {
    try {
      await chat.sendImage(conv, p)
    } catch (e: any) {
      ui.notify(String(e?.message ?? e) || '图片发送失败')
    }
  }

  if (!files.length) return

  ui.focusTransfer()
  const ok = await transfers.sendFiles(chat.activePeerId, files)
  ui.notify(
    ok === files.length
      ? `已开始发送 ${ok} 个文件`
      : `已发起 ${ok}/${files.length} 个文件，失败的请看右侧`
  )
}
```

**Step 3: 工具栏按钮 + 粘贴**

`ChatView.vue` 的 `.tools` 区域，在回形针之后插入：

```vue
        <button
          class="icon-btn"
          :disabled="!conv || sending"
          title="发送图片"
          aria-label="发送图片"
          @click="attachImage"
        >
          <Icon name="image" :size="17" />
        </button>
```

> 若 `Icon` 组件没有 `image` / `photo` 图标，先在 `frontend/src/components/Icon.vue` 的图标表里补一个（照抄同文件里已有的 24×24 线性图标写法，`<rect>` + `<circle>` 两笔即可）。

textarea 上挂粘贴处理：

```vue
        <textarea
          ref="draftEl"
          v-model="draft"
          class="scroll"
          :disabled="!conv"
          rows="1"
          :placeholder="conv ? '输入消息…' : '先选择一个会话'"
          @keydown="onKeydown"
          @paste="onPaste"
        />
```

```ts
/**
 * 粘贴图片：剪贴板里的 File 只有 Blob 没有本地路径，
 * 因此走 sendImageBytes（文本粘贴不拦截，交给浏览器默认行为）。
 */
async function onPaste(e: ClipboardEvent): Promise<void> {
  const c = conv.value
  if (!c) return
  const files = Array.from(e.clipboardData?.files ?? []).filter((f) => f.type.startsWith('image/'))
  if (!files.length) return

  e.preventDefault()
  for (const f of files) {
    try {
      const buf = new Uint8Array(await f.arrayBuffer())
      await chat.sendImageBytes(c, f.name || 'clipboard.png', buf)
    } catch (err: any) {
      ui.notify(String(err?.message ?? err) || '粘贴图片失败')
    }
  }
  void toBottom()
}
```

**Step 4: 验证**

Run: `cd frontend; npm run typecheck`
Expected: PASS

**建议提交信息：** `feat(web): 选图/拖拽/粘贴三个图片入口`

---

## Task 9: emoji 面板

**Files:**

- Create: `frontend/src/constants/emoji.ts`
- Create: `frontend/src/components/EmojiPicker.vue`
- Modify: `frontend/src/views/ChatView.vue`

**Step 1: 常量表**

创建 `frontend/src/constants/emoji.ts`：

```ts
/**
 * 内置 emoji 表。
 *
 * 为什么自己维护而不是引库：emoji 就是一串字符，引一个包进来只为拿常量
 * 并不划算；而这份表覆盖的是聊天里真正高频的那两百个，
 * 比一个「全集」列表（上千个、需要搜索框）更好用。
 */
export interface EmojiGroup {
  name: string
  items: string[]
}

export const EMOJI_GROUPS: EmojiGroup[] = [
  {
    name: '常用',
    items: ['😀', '😄', '😁', '😆', '😅', '🤣', '😂', '🙂', '😉', '😊', '😍', '🥰', '😘', '😜', '🤗', '🤔', '😐', '😴', '😭', '😡', '🥳', '😎', '🤝', '🙏']
  },
  {
    name: '手势',
    items: ['👍', '👎', '👌', '👏', '🙌', '🤟', '✌️', '🤞', '💪', '👋', '🫡', '🖐️', '☝️', '👀', '🫶', '🤝', '🧠', '❤️', '💔', '✨', '🔥', '🎉', '💯', '✅']
  },
  {
    name: '心情',
    items: ['😶', '🙃', '😬', '🤯', '😱', '🥶', '🥵', '😇', '🤩', '😋', '😷', '🤒', '🥺', '😤', '😮‍💨', '🙄', '😏', '😔', '😞', '💤', '💡', '❓', '❗', '⚠️']
  },
  {
    name: '工作',
    items: ['💻', '🖥️', '⌨️', '🖱️', '📱', '📁', '📂', '📄', '📊', '📈', '📉', '🗂️', '📌', '📎', '🔒', '🔑', '🛠️', '⚙️', '🔍', '🧪', '🚀', '📦', '🧾', '⏰']
  }
]
```

**Step 2: 面板组件**

创建 `frontend/src/components/EmojiPicker.vue`：

```vue
<script setup lang="ts">
/**
 * emoji 选择面板。
 *
 * 不用 Modal：弹层不该抢走输入框的焦点 —— 用户选完表情还要接着打字，
 * 每选一次都要重新点一下输入框是不可接受的。
 */
import { onBeforeUnmount, onMounted, ref } from 'vue'

import { EMOJI_GROUPS } from '../constants/emoji'

const emit = defineEmits<{ (e: 'pick', ch: string): void; (e: 'close'): void }>()

const root = ref<HTMLElement | null>(null)

function onDocClick(e: MouseEvent): void {
  if (!root.value?.contains(e.target as Node)) emit('close')
}

function onKey(e: KeyboardEvent): void {
  if (e.key === 'Escape') emit('close')
}

onMounted(() => {
  document.addEventListener('click', onDocClick, true)
  document.addEventListener('keydown', onKey)
})

onBeforeUnmount(() => {
  document.removeEventListener('click', onDocClick, true)
  document.removeEventListener('keydown', onKey)
})
</script>

<template>
  <div ref="root" class="picker">
    <div v-for="g in EMOJI_GROUPS" :key="g.name" class="group">
      <div class="gname">{{ g.name }}</div>
      <div class="grid">
        <button
          v-for="ch in g.items"
          :key="ch"
          class="cell"
          type="button"
          @click="emit('pick', ch)"
        >
          {{ ch }}
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.picker {
  width: 268px;
  max-height: 232px;
  overflow-y: auto;
  padding: 8px;
  border-radius: var(--r-lg);
  background: var(--surface);
  box-shadow: inset 0 0 0 1px var(--edge), 0 18px 40px -22px rgba(2, 0, 12, 0.95);
}

.gname {
  margin: 2px 0 4px;
  font-size: var(--fs-xs);
  color: var(--t3);
}

.grid {
  display: grid;
  grid-template-columns: repeat(8, 1fr);
  gap: 2px;
  margin-bottom: 6px;
}

.cell {
  padding: 0;
  height: 28px;
  font-size: 17px;
  line-height: 1;
  border-radius: var(--r-sm);
  background: transparent;
}

.cell:hover {
  background: var(--hover);
}
</style>
```

**Step 3: 插入到光标处**

`ChatView.vue`：

```ts
import EmojiPicker from '../components/EmojiPicker.vue'

const emojiOpen = ref(false)

/**
 * 把表情插入到光标处。
 *
 * 必须手动记录并恢复光标：v-model 写回 draft 之后，
 * Vue 重新渲染会把 textarea 的选区重置到末尾 ——
 * 结果是「在句子中间插一个表情，光标跳到结尾」。
 */
async function insertEmoji(ch: string): Promise<void> {
  const el = draftEl.value
  const start = el?.selectionStart ?? draft.value.length
  const end = el?.selectionEnd ?? draft.value.length
  draft.value = draft.value.slice(0, start) + ch + draft.value.slice(end)

  await nextTick()
  el?.focus()
  const caret = start + ch.length
  el?.setSelectionRange(caret, caret)
  void autoGrow()
}
```

工具栏：

```vue
        <div class="emoji-wrap">
          <button
            class="icon-btn"
            :disabled="!conv"
            title="表情"
            aria-label="插入表情"
            @click="emojiOpen = !emojiOpen"
          >
            <Icon name="smile" :size="17" />
          </button>
          <EmojiPicker
            v-if="emojiOpen"
            class="emoji-pop"
            @pick="insertEmoji"
            @close="emojiOpen = false"
          />
        </div>
```

样式（放在 `ChatView.vue` 的 scoped 样式里）：

```css
.emoji-wrap {
  position: relative;
}

/* 面板向上弹出：它挂在输入框上方，向下弹会盖住输入区 */
.emoji-pop {
  position: absolute;
  bottom: calc(100% + 8px);
  left: 0;
  z-index: 20;
}
```

> 若 `Icon` 没有 `smile` 图标，同样在 `Icon.vue` 里补一个（圆 + 两眼的极简画法即可）。

**Step 4: 验证**

Run: `cd frontend; npm run typecheck && npm run dev`
Expected: 面板弹出、点击表情插入到光标处（在文字中间试一次）、Esc 与点外部都能关闭、插入后输入框仍然聚焦。

**建议提交信息：** `feat(web): emoji 面板与光标处插入`

---

## Task 10: 整体验收

**Step 1: Go 侧全量**

Run: `go test ./...`
Expected: PASS

**Step 2: 构建桌面应用**

Run: `$env:EXTRA_TAGS='wails'; wails3 build DEV=true`
Expected: 产出 `bin/SwarmLink.exe`，编译日志里能看到 `go build -tags wails`

**Step 3: 启动开发模式**

Run: `$env:EXTRA_TAGS = 'wails'; wails3 dev -config ./build/config.yml -port 9245`
Expected: 窗口打开，`vite` 在 9245 上就绪

**Step 4: 手工走查清单（逐条过）**

1. 聊天里点图片按钮选一张 JPEG → 气泡内显示图片；
2. 拖一张 PNG 进聊天区 → 显示图片，且**右栏不会被切到传输面板**；
3. 截图后 Ctrl+V → 显示图片；
4. 输入框里打几个字，把光标移到中间，点一个 emoji → 表情插在光标处，光标在表情之后；
5. 发一张图后再发一条文字 → 两条各自是独立气泡，顺序为 图→文；
6. 点击图片 → 查看器打开 → Esc 关闭；
7. 查看器里「另存为」→ 选路径 → 文件确实落盘且能打开；
8. 粘贴一个 20MB 的大图 → 出现「图片过大…请改用发送文件」提示，不产生坏气泡；
9. 把 `readme.txt` 改名成 `readme.png` 后发送 → 报「不是可识别的图片格式」；
10. **右栏传输列表：以上所有操作后，列表里不得出现任何新记录**（本次的硬约束）；
11. 关掉应用重开 → 历史图片仍然正常显示（验证内联内容的持久化）；
12. 群聊里发一张图 → 对方（或第二实例）能收到并渲染。

**Step 5: 双实例互通（可选但推荐）**

用两个不同配置目录的实例互相发图，验证单聊与群聊两种链路。若第二个实例不方便起，至少验证第 11 条（重启后历史仍在）。

**建议提交信息：** `test: 图片与表情功能手工走查通过`

---

## 风险与注意事项

1. **消息表体积会增长**：每张图最多 300KB 存进 `messages.content`。若以后聊天记录量级上百万条，需要引入外置存储（届时按设计文档的路线 B 演进）。
2. **EXIF 方向未处理**：手机拍的 JPEG 若带旋转 EXIF，可能显示为横躺。v1 不做（桌面端截图占绝大多数），发现频繁出现时再引入 EXIF 解析。
3. **`Dialogs.SaveFile` 的参数名**因 Wails beta 版本而异，Task 7 里必须在浏览器与桌面各点一次验证。
4. **绑定与前端必须同步更新**：Task 5 生成绑定后，Task 6 才能 typecheck 通过；顺序不能颠倒。
5. **动图降级提示**（`imagecodec.Inline.Downgraded`）只在 Go 侧标记，UI 提示留到后续批次，本计划不实现。
