package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/share"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// capturingSession 记录本机发出的帧，供断言「服务端回了什么」。
type capturingSession struct {
	peer  identity.NodeID
	frame protocol.Frame
	sent  bool
}

func (s *capturingSession) PeerID() identity.NodeID { return s.peer }
func (s *capturingSession) Send(f protocol.Frame) error {
	s.frame = f
	s.sent = true
	return nil
}
func (s *capturingSession) Recv() (protocol.Frame, error) { return protocol.Frame{}, errNoSession }
func (s *capturingSession) RTT() time.Duration            { return 0 }
func (s *capturingSession) Close() error                  { return nil }

// shareFixture 组装一个只跑共享逻辑的 ShareApp（传输侧用真实的 TransferApp +
// 内存仓储：共享下载的落点就是「往传输队列里塞任务」，用替身就测不到那一步）。
type shareFixture struct {
	app     *ShareApp
	repo    *mem.Shares
	peers   *mem.Peers
	groups  *mem.Groups
	trans   *TransferApp
	tjobs   *mem.Messages
	self    identity.NodeID
	peerID  identity.NodeID
	clk     *clock.Fake
	rootDir string
}

func newShareFixture(t *testing.T) *shareFixture {
	t.Helper()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	bus := eventbus.New()
	repo := mem.NewShares()
	peers := mem.NewPeers(clk)
	groups := mem.NewGroups()
	tjobs := mem.NewMessages(clk)

	trans := NewTransferApp(self, tjobs, nil, noConns{}, peers, bus, clk, nil)
	root := t.TempDir()
	app := NewShareApp(self, repo, peers, groups, trans, bus, clk, nil)
	app.Start(context.Background())

	return &shareFixture{
		app: app, repo: repo, peers: peers, groups: groups,
		trans: trans, tjobs: tjobs, self: self, peerID: peerID, clk: clk, rootDir: root,
	}
}

func (f *shareFixture) publish(t *testing.T, vis share.Visibility) share.Share {
	t.Helper()
	sh, err := f.app.Publish(context.Background(), PublishRequest{
		RootPath: f.rootDir, Name: "共享包", Visibility: vis,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return sh
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 发布必须把目录扫成清单：嵌套文件按相对路径记录，空目录单成一条 ——
// 少了空目录那条占位，接收端建不出它（目录不会因为「里面的文件」而存在）。
func TestSharePublishScansTree(t *testing.T) {
	f := newShareFixture(t)
	writeFile(t, filepath.Join(f.rootDir, "a.txt"), "A")
	writeFile(t, filepath.Join(f.rootDir, "docs", "b.md"), "BB")
	if err := os.MkdirAll(filepath.Join(f.rootDir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	sh := f.publish(t, share.Visibility{Type: share.VisibilityAll})
	if len(sh.Items) != 3 {
		t.Fatalf("应有 3 项（2 文件 + 1 空目录），得到 %#v", sh.Items)
	}
	if sh.TotalSize != 3 {
		t.Fatalf("总大小应为 3，得到 %d", sh.TotalSize)
	}

	byPath := map[string]share.Item{}
	for _, it := range sh.Items {
		byPath[it.Path] = it
	}
	if _, ok := byPath["docs/b.md"]; !ok {
		t.Fatalf("嵌套文件必须以斜杠相对路径记录：%#v", byPath)
	}
	if it, ok := byPath["empty"]; !ok || !it.IsDir {
		t.Fatalf("空目录必须是 IsDir 占位项：%#v", byPath)
	}

	// 同一目录重复发布要被拦下：两条共享指向同一个根时，
	// 「取消其中一条」的语义会变得没法解释（文件还在被另一条共享着）。
	if _, err := f.app.Publish(context.Background(), PublishRequest{
		RootPath: f.rootDir, Name: "又一个", Visibility: share.Visibility{Type: share.VisibilityAll},
	}); err == nil {
		t.Fatal("同一根目录重复发布应当报错")
	}

	// 摘要缓存必须已经跟上（广播构造会读它）。
	if got := f.app.Summaries(); len(got) != 1 || got[0].FileCount != 3 {
		t.Fatalf("本地摘要缓存不对：%#v", got)
	}
}

// 服务端是【唯一的权威访问控制点】：请求方是否在可见范围内，只由它判定。
func TestShareHandleIndexReqEnforcesVisibility(t *testing.T) {
	f := newShareFixture(t)
	writeFile(t, filepath.Join(f.rootDir, "secret.txt"), "top-secret")

	// 1) 名单里没有请求方 → 拒绝，且必须回一个可读原因（对方看不到我们的日志表）。
	sh := f.publish(t, share.Visibility{
		Type:    share.VisibilityUsers,
		Allowed: []string{mustNodeID(t, "0f0f0f0f0f0f0f0f").String()},
	})
	sess := &capturingSession{peer: f.peerID}
	req, err := protocol.EncodeJSON(protocol.ShareIndexReq{ShareID: sh.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.HandleIndexReq(sess, protocol.New(protocol.TypeShareIndexReq, req)); err != nil {
		t.Fatalf("HandleIndexReq: %v", err)
	}
	var denied protocol.ShareIndexResp
	if !sess.sent {
		t.Fatal("拒绝时也必须回包：请求方需要知道为什么")
	}
	if err := protocol.DecodeJSON(sess.frame.Payload, &denied); err != nil {
		t.Fatal(err)
	}
	if denied.OK || !strings.Contains(denied.Error, "无权") {
		t.Fatalf("越权请求应被拒绝并说明原因，得到 %#v", denied)
	}

	// 2) 名单里有请求方 → 放行，并给出完整清单。
	if err := f.repo.Delete(sh.ID); err != nil {
		t.Fatal(err)
	}
	allowed := f.publish(t, share.Visibility{
		Type:    share.VisibilityUsers,
		Allowed: []string{f.peerID.String()},
	})
	ok, err := protocol.EncodeJSON(protocol.ShareIndexReq{ShareID: allowed.ID})
	if err != nil {
		t.Fatal(err)
	}
	sess2 := &capturingSession{peer: f.peerID}
	if err := f.app.HandleIndexReq(sess2, protocol.New(protocol.TypeShareIndexReq, ok)); err != nil {
		t.Fatal(err)
	}
	var granted protocol.ShareIndexResp
	if err := protocol.DecodeJSON(sess2.frame.Payload, &granted); err != nil {
		t.Fatal(err)
	}
	if !granted.OK || len(granted.Items) != 1 || granted.Items[0].Path != "secret.txt" {
		t.Fatalf("放行时清单不对：%#v", granted)
	}
	if granted.Owner != f.self.String() {
		t.Fatalf("响应必须带上 owner（请求方据此校验来源）：%q", granted.Owner)
	}
}

// 「仅群组」的默认方向必须是拒绝：没有群仓储、或请求者不在群里，都不该放行。
func TestShareGroupVisibilityDefaultsToDeny(t *testing.T) {
	f := newShareFixture(t)
	writeFile(t, filepath.Join(f.rootDir, "f.txt"), "x")
	sh := f.publish(t, share.Visibility{
		Type:    share.VisibilityGroup,
		Allowed: []string{"abcdef0123456789abcdef0123456789"},
	})

	sess := &capturingSession{peer: f.peerID}
	req, err := protocol.EncodeJSON(protocol.ShareIndexReq{ShareID: sh.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.HandleIndexReq(sess, protocol.New(protocol.TypeShareIndexReq, req)); err != nil {
		t.Fatal(err)
	}
	var resp protocol.ShareIndexResp
	if err := protocol.DecodeJSON(sess.frame.Payload, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK {
		t.Fatalf("不在群里的请求方必须被拒绝：%#v", resp)
	}
}

// 拉取请求必须把整批任务落成【同一个批次 + 各自的相对路径】——
// 这是接收端还原目录树的全部依据（见 ShareApp.HandlePullReq 的六道关口）。
func TestShareHandlePullReqEnqueuesBatch(t *testing.T) {
	f := newShareFixture(t)
	writeFile(t, filepath.Join(f.rootDir, "a.txt"), "A")
	writeFile(t, filepath.Join(f.rootDir, "docs", "b.md"), "BB")
	sh := f.publish(t, share.Visibility{
		Type:    share.VisibilityUsers,
		Allowed: []string{f.peerID.String()},
	})

	sess := &capturingSession{peer: f.peerID}
	req, err := protocol.EncodeJSON(protocol.SharePullReq{
		ShareID:  sh.ID,
		Paths:    []string{"a.txt", "docs/b.md", "不存在.txt"},
		BatchID:  "batch-1",
		RootName: sh.Name,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.HandlePullReq(sess, protocol.New(protocol.TypeSharePullReq, req)); err != nil {
		t.Fatalf("HandlePullReq: %v", err)
	}

	var resp protocol.SharePullResp
	if err := protocol.DecodeJSON(sess.frame.Payload, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.BatchID != "batch-1" {
		t.Fatalf("响应必须原样回填 batch_id（请求方据此对上等待中的请求）：%q", resp.BatchID)
	}
	if !resp.OK || resp.Accepted != 2 || len(resp.Rejected) != 1 {
		t.Fatalf("受理结果不对：%#v", resp)
	}

	jobs, err := f.tjobs.ListByBatch("batch-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("应落 2 条任务，得到 %d", len(jobs))
	}
	rels := map[string]string{}
	for _, j := range jobs {
		if j.PeerID != f.peerID {
			t.Fatalf("任务目标不对：%s", j.PeerID)
		}
		rels[j.RelPath] = j.LocalPath
	}
	want := map[string]string{
		"共享包/a.txt":     filepath.Join(f.rootDir, "a.txt"),
		"共享包/docs/b.md": filepath.Join(f.rootDir, "docs", "b.md"),
	}
	for rel, abs := range want {
		got, ok := rels[rel]
		if !ok {
			t.Fatalf("缺少 rel_path %q（实际 %v）", rel, rels)
		}
		if got != abs {
			t.Fatalf("rel_path %q 的本地路径 = %q，期望 %q", rel, got, abs)
		}
	}
}

// 拉取请求也必须过可见性这一关：清单之外的路径一律拒掉。
func TestShareHandlePullReqRejectsOutsiderAndBadPaths(t *testing.T) {
	f := newShareFixture(t)
	writeFile(t, filepath.Join(f.rootDir, "a.txt"), "A")
	sh := f.publish(t, share.Visibility{
		Type:    share.VisibilityUsers,
		Allowed: []string{mustNodeID(t, "0f0f0f0f0f0f0f0f").String()},
	})

	req, err := protocol.EncodeJSON(protocol.SharePullReq{
		ShareID: sh.ID, Paths: []string{"a.txt"}, BatchID: "batch-x", RootName: sh.Name,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess := &capturingSession{peer: f.peerID}
	if err := f.app.HandlePullReq(sess, protocol.New(protocol.TypeSharePullReq, req)); err != nil {
		t.Fatal(err)
	}
	var resp protocol.SharePullResp
	if err := protocol.DecodeJSON(sess.frame.Payload, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Accepted != 0 {
		t.Fatalf("名单外的人不该被受理：%#v", resp)
	}
	if jobs, _ := f.tjobs.ListByBatch("batch-x"); len(jobs) != 0 {
		t.Fatalf("被拒的请求不该产生任何任务，得到 %d 条", len(jobs))
	}

	// 放行之后，越界路径（`../`）必须在共享方这一侧就被拒掉，
	// 而不是靠接收端清洗 —— 那是第二道防线，不是第一道。
	if err := f.repo.Delete(sh.ID); err != nil {
		t.Fatal(err)
	}
	allowed := f.publish(t, share.Visibility{
		Type: share.VisibilityUsers, Allowed: []string{f.peerID.String()},
	})
	bad, err := protocol.EncodeJSON(protocol.SharePullReq{
		ShareID: allowed.ID, Paths: []string{"../../etc/passwd"}, BatchID: "batch-y",
		RootName: allowed.Name,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess2 := &capturingSession{peer: f.peerID}
	if err := f.app.HandlePullReq(sess2, protocol.New(protocol.TypeSharePullReq, bad)); err != nil {
		t.Fatal(err)
	}
	var resp2 protocol.SharePullResp
	if err := protocol.DecodeJSON(sess2.frame.Payload, &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.OK || resp2.Accepted != 0 {
		t.Fatalf("越界路径不该被受理：%#v", resp2)
	}
	if jobs, _ := f.tjobs.ListByBatch("batch-y"); len(jobs) != 0 {
		t.Fatalf("越界请求不该产生任何任务，得到 %d 条", len(jobs))
	}
}

// 改可见性必须落到库、更新摘要缓存并重新广播 —— 可见性写在摘要里，
// 对端靠它过滤；只改库不广播的话「改成指定人」要等下一轮周期通告才生效。
func TestShareSetVisibilityPersistsAndReannounces(t *testing.T) {
	f := newShareFixture(t)
	writeFile(t, filepath.Join(f.rootDir, "a.txt"), "A")
	sh := f.publish(t, share.Visibility{
		Type: share.VisibilityUsers, Allowed: []string{f.peerID.String()},
	})

	updated, err := f.app.SetVisibility(sh.ID, share.Visibility{Type: share.VisibilityAll})
	if err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	if updated.Visibility.Type != share.VisibilityAll {
		t.Fatalf("返回值里可见性没变：%#v", updated.Visibility)
	}
	if got, _ := f.repo.Get(sh.ID); got.Visibility.Type != share.VisibilityAll {
		t.Fatalf("库里可见性没变：%#v", got.Visibility)
	}
	if got := f.app.Summaries(); len(got) != 1 || got[0].Visibility.Type != share.VisibilityAll {
		t.Fatalf("摘要缓存没跟上（广播会携带旧可见性）：%#v", got)
	}

	// 名单被清干净（不是残留一个旧的 id）。
	if got, _ := f.repo.Get(sh.ID); len(got.Visibility.Allowed) != 0 {
		t.Fatalf("改成「全部」后名单应为空：%#v", got.Visibility.Allowed)
	}

	// 幂等：再设一次同一个值不报错。
	if _, err := f.app.SetVisibility(sh.ID, share.Visibility{Type: share.VisibilityAll}); err != nil {
		t.Fatalf("重复设置应当幂等：%v", err)
	}
}

func TestShareSetVisibilityRejectsBadInput(t *testing.T) {
	f := newShareFixture(t)
	writeFile(t, filepath.Join(f.rootDir, "a.txt"), "A")
	sh := f.publish(t, share.Visibility{Type: share.VisibilityAll})

	// 「指定人」给了空名单：必须拒 —— 否则这条共享会变成谁都不许看，
	// 而用户在界面上只看到一个成功提示。
	if _, err := f.app.SetVisibility(sh.ID, share.Visibility{Type: share.VisibilityUsers}); err == nil {
		t.Fatal("「指定人」空名单应当被拒")
	}
	// 「仅群组」必须恰好一个群。
	if _, err := f.app.SetVisibility(sh.ID, share.Visibility{
		Type: share.VisibilityGroup, Allowed: []string{"a", "b"},
	}); err == nil {
		t.Fatal("「仅群组」多选群应当被拒")
	}
	// 未知类型。
	if _, err := f.app.SetVisibility(sh.ID, share.Visibility{Type: "wat"}); err == nil {
		t.Fatal("未知可见性类型应当被拒")
	}
	// 不存在的共享。
	if _, err := f.app.SetVisibility("nope", share.Visibility{Type: share.VisibilityAll}); err == nil {
		t.Fatal("不存在的共享应当报错")
	}

	// 失败不能把原值改坏。
	if got, _ := f.repo.Get(sh.ID); got.Visibility.Type != share.VisibilityAll {
		t.Fatalf("失败的设置改动了库里的值：%#v", got.Visibility)
	}
}

// 拉取的批次根名：目录共享用（清洗过的）提议名，单文件共享强制用容器目录 ——
// 否则会落到 `<下载目录>/报告.pdf/报告.pdf`。
func TestSharePullRootName(t *testing.T) {
	dirShare := share.Share{Name: "照片", IsDir: true}
	fileShare := share.Share{Name: "报告.pdf", IsDir: false}

	cases := []struct {
		name     string
		sh       share.Share
		proposed string
		want     string
	}{
		{"目录共享用请求方的提议", dirShare, "三号线联调", "三号线联调"},
		{"提议为空时退到共享名", dirShare, "", "照片"},
		// 清洗走的是接收端同一套规则（SanitizeFileName）：目录成分被剥掉，
		// 尾部的点被去掉 —— 让这里与落盘端一致，而不是各写一套「像不像合法名」。
		{"提议带目录成分时只取最后一段", dirShare, "备份/2024", "2024"},
		{"提议尾部是点空格时被去掉", dirShare, "周报...", "周报"},
		{"提议清洗后为空时退到共享名", dirShare, " . ", "照片"},
		{"单文件共享忽略提议", fileShare, "报告.pdf", singleFileRootName},
		{"单文件共享忽略空提议", fileShare, "", singleFileRootName},
	}
	for _, c := range cases {
		if got := pullRootName(c.sh, c.proposed); got != c.want {
			t.Errorf("%s：pullRootName = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// 单文件共享的整个拉取路径：relPath 必须是「共享文件/文件名」两段，
// 否则接收端建不出文件（批次模型要求至少两段）。
func TestSharePullSingleFileUsesContainerRoot(t *testing.T) {
	f := newShareFixture(t)
	writeFile(t, filepath.Join(f.rootDir, "报告.pdf"), "pdf")
	sh, err := f.app.Publish(context.Background(), PublishRequest{
		RootPath: filepath.Join(f.rootDir, "报告.pdf"), Name: "报告.pdf",
		Visibility: share.Visibility{Type: share.VisibilityAll},
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if sh.IsDir || len(sh.Items) != 1 {
		t.Fatalf("单文件共享的形状不对：IsDir=%v items=%#v", sh.IsDir, sh.Items)
	}

	sess := &capturingSession{peer: f.peerID}
	req, err := protocol.EncodeJSON(protocol.SharePullReq{
		ShareID: sh.ID, Paths: []string{"报告.pdf"}, BatchID: "batch-single",
		// 请求方按自己的理解提议根名 = 共享名（= 文件名），服务端必须忽略它。
		RootName: "报告.pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.HandlePullReq(sess, protocol.New(protocol.TypeSharePullReq, req)); err != nil {
		t.Fatal(err)
	}
	var resp protocol.SharePullResp
	if err := protocol.DecodeJSON(sess.frame.Payload, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Accepted != 1 {
		t.Fatalf("受理结果不对：%#v", resp)
	}

	jobs, err := f.tjobs.ListByBatch("batch-single")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("应落 1 条任务，得到 %d", len(jobs))
	}
	if want := singleFileRootName + "/报告.pdf"; jobs[0].RelPath != want {
		t.Fatalf("rel_path = %q，期望 %q（不能是 报告.pdf/报告.pdf）", jobs[0].RelPath, want)
	}
}

// 取消共享后，服务端必须立刻停止提供清单（幂等，重复取消不报错）。
func TestShareUnpublishStopsServing(t *testing.T) {
	f := newShareFixture(t)
	writeFile(t, filepath.Join(f.rootDir, "a.txt"), "A")
	sh := f.publish(t, share.Visibility{Type: share.VisibilityAll})

	if err := f.app.Unpublish(sh.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Unpublish(sh.ID); err != nil {
		t.Fatalf("重复取消必须是幂等的：%v", err)
	}
	if got := f.app.Summaries(); len(got) != 0 {
		t.Fatalf("取消后摘要缓存应为空：%#v", got)
	}

	req, err := protocol.EncodeJSON(protocol.ShareIndexReq{ShareID: sh.ID})
	if err != nil {
		t.Fatal(err)
	}
	sess := &capturingSession{peer: f.peerID}
	if err := f.app.HandleIndexReq(sess, protocol.New(protocol.TypeShareIndexReq, req)); err != nil {
		t.Fatal(err)
	}
	var resp protocol.ShareIndexResp
	if err := protocol.DecodeJSON(sess.frame.Payload, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || !strings.Contains(resp.Error, "取消") {
		t.Fatalf("已取消的共享必须明确拒绝：%#v", resp)
	}
}

// 刷新索引：磁盘上新增的文件必须出现在新清单里（整份替换，而不是增量合并）。
func TestShareRefreshIndexReplacesItems(t *testing.T) {
	f := newShareFixture(t)
	writeFile(t, filepath.Join(f.rootDir, "a.txt"), "A")
	sh := f.publish(t, share.Visibility{Type: share.VisibilityAll})
	if len(sh.Items) != 1 {
		t.Fatalf("初始应有 1 项：%#v", sh.Items)
	}

	writeFile(t, filepath.Join(f.rootDir, "b.txt"), "BB")
	updated, err := f.app.RefreshIndex(context.Background(), sh.ID)
	if err != nil {
		t.Fatalf("RefreshIndex: %v", err)
	}
	if len(updated.Items) != 2 || updated.TotalSize != 3 {
		t.Fatalf("刷新后清单不对：%#v", updated.Items)
	}

	// 删掉一个再刷：清单必须跟着变少（增量合并做不到这一点）。
	if err := os.Remove(filepath.Join(f.rootDir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	again, err := f.app.RefreshIndex(context.Background(), sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Items) != 1 {
		t.Fatalf("删除后刷新应只剩 1 项：%#v", again.Items)
	}
}
