# 聊天图片与表情（2026-09-22 设计）

## 1. 背景

v1.0 的聊天只能发纯文本。本次为聊天增加两类能力：

- **图片消息**：选本地图片 / 拖拽图片文件 / Ctrl+V 粘贴截图，直接在气泡里显示。
- **表情**：输入框内置 emoji 选择面板，把 Unicode emoji 插入到光标处。

## 2. 目标与非目标

**目标**

- 图片在气泡内直接显示，支持点开放大与「另存为」。
- 图片**绝不出现在传输列表**——传输列表只展示文件传输任务。
- 表情走普通文本，不引入新消息类型。
- 单聊与群聊都支持图片。

**非目标（YAGNI，明确不做）**

- 不做 caption 同体消息（一条消息同时含图与文）：`content` 会变成复合结构，去重、幂等、群发、重发都要重新推导。图片与文字各自成条。
- 不做图片消息的编辑、不做「从图片消息复制文字」。
- 不做大文件原图传输：需要原图时引导用户走既有的文件传输。
- 不做相邻「图 + 文」的额外视觉聚类：`ChatView` 现有的 `sameRun`（同发送者、同方向、5 分钟内）已经会把它们视为一组，不需要新逻辑。

## 3. 关键决策

### 3.1 图片内联进消息体（而非复用分块传输链路）

| 方案 | 结论 |
| --- | --- |
| A. 压缩后 base64 内联进 `content` | **采用**。零协议改动（`msg_type` 早已预留），完全不碰 transfer 模块，因此传输列表天然干净；消息自包含，重启后历史图片照常显示，备份一份 SQLite 即完整聊天记录。代价：图片本体进消息表；群聊 N 倍放大；无断点续传。 |
| B. 复用分块传输引擎 + 消息附件通道 | 否决。要给 `transfer_jobs` 加 `purpose` 并在 `List()` 里过滤（否则就会污染传输列表）、新增表与事件、接收方落盘、还要给 WebView 补读图片字节的接口。工作量约为 A 的 3–4 倍，而聊天图片 99% 是截图或已压缩的图。 |

### 3.2 图片与文字永远是两条消息

- 输入框里的文字始终是独立的一条文本消息，由 Enter 发出；三个图片入口都是「选中即发」，各自成为独立的一条图片消息。不存在「图片待发 + 文字待发」的草稿态，无需草稿区。
- 顺序由 `sentAt` 决定：`frontend/src/stores/chat.ts:121` 已按 `sentAt` 排序，历史则把后端返回的「新→旧」反转。因此「先粘图再发文字」的顺序稳定是 图→文。即使文本帧先到达接收方，插入后也会按时间戳归位。
- 两条消息各自持有送达状态，图片发送失败不会吞掉用户刚打的文字。

### 3.3 表情走文本

emoji 是普通字符，插入 `textarea` 后仍走现有文本消息链路，`msg_type` 保持 `text`。不引第三方 emoji 库，内置常量表。

## 4. 数据格式

### 4.1 图片消息

`msg_type = 'image'` 时，`content` 是一个小 JSON（不是裸 base64，也不是 data URL）：

```json
{ "v": 1, "mime": "image/jpeg", "w": 1280, "h": 720, "b64": "/9j/4AAQ..." }
```

- `w` / `h` 用于前端先按原始宽高比占位，避免图片解码完成时消息列表跳动。
- `mime` 用于保留 PNG 透明通道与 GIF 动图。
- `v` 为格式版本，预留演进。
- `b64` 是压缩后图片字节的标准 base64（无 `data:` 前缀、无换行）。

存放位置：`internal/domain/message`（纯数据结构 + 编解码，无 I/O，符合红线 R1）。提供：

```go
type InlineImage struct {
    V    int    `json:"v"`
    MIME string `json:"mime"`
    W    int    `json:"w"`
    H    int    `json:"h"`
    B64  string `json:"b64"`
}

func EncodeInlineImage(img InlineImage) (string, error)   // → content
func DecodeInlineImage(content string) (InlineImage, error)
func (i InlineImage) Bytes() ([]byte, error)              // base64 解码 + 上限校验
```

`DB schema 无需迁移`：`messages.msg_type`（`DEFAULT 'text'`）与 `content TEXT` 已就位。

### 4.2 常量与上限

| 常量 | 位置 | 值 | 用途 |
| --- | --- | --- | --- |
| `MsgTypeImage` | `domain/message` | `"image"` | 消息类型 |
| `MaxInlineImageBytes` | `domain/message` | `512 << 10` | 接收侧解码后的字节上限（防滥用） |
| `DefaultMaxEdge` | `infra/imagecodec` | `1280` | 发送侧长边上限 |
| `DefaultMaxBytes` | `infra/imagecodec` | `300 << 10` | 发送侧压缩目标 |
| `MaxFrameSize` | `domain/protocol` | `16 << 20` | 既有硬上限，压缩后不会触及 |

## 5. 后端设计

### 5.1 新增 `internal/infra/imagecodec`

纯计算包（不读文件、不联网），输入输出都是 `[]byte`，因此可以脱离文件系统做单测。

```go
type Inline struct {
    MIME string // image/jpeg | image/png | image/gif
    W, H int
    Data []byte // 压缩后的原始字节
}

// Prepare 规范化一张图片：长边 > maxEdge 则等比缩小，JPEG 质量 85→60 逐级降，
// 直到 <= maxBytes。PNG（带 alpha）与 GIF 在【原尺寸已 <= maxBytes】时原样保留。
func Prepare(raw []byte, maxEdge, maxBytes int) (Inline, error)
```

错误：`ErrEmpty` / `ErrNotImage` / `ErrTooLarge`（均为可 `errors.Is` 判定的哨兵错误）。

处理规则：

1. **按魔数嗅探格式，不信扩展名**；解码器按格式**显式分发**（`jpeg.Decode` / `png.Decode` / `gif.Decode` / `webp.Decode` / `bmp.Decode`），不用依赖 `init()` 注册的 `image.Decode`。
   - 理由：通用解码函数一旦漏掉某个格式包的 import，就会静默退化成「不是可识别的图片格式」；而**只要测试文件恰好 import 了那个包，测试还会通过**（测试通过、线上失败）。显式分发把这类缺陷变成编译期错误。
   - 尺寸用对应格式的 `DecodeConfig` 只读文件头取得：原样保留的路径不该为了 W/H 把整张位图解码进内存。
2. 长边 > 1280 → 等比缩到 1280（`golang.org/x/image/draw` 的 CatmullRom）。
3. JPEG：质量 85 / 80 / 75 / 70 / 65 / 60 逐级编码，取第一个 ≤ 300KB 的结果。
4. 小图不放大：原尺寸已满足条件时优先原样输出。
5. **无损格式（PNG / GIF / WebP）只要字节数 ≤ 300KB 就原样保留，不额外要求尺寸 ≤ 1280px**：透明通道与动画一旦 JPEG 化就永久丢失，而尺寸过大只影响显示（显示尺寸由前端钳制）。超过 300KB 才转入 JPEG 重编码，此时 GIF 会在 UI 提示「动图过大，将按静态图发送」。
6. 最终仍 > 300KB → `ErrTooLarge`。
7. 支持的输入格式：JPEG / PNG / GIF / WebP / BMP（BMP 无值得保留的特性，一律转 JPEG）。前端的文件过滤器与拖拽分流与此集合保持一致。

### 5.2 协议：`GroupMsg` 补字段

`internal/domain/protocol/wire.go` 的 `GroupMsg` 增加 `msg_type` 与 `file_id`（单聊的 `Chat` 早已具备）。JSON 新增字段对旧版本是「忽略未知字段」，向后兼容，不需要 bump `Version`。

### 5.3 `ChatApp` / `GroupApp`

- 把现有 `SendMessage` 的构造逻辑抽成内部 `send(ctx, peerID, content string, msgType message.MsgType)`；`SendMessage` 与新增的 `SendImage` 都走它，避免两条路径的安全检查（空内容、上限）分叉。
- 新增：
  - `ChatApp.SendImage(ctx, peerID, content string) (message.Message, error)`
  - `GroupApp.SendGroupImage(ctx, groupID, content string) (message.Message, error)`
- `trySend` / `sendGroupMsgTo` 已透传 `MsgType`，`sendGroupMsgTo` 需补上 `MsgType` 字段。
- 接收侧（`HandleChat` / `HandleGroupMsg`）：当 `msg_type == 'image'` 时校验 `content` 能解析且解码后 ≤ `MaxInlineImageBytes`，`w/h` 钳制到 `[0, 20000]`。
  - **校验失败的处理**：丢弃该消息（不入库、不发事件），但**仍然回 ACK**。理由：不回 ACK 会让发送方按 outbox 无限重发同一条垃圾消息。
- 群聊发送侧维持既有语义：扇出给在线成员，离线成员不接收。

### 5.4 Wails 服务层

`ChatService` 新增：

```go
func (s *ChatService) SendImage(peerID, path string) (MessageDTO, error)          // 选文件 / 拖拽
func (s *ChatService) SendImageBytes(peerID, name string, data []byte) (MessageDTO, error) // 粘贴截图
func (s *ChatService) SaveImage(msgID, destPath string) error                     // 另存为
```

`GroupService` 新增同形的 `SendImage` / `SendImageBytes`。

- `SendImage` 复用现有的 `checkSendableFile(path)` 做路径的同步校验（路径打错立刻报错），再 `os.ReadFile` + `imagecodec.Prepare` + `message.EncodeInlineImage` + `ChatApp.SendImage`。
- `SendImageBytes` 直接吃字节（浏览器只能拿到 `Blob`，拿不到路径）。
- 错误需带上可操作的中文提示：「不是可识别的图片格式」「图片过大，请改用文件发送」「无法读取该图片」。
- `SaveImage` 从 store 读消息 → 解析 → 解码 → 写 `destPath`（写入前校验 `destPath` 非空、目录存在）。

改动导出方法后必须重新生成绑定：`wails3 generate bindings -f "-tags wails" -clean=true -ts -i .`（`frontend/bindings/` 已入库）。

## 6. 前端设计

### 6.1 表情

- 新增 `frontend/src/constants/emoji.ts`：约 8 组、每组 24 个，共约 200 个常用 emoji（纯常量，不引库）。
- 新增 `frontend/src/components/EmojiPicker.vue`：不吃焦点的 popover，点击表情 `emit('pick', ch)`，Esc / 点外部关闭。
- `ChatView.vue`：输入区工具栏加笑脸按钮；插入逻辑要处理「Vue `v-model` 更新后光标跳到末尾」的问题——插入前记录 `selectionStart/End`，写回 `draft` 后 `nextTick` 里 `setSelectionRange` 把光标放到插入内容之后，并恢复焦点。

### 6.2 图片的三个入口

| 入口 | 实现 |
| --- | --- |
| 选文件 | 工具栏回形针右侧新增图片按钮 → `pickFiles()`（对话框标题与过滤改为图片） → `SendImage(path)` |
| 拖拽 | `events/index.ts` 的 `handleFilesDropped` 按扩展名分流：图片走 `chat.sendImage`，其余照旧走 `transfers.sendFiles`。分流必须发生在 `peerId` 检查**之前** —— 群会话没有 `peerId`，先卡它会把群里的图片一并挡掉。文件分支遇到群会话时给出「文件传输暂不支持群聊」的明确提示。图片分支**不要**调用 `ui.focusTransfer()`（否则会无谓地切走右栏面板）。 |
| 粘贴 | `ChatView.vue` 的 `textarea` 监听 `paste`：若 `clipboardData.files` 含 `image/*` 则 `preventDefault()`，读 `arrayBuffer()` 后调 `SendImageBytes(name, bytes)` |

扩展名分流只是 UX 加速；**真实格式判定以后端魔数嗅探为准**。前端认为是图片但后端判定不是时，按错误提示处理，不静默降级为文件传输（避免语义混乱）。

### 6.3 气泡渲染

- `api/types.ts` 加 `ImageContent` 类型与 `parseImageContent(content): ImageContent | null`。
- `ChatBubble.vue` 增加分支：`msgType === 'image'` → 渲染 `<img>`（圆角、上限约 220×220、按 `w/h` 撑出占位比例、`object-fit: contain`），失败或解析失败时显示占位块「图片显示失败」；`v-else` 走现有文本分支（含 `white-space: pre-wrap`）。
- 图片气泡的 `padding` 收紧（图片自带留白），时间戳与送达状态仍走 `.meta`。
- 点击图片 emit `zoom` → `ChatView` 打开新组件 `ImageLightbox.vue`（复用现有 `Modal.vue` 的遮罩与 Esc 行为），展示全图并提供「另存为」（`Dialogs.SaveFile` 取路径 → `SaveImage(msgID, dest)`）。

### 6.4 store 与 API 层

- `stores/chat.ts` 新增 `sendImage(conv, path)` 与 `sendImageBytes(conv, name, bytes)`，内部沿用 `upsert` / `bump`，复用与 `send` 相同的事件回填路径（图片消息同样会等到 `chat:new-message` / `chat:delivered`）。
- `api/types.ts` 的 `SwarmApi` 接口、`api/bindings.ts` 的 `realApi` 映射、`api/mock.ts` 的降级实现同步补齐：mock 用 canvas 现场画一张小图，保证 `npm run dev` 在浏览器里也能完整走查图片气泡与粘贴。

### 6.5 传输列表

整条链路不触碰 `TransferService` / `transfer_jobs`，图片不会出现在传输列表——这是本次的硬约束，列入手工回归项。

## 7. 错误处理与边界

| 场景 | 行为 |
| --- | --- |
| 非图片格式（含改名的 `.txt`） | 后端嗅探拒绝，toast「不是可识别的图片格式」 |
| 压缩后仍 > 300KB | toast「图片过大，请改用文件发送」 |
| GIF 超限 | 提示将按静态图发送后继续 |
| 粘贴时剪贴板无图片 | 不拦截，走默认粘贴（文本） |
| 群聊 | 图片与单聊同链路（扇出），按钮对群聊同样可用 |
| 接收侧内容畸形/超上限 | 丢弃消息、不入库、不发事件，但仍回 ACK |
| 历史图片解析失败（脏数据） | 渲染占位块，不崩 |

## 8. 测试与验收

**Go 单测**

1. `internal/infra/imagecodec/imagecodec_test.go`
   - 3000×2000 JPEG → 长边 1280 且 ≤ 300KB，且 `W/H` 与解码后真实尺寸一致；
   - 800×600 小图不放大；
   - 带透明的小 PNG 保留 PNG；小 GIF 保留 GIF；
   - `.txt` 改名 `.png` → `ErrNotImage`；空文件 → `ErrEmpty`；超大图 → `ErrTooLarge`。
2. `internal/app/chat_app_test.go`（新建，风格对齐 `transfer_app_test.go`：stub repo + 真实 `eventbus` + 断言落库与事件）
   - `SendImage` 落库后 `msg_type == "image"` 且 `content` 可解析；
   - `HandleChat` 收到合法图片消息 → 落库 + 发 `chat:new-message`；
   - 收到畸形 / 超限 image `content` → 不落库、不发事件、**仍回 ACK**；
   - 群聊 `GroupMsg` 的 `msg_type` 端到端透传。

**前端**

- `npm run typecheck` + `npm run build`；
- 浏览器 mock 走查四件事：发图、Ctrl+V 粘贴、表情插入到光标处、重开会话后历史图片仍在。

**手工回归（本次最容易破的约束）**

- 发一张图后，右栏传输列表**不得出现任何新记录**。

## 9. 交付批次

1. **第一批（后端）**：`imagecodec` + 消息内联格式 + `ChatApp/GroupApp` 两条发送路径 + `GroupMsg` 补字段 + 接收校验 + 单测。产出：命令行级验证可发送图片消息。
2. **第二批（前端）**：三个图片入口 + 气泡渲染 + lightbox/另存为 + emoji 面板 + mock 降级。产出：`wails3 build -tags wails` 后可完整手工走查。
3. **第三批（打磨，可选）**：查看器手势/键盘导航、超大图提示文案细化。
