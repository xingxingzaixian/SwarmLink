// Command vergen 把 internal/version.Version 回写到各平台的打包元数据。
//
// 存在理由：应用版本在运行时只有一个真源（内嵌在二进制里的 Go 常量），
// 而安装包元数据是另外几份文件（wails 的 config.yml、Windows 的 info.json、
// macOS 的 Info.plist、Linux 的 nfpm.yaml、前端的 package.json）。
// 它们不一致时，用户会看到「exe 属性 1.0.1、界面里 1.0.2、局域网广播 1.0.0」，
// 而局域网更新拿到这种数据会直接陷入「装完还是旧版 → 再拉一次」的死循环。
//
// 用法：
//
//	go run ./scripts/vergen           # 以 version.Version 为准写入各文件
//	go run ./scripts/vergen -check    # 只校验，不一致时以非零码退出（CI / 打包前）
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/swarmlink/swarmlink/internal/version"
)

// rule 是一条「在某个文件里把版本号改成 x」的规则。
type rule struct {
	path    string
	pattern *regexp.Regexp
	// repl 里的 $1 / $2 是模式的前后分组，版本号插在中间。
	repl string
}

func main() {
	check := flag.Bool("check", false, "只校验各文件是否与 internal/version.Version 一致")
	flag.Parse()

	v := version.Version
	if n, err := version.Parse(v); err != nil || n.IsZero() {
		fail("internal/version.Version=%q 不是合法的发布版本号（必须是 1~4 段数字）", v)
	}

	rules := []rule{
		{
			path:    "build/config.yml",
			pattern: regexp.MustCompile(`(?m)^(\s{2}version:\s*")[^"]*(")`),
			repl:    "${1}" + v + "${2}",
		},
		{
			path:    "build/windows/info.json",
			pattern: regexp.MustCompile(`("file_version"\s*:\s*")[^"]*(")`),
			repl:    "${1}" + v + "${2}",
		},
		{
			path:    "build/windows/info.json",
			pattern: regexp.MustCompile(`("ProductVersion"\s*:\s*")[^"]*(")`),
			repl:    "${1}" + v + "${2}",
		},
		{
			path:    "build/darwin/Info.plist",
			pattern: regexp.MustCompile(`(?s)(<key>CFBundleShortVersionString</key>\s*<string>)[^<]*(</string>)`),
			repl:    "${1}" + v + "${2}",
		},
		{
			path:    "build/darwin/Info.plist",
			pattern: regexp.MustCompile(`(?s)(<key>CFBundleVersion</key>\s*<string>)[^<]*(</string>)`),
			repl:    "${1}" + v + "${2}",
		},
		{
			path:    "build/darwin/Info.dev.plist",
			pattern: regexp.MustCompile(`(?s)(<key>CFBundleShortVersionString</key>\s*<string>)[^<]*(</string>)`),
			repl:    "${1}" + v + "${2}",
		},
		{
			path:    "build/darwin/Info.dev.plist",
			pattern: regexp.MustCompile(`(?s)(<key>CFBundleVersion</key>\s*<string>)[^<]*(</string>)`),
			repl:    "${1}" + v + "${2}",
		},
		{
			path:    "build/linux/nfpm/nfpm.yaml",
			pattern: regexp.MustCompile(`(?m)^(version:\s*")[^"]*(")`),
			repl:    "${1}" + v + "${2}",
		},
		{
			path:    "frontend/package.json",
			pattern: regexp.MustCompile(`(?m)^(\s{2}"version"\s*:\s*")[^"]*(")`),
			repl:    "${1}" + v + "${2}",
		},
	}

	changed := 0
	for _, r := range rules {
		data, err := os.ReadFile(r.path)
		if err != nil {
			fail("读取 %s 失败：%v", r.path, err)
		}
		next := r.pattern.ReplaceAllString(string(data), r.repl)
		if next == string(data) {
			continue
		}
		if *check {
			fail("%s 的版本号与 internal/version.Version=%s 不一致（请运行 go run ./scripts/vergen）", r.path, v)
		}
		// 保留原文件的行尾：这些文件里有的是 CRLF（Windows 侧手工编辑过），
		// 统一改成 LF 会在 diff 里制造整文件改动。
		if err := os.WriteFile(r.path, []byte(next), 0o644); err != nil {
			fail("写入 %s 失败：%v", r.path, err)
		}
		changed++
		fmt.Printf("已更新 %s\n", r.path)
	}

	if *check {
		fmt.Printf("版本一致：%s\n", v)
		return
	}
	if changed == 0 {
		fmt.Printf("版本已是 %s，无需修改\n", v)
	}
}

func fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, strings.TrimSpace(msg))
	os.Exit(1)
}
