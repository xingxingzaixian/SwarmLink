// Package version 定义应用版本号与版本比较规则。
//
// 为什么需要这个包：Go 侧此前没有任何「应用版本」概念（build/config.yml 里
// 那个 1.0.0 只写进安装包元数据，运行时读不到），于是「局域网里谁比我新」
// 这个问题在代码里根本无法回答。
//
// 设计约束（见 ADR：局域网更新）：
//   - 版本号【手工维护】，唯一真源就是本文件的 Version 常量；
//   - 打包脚本在构建前把它回写到 build/config.yml / info.json / Info.plist，
//     不一致直接报错，避免「exe 属性 1.0.2、广播 1.0.1」这种升级死循环；
//   - 解析失败（如开发期的 "dev"）一律视为 0.0.0，永不参与更新 ——
//     开发机不能被生产机拉升级，也不能把生产机拉下来。
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 是应用的唯一版本真源。
//
// 【发布前手改这一行】。格式：1~4 段十进制数字（"1.2" / "1.2.0" / "1.2.0.3"
// 都合法），可以带 "-beta" 之类的后缀，后缀在比较时被忽略。
//
// 不要在这里写 "dev" 期望它表示「开发版」—— 那会被解析成 0.0.0，
// 效果同样是「不参与更新」，但字符串更难看懂。
const Version = "0.1.5"

// parts 是版本被拆成的四段数字。
const parts = 4

// Number 是解析后的可比较版本号。
//
// 用定长数组而不是切片：比较时永远有 4 段可对齐，缺的段补 0，
// "1.2" 与 "1.2.0.0" 因此天然相等（这是运维最常写错的两种写法）。
type Number struct {
	parts [parts]uint64
}

// Parse 解析版本号。
//
// 规则：
//   - 以 '-' 或 '+' 为界，其后的一切（预发布/构建元数据）丢弃；
//   - 剩余部分按 '.' 切成 1~4 段十进制数字，缺的段补 0；
//   - 空串、超过 4 段、非数字一律报错（调用方按 0.0.0 处理）。
func Parse(s string) (Number, error) {
	var n Number
	s = strings.TrimSpace(s)
	if s == "" {
		return n, fmt.Errorf("version: 版本号为空")
	}
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		// 只截断后缀，不校验后缀内容：预发布标记不参与「谁更新」的判断，
		// 校验它只会让一次手误（"1.2.0-rc.1"）变成"整台机器永远不更新"。
		s = s[:i]
	}
	segs := strings.Split(s, ".")
	if len(segs) > parts {
		return n, fmt.Errorf("version: %q 超过 %d 段", s, parts)
	}
	for i, seg := range segs {
		if seg == "" {
			return n, fmt.Errorf("version: %q 含空段", s)
		}
		v, err := strconv.ParseUint(seg, 10, 64)
		if err != nil {
			return n, fmt.Errorf("version: %q 第 %d 段不是十进制数字", s, i+1)
		}
		n.parts[i] = v
	}
	return n, nil
}

// ParseLenient 解析版本号，失败时返回 0.0.0（永不参与更新）。
func ParseLenient(s string) Number {
	n, err := Parse(s)
	if err != nil {
		return Number{}
	}
	return n
}

// Current 返回当前运行的版本号（解析失败即 0.0.0）。
func Current() Number { return ParseLenient(Version) }

// IsZero 判断是否为 0.0.0（「版本未知」）。
func (n Number) IsZero() bool { return n == Number{} }

// Compare 比较两个版本：返回 -1 / 0 / 1。
//
// 逐段比较、缺失补 0，因此 1.2 == 1.2.0，1.2.0.1 > 1.2。
func (n Number) Compare(o Number) int {
	for i := 0; i < parts; i++ {
		switch {
		case n.parts[i] < o.parts[i]:
			return -1
		case n.parts[i] > o.parts[i]:
			return 1
		}
	}
	return 0
}

// Newer 判断 n 是否严格高于 o。相等不算「更新」—— 否则同版本机器会互相
// 认为对方更新，形成升级死循环。
func (n Number) Newer(o Number) bool { return n.Compare(o) > 0 }

// String 还原成点分形式（用于日志与界面，规范化为四段中的非零前缀）。
func (n Number) String() string {
	last := parts - 1
	for last > 0 && n.parts[last] == 0 {
		last--
	}
	var b strings.Builder
	for i := 0; i <= last; i++ {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.FormatUint(n.parts[i], 10))
	}
	return b.String()
}

// Compare 是字符串版本比较的便捷函数：任一方解析失败按 0.0.0 处理。
func Compare(a, b string) int { return ParseLenient(a).Compare(ParseLenient(b)) }

// IsUpgrade 判断 candidate 是否比 current 严格更新。
//
// 两边都必须是可解析、非零的版本，否则一律返回 false：
//   - candidate 不可解析（对端是开发版 / 报文被改坏）→ No；
//   - current 不可解析（本机是 0.0.0 的开发构建）→ 也 No。
//
// 第二条容易被漏掉，后果却是最尴尬的那种：开发机把自己当成「0.0.0 的旧版」
// 去升级成生产版本，下一次启动它就变成了生产环境里的一个「正常节点」。
func IsUpgrade(candidate, current string) bool {
	c := ParseLenient(candidate)
	cur := ParseLenient(current)
	if c.IsZero() || cur.IsZero() {
		return false
	}
	return c.Newer(cur)
}
