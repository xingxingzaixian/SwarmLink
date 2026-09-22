#!/usr/bin/env bash
#
# SwarmLink 开发模式启动器（macOS / Linux）
#
# 用法：
#   ./start_dev.sh                        用默认端口 9245 启动
#   ./start_dev.sh -port 9250             参数原样透传给 wails3 dev
#   SWARMLINK_DEV_PORT=9250 ./start_dev.sh
#   SWARMLINK_DEV_CHECK=1 ./start_dev.sh  只做环境检查、不启动（换机器或排障时用）
#
# 设计取舍：这里【只做启动前检查】，不放任何构建逻辑。
# 开发模式真正的构建命令在 build/config.yml 的 dev_mode.executes 里
# （其中 `wails3 build -tags wails DEV=true` 的 -tags 是必需的，见该文件注释）。
# 同一件事有两份定义，早晚会漂移成互相矛盾的真相。

set -euo pipefail

# 无论从哪个目录调用，都切回仓库根 —— 后面所有相对路径都以此为准。
cd "$(dirname "$0")"

PORT="${SWARMLINK_DEV_PORT:-9245}"

say() { printf '[%s] %s\n' "$1" "$2"; }
die() {
	printf '\n[启动失败] %s\n' "$1" >&2
	shift
	while [ $# -gt 0 ]; do
		printf '           %s\n' "$1" >&2
		shift
	done
	exit 1
}

# ------------------------------------------------------------------ 版本基准
# Wails 的 CLI 和库是两份独立安装：库版本写在 go.mod，CLI 由 go install 装进 GOBIN。
# 两者不一致时报错点离原因很远（旧 CLI 不认识脚手架的新 flag），
# 所以在这里当场对齐、当场报清楚。CI 和 scripts/pack.ps1 用的是同一条规矩。
want=""
if [ -f go.mod ]; then
	want=$(awk '$1 == "github.com/wailsapp/wails/v3" { print $2; exit }' go.mod)
fi
if [ -n "$want" ]; then
	install_hint="go install github.com/wailsapp/wails/v3/cmd/wails3@${want}"
else
	install_hint="go install github.com/wailsapp/wails/v3/cmd/wails3@<go.mod 里锁定的版本>"
fi

# ------------------------------------------------------------------ 必需命令
missing=""
for cmd in go npm wails3; do
	command -v "$cmd" >/dev/null 2>&1 || missing="$missing $cmd"
done
if [ -n "$missing" ]; then
	die "缺少必需命令：${missing# }" \
		"go      https://go.dev/dl/" \
		"npm     https://nodejs.org/  Node 20 或更高，npm 随 Node 一起安装" \
		"wails3  ${install_hint}"
fi

# ------------------------------------------------------------------ CLI 版本
# 注意 wails3 把版本号写在 stderr，所以必须 2>&1 合并，否则永远读到空串。
raw=$(wails3 version 2>&1 | tr -d '\r' || true)
have="unknown"
if [[ "$raw" =~ v[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.-]* ]]; then
	have="${BASH_REMATCH[0]}"
fi
if [ -n "$want" ] && [ "$have" != "$want" ]; then
	die "wails3 CLI 版本与 go.mod 不一致" \
		"需要：${want}  来自 go.mod" \
		"当前：${have}  来自 $(command -v wails3)" \
		"修复：${install_hint}"
fi

# ------------------------------------------------------------------ 平台依赖
# Linux 走的是 GTK4 + WebKitGTK 6.0（不是 GTK3！），缺了会在 go build 阶段才炸，
# 报错长得像 "Package gtk4 was not found in the pkg-config search path"。
if [ "$(uname -s)" = "Linux" ]; then
	if command -v pkg-config >/dev/null 2>&1; then
		if ! pkg-config --exists gtk4 webkitgtk-6.0 2>/dev/null; then
			die "缺少 GTK4 / WebKitGTK 6.0 开发包" \
				"Debian/Ubuntu: sudo apt-get install -y libgtk-4-dev libwebkitgtk-6.0-dev" \
				"其他发行版：   wails3 doctor 会按你的包管理器给出安装命令"
		fi
	else
		say "提醒" "没有 pkg-config，跳过 GTK4 检查；缺失时会在构建阶段才暴露"
	fi
fi

# ------------------------------------------------------------------ 前端依赖
# 首次 clone 或换分支后 node_modules 可能不存在，先装上，省得第一把就失败在 vite 上。
if [ ! -d frontend/node_modules ]; then
	say "准备" "frontend/node_modules 不存在，先执行 npm install（首次可能要几分钟）"
	(cd frontend && npm install)
fi

# ------------------------------------------------------------------ 只检查
if [ "${SWARMLINK_DEV_CHECK:-0}" = "1" ]; then
	say "完成" "环境检查通过（SWARMLINK_DEV_CHECK=1，未启动）"
	exit 0
fi

# ------------------------------------------------------------------ 启动
say "启动" "wails3 dev  端口 ${PORT}"
say "说明" "改 *.go 会自动重建并重启；改前端由 Vite 热更新接管，不会触发 Go 重建"
say "地址" "http://localhost:${PORT}"

# 不带参数时给一套默认值；带了参数就完全听调用方的，避免出现两份 -port。
if [ $# -eq 0 ]; then
	set -- -config ./build/config.yml -port "$PORT"
fi

exec wails3 dev "$@"
