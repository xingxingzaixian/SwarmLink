package fsutil

import (
	"slices"
	"strings"
	"testing"
)

// 相对路径清洗：接受集要还原成分段，拒绝集是主体。
//
// 拒绝集每一条都是「远端可以发过来的东西」而不是假想输入：rel_path 由对端
// 决定，接收端唯一的防线就是这里。
func TestSanitizeRelPath(t *testing.T) {
	ok := []struct {
		in   string
		want []string
	}{
		{"照片/a.jpg", []string{"照片", "a.jpg"}},
		{`照片\a.jpg`, []string{"照片", "a.jpg"}}, // 对端可能是 Windows 分隔符
		{"照片/子/深/c.txt", []string{"照片", "子", "深", "c.txt"}},
		// 段内空格合法，但尾随空格会被 SanitizeFileName 去掉（两套平台都不该有它）
		{" 间距 /a.txt", []string{" 间距", "a.txt"}},
		{"单个名字", []string{"单个名字"}},
	}
	for _, tc := range ok {
		got, err := SanitizeRelPath(tc.in)
		if err != nil {
			t.Errorf("SanitizeRelPath(%q) 不该报错: %v", tc.in, err)
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("SanitizeRelPath(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}

	bad := []struct {
		in  string
		why string
	}{
		{"", "空相对路径不是一条路径"},
		{"../../etc/passwd", "穿越"},
		{"照片/../../evil.txt", "中段穿越"},
		{"/etc/passwd", "绝对路径"},
		{`C:\Windows\evil.dll`, "盘符（伪装成一段）"},
		{`照片/..`, "尾段是 .."},
		{"照片/.", "段是 ."},
		{"照片//a.txt", "空段（双斜杠是畸形输入，不该被悄悄折叠）"},
		{"照片/", "尾随分隔符会切出空段"},
		{"照片/CON.txt", "Windows 保留设备名"},
		{"a/" + strings.Repeat("b/", MaxRelPathDepth+2) + "c", "层数超限"},
	}
	for _, tc := range bad {
		t.Run(tc.in, func(t *testing.T) {
			got, err := SanitizeRelPath(tc.in)
			if err == nil {
				t.Fatalf("%s：SanitizeRelPath(%q) 必须被拒绝，实际返回 %q", tc.why, tc.in, got)
			}
		})
	}
}

// 单段超长：必须【继承 SanitizeFileName 的既有契约】，即按 UTF-8 边界截断，
// 而不是拒绝。
//
// 两处规则的取舍只有一份：如果这里改成拒绝，同一个名字走单文件传输能收、
// 走文件夹传输却被拒 —— 那种不一致最难解释。截断后同名冲突由 filesink
// 的 (1)/(2) 兜住，不会丢文件。
func TestSanitizeRelPathTruncatesLongSegment(t *testing.T) {
	got, err := SanitizeRelPath("照片/" + strings.Repeat("a", 260) + ".txt")
	if err != nil {
		t.Fatalf("超长段应当截断而不是报错，得到 %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("段数 = %d，期望 2", len(got))
	}
	if len(got[1]) > maxNameBytes {
		t.Fatalf("截断后长度 = %d，超过上限 %d", len(got[1]), maxNameBytes)
	}
}

// 层数恰好等于上限时必须放行（边界不能写成 off-by-one）。
func TestSanitizeRelPathDepthBoundary(t *testing.T) {
	exact := strings.TrimSuffix(strings.Repeat("d/", MaxRelPathDepth), "/")
	got, err := SanitizeRelPath(exact)
	if err != nil {
		t.Fatalf("恰好 %d 层必须放行，得到 %v", MaxRelPathDepth, err)
	}
	if len(got) != MaxRelPathDepth {
		t.Fatalf("段数 = %d，期望 %d", len(got), MaxRelPathDepth)
	}
}
