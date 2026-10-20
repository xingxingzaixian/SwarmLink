package protocol

import "testing"

// 分块载荷是二进制布局：偏移量必须原样往返，否则接收端会把它当成
// 「偏移错乱」直接中止下载 —— 而这个错误在真机上极难复现。
func TestUpdatePkgChunkRoundTrip(t *testing.T) {
	in := UpdatePkgChunk{Offset: 1 << 33, Data: []byte("hello")}
	got, err := DecodeUpdatePkgChunk(in.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Offset != in.Offset {
		t.Fatalf("offset=%d want %d", got.Offset, in.Offset)
	}
	if string(got.Data) != string(in.Data) {
		t.Fatalf("data=%q want %q", got.Data, in.Data)
	}
}

// 短载荷来自网络，必须返回错误而不是 panic。
func TestDecodeUpdatePkgChunkShort(t *testing.T) {
	for _, b := range [][]byte{nil, {}, {1, 2, 3}} {
		if _, err := DecodeUpdatePkgChunk(b); err == nil {
			t.Fatalf("长度 %d 应当报错", len(b))
		}
	}
}

// 空数据块是合法的（结尾帧之前的空块），不得被当成畸形。
func TestDecodeUpdatePkgChunkEmptyData(t *testing.T) {
	got, err := DecodeUpdatePkgChunk(UpdatePkgChunk{Offset: 7}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Offset != 7 || len(got.Data) != 0 {
		t.Fatalf("got %+v", got)
	}
}
