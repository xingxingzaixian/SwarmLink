package share

import (
	"testing"
	"time"
)

func TestVisibilityAllows(t *testing.T) {
	owner := "aaaaaaaaaaaaaaaa"
	cases := []struct {
		name      string
		vis       Visibility
		requester string
		groups    []string
		want      bool
	}{
		{"全部可见-陌生人", Visibility{Type: VisibilityAll}, "bbbbbbbbbbbbbbbb", nil, true},
		{"全部可见-owner", Visibility{Type: VisibilityAll}, owner, nil, true},
		{"指定人-命中", Visibility{Type: VisibilityUsers, Allowed: []string{"bbbbbbbbbbbbbbbb"}}, "bbbbbbbbbbbbbbbb", nil, true},
		{"指定人-未命中", Visibility{Type: VisibilityUsers, Allowed: []string{"bbbbbbbbbbbbbbbb"}}, "cccccccccccccccc", nil, false},
		{"指定人-白名单为空", Visibility{Type: VisibilityUsers}, "bbbbbbbbbbbbbbbb", nil, false},
		{"指定人-owner 恒可见", Visibility{Type: VisibilityUsers, Allowed: []string{"bbbbbbbbbbbbbbbb"}}, owner, nil, true},
		{"群组-成员可见", Visibility{Type: VisibilityGroup, Allowed: []string{"g1"}}, "bbbbbbbbbbbbbbbb", []string{"g0", "g1"}, true},
		{"群组-非成员不可见", Visibility{Type: VisibilityGroup, Allowed: []string{"g1"}}, "bbbbbbbbbbbbbbbb", []string{"g0"}, false},
		{"未知类型一律拒绝", Visibility{Type: "weird", Allowed: []string{"bbbbbbbbbbbbbbbb"}}, "bbbbbbbbbbbbbbbb", nil, false},
		{"空请求者拒绝", Visibility{Type: VisibilityAll}, "", nil, false},
		{"大小写不敏感", Visibility{Type: VisibilityUsers, Allowed: []string{"BBBBBBBBBBBBBBBB"}}, "bbbbbbbbbbbbbbbb", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.vis.Allows(tc.requester, tc.groups, owner); got != tc.want {
				t.Fatalf("Allows(%q) = %v, want %v", tc.requester, got, tc.want)
			}
		})
	}
}

func TestVisibilityNormalize(t *testing.T) {
	got := Visibility{
		Type:    VisibilityUsers,
		Allowed: []string{" BB ", "aa", "", "AA", "bb"},
	}.Normalize()
	if len(got.Allowed) != 2 || got.Allowed[0] != "aa" || got.Allowed[1] != "bb" {
		t.Fatalf("归一化结果错误: %#v", got.Allowed)
	}

	// all 归一化时清空白名单：留着它会让摘要字节数随无意义的残留变化。
	all := Visibility{Type: VisibilityAll, Allowed: []string{"aa"}}.Normalize()
	if len(all.Allowed) != 0 {
		t.Fatalf("all 应当清空白名单，得到 %#v", all.Allowed)
	}

	empty := Visibility{}.Normalize()
	if empty.Type != DefaultVisibility {
		t.Fatalf("空可见性应回落到默认值，得到 %q", empty.Type)
	}
}

func TestVisibilityValidate(t *testing.T) {
	if err := (Visibility{Type: VisibilityUsers}).Validate(); err == nil {
		t.Fatal("「指定人」无名单应当报错")
	}
	if err := (Visibility{Type: VisibilityGroup, Allowed: []string{"g1", "g2"}}).Validate(); err == nil {
		t.Fatal("「仅群组」指定多个群应当报错")
	}
	if err := (Visibility{Type: VisibilityGroup, Allowed: []string{"g1"}}).Validate(); err != nil {
		t.Fatalf("合法的群可见性不应报错: %v", err)
	}
	if err := (Visibility{Type: VisibilityAll}).Validate(); err != nil {
		t.Fatalf("全部可见不应报错: %v", err)
	}
}

func TestCleanRelPath(t *testing.T) {
	ok := []struct{ in, want string }{
		{"a.txt", "a.txt"},
		{"2026/W01.docx", "2026/W01.docx"},
		{`2026\W01.docx`, "2026/W01.docx"},
		{"  trim/name.txt  ", "trim/name.txt"},
	}
	for _, tc := range ok {
		got, err := CleanRelPath(tc.in)
		if err != nil {
			t.Fatalf("CleanRelPath(%q) 意外报错: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("CleanRelPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	bad := []string{"", "  ", "/abs.txt", `\abs.txt`, "../escape", "a/../b", "a//b", "C:/x.txt", "a/./b", "a/b/"}
	for _, in := range bad {
		if got, err := CleanRelPath(in); err == nil {
			t.Fatalf("CleanRelPath(%q) 应当报错，得到 %q", in, got)
		}
	}
}

func TestJoinRelPath(t *testing.T) {
	got, err := JoinRelPath("周报模板", "2026/W01.docx")
	if err != nil || got != "周报模板/2026/W01.docx" {
		t.Fatalf("JoinRelPath = %q, %v", got, err)
	}
	if _, err := JoinRelPath("", "a.txt"); err == nil {
		t.Fatal("空根名应当报错")
	}
	if _, err := JoinRelPath("root", "../x"); err == nil {
		t.Fatal("越界路径应当报错")
	}
}

func TestShareValidateAndSummary(t *testing.T) {
	s := Share{
		ID:    "s1",
		Name:  "周报模板",
		Owner: "aaaaaaaaaaaaaaaa",
		IsDir: true,
		Visibility: Visibility{
			Type:    VisibilityUsers,
			Allowed: []string{"bbbbbbbbbbbbbbbb"},
		},
		Items: []Item{
			{Path: "a.txt", Name: "a.txt", Size: 10},
			{Path: "b/c.txt", Name: "c.txt", Size: 5},
		},
		TotalSize: 15,
		CreatedAt: time.Unix(1000, 0),
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("合法共享不应报错: %v", err)
	}

	sum := s.Summary()
	if sum.FileCount != 2 || sum.TotalSize != 15 || sum.OwnerID != s.Owner {
		t.Fatalf("摘要字段不对: %#v", sum)
	}
	if !sum.VisibleTo("bbbbbbbbbbbbbbbb", nil) {
		t.Fatal("白名单内的人应当可见")
	}
	if sum.VisibleTo("cccccccccccccccc", nil) {
		t.Fatal("白名单外的人不应可见")
	}

	if _, ok := s.ItemByPath("b/c.txt"); !ok {
		t.Fatal("应当能按路径查到条目")
	}
	if _, ok := s.ItemByPath("nope.txt"); ok {
		t.Fatal("不存在的路径不该命中")
	}

	// 非法条目路径必须让整条共享不可发布。
	bad := s
	bad.Items = []Item{{Path: "../escape"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("含越界条目的共享应当校验失败")
	}
}

func TestShareValidateRejectsBadInput(t *testing.T) {
	base := Share{
		ID:         "s1",
		Name:       "ok",
		Owner:      "aaaaaaaaaaaaaaaa",
		Visibility: Visibility{Type: VisibilityAll},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("基线应当合法: %v", err)
	}

	noID := base
	noID.ID = ""
	if err := noID.Validate(); err == nil {
		t.Fatal("空 id 应当报错")
	}

	noName := base
	noName.Name = "  "
	if err := noName.Validate(); err == nil {
		t.Fatal("空名应当报错")
	}

	long := base
	long.Name = string(make([]rune, MaxNameLen+1))
	// 用可打印字符填充，避免把 0 值 rune 当作空白裁掉。
	runes := make([]rune, 0, MaxNameLen+1)
	for i := 0; i <= MaxNameLen; i++ {
		runes = append(runes, 'x')
	}
	long.Name = string(runes)
	if err := long.Validate(); err == nil {
		t.Fatal("超长共享名应当报错")
	}

	tooMany := base
	for i := 0; i < MaxItems+1; i++ {
		tooMany.Items = append(tooMany.Items, Item{Path: "a", Name: "a"})
	}
	if err := tooMany.Validate(); err == nil {
		t.Fatal("条目超限应当报错")
	}
}

func TestNormalizeSummaries(t *testing.T) {
	in := []Summary{
		{ShareID: "b", Name: "B", OwnerID: "o1"},
		{ShareID: "a", Name: "A", OwnerID: "o1"},
		{ShareID: "", Name: "bad", OwnerID: "o1"},
		{ShareID: "c", Name: "  ", OwnerID: "o1"},
		{ShareID: "d", Name: "D", OwnerID: ""},
		{ShareID: "e", Name: "E", OwnerID: "o1", FileCount: -3, TotalSize: -1},
	}
	out := NormalizeSummaries(in)
	if len(out) != 3 {
		t.Fatalf("应过滤掉畸形项，得到 %d 条: %#v", len(out), out)
	}
	// 排序必须确定：a < b < e。
	if out[0].ShareID != "a" || out[1].ShareID != "b" || out[2].ShareID != "e" {
		t.Fatalf("排序结果不对: %#v", out)
	}
	if out[2].FileCount != 0 || out[2].TotalSize != 0 {
		t.Fatalf("负计数应归零: %#v", out[2])
	}

	// 截断到上限。
	many := make([]Summary, 0, MaxAnnouncedShares+5)
	for i := 0; i < MaxAnnouncedShares+5; i++ {
		many = append(many, Summary{ShareID: string(rune('A' + i)), Name: "n", OwnerID: "o"})
	}
	if got := len(NormalizeSummaries(many)); got != MaxAnnouncedShares {
		t.Fatalf("应截断到 %d，得到 %d", MaxAnnouncedShares, got)
	}
}
