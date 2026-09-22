package wails

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/image/bmp"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/app"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// 本文件只验证【服务层往下传了什么】，因此全部依赖都用最小 fake：
// 真实网络与落盘不是这里的关注点，把它们拉进来只会让测试变慢变脆。

// NodeID 是 8 字节指纹（16 个 hex 字符），见 identity.nodeIDLen。
const (
	testSelfHex = "1122334455667788"
	testPeerHex = "aabbccddeeff0011"
)

var errNoSession = errors.New("test: 没有可用会话")

// ---------------------------------------------------------------------------
// fake
// ---------------------------------------------------------------------------

type memTransferRepo struct {
	mu   sync.Mutex
	jobs map[string]transfer.Job
}

func newMemTransferRepo() *memTransferRepo {
	return &memTransferRepo{jobs: make(map[string]transfer.Job)}
}

func (r *memTransferRepo) UpsertJob(j transfer.Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[j.JobID] = j
	return nil
}

func (r *memTransferRepo) GetJob(jobID string) (transfer.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[jobID]
	if !ok {
		return transfer.Job{}, os.ErrNotExist
	}
	return j, nil
}

func (r *memTransferRepo) get(jobID string) (transfer.Job, bool) {
	j, err := r.GetJob(jobID)
	return j, err == nil
}

func (r *memTransferRepo) FindResumable(identity.NodeID, string) (transfer.Job, bool) {
	return transfer.Job{}, false
}

func (r *memTransferRepo) SaveBitmap(string, transfer.ChunkBitmap, int64) error { return nil }

func (r *memTransferRepo) ListActive() ([]transfer.Job, error)                 { return nil, nil }
func (r *memTransferRepo) ListRecent(limit int) ([]transfer.Job, error)         { return nil, nil }
func (r *memTransferRepo) PurgeFinished(keep int, olderThan time.Time) (int, error) {
	return 0, nil
}

// stubConns 永远没有可用会话：发送会在「任务已建好之后」失败，
// 这正好让我们观察到任务是否用【正确的 job_id】落了库。
type stubConns struct{}

func (stubConns) Dial(context.Context, identity.NodeID, string) (ports.Session, error) {
	return nil, errNoSession
}
func (stubConns) Accept(context.Context) (ports.Session, error)      { return nil, errNoSession }
func (stubConns) SessionOf(identity.NodeID) (ports.Session, bool)    { return nil, false }
func (stubConns) Broadcast(protocol.Frame, ...identity.NodeID) error { return nil }
func (stubConns) Close(identity.NodeID) error                        { return nil }

type emptyDir struct{}

func (emptyDir) Upsert(peer.Peer) error                           { return nil }
func (emptyDir) Get(identity.NodeID) (peer.Peer, bool)            { return peer.Peer{}, false }
func (emptyDir) List(ports.PeerFilter) []peer.Peer                { return nil }
func (emptyDir) MarkOffline(identity.NodeID, time.Time) error     { return nil }
func (emptyDir) SeenRecently(identity.NodeID, time.Duration) bool { return false }

func selfID(t *testing.T) identity.NodeID {
	t.Helper()
	id, err := identity.ParseNodeID(testSelfHex)
	if err != nil {
		t.Fatalf("解析自身 NodeID: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// SendFile 的返回值契约
// ---------------------------------------------------------------------------

// TestSendFileReturnsTheRealJobID 锁住服务层的核心契约：
// 返回的 job_id 必须【就是】任务落库所用的那个 id。
//
// 这是界面能否显示进度的前提 —— 前端拿这个 id 建任务条目，
// 再靠 transfer:progress / done / error 事件里的 job_id 匹配更新。
// 一旦服务层另造一个占位 id，任务条目就永远等不到自己的事件，
// 表现为「文件发出去了但进度条永远不动」。
func TestSendFileReturnsTheRealJobID(t *testing.T) {
	repo := newMemTransferRepo()
	tapp := app.NewTransferApp(selfID(t), repo, nil, stubConns{}, emptyDir{}, nil, clock.New(), nil)
	svc := &TransferService{d: Deps{Transfer: tapp}}

	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(path, []byte("swarmlink payload"), 0o644); err != nil {
		t.Fatalf("写测试文件: %v", err)
	}

	jobID, err := svc.SendFile(testPeerHex, path)
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	if jobID == "" {
		t.Fatal("SendFile 必须返回 job_id")
	}

	// 任务在后台 goroutine 里创建，轮询等它出现
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := repo.get(jobID); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("返回的 job_id %q 从未落库：界面拿到的 id 与真实任务对不上", jobID)
		}
		time.Sleep(10 * time.Millisecond)
	}

	job, _ := repo.get(jobID)
	if job.FileName != "payload.bin" {
		t.Errorf("job.FileName = %q, 期望 payload.bin", job.FileName)
	}
	if got := job.PeerID.String(); got != testPeerHex {
		t.Errorf("job.PeerID = %q, 期望 %q", got, testPeerHex)
	}
	if job.Direction != transfer.DirectionSend {
		t.Errorf("job.Direction = %q, 期望 %q", job.Direction, transfer.DirectionSend)
	}
	if job.FileSize != int64(len("swarmlink payload")) {
		t.Errorf("job.FileSize = %d", job.FileSize)
	}
}

// ---------------------------------------------------------------------------
// 图片消息：路径/字节两个入口 + 另存为
// ---------------------------------------------------------------------------

func newChatService(t *testing.T) *ChatService {
	t.Helper()
	svc, _ := newChatServiceWithStore(t)
	return svc
}

// newChatServiceWithStore 额外返回仓储：有些断言要证明「除了消息，没写别的」。
func newChatServiceWithStore(t *testing.T) (*ChatService, *mem.Messages) {
	t.Helper()
	clk := clock.New()
	store := mem.NewMessages(clk)
	capp := app.NewChatApp(selfID(t), store, stubConns{}, mem.NewPeers(clk), eventbus.New(), clk, nil)
	return &ChatService{d: Deps{Self: SelfInfo{NodeID: selfID(t)}, Chat: capp}}, store
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

// 造 GIF / BMP 夹具。
//
// 注意：这两个 import 让本文件对「解码器注册」失去了判别力，
// 因此本测试【不是】注册机制的守卫 —— 真正的守卫是 imagecodec 里
// 「按魔数显式分发 + 显式 import」带来的编译期约束。
// 它的价值在于端到端：用户点「发送」到消息落库这条路上，
// 前端选择器放行的格式确实能走通。
func writeTestGIF(t *testing.T) string {
	t.Helper()
	pal := color.Palette{color.White, color.Black}
	img := image.NewPaletted(image.Rect(0, 0, 24, 16), pal)
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 2)
	}
	p := filepath.Join(t.TempDir(), "anim.gif")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建文件: %v", err)
	}
	defer f.Close()
	if err := gif.Encode(f, img, nil); err != nil {
		t.Fatalf("写 GIF: %v", err)
	}
	return p
}

func writeTestBMP(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 24, 16))
	p := filepath.Join(t.TempDir(), "shot.bmp")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建文件: %v", err)
	}
	defer f.Close()
	if err := bmp.Encode(f, img); err != nil {
		t.Fatalf("写 BMP: %v", err)
	}
	return p
}

// GIF / BMP 在发送链路上必须真的能走通：前端的选择器与拖拽分流都放行了
// 这些后缀，若后端解不开，用户会在点了「发送」之后才收到
// 「不是可识别的图片格式」——而文件明明是一张正常的图片。
func TestSendImageAcceptsGIFAndBMP(t *testing.T) {
	cases := map[string]func(*testing.T) string{
		"gif 保留动图类型": writeTestGIF,
		"bmp 能解码":     writeTestBMP,
	}
	wantMIME := map[string]string{
		"gif 保留动图类型": "image/gif",
		"bmp 能解码":     "image/jpeg", // BMP 没有值得保留的特性，统一转 JPEG
	}

	for name, make := range cases {
		t.Run(name, func(t *testing.T) {
			svc := newChatService(t)
			dto, err := svc.SendImage(testPeerHex, make(t))
			if err != nil {
				t.Fatalf("SendImage: %v", err)
			}
			img, err := message.DecodeInlineImage(dto.Content)
			if err != nil {
				t.Fatalf("解析内联图: %v", err)
			}
			if img.MIME != wantMIME[name] {
				t.Errorf("MIME = %q，期望 %q", img.MIME, wantMIME[name])
			}
		})
	}
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

// 发图片【绝不能】产生传输任务：传输列表只展示文件传输，
// 图片是消息链路的产物。这条约束一旦破了，用户会在传输列表里
// 看到一堆自己没发过的图片任务，而界面上没有任何东西能解释它们。
//
// mem 仓储同时实现了 ports.ChatStore 与 ports.TransferRepo，
// 因此可以直接断言「传了图，但没有任何 transfer job 落库」。
func TestSendImageCreatesNoTransferJob(t *testing.T) {
	svc, store := newChatServiceWithStore(t)

	if _, err := svc.SendImage(testPeerHex, writeTestPNG(t)); err != nil {
		t.Fatalf("SendImage: %v", err)
	}

	active, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	recent, err := store.ListRecent(50)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(active) != 0 || len(recent) != 0 {
		t.Fatalf("图片消息产生了传输任务：active=%d recent=%d，传输列表会凭空多出条目", len(active), len(recent))
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
	dto, err := svc.SendImageBytes(testPeerHex, "clipboard.png", base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("SendImageBytes: %v", err)
	}
	if dto.MsgType != string(message.MsgTypeImage) {
		t.Errorf("MsgType = %q，期望 image", dto.MsgType)
	}
	if _, err := message.DecodeInlineImage(dto.Content); err != nil {
		t.Fatalf("DTO.content 不是合法的内联图片: %v", err)
	}
}

func TestSendImageBytesRejectsBadPayload(t *testing.T) {
	svc := newChatService(t)
	cases := map[string]string{
		"空数据":     "",
		"非法 base64": "这不是 base64",
		"不是图片":    base64.StdEncoding.EncodeToString([]byte("plain text")),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.SendImageBytes(testPeerHex, "bad.png", data); err == nil {
				t.Fatal("坏数据必须同步报错")
			}
		})
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

// 目标目录不存在时要说清是「位置不对」，而不是让用户对着
// 「保存图片失败: open …: The system cannot find the path specified」猜。
func TestSaveImageRejectsMissingDirectory(t *testing.T) {
	svc := newChatService(t)
	sent, err := svc.SendImage(testPeerHex, writeTestPNG(t))
	if err != nil {
		t.Fatalf("SendImage: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "不存在的目录", "x.png")
	err = svc.SaveImage(sent.MsgID, dest)
	if err == nil {
		t.Fatal("目标目录不存在必须报错")
	}
	if !strings.Contains(err.Error(), "保存位置不存在") {
		t.Errorf("错误信息 = %q，期望点明是保存位置的问题", err.Error())
	}
}

// 对文本消息调用另存为必须是明确错误，而不是写出一堆乱码。
func TestSaveImageRejectsNonImageMessage(t *testing.T) {
	svc := newChatService(t)
	dto, err := svc.SendMessage(testPeerHex, "普通文本")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if err := svc.SaveImage(dto.MsgID, filepath.Join(t.TempDir(), "x.jpg")); err == nil {
		t.Fatal("文本消息不得被当作图片保存")
	}
}

// TestSendFileRejectsBadPathSynchronously 保证「路径打错 / 选到目录 / 空文件」
// 这三类失误在 RPC 返回前就报错。
//
// 否则它们会变成一个永远停在「排队中」的任务，用户只能靠猜。
// 注意 Deps 是零值：这些错误都必须在触碰 app 层之前返回，
// 真的走到 app 层会因 nil 解引用崩溃 —— 崩溃本身就是失败信号。
func TestSendFileRejectsBadPathSynchronously(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("写测试文件: %v", err)
	}

	cases := []struct {
		name string
		peer string
		path string
	}{
		{"文件不存在", testPeerHex, filepath.Join(dir, "nope.bin")},
		{"路径是目录", testPeerHex, dir},
		{"空文件", testPeerHex, empty},
		{"空路径", testPeerHex, ""},
		{"非法 NodeID", "not-a-node-id", empty},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &TransferService{}
			if _, err := svc.SendFile(tc.peer, tc.path); err == nil {
				t.Fatal("必须同步返回错误，而不是留一个跑不起来的任务")
			}
		})
	}
}
