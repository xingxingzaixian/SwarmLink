<#
.SYNOPSIS
    打包 SwarmLink 桌面客户端（替代 Makefile，供没有 make 的 Windows 使用）。

.DESCRIPTION
    wails3 背后是内置的 go-task，而 go-task 会以【子进程】方式调用 go / npm。
    子进程继承的是本脚本所在会话的 PATH，不是你在某个终端里临时改过的那份。
    因此这里显式把工具链挂上 PATH，避免 "go": executable file not found。

    另一个坑更要命：桌面入口 main_wails.go 带 `//go:build wails`，
    而 build/*/Taskfile.yml 的生产 BUILD_FLAGS 只有 `-tags production`，
    wails3 CLI 也不会替你补这个 tag（它只在 -tags 非空时才设 EXTRA_TAGS）。
    少了它，产物是 2.4MB 的占位程序，却仍会被正常打包成安装包 ——
    所以这里强制导出 EXTRA_TAGS=wails，并在最后校验产物大小。

.PARAMETER Target
    目标平台：windows / linux / darwin。

.PARAMETER Action
    build   = 只出可执行文件
    package = 出安装包（需要的额外工具见下方备注）

.PARAMETER Arch
    目标架构：amd64 / arm64。

.EXAMPLE
    .\scripts\pack.ps1 windows build
    .\scripts\pack.ps1 windows package
    .\scripts\pack.ps1 linux   build      # 需要 Docker
    .\scripts\pack.ps1 darwin  build      # 需要 Docker，且产物未签名

.NOTES
    各平台的前置条件（缺哪个都会在对应步骤报错）：
      windows : 需要 NSIS 的 makensis，否则 package 会在 create:nsis:installer 失败。
                winget install NSIS.NSIS
      linux   : 需要 Docker Desktop。二进制可用 wails-cross 镜像交叉编译，
                但 AppImage / deb / rpm 的【打包】步骤依赖 Linux 工具链，本机做不完整。
      darwin  : 二进制可用 wails-cross 交叉编译，但：
                  - .app 合成用的是 mkdir/cp 等 POSIX 命令；
                  - .dmg 生成任务写了 platforms: [darwin]，Windows 上直接跳过；
                  - 无法 codesign，产物在 macOS 上会被 Gatekeeper 拦。
                可发布的 .dmg 只能在 macOS 上出。
#>
[CmdletBinding()]
param(
    [ValidateSet('windows', 'linux', 'darwin')]
    [string]$Target = 'windows',

    # 刻意排在 Arch 之前：这样最常见的调用就是 `pack.ps1 windows build`，
    # 不用写成 `pack.ps1 windows -Action build`。
    [ValidateSet('build', 'package')]
    [string]$Action = 'package',

    [ValidateSet('amd64', 'arm64')]
    [string]$Arch = 'amd64'
)

$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
try {
    # ------------------------------------------------------------------ 工具链
    # version-fox 把每个版本放在 cache/<sdk>/v-x.y.z/ 下，版本目录名不可预测，
    # 所以按「能找到的最新的一个」来选，而不是写死版本号。
    function Add-ToolchainToPath {
        $vf = Join-Path $env:USERPROFILE '.version-fox\cache'
        if (-not (Test-Path $vf)) { return }

        # 递归会撞上两类不该选的东西，必须显式排除：
        #
        #  1. current（junction）—— 它指向的是【上次被激活】的版本，不是最新的
        #  2. packages\pkg\mod\golang.org\toolchain@* —— GOTOOLCHAIN=auto 自动
        #     下载的临时工具链。它的路径排在当前版本目录之后且字典序更大
        #     （"...\golang-1.25.5\packages..." > "...\golang-1.25.5\bin..."），
        #     于是降序取第一个会正好选中它 —— 这就是那个经典报错的来源：
        #       compile: version "go1.25.5" does not match go tool version "go1.26.3"
        #     即 PATH 里是自动下载的 1.26.3，GOROOT 却还是 vfox 写死的 current。
        $goRoot = Join-Path $vf 'golang'
        $go = Get-ChildItem $goRoot -Recurse -Filter 'go.exe' -ErrorAction SilentlyContinue |
            Where-Object {
                $_.FullName -notlike "*\current\*" -and $_.FullName -notlike "*toolchain@*"
            } |
            Sort-Object FullName -Descending | Select-Object -First 1
        if ($go) {
            $env:Path = "$($go.DirectoryName);$env:Path"

            # GOROOT 必须跟着 PATH 里的这份 go.exe 走。
            #
            # 踩过的坑：vfox 把 GOROOT 写成 ...\golang\current，而 current 是个
            # junction，指向的是【上一个】被激活的版本。一旦 PATH 里的 go.exe
            # 来自别的版本目录（上面按版本号挑出来的那份），就会出现
            #   compile: version "go1.25.5" does not match go tool version "go1.26.3"
            # —— go 命令用 GOROOT 里的标准库包，两者版本不一致。
            # 显式覆盖，让「用哪个 go」这件事只由一个变量决定。
            $env:GOROOT = Split-Path -Parent $go.DirectoryName
        }

        # node 与 npm 必须来自【同一个】安装目录，否则 npm 会去找不匹配的 node。
        $node = Get-ChildItem (Join-Path $vf 'nodejs') -Recurse -Filter 'node.exe' -ErrorAction SilentlyContinue |
            Where-Object { $_.DirectoryName -match 'v-(\d+)' } |
            Sort-Object { [version]($_.FullName -replace '.*\\v-(\d+\.\d+\.\d+)\\.*', '$1') } -Descending |
            Select-Object -First 1
        if ($node) { $env:Path = "$($node.DirectoryName);$env:Path" }
    }
    Add-ToolchainToPath

    # go install 装的 wails3 落在 $(go env GOPATH)\bin，但机器上可能存在第二份
    # 旧副本排在 PATH 更前面把它挡掉（本机就是：C:\Users\DELL\go\bin\wails3.exe
    # 是 alpha.9，盖住了 F:\dev\go\bin\wails3.exe 的 beta.23）。
    # 把 GOPATH\bin 提到最前，确保用的是 go install 装的那一份。
    $gopath = (& go env GOPATH 2>$null | Out-String).Trim()
    if ($gopath) {
        $gopathBin = Join-Path $gopath 'bin'
        if (Test-Path $gopathBin) { $env:Path = "$gopathBin;$env:Path" }
    }

    foreach ($tool in 'go', 'npm', 'wails3') {
        if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
            throw "找不到 $tool。请确认已安装并在 PATH 中（go/npm 常由 version-fox 等版本管理器托管）。"
        }
    }

    # ---------------------------------------------------- CLI 版本必须与 go.mod 一致
    # Wails 的 CLI 和库是两份独立安装：库版本写在 go.mod，而 build/ 下的脚手架
    # 由某个具体版本的 CLI 生成。两者不匹配时，报错会出现在离原因很远的地方 ——
    # 例如 alpha.9 不认识脚手架的 -iconcomposerinput，表现为
    # "flag provided but not defined"，看起来像脚手架坏了。
    $want = $null
    foreach ($line in Get-Content (Join-Path $repoRoot 'go.mod')) {
        if ($line -match 'wailsapp/wails/v3\s+(v\S+)') { $want = $Matches[1]; break }
    }
    # `wails3 version` 把版本号写进 stderr。在 PowerShell 里直接 2>&1 会把它
    # 变成 ErrorRecord，于是两头不讨好：$ErrorActionPreference='Stop' 会直接抛，
    # 'SilentlyContinue' 又会把它丢掉（$raw 变成空串）。
    # 交给 cmd 在 OS 层合并 —— 回到 PowerShell 的就是普通文本，不经过流的机制。
    $raw = (cmd /c 'wails3 version 2>&1' | Out-String)

    # 取整段文本里第一个 vX.Y.Z 形态的串：合并后的输出可能带前后缀，
    # 按行首匹配不可靠。
    $have = 'unknown'
    if ($raw -match '\bv\d+\.\d+\.\d+[0-9A-Za-z.\-]*') { $have = $Matches[0] }

    if ($want -and $have -ne $want) {
        throw @"
wails3 CLI 版本不匹配。
  需要：$want  （来自 go.mod）
  当前：$have  （$((Get-Command wails3).Source)）

修复：
  go install github.com/wailsapp/wails/v3/cmd/wails3@$want
"@
    }
    Write-Host "    wails3 $have  ($((Get-Command wails3).Source))"

    # ------------------------------------------------------- 谁都不能忘的 build tag
    # 这是全脚本存在的首要理由，见文件头说明。
    $env:EXTRA_TAGS = 'wails'

    Write-Host "==> $Target/$Arch : $Action" -ForegroundColor Cyan
    Write-Host "    EXTRA_TAGS=$env:EXTRA_TAGS"

    if ($Target -in 'linux', 'darwin' -and -not (Get-Command docker -ErrorAction SilentlyContinue)) {
        Write-Warning "目标是 $Target，而交叉编译需要 Docker。先装 Docker Desktop，并执行一次：wails3 task setup:docker"
    }

    # NSIS 的出包装目录里挂着 project.nsi / wails_tools.nsh，所以 makensis 必须可用。
    # 难点在于：NSIS 的安装程序【不会】把自己写进 PATH，装好了也照样找不到，
    # 因此这里主动去几个标准安装位置兜一层。
    if ($Target -eq 'windows' -and $Action -eq 'package') {
        if (-not (Get-Command makensis -ErrorAction SilentlyContinue)) {
            foreach ($dir in @(
                ${env:ProgramFiles(x86)}, $env:ProgramFiles, "$env:LOCALAPPDATA\Programs"
            )) {
                if (-not $dir) { continue }
                $candidate = Join-Path $dir 'NSIS'
                if (Test-Path (Join-Path $candidate 'makensis.exe')) {
                    $env:Path = "$candidate;$env:Path"
                    Write-Host "    找到 NSIS：$candidate（已加入本次 PATH）" -ForegroundColor Yellow
                    break
                }
            }
        }

        if (-not (Get-Command makensis -ErrorAction SilentlyContinue)) {
            throw @"
没找到 NSIS（makensis）—— Windows 安装包必需的工具。

安装（任选其一）：
  winget install NSIS.NSIS
  https://nsis.sourceforge.io/Download

装完若仍报这个错，说明它没被加进 PATH。
把 NSIS 的安装目录发我，或先手动执行一次再跑本脚本：
  `$env:Path = "C:\Program Files (x86)\NSIS;`$env:Path"
"@
        }
        Write-Host "    makensis: $((Get-Command makensis).Source)"
    }

    # 直接调平台任务，绕开根任务按 {{.GOOS}} 分发的这一层 ——
    # 我们要的目标平台是显式参数，不该再依赖宿主机 OS 推断。
    #
    # 同理：wails3 会把大量日志写进 stderr，不能让它触发 Stop。
    # 成功与否一律以 $LASTEXITCODE 为准。
    $ErrorActionPreference = 'Continue'
    & wails3 task "${Target}:${Action}" "ARCH=$Arch"
    $code = $LASTEXITCODE
    $ErrorActionPreference = 'Stop'
    if ($code -ne 0) { throw "wails3 task ${Target}:${Action} 失败（exit $code）" }

    # ------------------------------------------------------------------ 产物校验
    # 「编出占位程序但构建成功」是这个项目最隐蔽的失败模式，宁可在这里挡住。
    $ext = if ($Target -eq 'windows') { '.exe' } else { '' }
    $bin = Join-Path $repoRoot "bin/SwarmLink$ext"
    if (Test-Path $bin) {
        $mb = [math]::Round((Get-Item $bin).Length / 1MB, 2)
        if ($mb -lt 5) {
            throw "产物只有 ${mb}MB，几乎肯定是占位程序（缺 wails tag）。检查 EXTRA_TAGS 是否被覆盖。"
        }
        Write-Host "==> 产物 bin/SwarmLink$ext = ${mb}MB" -ForegroundColor Green
    } else {
        Write-Warning "没有在 bin/ 找到 SwarmLink$ext（可能产物名或路径与预期不同，请检查上面的日志）"
    }

    Write-Host "==> 完成" -ForegroundColor Green
} finally {
    Pop-Location
}
