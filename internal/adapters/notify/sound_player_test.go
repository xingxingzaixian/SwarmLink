package notify

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// 「找不到播放器就静默不响」是提示音最容易被漏掉的失败方式：代码一路顺风、
// 没有任何报错，用户只是从来听不到声音。因此候选选择必须能脱离真实环境测。
func TestPickPlayer(t *testing.T) {
	cands := []soundPlayer{
		{name: "missing-1", args: func(string) []string { return nil }},
		{name: "present-2", args: func(p string) []string { return []string{p} }},
		{name: "present-3", args: func(string) []string { return nil }},
	}
	look := func(name string) (string, error) {
		if name == "missing-1" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + name, nil
	}

	got, ok := pickPlayer(cands, look)
	if !ok {
		t.Fatal("有可用播放器时却报告不可用")
	}
	if got.name != "present-2" {
		t.Fatalf("选中 %q，期望按顺序取第一个可用的 present-2", got.name)
	}
	if args := got.args("/tmp/a.wav"); len(args) != 1 || args[0] != "/tmp/a.wav" {
		t.Errorf("参数 = %v，期望把文件路径原样传给播放器", args)
	}
}

// 一个候选都找不到时必须报告 false：调用方据此静默，而不是反复起进程失败。
func TestPickPlayerNoneAvailable(t *testing.T) {
	look := func(string) (string, error) { return "", errors.New("not found") }
	cands := []soundPlayer{{name: "paplay", args: func(string) []string { return nil }}}

	if _, ok := pickPlayer(cands, look); ok {
		t.Fatal("一个播放器都找不到时不应返回可用项")
	}
}

// 临时文件必须真的带上内嵌音频的字节：写坏了同样表现为「从来不响」，
// 而且播放器只会返回一个含糊的错误码。
func TestWriteTempSound(t *testing.T) {
	want := Sound()
	path, err := writeTempSound(want)
	if err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	defer os.Remove(path)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回临时文件失败: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("临时文件内容与内嵌音频不一致（%d vs %d 字节）", len(got), len(want))
	}
}
