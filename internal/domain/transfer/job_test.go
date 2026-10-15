package transfer

import (
	"testing"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

func TestChunkCountAndArithmetic(t *testing.T) {
	const chunk = int64(512 * 1024)
	cases := []struct {
		size int64
		want int
	}{
		{0, 0},
		{1, 1},
		{chunk, 1},
		{chunk + 1, 2},
		{chunk * 2048, 2048},
	}
	for _, c := range cases {
		if got := ChunkCount(c.size, chunk); got != c.want {
			t.Fatalf("size %d: want %d got %d", c.size, c.want, got)
		}
	}
}

func TestOffsetAndTailSize(t *testing.T) {
	const chunk = int64(512 * 1024)
	size := chunk*2 + 100
	if got := OffsetOf(2, chunk); got != chunk*2 {
		t.Fatalf("offset wrong: %d", got)
	}
	if got := SizeOf(0, size, chunk); got != chunk {
		t.Fatalf("full chunk size wrong: %d", got)
	}
	if got := SizeOf(2, size, chunk); got != 100 {
		t.Fatalf("tail size wrong: %d", got)
	}
	if got := SizeOf(3, size, chunk); got != 0 {
		t.Fatalf("out of range size must be 0, got %d", got)
	}
}

func mkJob() *Job {
	var pid identity.NodeID
	pid[0] = 1
	return &Job{
		JobID:       "j1",
		PeerID:      pid,
		FileSize:    512*1024*4 + 10,
		ChunkSize:   512 * 1024,
		TotalChunks: 5,
		Direction:   DirectionSend,
		Bitmap:      NewChunkBitmap(5),
		Status:      StateTransferring,
	}
}

func TestJobMissingChunksAndPercent(t *testing.T) {
	j := mkJob()
	j.Bitmap.Set(0)
	j.Bitmap.Set(1)
	missing := j.MissingChunks()
	if len(missing) != 3 {
		t.Fatalf("want 3 missing got %v", missing)
	}
	if p := j.Percent(); p != 40 {
		t.Fatalf("want 40%% got %v", p)
	}
	j.SetCompleted()
	if j.Completed != 2 {
		t.Fatalf("want completed=2 got %d", j.Completed)
	}
}

func TestLocateBadChunksDoesNotClearBitmap(t *testing.T) {
	j := mkJob()
	for i := 0; i < j.TotalChunks; i++ {
		j.Bitmap.Set(i)
	}
	expected := []string{"h0", "h1", "h2", "h3", "h4"}
	bad := j.LocateBadChunks(expected, func(i int) string {
		if i == 2 {
			return "corrupt"
		}
		return expected[i]
	})
	if len(bad) != 1 || bad[0] != 2 {
		t.Fatalf("want [2] got %v", bad)
	}
	// 位图必须保持完整（否则时间全部白费）
	if j.Bitmap.CompletedCount() != 5 {
		t.Fatal("bitmap must not be cleared on verify failure")
	}
}

func TestLocateBadChunksIgnoresIncomplete(t *testing.T) {
	j := mkJob()
	j.Bitmap.Set(0) // 只完成 0，其余未完成
	bad := j.LocateBadChunks([]string{"h0", "h1", "h2", "h3", "h4"}, func(i int) string { return "different" })
	for _, b := range bad {
		if b != 0 {
			t.Fatalf("only completed chunks may be flagged as bad, got %v", bad)
		}
	}
}

// ResumePercent 是「续传自 XX%」的唯一数据源，必须与 Percent 分工清楚：
// Percent 读位图（一路上涨），它读 ResumeFrom（本次尝试的起点，写死不动）。
func TestJobResumePercent(t *testing.T) {
	cases := []struct {
		name        string
		resumeFrom  int64
		totalChunks int
		want        float64
	}{
		{"从头传", 0, 8, 0},
		{"从第 1 块接上", 1, 8, 12.5},
		{"从一半接上", 4, 8, 50},
		{"正好全部已完成", 8, 8, 100},
		// 对端声称的已完成块数可能超过本机算出的总块数（分块大小不一致的历史行）：
		// 钳到 100，别让界面出现「续传自 120%」。
		{"超出总块数要钳住", 9, 8, 100},
		// 没有总块数就无从谈起：0 表示「不显示」，而不是除零或 NaN。
		{"总块数为零", 4, 0, 0},
		{"负数起点", -1, 8, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := &Job{ResumeFrom: c.resumeFrom, TotalChunks: c.totalChunks}
			if got := j.ResumePercent(); got != c.want {
				t.Errorf("ResumePercent() = %v，期望 %v（resumeFrom=%d total=%d）",
					got, c.want, c.resumeFrom, c.totalChunks)
			}
		})
	}
}

func TestJobValidate(t *testing.T) {
	j := mkJob()
	if err := j.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := mkJob()
	bad.TotalChunks = 99
	if err := bad.Validate(); err == nil {
		t.Fatal("expected inconsistency error")
	}
	noPeer := mkJob()
	noPeer.PeerID = identity.NodeID{}
	if err := noPeer.Validate(); err == nil {
		t.Fatal("expected empty peer error")
	}
}
