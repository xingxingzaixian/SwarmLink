package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestFileChunkRoundTrip(t *testing.T) {
	// 尾块不整：Data 长度小于 chunk_size 是常态
	in := FileChunk{JobID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", Index: 42, Offset: 42 * 524288, Data: []byte("tail")}

	raw, err := EncodeFileChunk(in)
	if err != nil {
		t.Fatal(err)
	}
	// 布局：2B jobIDLen | jobID | 4B index | 8B offset | data
	if len(raw) != 2+len(in.JobID)+4+8+len(in.Data) {
		t.Fatalf("unexpected encoded length %d", len(raw))
	}

	out, err := DecodeFileChunk(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.JobID != in.JobID || out.Index != in.Index || out.Offset != in.Offset {
		t.Fatalf("header mismatch: %+v", out)
	}
	if !bytes.Equal(out.Data, in.Data) {
		t.Fatalf("data mismatch: %q", out.Data)
	}
}

func TestFileChunkEmptyData(t *testing.T) {
	raw, err := EncodeFileChunk(FileChunk{JobID: "j", Index: 0, Offset: 0})
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeFileChunk(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 0 || out.JobID != "j" {
		t.Fatalf("unexpected: %+v", out)
	}
}

func TestDecodeFileChunkRejectsMalformedInput(t *testing.T) {
	cases := map[string][]byte{
		"empty":            {},
		"only length byte": {0x00},
		// 声称 jobID 长 100，但缓冲区远不够
		"truncated header": append([]byte{0x00, 0x64}, []byte("short")...),
	}
	for name, raw := range cases {
		if _, err := DecodeFileChunk(raw); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestEncodeFileChunkRejectsOverlongJobID(t *testing.T) {
	// jobID 长度字段只有 2 字节，超过 0xFFFF 必须报错而不是静默截断
	huge := strings.Repeat("x", 0x10000)
	if _, err := EncodeFileChunk(FileChunk{JobID: huge}); err == nil {
		t.Fatal("expected error for overlong job id")
	}
}

func TestDecodedChunkDoesNotAliasInput(t *testing.T) {
	raw, err := EncodeFileChunk(FileChunk{JobID: "j", Data: []byte("abcd")})
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeFileChunk(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 修改输入缓冲区不应影响已解码的块（否则位图/校验会出现诡异的间歇性失败）
	for i := range raw {
		raw[i] = 0
	}
	if !bytes.Equal(out.Data, []byte("abcd")) {
		t.Fatalf("decoded chunk aliases the input buffer: %q", out.Data)
	}
}

func TestPeekType(t *testing.T) {
	hdr := make([]byte, HeaderSize)
	hdr[4] = TypeFileChunk
	if typ, ok := PeekType(hdr); !ok || typ != TypeFileChunk {
		t.Fatalf("want %#x got %#x ok=%v", TypeFileChunk, typ, ok)
	}
	if _, ok := PeekType(make([]byte, HeaderSize-1)); ok {
		t.Fatal("short header must not be peekable")
	}
}
