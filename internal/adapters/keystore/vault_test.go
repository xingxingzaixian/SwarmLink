package keystore

import (
	"errors"
	"os"
	"testing"
)

// 原子写：save 必须走「写 .tmp → rename」，因此
//
//	(a) save 之后不该残留 .tmp（残留说明实现没做替换）；
//	(b) 即使目录里躺着上一次被打断留下的 .tmp，load 也必须读正式文件。
func TestVaultSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	v := newVault(dir)

	if err := v.save([]byte("first")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(v.filePath() + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("save 之后不该残留 .tmp：说明没有走 rename 的原子替换")
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
}

// 文件不存在时必须是可识别的 ErrNotExist —— A4 的三分支全靠它区分
// 「没有托管副本」与「托管副本损坏」。
func TestVaultMissingFileIsErrNotExist(t *testing.T) {
	v := newVault(t.TempDir())
	if _, err := v.load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺失时必须返回 ErrNotExist，得到 %v", err)
	}
}
