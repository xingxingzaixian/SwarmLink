package keystore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tmpLeftovers 返回 identity 目录下残留的临时文件（任何命名）。
//
// 断言「不留残渣」而不是断言某个固定文件名：临时名带 pid，写死名字会让断言永远通过。
func tmpLeftovers(t *testing.T, v *vault) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(v.filePath()))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), vaultFileName+".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}

// 本用例钉住的是【不留残渣】与【残渣不被读取】两件事。
//
// 真原子性（进程被杀死/断电后正式文件仍是完整旧内容）与 fsync 在单测里不可观测，
// 因此不在覆盖范围内 —— 那是 A2 记录在 ADR-012 的同类残余缺口。
func TestVaultSaveIsAtomic(t *testing.T) {
	v := newVault(t.TempDir())

	if err := v.save([]byte("first")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if left := tmpLeftovers(t, v); len(left) != 0 {
		t.Fatalf("save 之后不该残留临时文件：%v（说明没有走 rename 的原子替换）", left)
	}

	// 模拟「上一次写到一半被打断」：残留一个半截 .tmp，正式文件仍是上一份完整内容
	if err := os.WriteFile(v.filePath()+".tmp", []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := v.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("残留 .tmp 污染了正式文件：got %q", got)
	}
}

// rename 失败时必须报错并清理临时文件 —— 这条覆盖 save 的失败出口
// （原用例完全没碰失败路径，去掉清理也不会有任何断言变红）。
func TestVaultSaveCleansUpWhenRenameFails(t *testing.T) {
	v := newVault(t.TempDir())
	if err := v.save([]byte("first")); err != nil {
		t.Fatal(err)
	}

	// 用一个【目录】占住正式文件路径：rename 必然失败（Windows 与 Unix 都是确定性的）
	if err := os.Remove(v.filePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(v.filePath(), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := v.save([]byte("second")); err == nil {
		t.Fatal("rename 失败必须报错")
	}
	if left := tmpLeftovers(t, v); len(left) != 0 {
		t.Fatalf("失败路径必须清理临时文件：%v", left)
	}
}

// 禁用状态（vaultDir == ""）下所有操作都必须无害：便携模式与守卫命中时走的就是这条路径。
func TestVaultDisabledIsInert(t *testing.T) {
	v := newVault("")

	if v.enabled() {
		t.Fatal("空目录必须表示禁用")
	}
	if err := v.save([]byte("x")); err != nil {
		t.Fatalf("禁用时 save 不应报错：%v", err)
	}
	if _, err := v.load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("禁用时 load 应返回 ErrNotExist（调用方据此区分「没有副本」与「副本坏了」），得到 %v", err)
	}

	// nil 接收者等价于禁用：LoadOrCreate 在禁用时可能拿到零值
	var nilVault *vault
	if nilVault.enabled() {
		t.Fatal("nil vault 必须表示禁用")
	}
	if err := nilVault.save([]byte("x")); err != nil {
		t.Fatalf("nil vault 的 save 不应报错：%v", err)
	}
}

// 文件不存在时必须是可识别的 ErrNotExist —— A4 的三分支全靠它区分
// 「没有托管副本」与「托管副本损坏」。
func TestVaultMissingFileIsErrNotExist(t *testing.T) {
	v := newVault(t.TempDir())
	if _, err := v.load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺失时必须返回 ErrNotExist，得到 %v", err)
	}
}

// 「读不出来」必须与「不存在」和「损坏」都不同：A4 据此选择降级而不是覆盖掉它。
// 用目录占住路径来构造一次确定性的读失败（不是文件不存在，也不是解密失败）。
func TestVaultUnreadableIsDistinctFromMissing(t *testing.T) {
	v := newVault(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(v.filePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(v.filePath(), 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := v.load()
	if err == nil {
		t.Fatal("期望读失败")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatal("这不是「不存在」，不能被当成首次运行")
	}
	if !errors.Is(err, errVaultUnavailable) {
		t.Fatalf("必须可被 errVaultUnavailable 识别，得到 %v", err)
	}
}

// 权限位：与 keystore_test.go 同一口径（只有落实 POSIX 权限位的平台才断言）。
func TestVaultFilePermissions(t *testing.T) {
	if !posixPerms {
		t.Skip("本平台不落实 POSIX 权限位")
	}
	v := newVault(t.TempDir())
	if err := v.save([]byte("x")); err != nil {
		t.Fatal(err)
	}

	dirInfo, err := os.Stat(filepath.Dir(v.filePath()))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("identity 目录权限 want 0700 got %#o", perm)
	}
	fileInfo, err := os.Stat(v.filePath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("副本文件权限 want 0600 got %#o", perm)
	}
}
