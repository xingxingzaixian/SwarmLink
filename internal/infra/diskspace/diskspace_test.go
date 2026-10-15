package diskspace

import (
	"os"
	"path/filepath"
	"testing"
)

// 真机探测：临时目录所在卷必须探得到、且回一个正数。
//
// 这条用例的价值不在"断言 0 以上"这么浅：它是【平台实现的编译+运行证明】。
// free_windows.go / free_unix.go 里全是 syscall 结构体字段，写错一个字段名
// 或偏移，光靠编译不一定暴露 —— 真跑一次才说明这条路是通的。
func TestFreeBytesOnTempDir(t *testing.T) {
	free, err := FreeBytes(t.TempDir())
	if err != nil {
		t.Fatalf("FreeBytes(tempDir): %v", err)
	}
	if free == 0 {
		t.Error("临时目录所在卷的可写空间不可能是 0（要么探测错了，要么这台机器真的满了）")
	}
}

// 文件路径也要能探：调用方手里往往是"某个文件的所在目录"，
// 平台实现内部要自己取它的目录，不能要求调用方先 filepath.Dir。
func TestFreeBytesAcceptsFilePath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FreeBytes(p); err != nil {
		t.Fatalf("FreeBytes(文件路径): %v", err)
	}
}

// 不存在的路径必须报错而不是回 0：回 0 会被调用方读成"磁盘满了"，
// 于是每一次传输都被拒，而真正的原因只是路径写错了。
func TestFreeBytesOnMissingPathFails(t *testing.T) {
	if _, err := FreeBytes(filepath.Join(t.TempDir(), "不存在", "更深")); err == nil {
		t.Fatal("不存在的路径必须报错")
	}
}
