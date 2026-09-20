package transfer

import "testing"

func TestBitmapSetClearIsSet(t *testing.T) {
	bm := NewChunkBitmap(10)
	if bm.Len() < 10 {
		t.Fatalf("capacity too small: %d", bm.Len())
	}
	bm.Set(0)
	bm.Set(3)
	bm.Set(9)
	if !bm.IsSet(0) || !bm.IsSet(3) || !bm.IsSet(9) {
		t.Fatal("set/isset broken")
	}
	if bm.IsSet(1) {
		t.Fatal("bit 1 should be clear")
	}
	bm.Clear(3)
	if bm.IsSet(3) {
		t.Fatal("clear broken")
	}
}

func TestBitmapOutOfRangeIsNoop(t *testing.T) {
	bm := NewChunkBitmap(4)
	bm.Set(-1)
	bm.Set(100)
	if bm.IsSet(100) || bm.IsSet(-1) {
		t.Fatal("out of range must be no-op")
	}
}

func TestBitmapCompletedCountAndAllSet(t *testing.T) {
	bm := NewChunkBitmap(10)
	for _, i := range []int{0, 1, 2, 5} {
		bm.Set(i)
	}
	if bm.CompletedCount() != 4 {
		t.Fatalf("want 4 got %d", bm.CompletedCount())
	}
	if bm.AllSet(3) != true {
		t.Fatal("first 3 should be all set")
	}
	if bm.AllSet(10) {
		t.Fatal("not all 10 set")
	}
}

func TestBitmapRoundTrip(t *testing.T) {
	bm := NewChunkBitmap(10)
	bm.Set(0)
	bm.Set(3)
	bm.Set(9)
	b, err := bm.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var out ChunkBitmap
	if err := out.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	if out.CompletedCount() != 3 || !out.IsSet(3) || out.IsSet(1) {
		t.Fatal("roundtrip broken")
	}
}

func TestBitmapBase64RoundTrip(t *testing.T) {
	bm := NewChunkBitmap(2048) // 1 GB 文件
	bm.Set(0)
	bm.Set(2047)
	enc := bm.EncodeBase64()
	back, err := DecodeBase64(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !back.IsSet(0) || !back.IsSet(2047) || back.CompletedCount() != 2 {
		t.Fatal("base64 roundtrip broken")
	}
	// 2048 位 = 256 字节
	if len(bm.Bytes()) != 256 {
		t.Fatalf("1GB@512KB want 256 bytes got %d", len(bm.Bytes()))
	}
}

func TestBitmapMissing(t *testing.T) {
	bm := NewChunkBitmap(5)
	bm.Set(1)
	bm.Set(3)
	got := bm.Missing(5)
	want := []int{0, 2, 4}
	if len(got) != len(want) {
		t.Fatalf("want %v got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v got %v", want, got)
		}
	}
}

func TestBitmapEnsureLenGrows(t *testing.T) {
	var bm ChunkBitmap
	bm.EnsureLen(20)
	bm.Set(19)
	if !bm.IsSet(19) {
		t.Fatal("EnsureLen did not grow")
	}
	bm.EnsureLen(5) // 不缩小
	if bm.Len() < 20 {
		t.Fatal("EnsureLen must not shrink")
	}
}

func TestBitmapCopyIsDeep(t *testing.T) {
	bm := NewChunkBitmap(8)
	bm.Set(0)
	cp := bm.Copy()
	cp.Set(1)
	if bm.IsSet(1) {
		t.Fatal("Copy is not deep")
	}
}
