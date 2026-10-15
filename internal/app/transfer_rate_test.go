package app

import (
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/filesink"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// rateBase 是速率测试的虚拟时钟起点（与 domain 侧保持一致）。
func rateBase() time.Time { return time.Unix(1_700_000_000, 0) }

// progressCollector 收齐 transfer.progress 事件，供断言速度/ETA 用。
type progressCollector struct {
	ch chan eventbus.TransferProgress
}

func newProgressCollector(bus *eventbus.Bus) *progressCollector {
	c := &progressCollector{ch: make(chan eventbus.TransferProgress, 64)}
	bus.Subscribe(eventbus.TopicTransferProgress, func(p any) {
		if ev, ok := p.(eventbus.TransferProgress); ok {
			select {
			case c.ch <- ev:
			default:
			}
		}
	})
	return c
}

// next 取下一个进度事件；超时即失败（速度/ETA 丢了就是丢了，
// 不能靠「等等看」蒙过去）。
func (c *progressCollector) next(t *testing.T, what string) eventbus.TransferProgress {
	t.Helper()
	select {
	case ev := <-c.ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("没有收到进度事件：%s", what)
		return eventbus.TransferProgress{}
	}
}

// 发送侧进度必须带上真实的速度与剩余时间（D-06）。
//
// 这条钉住的是「通路最后一环」：字段、前端渲染、事件桥映射都已存在，
// 缺的只是后端发布时赋值 —— 不赋值时前端渲染的是恒为 0 的空格子。
func TestSendProgressCarriesSpeedAndETA(t *testing.T) {
	clk := clock.NewFake(rateBase())
	bus := eventbus.New()
	got := newProgressCollector(bus)

	a := NewTransferApp(identity.NodeID{}, &stubTransferRepo{}, nil, nil, nil, bus, clk, nil)
	a.ChunkSize = 1024

	sj := &sendJob{
		job: transfer.Job{
			JobID: "s1", PeerID: mustNodeID(t, testPeerHex),
			FileSize: 8192, ChunkSize: 1024, TotalChunks: 8,
		},
		acked: transfer.NewChunkBitmap(8),
	}
	var last time.Time

	// 第一拍：只有起点样本，还没有速率可算
	a.publishSendProgress(sj, &last)
	ev := got.next(t, "发送第一拍")
	if ev.Percent != 0 || ev.Speed != 0 || ev.ETA != 0 {
		t.Fatalf("起点事件应全零：percent=%v speed=%v eta=%v", ev.Percent, ev.Speed, ev.ETA)
	}

	// 第二拍：1 秒后确认了一半（4 块 = 4096 B）→ 4096 B/s，还剩 4096 B → 1 秒
	for i := 0; i < 4; i++ {
		sj.acked.Set(i)
	}
	clk.Advance(time.Second)
	a.publishSendProgress(sj, &last)
	ev = got.next(t, "发送第二拍")
	if ev.Percent != 50 {
		t.Errorf("percent = %v，期望 50", ev.Percent)
	}
	if ev.Speed != 4096 {
		t.Errorf("speed = %v，期望 4096（1 秒确认 4KB）", ev.Speed)
	}
	if ev.ETA != time.Second {
		t.Errorf("eta = %v，期望 1s（还剩 4KB @ 4KB/s）", ev.ETA)
	}

	// 节流窗口内的重复调用不能再发事件（4 Hz 是给前端的，不是给事件总线的）
	a.publishSendProgress(sj, &last)
	select {
	case ev := <-got.ch:
		t.Fatalf("节流窗口内又发了一条进度事件：%+v", ev)
	default:
	}

	// 第三拍：再 1 秒确认完全部。窗口内 (8192-0)/2s = 4096 B/s；没有剩余 → ETA 0
	for i := 4; i < 8; i++ {
		sj.acked.Set(i)
	}
	clk.Advance(time.Second)
	a.publishSendProgress(sj, &last)
	ev = got.next(t, "发送第三拍")
	if ev.Percent != 100 {
		t.Errorf("percent = %v，期望 100", ev.Percent)
	}
	if ev.Speed != 4096 {
		t.Errorf("speed = %v，期望 4096", ev.Speed)
	}
	if ev.ETA != 0 {
		t.Errorf("eta = %v，期望 0（没有剩余字节）", ev.ETA)
	}
}

// 接收侧进度同样必须带速度与剩余时间。
func TestRecvProgressCarriesSpeedAndETA(t *testing.T) {
	clk := clock.NewFake(rateBase())
	bus := eventbus.New()
	got := newProgressCollector(bus)

	a := NewTransferApp(identity.NodeID{}, &stubTransferRepo{}, nil, noConns{}, nil, bus, clk, nil)
	a.ChunkSize = 1024

	rj := &recvJob{
		job: transfer.Job{
			JobID: "r1", PeerID: mustNodeID(t, testPeerHex),
			FileSize: 8192, ChunkSize: 1024, TotalChunks: 8,
		},
		bitmap: transfer.NewChunkBitmap(8),
		// 让第一拍就过节流：publishRecvProgress 按 lastAck 节流，
		// 而它【不】回写 lastAck（由 ACK 路径回写），所以这里只要
		// 给一个已经过去够久的时刻，后续每一拍都会发布。
		lastAck: clk.Now().Add(-time.Second),
	}

	// 第一拍：起点
	a.publishRecvProgress(rj)
	ev := got.next(t, "接收第一拍")
	if ev.Percent != 0 || ev.Speed != 0 || ev.ETA != 0 {
		t.Fatalf("起点事件应全零：percent=%v speed=%v eta=%v", ev.Percent, ev.Speed, ev.ETA)
	}

	// 第二拍：1 秒后写了一半
	for i := 0; i < 4; i++ {
		rj.bitmap.Set(i)
	}
	clk.Advance(time.Second)
	a.publishRecvProgress(rj)
	ev = got.next(t, "接收第二拍")
	if ev.Percent != 50 || ev.Speed != 4096 || ev.ETA != time.Second {
		t.Fatalf("第二拍 = %+v，期望 percent=50 speed=4096 eta=1s", ev)
	}
}

// 端到端走一遍真实的收块路径（HandleFileChunk → 位图 → 速率 → 事件）：
// 白色盒测试只验证赋值，这条验证「块真的在动的时候速度不为 0」。
func TestRecvProgressThroughChunkIngest(t *testing.T) {
	clk := clock.NewFake(rateBase())
	bus := eventbus.New()
	got := newProgressCollector(bus)

	repo := &stubTransferRepo{}
	dir := t.TempDir()
	sink, err := filesink.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := NewTransferApp(identity.NodeID{}, repo, sink, noConns{}, nil, bus, clk, nil)
	a.ChunkSize = 1024
	t.Cleanup(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, rj := range a.receivers {
			if rj.f != nil {
				_ = rj.f.Close()
			}
		}
	})

	peerID := mustNodeID(t, testPeerHex)
	sess := &metaAckRecorder{peer: peerID}
	meta := protocol.FileMeta{
		JobID: "ingest", FileID: "h-ingest", FileName: "f.bin",
		FileSize: 8192, ChunkSize: 1024, TotalChunks: 8,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatalf("HandleFileMeta: %v", err)
	}

	// 每 300ms 来一块：低于 ACK 间隔（500ms），因此每个块都会发一次进度。
	for i := 0; i < 7; i++ {
		clk.Advance(300 * time.Millisecond)
		payload, err := protocol.EncodeFileChunk(protocol.FileChunk{
			JobID: meta.JobID, Index: i, Offset: int64(i) * 1024, Data: make([]byte, 1024),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.HandleFileChunk(sess, protocol.New(protocol.TypeFileChunk, payload)); err != nil {
			t.Fatalf("HandleFileChunk(%d): %v", i, err)
		}
	}

	// 第一拍是基线样本（速率必然为 0），从第二拍起才该有速度
	first := got.next(t, "收块第一拍")
	if first.Percent == 0 && first.Speed != 0 {
		t.Fatalf("基线样本不该有速率：%+v", first)
	}
	if first.Speed != 0 {
		t.Fatalf("基线拍 speed = %v，期望 0（窗口里只有一个样本）", first.Speed)
	}

	// 收齐这一轮剩余的事件，看最后一个：7/8 块、速率 ≈ 1024B/300ms、还剩 1 块
	var lastEv eventbus.TransferProgress
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case ev := <-got.ch:
			lastEv = ev
			continue
		default:
		}
		break
	}
	const wantSpeed = float64(1024) / 0.3
	if lastEv.Percent < 80 || lastEv.Percent > 90 {
		t.Errorf("最后一个事件的 percent = %v，期望 ≈ 87.5", lastEv.Percent)
	}
	if lastEv.Speed < wantSpeed*0.9 || lastEv.Speed > wantSpeed*1.1 {
		t.Errorf("speed = %v，期望 ≈ %v", lastEv.Speed, wantSpeed)
	}
	if lastEv.ETA <= 0 || lastEv.ETA > 2*time.Second {
		t.Errorf("eta = %v，期望 0~2s（还剩 1KB @ 3.4KB/s）", lastEv.ETA)
	}
}

// chunkBytesDone 必须按文件大小截断：最后一个块通常不满，
// 不截断会让小文件的速率与 ETA 虚高（4KB/1KB 块时误差 25%）。
func TestChunkBytesDoneClampsToFileSize(t *testing.T) {
	cases := []struct {
		name                string
		completed           int
		chunkSize, fileSize int64
		want                int64
	}{
		{"正常比例", 4, 1024, 8192, 4096},
		{"最后一块不满要截断", 3, 3000, 8192, 8192},
		{"恰好相等", 8, 1024, 8192, 8192},
		{"没有完成", 0, 1024, 8192, 0},
		{"零块大小", 3, 0, 8192, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := chunkBytesDone(c.completed, c.chunkSize, c.fileSize); got != c.want {
				t.Errorf("chunkBytesDone(%d, %d, %d) = %d，期望 %d",
					c.completed, c.chunkSize, c.fileSize, got, c.want)
			}
		})
	}
}
