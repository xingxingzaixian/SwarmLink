# 截屏（本地单帧 + 交互式选区标注）· 实施计划

**日期**：2026-10-20
**依据**：`docs/plans/2026-10-20-screenshot-design.md`
**提交节奏**：每个 Task 一组改动，自成回滚锚点。（本仓约定：不由工具执行 git 命令，提交由人来做。）
**无新增 ADR**：不新增协议帧、不改线上格式、不动接收侧 512KB 图片上限。

---

> **任务清单说明**：T1–T10 是「整屏截屏」阶段的任务，完成情况见文末
> 《实施记录》表（表里已标 ✅）；T11–T14 是 2026-10-20 追加的
> **交互式截屏**（覆盖层 + 选区 + 标注）阶段。

## T1 领域层：ports 接口

- [ ] `internal/domain/ports/ports.go`：
      - 新增 `ScreenImage` 值类型（`Width` / `Height` / `Data`，`Data` 是 RGBA8888）。
      - 新增 `ScreenCapturer` 接口（`Capture(ctx, excludeSelf bool) (ScreenImage, error)`）。
      - 注释写清两条契约：**平台中立**（不出现 HWND 之类平台概念）、
        **不留残余状态**（实现必须把系统级改动复原）。
      - 红线 R1 自检：本段只加纯类型与接口，不得 import `os` / `syscall` / `net`。

## T2 适配器：screencap（Windows 主实现）

- [ ] `internal/adapters/screencap/screencap.go`（新）：
      - `New()` 构造 `*Capturer`；实现 `ports.ScreenCapturer`。
      - 平台无关的部分放这里：模式常量（`keep_window` / `hide_window`）、
        非法模式的兜底、错误文案。
- [ ] `internal/adapters/screencap/screencap_windows.go`（新，`//go:build windows`）：
      - `GetCursorPos` → `MonitorFromPoint` → `GetMonitorInfoW` 取显示器矩形。
      - `GetDC(NULL)` / `CreateCompatibleDC` / `CreateCompatibleBitmap` / `BitBlt` /
        `GetDIBits` 取像素。
      - DPI：抓屏前 `SetThreadDpiAwarenessContext(-4)`，抓完恢复原值。
      - 排除：`EnumWindows` + `GetWindowThreadProcessId` 取本进程顶层窗口 →
        逐个 `SetWindowDisplayAffinity`（优先 `WDA_EXCLUDEFROMCAPTURE`，
        老系统降级 `WDA_MONITOR`）→ **`defer` 全部复位为 `WDA_NONE`**。
      - 资源释放：`ReleaseDC` / `DeleteDC` / `DeleteObject` 全部 `defer`。
      - 失败时错误信息带上 Win32 错误码（`GetLastError`）。
- [ ] 注意：R2 允许本包 import `internal/domain/ports`（不是适配器），不得 import
      任何其它 `internal/adapters/*`。

## T3 适配器：其余平台

- [ ] `internal/adapters/screencap/screencap_other.go`（新，`//go:build !windows && !darwin && !linux`）：
      返回明确的「当前平台不支持截屏」。
- [ ] `internal/adapters/screencap/screencap_darwin.go`（新，`//go:build darwin && cgo`）：
      `CGWindowListCopyWindowInfo` → 按 `kCGWindowOwnerPID` 过滤掉本进程窗口 →
      `CGWindowListCreateImage`；显示器几何用 `CGGetDisplaysWithPoint`。
- [ ] `internal/adapters/screencap/screencap_linux.go`（新，`//go:build linux`）：
      - 检测 `WAYLAND_DISPLAY`：存在则返回「Wayland 会话不支持截屏」。
      - X11 路线：`github.com/BurntSushi/xgb`（**纯 Go**），Composite 逐窗合成、
        按 `_NET_WM_PID` 跳过本进程窗口。
      - ⚠ 新增依赖需要 `go mod tidy` 且要求能访问模块源；若环境不可用，
        本 Task 降级为「返回明确错误」，并在 §7 已知限制里写明「Linux 暂不可用」。
- [ ] `internal/adapters/screencap/screencap_darwin.go` 的 `!cgo` 变体：
      `//go:build darwin && !cgo` → 明确错误。

## T4 配置项

- [ ] `internal/adapters/config/config.go`：
      - 新增 `Screenshot` 段：`Enabled bool`（`toml:"enabled"`）、
        `LastMode string`（`toml:"last_mode"`）。
      - `Default()` 里 `Enabled = true`、`LastMode = "keep_window"`。
      - **不要**用 `if !c.Screenshot.Enabled { ... }` 回填（`false` 是合法选择）。
      - `Validate()` 若有分段校验，确认新段不被误拒。

## T5 服务层：Wails ScreenshotService

- [ ] `internal/adapters/wails/dto.go`：新增 `ScreenshotDTO`
      （`mime` / `width` / `height` / `dataBase64`）。
- [ ] `internal/adapters/wails/services.go`：
      - `Deps` 新增 `Capturer ports.ScreenCapturer`。
      - `ServiceSet` 新增 `Screenshot *ScreenshotService`；`NewServices` 里构造。
      - `ScreenshotService.Capture(mode string) (ScreenshotDTO, error)`：
        1. `mode` 归一（空串 → 配置里的 `last_mode`；非法值 → `keep_window` + 记日志）；
        2. 总开关检查（`cfg.Screenshot.Enabled`）；
        3. 调 `Capturer.Capture(ctx, mode == hide_window)`；
        4. `jpeg.Encode(q=92)` → `imagecodec.Prepare(raw, 1920, 480<<10)`；
        5. 出 base64。
      - `ScreenshotService.RememberMode(mode string) error`：写盘 `cfg.Screenshot.LastMode`。
        **只写 last_mode，不碰 enabled**（避免与设置页的写者互相覆盖）。
      - `SettingsService.Get()` 补 `ScreenshotEnabled` / `ScreenshotLastMode` 两个字段；
        `SettingsService.Save()` **只**写 `ScreenshotEnabled`（last_mode 由专用接口写），
        并在注释里说明这个分工。
- [ ] `internal/adapters/wails/dto.go`：`SettingsDTO` 补 `screenshotEnabled` / `screenshotLastMode`。

## T6 组合根与桌面外壳

- [ ] `main_wails.go`：构造 `screencap.New()` → 放进 `wails.Deps.Capturer`
      （**实际落在组合根 `main_wails.go`，不是 `internal/bootstrap`** ——
      桌面壳层的窗口能力只有 `main_wails.go` 拿得到，装配分两处反而更难跟）。
- [ ] `main_wails.go`：在服务的注册列表里加入 `services.Screenshot`。
- [ ] `main_wails.go`：**不需要**注入窗口句柄（接口是 `excludeSelf bool`）。

## T7 前端：绑定与 API 层

- [ ] 重新生成绑定（在仓库根目录）：
      `wails3 generate bindings -f "-tags wails" -clean=true -ts -i .`
      生成后 `git status --short -- frontend/bindings` 核对只出现预期文件
      （应新增 `screenshotservice.ts`）。
- [ ] `frontend/src/api/types.ts`：
      - `Settings` 补 `screenshotEnabled: boolean` / `screenshotLastMode: string`。
      - `SwarmApi` 补 `captureScreenshot(mode: string): Promise<Screenshot>`、
        `rememberScreenshotMode(mode: string): Promise<void>`。
      - 新增 `Screenshot` 接口（`mime` / `width` / `height` / `dataBase64`）。
- [ ] `frontend/src/api/bindings.ts`：`realApi` 补两个方法的映射。
- [ ] `frontend/src/api/mock.ts`：补两个方法的假实现（浏览器预览用）。

## T8 前端：聊天界面

- [ ] `frontend/src/components/Icon.vue`：新增 `camera` 图标（24 网格、1.7 描边）。
- [ ] `frontend/src/views/ChatView.vue`：
      - 工具栏加「截屏」按钮组（主键 + 下拉箭头 + 菜单）。
      - 草稿状态 `shotDraft`；`.entry` 上方加草稿缩略图行（缩略图 + × + 重截）。
      - `captureShot(mode)`：调 `api.captureScreenshot(mode)` → 落草稿 → `ui.notify`；
        失败按设计文档 §6 的文案表提示。
      - 发送逻辑：草稿存在时先发图片再发文本；发送后清草稿。
      - 发送键可用条件放宽为「有文字 **或** 有草稿」。
      - 禁用条件：`!conv || groupDissolved || !settings.screenshotEnabled`。

## T9 前端：设置页

- [ ] `frontend/src/views/SettingsView.vue`：新增「截屏」分类（或并入「通用」）：
      「允许截屏」开关（绑定 `settings.screenshotEnabled`）。
      关闭时聊天页的截屏按钮同步禁用。

## T10 文档回写

- [ ] `docs/plans/2026-10-14-PRD差距与待开发清单.md`：
      - §6「明确不做」里「远程控制 / 屏幕共享」一行补一句：
        本地单帧截屏**不等于**屏幕共享，已于 2026-10-20 独立实现（见本文档）。
      - §4 决策表可加一行说明「本地截屏 vs 远程控制的边界」。
- [ ] `README.md`：功能清单补「截屏（本地单帧，可选隐藏本窗口）」。

## T11 交互式截图：领域层、配置与抓屏接口

> 追加于 2026-10-20：初版只做了「一键整屏抓屏」，被指出「太简单」——
> 主流软件都是鼠标拖拽框选 + 简单标注。从 T11 起补上交互式形态。

- [ ] `internal/domain/ports/ports.go`：
      - 新增 `ScreenshotModeInteractive = "interactive"`；
        `NormalizeScreenshotMode` 改为三值归一，兜底 `interactive`。
      - 新增 `ScreenshotExcludesSelf(mode) bool` —— 统一回答「这个模式要不要
        排除本应用窗口」。
      - 新增 `Display` 值类型：**DIP 坐标**（虚拟桌面布局）+ **物理像素坐标**
        （抓屏与摆窗用）+ `ScaleFactor`（三套坐标换算的桥梁）。
      - `ScreenCapturer` 接口补 `Displays() ([]Display, error)` 与
        `CaptureDisplay(ctx, index, excludeSelf) (ScreenImage, error)`
        —— 前者给覆盖层摆窗，后者按**序号**抓（macOS 的 CoreGraphics
        只能按 display ID 抓整块屏，按序号抓才不会让 macOS 变成特例）。
- [ ] `internal/adapters/config/config.go`：`last_mode` 默认值改为 `interactive`；
      校验错误信息改为 `want interactive|keep_window|hide_window`。
- [ ] `internal/adapters/screencap/*`：各平台实现 `Displays()` 与
      `CaptureDisplay()`；Windows 的 `CreateCompatibleBitmap + GetDIBits`
      换成 `CreateDIBSection`（`GetDIBits` 在本机失败且 `GetLastError` 返回 0，
      极难定位）。

## T12 交互式截图：覆盖层宿主与服务层

- [ ] `internal/adapters/wails/overlay_wails.go`：`OverlayHost` 的 Wails 实现：
      `OpenOverlay` / `ShowOverlay` / `FocusOverlay` / `CloseOverlays` / `Emit`。
      窗口选项：无边框、置顶、不可缩放、透明背景、**创建时隐藏**
      （等前端画好第一帧再显示，否则会闪白屏）。
- [ ] `internal/adapters/wails/overlay_windows.go`：Windows 用
      `w32.SetWindowPos` + **物理像素**摆窗（修混合缩放多屏错位）；
      `overlay_other.go` 为其它平台的空实现（用 DIP 坐标）。
- [ ] `internal/adapters/wails/screenshot_overlay.go`：会话状态（每块屏一张底图）
      + 六个服务方法：`BeginCapture` / `GetOverlayImage` / `OverlayReady` /
      `FocusOverlay` / `CommitOverlay` / `CancelOverlay`。
      结果通过事件 `screenshot:ready` 回主窗口。
- [ ] `internal/adapters/wails/screenshot_service.go`：`Capture("interactive")`
      明确报错 —— 交互式不走整屏抓取这条路，别让它返回一张整屏图。
- [ ] `main_wails.go`：创建 `overlayHost` → 注入 `Deps.Overlay`；
      `*application.App` 在服务注册之后才存在 → 用 `SetApp` **延迟绑定**。

## T13 交互式截图：前端

- [ ] `frontend/src/main.ts`：按 URL query 分流 ——
      `?mode=screenshot&display=N` 渲染 `ScreenshotOverlay.vue`，否则渲染主应用。
      （同一份产物跑两个角色，避免两套构建。）
- [ ] `frontend/src/views/ScreenshotOverlay.vue`（新建，核心）：
      - 双 Canvas（底图层 + 标注层）+ DOM 选区框 + 底部工具栏。
      - 坐标：画布位图 = **物理像素**；CSS 布局 = **DIP**；
        鼠标坐标 ÷ `scaleFactor` 换算到物理像素。
      - 遮罩用**四个矩形**围出选区（不用 `destination-out` 挖洞 ——
        挖洞会让选区露出**实时桌面**而不是抓屏那一刻的画面）。
      - 五种标注：矩形 / 箭头 / 画笔 / 文字 / 马赛克；`Ctrl+Z` 撤销。
      - 快捷键：`Esc` 取消、`Enter` 完成。
      - 导出：quality × scale 逐档组合降到 ≤460KB（协议上限 512KB）。
      - **第一次鼠标按下**时调 `focusOverlay(displayIndex)` 抢键盘焦点
        （多屏时焦点可能在另一块屏上，不抢回来的话 Esc / Enter 会打空）。
- [ ] `frontend/src/api/{types,bindings,mock}.ts`：新增 6 个方法
      （`beginCapture` / `getOverlayImage` / `overlayReady` / `focusOverlay` /
      `commitOverlay` / `cancelOverlay`）与 `OverlaySession` / `OverlayDisplay` /
      `OverlayImage` 类型；`bindings.ts` 里把可空数组收口成 `?? []`。
- [ ] `frontend/src/events/index.ts`：订阅 `screenshot:ready` → 写入草稿 store。
- [ ] `frontend/src/stores/screenshot.ts`：草稿从 ChatView 的局部状态**移入 store**
      （跨窗口回传需要一个跨组件的落点）。
- [ ] `frontend/src/views/ChatView.vue`：下拉菜单三项；`interactive` 走
      `beginCapture()`（结果走事件），其余两项走 `captureScreenshot(mode)`；
      发送成功 / 切会话 / 丢弃都统一 `clearDraft()`。
- [ ] `frontend/src/components/Icon.vue`：新增 `cursor` / `square` /
      `arrowUpRight` / `text` / `grid` / `rotateCcw` / `maximize` 图标。
- [ ] `frontend/src/views/SettingsView.vue`：说明文字改成介绍交互式截屏
      （拖拽选区 + 画框 / 箭头 / 文字 / 马赛克）与两种整屏截屏。

## T14 交互式截图：文档回写

- [ ] 设计文档补 §3.4（交互式截图）、§4.5（覆盖层时序与接口）、
      模式表补 `interactive`、配置默认改 `interactive`、
      §7 已知限制补 4 条、§8 验收补 §8.5 交互式专项。
- [ ] 本文档补 T11–T14 任务与实施记录。
- [ ] `README.md` 功能表改成「交互式选区 + 标注」。
- [ ] `docs/plans/2026-10-14-PRD差距与待开发清单.md` §6 边界补记微调
      （交互式同样是「本机用户自己点」，不构成对「远程控制 / 屏幕共享」的破例）。

## 回归

- [ ] `go build ./...`
- [ ] `go vet ./internal/...`
- [ ] `go test ./... -count=1`
- [ ] `go test -tags wails ./... -count=1`（含 archtest 三条红线 + gofmt 闸门）
- [ ] `cd frontend && npm run build`（类型检查 + 打包）
- [ ] 真机走查：按设计文档 §8.1 逐条；§8.2 的三条阻塞性验证必须通过。

---

## 实施记录（2026-10-20）

### 状态

| Task | 状态 | 落地位置 |
| --- | --- | --- |
| T1 ports 接口 | ✅ | `internal/domain/ports/ports.go`：`ScreenImage` / `ScreenCapturer` / 模式常量与归一函数 |
| T2 screencap（Windows） | ✅ | `internal/adapters/screencap/screencap_windows.go`（手写 Win32：DPI 感知 + DDA 排除） |
| T3 其余平台 | ✅ | `screencap_darwin.go`（cgo + CoreGraphics）/ `screencap_darwin_nocgo.go` / `screencap_linux.go`（xgb + Composite）/ `screencap_other.go` |
| T4 配置项 | ✅ | `internal/adapters/config/config.go`：`[screenshot] enabled / last_mode` |
| T5 Wails 服务 | ✅ | `internal/adapters/wails/screenshot_service.go` + `dto.go` + `services.go` |
| T6 组合根 | ✅ | `main_wails.go`：`Deps.Capturer = screencap.New()` + 注册 `services.Screenshot` |
| T7 前端绑定与 API | ✅ | 绑定已重新生成（新增 `screenshotservice.ts`）；`api/types.ts` / `api/bindings.ts` / `api/mock.ts` |
| T8 聊天界面 | ✅ | `views/ChatView.vue`：工具栏按钮组 + 草稿缩略图行 + 发送逻辑 |
| T9 设置页 | ✅ | `views/SettingsView.vue`：新增「截屏」分类 + 「允许截屏」开关 |
| T10 文档回写 | ✅ | 差距清单 §6 边界补记 + README 功能表 |
| T11 领域层 / 配置 / 抓屏接口 | ✅ | `ports.go`：`ScreenshotModeInteractive` / `Display` / `Displays()` / `CaptureDisplay()`；`config.go` 默认 `interactive`；`screencap/*` 改用 `CreateDIBSection` |
| T12 覆盖层宿主与服务层 | ✅ | `overlay_wails.go` / `overlay_windows.go` / `overlay_other.go` / `screenshot_overlay.go`；`main_wails.go` 延迟绑定 `SetApp` |
| T13 前端交互式 | ✅ | `views/ScreenshotOverlay.vue`（核心）+ `main.ts` 分流 + `api/*` + `events/index.ts` + `stores/screenshot.ts` + `ChatView.vue` + `Icon.vue` + `SettingsView.vue` |
| T14 文档回写（交互式） | ✅ | 设计文档 §3.4 / §4.5 / §7 / §8.5 + README + 差距清单 |

### 与原计划的偏差（都记在这儿，别让下一个人重新踩一遍）

1. **新增了「已压缩图片」通道**（`ScreenshotService.SendDraft`）——
   原计划里草稿直接走 `chat.sendImageBytes`，但那条路会用通用上限（1280px / 300KB）
   把 1920px 的截图**再压一次**。4K 屏截图会糊掉一大截，而这正是截屏最不该损失的。
   详见设计文档 §4.6。
2. **模式常量放在 domain 层**（`ports.ScreenshotModeKeepWindow` 等）而不是
   `screencap` 适配器包里 —— 服务层也要用它们，而红线 R2 禁止适配器互相 import。
3. **接口用 `excludeSelf bool` 而不是窗口句柄** —— 句柄是平台专有的，塞进 domain 接口
   会让 domain 层认识 Win32。各平台自己枚举本进程的窗口即可（顺带把托盘气泡也排除了）。
4. **新增依赖 `github.com/BurntSushi/xgb`**（纯 Go 的 X11 协议库，非 cgo）。
5. **新增 `frontend/src/stores/screenshot.ts`** —— 「允不允许截屏」由设置页写、
   「上次用哪种模式」由聊天页写，两个写者需要一个共同的落点。
6. **草稿放在 ChatView 的局部状态里**（不进 store）—— 草稿是「给某一个会话看的」，
   切换会话时应当丢弃；放进全局 store 会有误发到别的会话的风险。
   **【交互式改造后推翻】**：跨窗口回传（覆盖层 → 主窗口）需要一个跨组件的
   落点，草稿因此**移入 `stores/screenshot.ts`**。误发风险改由「切换会话时
   `clearDraft()`」来防。
7. **Wails v3 beta 的窗口 API 与直觉不同** —— 没有
   `app.NewWebviewWindowWithOptions` / `app.GetWebviewWindow`；
   正确写法是 `app.Window.NewWithOptions(...)` / `app.Window.GetByName(name)`
   / `app.Window.GetAll()`。
8. **`*application.App` 在服务注册之后才存在** —— 覆盖层宿主需要在运行时
   开窗，因此 `OverlayHost` 用 `SetApp` **延迟绑定**，而不是在构造时注入。
9. **覆盖层窗口用物理像素摆窗（Windows）** —— DIP 坐标在混合缩放多屏上
   换算回物理像素后与显示器真实边界对不上，改用 `w32.SetWindowPos` +
   物理像素坐标。
10. **遮罩不能挖洞** —— `globalCompositeOperation = 'destination-out'` 会让
    选区露出**实时桌面**而不是抓屏那一刻的画面；改用四个矩形围出选区。
11. **多屏的键盘焦点会打空 Esc / Enter** —— 焦点只在其中一个覆盖层窗口上；
    前端在第一次鼠标按下时调 `focusOverlay(displayIndex)` 抢回来。

### 自动化验证结果

- `gofmt -l .` —— 无输出
- `go build ./...`、`go build -tags wails ./...` —— 通过
- `GOOS=linux go build ./internal/adapters/screencap/` —— 通过（X11 分支的语法与类型）
- `go vet ./...` —— 通过
- `go test ./... -count=1` —— **archtest 三条红线、gofmt 闸门与其余全部包通过**；
  `test/e2e` 的 `TestFolderTransferRestoresTree` 见下面的 ⚠ 条
  （既有偶发，非本次回归）
- `npm run typecheck`、`npm run build` —— 通过
- ⚠ **`test/e2e` 的 `TestFolderTransferRestoresTree` 在全仓并发运行时偶发失败**
  （报 `timeout waiting for 接收端还原出完整目录树`，10s 超时，下载目录为空）。
  实测证据（2026-10-21）：
  - 全仓 `go test ./... -count=1`：3 次里失败 2 次（另 1 次通过）；
  - 单跑该用例：3 次里失败 1 次，随后 2 次通过；
  - **整包 `go test ./test/e2e/ -count=1`：连跑 2 次全过**（3.6~4.1s）。
  这是本仓**已记录过的既有现象**：`docs/plans/2026-10-10-identity-vault-and-peer-removal.md:1678`
  写明「`test/e2e` 在全仓并发执行时偶发失败（单包跑稳定通过）」，当时的受害者是
  `TestFileTransferResumesAfterInterruption` 与 `TestThreeNodesDiscoveryAndChat`。
  本次改动**没有触碰传输路径**（`statsOf` 只是把 `GroupService` 的方法提成包级函数，
  逻辑一字未改；文件夹传输不走群投递统计），因此判定为**同类时序偶发，不是回归**。
  但这确实是一条该修的测试稳定性问题，**建议单独立项**（与上面那条同类）。

### 留给人的部分（自动化做不了）

- **全部真机验证**（设计文档 §8.1 / §8.2 / §8.3）。
- **交互式截屏的全链路人工走查**（设计文档 §8.5）：覆盖层铺位、拖拽选区、
  五种标注、撤销、`Enter` / `Esc`、结果回主窗口草稿行。
- **多屏混合缩放**（150% + 100%）：覆盖层定位按物理像素做了修正，
  但开发机只有单屏 100%，**未验证**。
- **macOS 分支在 Windows 上无法编译验证**（darwin 目标需要 macOS SDK），
  它的正确性只有一次 macOS 上的编译 + 走查才能确认。
- **Linux X11 分支只做了交叉编译验证**，Composite 逐窗合成的 Z 序、裁剪与
  坐标换算的正确性需要 Linux 真机确认。

### 阻塞性未知项已解除

**DDA 对 GDI `BitBlt` 是否真的生效**（设计文档 §7.3）—— 已用一次性验证程序确认：
造一个 200×200 纯红窗口，分别以「不排除」「排除」抓屏统计红色像素：

```
画面尺寸：1920x1080（不排除）/ 1920x1080（排除）
红色像素数：不排除 = 30214，排除 = 590
结论：【DDA 对 BitBlt 生效】
```

即「隐藏本窗口」在 Windows 上走 DDA 这条路成立，不需要退回到
「真隐藏 + 等动画 + 恢复」的闪烁方案。验证程序是一次性的，已删除。
