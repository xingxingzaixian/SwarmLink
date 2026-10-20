package version

import "testing"

// 版本比较是「谁该升级」的唯一判据，所以每个边界都单独钉住。
func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		// 缺段补 0：运维最常写错的两种写法必须等价。
		{"1.2", "1.2.0", 0},
		{"1.2.0.0", "1.2", 0},
		{"1.2.1", "1.2", 1},
		{"1.2", "1.2.1", -1},
		{"1.10.0", "1.9.9", 1}, // 逐段数值比较，不是字符串比较
		{"2", "1.99.99", 1},
		{"1.0.0.1", "1.0.0", 1},
		// 预发布后缀被丢弃：1.2.0-beta 与 1.2.0 视为同一个版本。
		{"1.2.0-beta", "1.2.0", 0},
		{"1.2.0+build5", "1.2.0", 0},
		// 解析失败按 0.0.0：开发版永远不参与更新。
		{"dev", "1.0.0", -1},
		{"", "1.0.0", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{"", " ", "1.", ".1", "1.2.3.4.5", "a.b", "-1"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) 应当报错", s)
		}
	}
}

func TestIsUpgrade(t *testing.T) {
	if !IsUpgrade("1.0.1", "1.0.0") {
		t.Fatal("更高的版本必须判定为可升级")
	}
	if IsUpgrade("1.0.0", "1.0.0") {
		t.Fatal("同版本不得判定为可升级（否则互相拉升级成死循环）")
	}
	if IsUpgrade("0.9.0", "1.0.0") {
		t.Fatal("低版本不得触发降级")
	}
	if IsUpgrade("dev", "1.0.0") || IsUpgrade("1.0.0", "dev") {
		t.Fatal("无法解析的版本号不得参与升级判断")
	}
}

// 当前版本必须可解析，否则整机永远不参与更新 —— 这是最容易漏的手误。
func TestCurrentVersionIsParseable(t *testing.T) {
	n, err := Parse(Version)
	if err != nil {
		t.Fatalf("Version=%q 不可解析: %v", Version, err)
	}
	if n.IsZero() {
		t.Fatalf("Version=%q 被解析成 0.0.0，将导致本机永不更新（发布前必须手改）", Version)
	}
}
