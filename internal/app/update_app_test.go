package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/version"
)

// newTestUpdateApp 构造一个只用于「重启后结算」的实例。
//
// 不调用 Start：那会拉起后台循环并立刻发 DNS/UDP，而这些用例只关心
// loadPersistLocked 这一个纯函数式的结算逻辑。
func newTestUpdateApp(t *testing.T, dir string) *UpdateApp {
	t.Helper()
	a := NewUpdateApp(identity.NodeID{}, nil, nil, dir, clock.New(), nil)
	return a
}

func writePersist(t *testing.T, dir string, p updatePersist) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "updates"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "updates", "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// 「上次退出时正在安装，这次启动版本已经变了」= 成功。
// 必须清掉状态文件，否则每次启动都会重放这段判断。
func TestLoadPersistSuccessClearsState(t *testing.T) {
	dir := t.TempDir()
	writePersist(t, dir, updatePersist{
		Phase:         string(UpdateApplying),
		TargetVersion: version.Version,
		SourcePeerID:  "1122334455667788",
	})

	a := newTestUpdateApp(t, dir)
	a.mu.Lock()
	a.loadPersistLocked()
	a.mu.Unlock()

	if a.st.Phase == UpdateFailed {
		t.Fatalf("版本已经是目标版本，不应判定为失败：%+v", a.st)
	}
	if _, err := os.Stat(filepath.Join(dir, "updates", "state.json")); !os.IsNotExist(err) {
		t.Fatalf("成功后应删除 state.json，err=%v", err)
	}
}

// 「上次退出时正在安装，这次启动版本没变」= 失败：必须拉黑来源并累加次数。
// 这是「装完还是旧版 → 再拉一次」死循环的唯一刹车。
func TestLoadPersistFailureBlacklistsSource(t *testing.T) {
	dir := t.TempDir()
	writePersist(t, dir, updatePersist{
		Phase:         string(UpdateApplying),
		TargetVersion: "9.9.9",
		SourcePeerID:  "1122334455667788",
		FailedCount:   1,
	})

	a := newTestUpdateApp(t, dir)
	a.mu.Lock()
	a.loadPersistLocked()
	a.mu.Unlock()

	if a.st.Phase != UpdateFailed {
		t.Fatalf("phase=%q want %q", a.st.Phase, UpdateFailed)
	}
	if a.attempted != 2 {
		t.Fatalf("attempted=%d want 2", a.attempted)
	}
	id, err := identity.ParseNodeID("1122334455667788")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.fail[id]; !ok {
		t.Fatal("失败后必须拉黑来源，否则下一轮会再拉一次同样的包")
	}
}

// 非 applying 状态不做任何结算：用户只是关掉过应用，没有半途而废的安装。
func TestLoadPersistIgnoresNonApplying(t *testing.T) {
	dir := t.TempDir()
	writePersist(t, dir, updatePersist{Phase: string(UpdateReady), TargetVersion: "9.9.9"})

	a := newTestUpdateApp(t, dir)
	a.mu.Lock()
	a.loadPersistLocked()
	a.mu.Unlock()

	if a.st.Phase != UpdateIdle {
		t.Fatalf("phase=%q want idle", a.st.Phase)
	}
	if len(a.fail) != 0 {
		t.Fatal("非安装态不应拉黑任何来源")
	}
}

// 安装包文件名必须带版本号：源节点靠它确认「手上的包就是我广播的那个版本」。
func TestUpdateInstallerName(t *testing.T) {
	got := UpdateInstallerName("1.2.3")
	if got != "swarmlink-1.2.3-installer"+UpdateInstallExt() {
		t.Fatalf("got %q", got)
	}
}
