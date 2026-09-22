@echo off
rem ============================================================================
rem  SwarmLink 开发模式启动器（Windows）
rem
rem  用法：
rem    start_dev.bat                        用默认端口 9245 启动
rem    start_dev.bat -port 9250             参数原样透传给 wails3 dev
rem    set SWARMLINK_DEV_PORT=9250 然后启动
rem    set SWARMLINK_DEV_CHECK=1 然后启动   只做环境检查、不启动
rem
rem  这个文件刻意只做启动前检查，不放任何构建逻辑：开发模式真正的构建命令在
rem  build/config.yml 的 dev_mode.executes 里，其中
rem  `wails3 build -tags wails DEV=true` 的 -tags 是必需的（见该文件注释）。
rem
rem  编码：本文件是 UTF-8 无 BOM，靠下面的 chcp 65001 让中文正常显示。
rem  千万不要用记事本另存为“UTF-8 带 BOM”，BOM 会让第 1 行 @echo off 直接报错。
rem ============================================================================

chcp 65001 >nul
setlocal
cd /d "%~dp0"

set "PORT=9245"
if defined SWARMLINK_DEV_PORT set "PORT=%SWARMLINK_DEV_PORT%"

rem ---------------------------------------------------------------- 版本基准
rem Wails 的 CLI 和库是两份独立安装：库版本在 go.mod，CLI 由 go install 装进 GOBIN。
rem 两者不一致时报错点离原因很远，所以这里当场对齐。
set "WANT="
if exist "go.mod" for /f "tokens=1,2" %%a in ('findstr /c:"github.com/wailsapp/wails/v3 v" go.mod') do set "WANT=%%b"

rem ---------------------------------------------------------------- 必需命令
set "MISSING="
call :need go
call :need npm
call :need wails3
if not "%MISSING%"=="" goto :err_missing

rem ---------------------------------------------------------------- CLI 版本
rem wails3 把版本号写在 stderr，所以必须 2^>^&1 合并后再找，否则永远匹配不到。
if not defined WANT goto :skip_version
wails3 version 2>&1 | findstr /c:"%WANT%" >nul 2>&1
if errorlevel 1 goto :err_version
:skip_version

rem ---------------------------------------------------------------- 前端依赖
rem 首次 clone 或换分支后 node_modules 可能不存在，先装上，省得第一把就失败在 vite 上。
if exist "frontend\node_modules" goto :deps_ok
echo [准备] frontend\node_modules 不存在，先执行 npm install，首次可能要几分钟...
pushd frontend
rem npm 是 .cmd，必须用 call，否则控制权不会回到本脚本。
call npm install
set "NPMRC=%ERRORLEVEL%"
popd
if not "%NPMRC%"=="0" goto :err_npm
:deps_ok

rem ---------------------------------------------------------------- 只检查
if "%SWARMLINK_DEV_CHECK%"=="1" goto :check_only

rem ---------------------------------------------------------------- 启动
echo [启动] wails3 dev   端口 %PORT%
echo [说明] 改 *.go 会自动重建并重启；改前端由 Vite 热更新接管，不会触发 Go 重建
echo [地址] http://localhost:%PORT%
echo.

rem 不带参数时给一套默认值；带了参数就完全听调用方的，避免出现两份 -port。
if "%~1"=="" goto :launch_default
wails3 dev %*
goto :launched

:launch_default
wails3 dev -config .\build\config.yml -port %PORT%

:launched
set "RC=%ERRORLEVEL%"
echo.
echo [退出] wails3 dev 已结束，exit code = %RC%
rem 双击运行的人关窗口就什么都看不到了，所以非零退出时停一下。
if not "%RC%"=="0" pause
endlocal
exit /b %RC%

rem ================================================================ 错误出口
rem 下面这条路径是双平台共有的：双击运行的人看不到一闪而过的报错，所以 pause。

:err_missing
echo.
echo [启动失败] 缺少必需命令：%MISSING%
echo   go      https://go.dev/dl/
echo   npm     https://nodejs.org/  Node 20 或更高，npm 随 Node 一起安装
if defined WANT echo   wails3  go install github.com/wailsapp/wails/v3/cmd/wails3@%WANT%
goto :fail

:err_version
echo.
echo [启动失败] wails3 CLI 版本与 go.mod 不一致
echo   需要：%WANT%   来自 go.mod
echo   当前：
wails3 version 2>&1
echo   修复：go install github.com/wailsapp/wails/v3/cmd/wails3@%WANT%
goto :fail

:err_npm
echo.
echo [启动失败] frontend 目录下的 npm install 失败
echo   先手动进入 frontend 目录执行 npm install，看清具体报错再来。
goto :fail

:check_only
echo [完成] 环境检查通过，SWARMLINK_DEV_CHECK=1，未启动
endlocal
exit /b 0

:fail
echo.
pause
endlocal
exit /b 1

rem ---------------------------------------------------------------- 辅助例程
rem 在 PATH 里找命令；找不到就记进 MISSING。用子例程而不是 for 块，
rem 是为了不依赖 enabledelayedexpansion —— 少一个开关，少一类诡异问题。
:need
where %~1 >nul 2>&1
if errorlevel 1 goto :eof
rem 这两行不用担心 parse-time 展开：`if defined` 是运行时判断，
rem 所以第一行刚设过、第二行一定能看见。
if defined MISSING set "MISSING=%MISSING%, %~1"
if not defined MISSING set "MISSING=%~1"
goto :eof
