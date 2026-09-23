#Requires -Version 5.1
<#
  SwarmLink 开发模式启动器（Windows）

  用法：
    start_dev.bat                        用默认端口 9245 启动
    start_dev.bat -port 9250             参数原样透传给 wails3 dev
    set SWARMLINK_DEV_PORT=9250 然后启动
    set SWARMLINK_DEV_CHECK=1 然后启动   只做环境检查、不启动

  这个文件刻意只做启动前检查，不放任何构建逻辑：开发模式真正的构建命令在
  build/config.yml 的 dev_mode.executes 里，其中
  `wails3 build -tags wails DEV=true` 的 -tags 是必需的（见该文件注释）。

  ---------------------------------------------------------------------------
  为什么是 PowerShell 而不是纯 .bat：

  cmd.exe 按【控制台代码页】解码批处理文件。中文 Windows 的控制台默认 936(GBK)，
  于是 UTF-8 的中文注释被按 GBK 解码，字节边界错位会把一行从中间劈开，
  后半截被当成命令执行 —— 报错长得像
      'ev.bat' is not recognized
      '/config.yml' is not recognized
  这跟工具链完全无关，纯粹是脚本编码问题。把 chcp 65001 放在第一行【也不能修好】：
  cmd 会预读文件，读到缓冲区时用的还是旧代码页。所以 .bat 只留一个纯 ASCII 的包装，
  中文全部放这里 —— PowerShell 按 UTF-8 读自己的脚本，并按控制台代码页输出，
  两种情况都正确。

  本文件必须保存为 UTF-8 带 BOM：PowerShell 5.1 对无 BOM 文件会按 ANSI 解读，
  中文会变成乱码。
  ---------------------------------------------------------------------------
#>

$ErrorActionPreference = 'Continue'

function Write-Info([string]$text) { Write-Host $text }
function Write-Fail([string]$text) { Write-Host $text -ForegroundColor Red }

# ------------------------------------------------------------------ 环境
# 脚本可能被从任意目录调用（双击、拖到终端里），一律切到脚本所在目录。
Set-Location -LiteralPath $PSScriptRoot

$checkOnly = $env:SWARMLINK_DEV_CHECK -eq '1'
$port = if ($env:SWARMLINK_DEV_PORT) { $env:SWARMLINK_DEV_PORT } else { '9245' }

# ------------------------------------------------------------------ 版本基准
# Wails 的 CLI 和库是两份独立安装：库版本在 go.mod，CLI 由 go install 装进 GOBIN。
# 两者不一致时报错点离原因很远，所以这里当场对齐。
$want = ''
$goMod = Join-Path $PSScriptRoot 'go.mod'
if (Test-Path -LiteralPath $goMod) {
	$line = Select-String -LiteralPath $goMod -Pattern 'github\.com/wailsapp/wails/v3 (v\S+)' |
		Select-Object -First 1
	if ($line) { $want = $line.Matches[0].Groups[1].Value }
}

# ------------------------------------------------------------------ 必需命令
$missing = @()
foreach ($cmd in @('go', 'npm', 'wails3')) {
	if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) { $missing += $cmd }
}
if ($missing.Count -gt 0) {
	Write-Fail ''
	Write-Fail "[启动失败] 缺少必需命令：$($missing -join ', ')"
	Write-Host '  go      https://go.dev/dl/'
	Write-Host '  npm     https://nodejs.org/  Node 20 或更高，npm 随 Node 一起安装'
	if ($want) { Write-Host "  wails3  go install github.com/wailsapp/wails/v3/cmd/wails3@$want" }
	Read-Host '按回车键退出'
	exit 1
}

# ------------------------------------------------------------------ CLI 版本
# wails3 把版本号写在 stderr，所以必须把 stderr 合并进来再找，否则永远匹配不到。
if ($want) {
	$raw = (& wails3 version 2>&1 | Out-String)
	if ($raw -notmatch [regex]::Escape($want)) {
		Write-Fail ''
		Write-Fail '[启动失败] wails3 CLI 版本与 go.mod 不一致'
		Write-Host "  需要：$want   来自 go.mod"
		Write-Host '  当前：'
		Write-Host ($raw.Trim())
		Write-Host "  修复：go install github.com/wailsapp/wails/v3/cmd/wails3@$want"
		Read-Host '按回车键退出'
		exit 1
	}
}

# ------------------------------------------------------------------ 前端依赖
# 首次 clone 或换分支后 node_modules 可能不存在，先装上，省得第一把就失败在 vite 上。
$nodeModules = Join-Path $PSScriptRoot 'frontend/node_modules'
if (-not (Test-Path -LiteralPath $nodeModules)) {
	Write-Info '[准备] frontend\node_modules 不存在，先执行 npm install，首次可能要几分钟...'
	Push-Location (Join-Path $PSScriptRoot 'frontend')
	# npm 是 .cmd，PowerShell 调用它同样要经过 shell，这里显式取退出码。
	& npm install
	$npmRc = $LASTEXITCODE
	Pop-Location
	if ($npmRc -ne 0) {
		Write-Fail ''
		Write-Fail '[启动失败] frontend 目录下的 npm install 失败'
		Write-Host '  先手动进入 frontend 目录执行 npm install，看清具体报错再来。'
		Read-Host '按回车键退出'
		exit 1
	}
}

if ($checkOnly) {
	Write-Info '[完成] 环境检查通过，SWARMLINK_DEV_CHECK=1，未启动'
	exit 0
}

# ------------------------------------------------------------------ 启动
Write-Info "[启动] wails3 dev   端口 $port"
Write-Info '[说明] 改 *.go 会自动重建并重启；改前端由 Vite 热更新接管，不会触发 Go 重建'
Write-Info "[地址] http://localhost:$port"
Write-Host ''

# 不带参数时给一套默认值；带了参数就完全听调用方的，避免出现两份 -port。
if ($args.Count -gt 0) {
	& wails3 dev @args
} else {
	& wails3 dev -config '.\build\config.yml' -port $port
}

$rc = $LASTEXITCODE
Write-Host ''
Write-Info "[退出] wails3 dev 已结束，exit code = $rc"
# 双击运行的人关窗口就什么都看不到了，所以非零退出时停一下。
if ($rc -ne 0) { Read-Host '按回车键退出' }
exit $rc
