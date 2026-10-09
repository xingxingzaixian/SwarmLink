package keystore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// posixPerms 报告当前平台是否真正落实 POSIX 权限位。
//
// Windows 不落实：os.WriteFile(0600) 只映射到「只读」属性，os.MkdirAll(0700) 同理，
// 于是 os.Stat 返回 0666 / 0777。这是 Go 在 Windows 上的既定语义，不是本适配器的
// 缺陷 —— keystore.go 的包注释与《架构设计书》都把它记为平台限制（私钥在 Windows
// 上不保证对其他用户不可读，只能靠 ACL，而不是权限位）。
//
// 因此权限断言只在落实权限位的平台上成立。做成常量而不是两处各写一个 if：两处
// 用例用的是同一条判据，将来若真的接上 Windows ACL，只需改这一处。
const posixPerms = runtime.GOOS != "windows"

func TestLoadOrCreateIsStableAndPersists(t *testing.T) {
	dir := t.TempDir()

	kp1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	kp2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if kp1.NodeID() != kp2.NodeID() {
		t.Fatal("node id not stable across loads")
	}

	fi, err := os.Stat(KeyPath(dir))
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if posixPerms {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("want 0600 got %#o", perm)
		}
	} else if fi.Size() == 0 {
		// Windows 上断言不了权限位，但也不该让这条用例退化成空跑：
		// 至少钉住「私钥确实落了盘且非空」—— 这才是本用例的主题。
		t.Fatal("key file is empty")
	}
}

func TestLoadOrCreateCreatesDirWith0700(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Fatalf("identity 必须是目录，实际 mode = %v", fi.Mode())
	}
	if posixPerms {
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Fatalf("want 0700 got %#o", perm)
		}
	}
}

func TestLoadOrCreateRejectsCorruptKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/identity", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(KeyPath(dir), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("expected error for corrupt key")
	}
}
