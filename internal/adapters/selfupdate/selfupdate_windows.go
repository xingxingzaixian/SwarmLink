//go:build windows

// Package selfupdate 负责「把下载好的安装器跑起来并重启应用」这一段平台相关逻辑。
//
// 为什么必须有一个分离进程（而不是直接 exec 安装器再退出）：
// Windows 不允许覆盖正在运行的 exe，安装器在应用还没退干净时写
// 安装目录会失败。所以顺序必须是「先起一个与本次进程无关的等待者 →
// 应用退出 → 等待者延时后跑安装器 → 安装完再启动应用」。
//
// 为什么用 .bat 而不是给 cmd /c 传一长串命令：cmd.exe 的引号解析规则与
// Go 的 exec 参数转义规则叠加后极容易出错（路径里有空格或 & 就炸），
// 落一个临时 .bat、只把它的路径交给 cmd，是唯一不需要猜转义的地方。
package selfupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Launch 拉起「等待 → 静默安装 → 重启应用」的分离进程并立即返回。
//
// 调用方必须在返回后退出自己的进程，否则安装器会撞上被占用的 exe。
func Launch(installer string) error {
	if installer == "" {
		return fmt.Errorf("selfupdate: 安装包路径为空")
	}
	if _, err := os.Stat(installer); err != nil {
		return fmt.Errorf("selfupdate: 安装包不存在: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("selfupdate: 取自身路径失败: %w", err)
	}

	dir, err := os.MkdirTemp("", "swarmlink-update-*")
	if err != nil {
		return fmt.Errorf("selfupdate: 创建临时目录失败: %w", err)
	}
	bat := filepath.Join(dir, "apply.bat")

	// 脚本内容：
	//   timeout 2s —— 等本进程退出（SingleInstance 也会挡住过早的重启）
	//   start /wait installer /S —— 静默安装，等它真的结束
	//   start app —— 装完把应用拉起来（用户不需要知道刚才发生过什么）
	//
	// 用 /nobreak 防止用户按键提前跳过等待；>nul 屏蔽 timeout 的输出，
	// 否则分离进程会持有一个看不见的控制台窗口。
	script := fmt.Sprintf("@echo off\r\n"+
		"timeout /t 2 /nobreak >nul\r\n"+
		"start \"\" /wait \"%s\" /S\r\n"+
		"start \"\" \"%s\"\r\n", installer, exe)
	if err := os.WriteFile(bat, []byte(script), 0o600); err != nil {
		return fmt.Errorf("selfupdate: 写临时脚本失败: %w", err)
	}

	// CREATE_NO_WINDOW：不弹黑框；DETACHED_PROCESS 由 CREATE_NO_WINDOW 隐含，
	// 这里显式给出前者即可。
	const createNoWindow = 0x08000000
	cmd := exec.Command("cmd.exe", "/c", bat)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	// 不接管 stdout/stderr：父进程马上要退出，管道会变成孤儿句柄。
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("selfupdate: 启动更新进程失败: %w", err)
	}
	// 不 Wait：它要活过本进程。句柄由系统在它自己退出时回收。
	_ = cmd.Process.Release()

	// 给脚本一点时间真正跑起来。不这样做的话，父进程在 Start 之后
	// 立刻退出时，极端情况下 cmd 还没来得及被调度（表现为「点了更新没反应」）。
	time.Sleep(200 * time.Millisecond)
	return nil
}
