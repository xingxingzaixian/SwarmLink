package message

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

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
// 这样调用方可以先用 w/h 占位，再决定要不要真的解码图片。
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
