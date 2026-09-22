package message

import (
	"encoding/base64"
	"encoding/json"
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

// 信封的字段名就是前后端之间的线格式契约：
// 前端 utils/image.ts 的 parseImageContent 按这几个键取值，
// 任何一次「顺手改个 tag」都会让图片气泡静默变成「图片显示失败」。
func TestInlineImageJSONKeysAreAContract(t *testing.T) {
	content, err := EncodeInlineImage(InlineImage{MIME: "image/png", W: 2, H: 3, B64: "aGk="})
	if err != nil {
		t.Fatalf("EncodeInlineImage: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		t.Fatalf("解码: %v", err)
	}
	for _, key := range []string{"v", "mime", "w", "h", "b64"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("缺少字段 %q（前端依赖它渲染图片）", key)
		}
	}
	if len(raw) != 5 {
		t.Errorf("字段数 = %d，期望 5：新增字段请同时更新前端 parser 与设计文档", len(raw))
	}
}

func TestMsgTypeImageIsStable(t *testing.T) {
	if MsgTypeImage != "image" {
		t.Fatalf("MsgTypeImage = %q，改成别的值会让已入库的历史消息解析不出来", MsgTypeImage)
	}
}
