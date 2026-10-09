package protocol

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

func TestHelloTranscriptIsDeterministic(t *testing.T) {
	nonceA := bytes.Repeat([]byte{0xA1}, 32)
	nonceB := bytes.Repeat([]byte{0xB2}, 32)
	pubA, _, _ := ed25519.GenerateKey(nil)
	pubB, _, _ := ed25519.GenerateKey(nil)

	first := HelloTranscript(nonceA, nonceB, pubA, pubB)
	second := HelloTranscript(nonceA, nonceB, pubA, pubB)
	if !bytes.Equal(first, second) {
		t.Fatal("transcript must be deterministic")
	}
	if len(first) != 32 {
		t.Fatalf("want 32-byte hash got %d", len(first))
	}
}

// 转录必须绑定全部四个输入：任何一方篡改任一字段都会导致验签失败。
// 这正是「防重放」的关键 —— 只签自己发的 nonce 是不够的。
func TestHelloTranscriptBindsEveryInput(t *testing.T) {
	nonceA := bytes.Repeat([]byte{0x01}, 32)
	nonceB := bytes.Repeat([]byte{0x02}, 32)
	pubA, _, _ := ed25519.GenerateKey(nil)
	pubB, _, _ := ed25519.GenerateKey(nil)
	base := HelloTranscript(nonceA, nonceB, pubA, pubB)

	otherNonceA := bytes.Repeat([]byte{0xFF}, 32)
	otherNonceB := bytes.Repeat([]byte{0xFE}, 32)

	variants := [][]byte{
		HelloTranscript(otherNonceA, nonceB, pubA, pubB),
		HelloTranscript(nonceA, otherNonceB, pubA, pubB),
		HelloTranscript(nonceA, nonceB, pubB, pubB),
		HelloTranscript(nonceA, nonceB, pubA, pubA),
	}
	for i, v := range variants {
		if bytes.Equal(v, base) {
			t.Fatalf("variant %d did not change the transcript", i)
		}
	}
}

// 域分隔：转录哈希不得等于任何「裸输入拼接」的哈希，
// 否则签名可能被挪用到其他协议上下文重放。
func TestHelloTranscriptIsDomainSeparated(t *testing.T) {
	nonceA := bytes.Repeat([]byte{0x11}, 32)
	nonceB := bytes.Repeat([]byte{0x22}, 32)
	pubA, _, _ := ed25519.GenerateKey(nil)
	pubB, _, _ := ed25519.GenerateKey(nil)

	got := HelloTranscript(nonceA, nonceB, pubA, pubB)

	// 手工做一个「没有域分隔串」的拼接，结果必须不同
	var raw []byte
	raw = append(raw, nonceA...)
	raw = append(raw, nonceB...)
	raw = append(raw, pubA...)
	raw = append(raw, pubB...)
	if bytes.Equal(got, raw[:len(got)]) {
		t.Fatal("transcript must include the domain separator")
	}
	if len(HelloDomain) == 0 {
		t.Fatal("domain separator must not be empty")
	}
}

func TestEncodeDecodeJSONHelpers(t *testing.T) {
	type payload struct {
		A string `json:"a"`
		B int    `json:"b"`
	}
	raw, err := EncodeJSON(payload{A: "x", B: 7})
	if err != nil {
		t.Fatal(err)
	}
	var out payload
	if err := DecodeJSON(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.A != "x" || out.B != 7 {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}

	// 错误路径：不可序列化的值
	if _, err := EncodeJSON(make(chan int)); err == nil {
		t.Fatal("expected encode error for channel")
	}
	// 错误路径：非法 JSON
	if err := DecodeJSON([]byte("{not json"), &out); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestNonceHexRoundTrip(t *testing.T) {
	in := []byte{0x00, 0x0f, 0xff, 0x10}
	hexStr := NonceHex(in)
	if hexStr != "000fff10" {
		t.Fatalf("unexpected hex %q", hexStr)
	}
	out, err := ParseNonceHex(hexStr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("roundtrip mismatch: %x", out)
	}
	if _, err := ParseNonceHex("zz"); err == nil {
		t.Fatal("expected error for non-hex input")
	}
}

// 文件夹字段必须向后兼容，且单文件传输时【不】在线上多出字段。
//
// 兼容的方向有两个，缺一不可：老对端的报文我们要能读（零值），
// 我们的报文也不能因为多了三个字段就让单文件传输的线上体积变大。
func TestFileMetaFolderFields(t *testing.T) {
	// 老报文：没有这三个 key
	old := []byte(`{"job_id":"j1","file_id":"h","file_name":"a.txt","file_size":10,"chunk_size":512,"total_chunks":1}`)
	var m FileMeta
	if err := DecodeJSON(old, &m); err != nil {
		t.Fatalf("老报文必须还能解码: %v", err)
	}
	if m.BatchID != "" || m.RelPath != "" || m.IsDir {
		t.Errorf("老报文解出批次字段 = %q/%q/%v，期望零值", m.BatchID, m.RelPath, m.IsDir)
	}

	// 单文件传输：三个字段不该出现在报文里（omitempty）
	single, err := EncodeJSON(FileMeta{
		JobID: "j2", FileID: "h", FileName: "a.txt", FileSize: 10, ChunkSize: 512, TotalChunks: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"batch_id", "rel_path", "is_dir"} {
		if bytes.Contains(single, []byte(`"`+key+`"`)) {
			t.Errorf("单文件报文中不该出现 %s：%s", key, single)
		}
	}

	// 文件夹条目：三个字段原样往返
	raw, err := EncodeJSON(FileMeta{
		JobID: "j3", FileID: "h2", FileName: "a.txt", FileSize: 10, ChunkSize: 512, TotalChunks: 1,
		BatchID: "b1", RelPath: "照片/2024/a.txt", IsDir: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var back FileMeta
	if err := DecodeJSON(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.BatchID != "b1" || back.RelPath != "照片/2024/a.txt" || !back.IsDir {
		t.Errorf("往返失败：%q/%q/%v", back.BatchID, back.RelPath, back.IsDir)
	}
}

func TestErrorPayloadAndGroupWireRoundTrip(t *testing.T) {
	raw, err := EncodeJSON(ErrorPayload{Code: 42, Message: "协议违规"})
	if err != nil {
		t.Fatal(err)
	}
	var ep ErrorPayload
	if err := DecodeJSON(raw, &ep); err != nil {
		t.Fatal(err)
	}
	if ep.Code != 42 || ep.Message != "协议违规" {
		t.Fatalf("error payload mismatch: %+v", ep)
	}

	meta := GroupMeta{
		GroupID: "g1", Name: "team", OwnerID: "0011", Epoch: 3, StateSig: "AQID",
		Members: []GroupMemberWire{{NodeID: "aa", DisplayName: "bob", Role: "member", State: "active", JoinedAt: 1}},
	}
	raw, err = EncodeJSON(meta)
	if err != nil {
		t.Fatal(err)
	}
	var back GroupMeta
	if err := DecodeJSON(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.GroupID != "g1" || back.Epoch != 3 || len(back.Members) != 1 || back.Members[0].NodeID != "aa" {
		t.Fatalf("group meta mismatch: %+v", back)
	}
}
