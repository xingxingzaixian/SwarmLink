//go:build linux

package notify

// bubbleTitle 是气泡通知的应用名。
//
// 与 Windows 提示框不同：系统通知必须有个标题，而正文才是「谁发来的、
// 说了什么」。标题固定用产品名，避免把发送者名字放到两个位置（正文里已经有）。
const bubbleTitle = "蜂群"

// bubbleCommand 返回 Linux 上真正执行的通知命令。
func bubbleCommand() string { return "notify-send" }

// bubbleArgs 组装 notify-send 的参数。
//
// 两个细节都不能省：
//   - `--app-name=` 让通知带上应用名（否则桌面上显示的是一串可执行文件名）；
//   - `--` 之后一律当位置参数。消息正文可能以 `-` 开头（例如「-- 我是分隔线」），
//     没有它就会被当成选项解析，通知会直接发不出去且报错信息很含糊。
//
// 正文里的 \n 由通知守护进程渲染成多行，与 Windows 提示框的排版一致。
func bubbleArgs(text string) []string {
	return []string{"--app-name=" + bubbleTitle, "--", bubbleTitle, text}
}
