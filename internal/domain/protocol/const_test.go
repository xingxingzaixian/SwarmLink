package protocol

import "testing"

func TestTypeNameCoversAllDeclaredTypes(t *testing.T) {
	cases := map[byte]string{
		TypeHello: "HELLO", TypeChat: "CHAT", TypePeerListReq: "PEER_LIST_REQ",
		TypePeerListResp: "PEER_LIST_RESP", TypeAuth: "AUTH", TypeAuthOK: "AUTH_OK",
		TypeError: "ERROR", TypePingAddr: "PING_ADDR",
		TypeFileMeta: "FILE_META", TypeFileMetaAck: "FILE_META_ACK",
		TypeFileChunk: "FILE_CHUNK", TypeFileChunkAck: "FILE_CHUNK_ACK",
		TypeFileDone: "FILE_DONE", TypeFileDoneAck: "FILE_DONE_ACK",
		TypeHeartbeat: "HEARTBEAT", TypeHeartbeatAck: "HEARTBEAT_ACK", TypeChatAck: "CHAT_ACK",
		TypeGroupMeta: "GROUP_META", TypeGroupMembersReq: "GROUP_MEMBERS_REQ",
		TypeGroupMembersRes: "GROUP_MEMBERS_RESP", TypeGroupMsg: "GROUP_MSG",
		TypeKeyExchange: "KEY_EXCHANGE", TypeKeyExchangeAck: "KEY_EXCHANGE_ACK",
	}
	for typ, want := range cases {
		if got := TypeName(typ); got != want {
			t.Fatalf("type %#x: want %s got %s", typ, want, got)
		}
	}
	if TypeName(0xEE) != "UNKNOWN" {
		t.Fatal("unknown type must map to UNKNOWN")
	}
}

func TestBytesConstantsAreStable(t *testing.T) {
	// 这些值属于协议 ABI，改即破坏兼容。
	if Magic0 != 0x53 || Magic1 != 0x4C {
		t.Fatal("TCP magic changed")
	}
	if Version != 0x02 || HeaderSize != 9 || MaxFrameSize != 16<<20 {
		t.Fatal("header constants changed")
	}
	if UDPMagic != [4]byte{0x53, 0x4C, 0x41, 0x4E} {
		t.Fatal("UDP magic changed")
	}
}
