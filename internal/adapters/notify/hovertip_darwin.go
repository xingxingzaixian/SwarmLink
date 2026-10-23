//go:build darwin

package notify

// bubbleTitle 是气泡通知的标题（应用名）。
//
// 与 Windows 提示框不同：系统通知必须有个标题，而正文才是「谁发来的、
// 说了什么」。标题固定用产品名，避免把发送者名字放到两个位置（正文里已经有）。
const bubbleTitle = "蜂群"

// bubbleCommand 返回 macOS 上真正执行的通知命令。
func bubbleCommand() string { return "osascript" }

// bubbleArgs 组装 osascript 的参数。
//
// 【关键】文案一律走 argv，【绝不】拼进脚本字符串：消息内容里只要出现一个
// 引号就能把 AppleScript 语法打断（内容来自对端，属于不可信输入），而 argv
// 是数据通道，转义问题由系统负责。
//
// on run argv 是 osascript 接收命令行参数的标准写法：
// `osascript -e 'on run argv' ... arg1 arg2`，arg1/arg2 即 item 1/2 of argv。
func bubbleArgs(text string) []string {
	return []string{
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		bubbleTitle, text,
	}
}
