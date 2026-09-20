package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := Frame{Type: TypeChat, Flags: FlagEncrypted, Payload: []byte("hello world")}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != HeaderSize+len(in.Payload) {
		t.Fatalf("frame length wrong: %d", len(raw))
	}
	out, err := Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != in.Type || out.Flags != in.Flags || !bytes.Equal(out.Payload, in.Payload) {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
}

func TestEncodeDecodeEmptyPayload(t *testing.T) {
	raw, err := Encode(New(TypeHeartbeat, nil))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != TypeHeartbeat || len(out.Payload) != 0 {
		t.Fatalf("bad empty frame: %+v", out)
	}
}

func TestReadRejectsBadMagic(t *testing.T) {
	raw, _ := Encode(New(TypeHello, nil))
	raw[0] = 0xFF
	if _, err := Read(bytes.NewReader(raw)); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("want ErrBadMagic got %v", err)
	}
}

func TestReadRejectsOversizedFrame(t *testing.T) {
	hdr := make([]byte, HeaderSize)
	hdr[0] = Magic0
	hdr[1] = Magic1
	hdr[2] = Version
	hdr[4] = TypeFileChunk
	binary.BigEndian.PutUint32(hdr[5:9], uint32(MaxFrameSize+1))
	if _, err := Read(bytes.NewReader(hdr)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge got %v", err)
	}
}

func TestEncodeRejectsOversizedPayload(t *testing.T) {
	big := make([]byte, MaxFrameSize+1)
	if _, err := Encode(New(TypeFileChunk, big)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge got %v", err)
	}
}

func TestReadShortHeader(t *testing.T) {
	if _, err := Read(bytes.NewReader([]byte{Magic0, Magic1})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want ErrUnexpectedEOF got %v", err)
	}
}

func TestUnknownFlagsArePreserved(t *testing.T) {
	raw, _ := Encode(Frame{Type: TypeChat, Flags: 0x80, Payload: []byte("x")})
	out, err := Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if out.Flags != 0x80 {
		t.Fatalf("unknown flags must be preserved, got %#x", out.Flags)
	}
}

func TestUnknownTypeIsReturnedNotErrored(t *testing.T) {
	// 未知 Type 必须能读完并按 Length 对齐返回，由调用方丢弃 → 不得报错
	raw, _ := Encode(New(0x7F, []byte("future")))
	out, err := Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != 0x7F {
		t.Fatal("unknown type must be returned as-is")
	}
	if TypeName(out.Type) != "UNKNOWN" {
		t.Fatal("TypeName should report UNKNOWN")
	}
}

func TestMultipleFramesInStream(t *testing.T) {
	var buf bytes.Buffer
	_, _ = Write(&buf, New(TypeHello, []byte("a")))
	_, _ = Write(&buf, New(TypeChat, []byte("bb")))
	_, _ = Write(&buf, New(TypeChatAck, []byte("ccc")))

	r := bytes.NewReader(buf.Bytes())
	for i, want := range []string{"a", "bb", "ccc"} {
		f, err := Read(r)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if string(f.Payload) != want {
			t.Fatalf("frame %d payload %q", i, f.Payload)
		}
	}
}

func TestIsKnownFlag(t *testing.T) {
	if !IsKnownFlag(FlagEncrypted | FlagFragEnd) {
		t.Fatal("known flags reported unknown")
	}
	if IsKnownFlag(0xF0) {
		t.Fatal("unknown flags reported known")
	}
}

func FuzzRead(f *testing.F) {
	good, _ := Encode(New(TypeChat, []byte("seed")))
	f.Add(good)
	f.Add([]byte{Magic0, Magic1, Version, 0, TypeHello, 0, 0, 0, 0})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on input %x: %v", data, r)
			}
		}()
		_, _ = Read(bytes.NewReader(data))
	})
}
