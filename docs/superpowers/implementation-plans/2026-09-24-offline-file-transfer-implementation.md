# 离线文件传输功能实现计划

## 概述

实现离线文件传输功能：当接收方不在线时，文件发送请求被缓存到本地，等接收方上线后自动传输。如果文件已被删除，则记录发送失败并通知用户。

## 实现步骤总览

1. 扩展 message.go 添加 `MsgTypeFileFailed` 和 `FileFailedContent`
2. 扩展 eventbus.go 添加 `TopicTransferFileFailed` 和 `TransferFileFailed`
3. 修改 ChatApp 添加 `FileMessageHandler` 接口和实现
4. 修改 ChatApp.trySend 处理文件消息
5. 修改 ChatApp.Start 订阅文件失败事件
6. 修改 TransferApp 实现 `HandleFileMessage`
7. 修改 bootstrap.Start 注入依赖
8. 修改前端类型定义和渲染逻辑
9. 编写单元测试和集成测试

---

## 步骤 1: 扩展 message.go

### 文件路径
`internal/domain/message/message.go`

### 修改内容

#### 1.1 添加新的消息类型常量
在 `MsgType` 常量区域添加：
```go
const (
    MsgTypeText  MsgType = "text"
    MsgTypeImage MsgType = "image"
    MsgTypeFile  MsgType = "file"
    MsgTypeFileFailed MsgType = "file_failed"  // 新增
)
```

#### 1.2 添加文件消息内容结构体
在文件末尾添加（`SortInPlace` 函数之后）：
```go
// FileMessageContent 文件消息内容（用于离线传输）
type FileMessageContent struct {
    JobID    string `json:"job_id"`
    FileName string `json:"file_name"`
    FilePath string `json:"file_path"`
}

// FileFailedContent 文件传输失败内容
type FileFailedContent struct {
    JobID    string `json:"job_id"`
    FileName string `json:"file_name"`
    Reason   string `json:"reason"`
}
```

### 验证方法
- 运行 `go build ./internal/domain/message` 确保编译通过
- 检查新常量和结构体是否正确导出

### 依赖
- 无前置依赖

---

## 步骤 2: 扩展 eventbus.go

### 文件路径
`internal/infra/eventbus/eventbus.go`

### 修改内容

#### 2.1 添加新的主题常量
在 `Topic` 常量区域添加：
```go
const (
    TopicPeerDiscovered     = "peer.discovered"
    TopicPeerOnline         = "peer.online"
    TopicPeerOffline        = "peer.offline"
    TopicChatReceived       = "chat.received"
    TopicChatDelivered      = "chat.delivered"
    TopicGroupUpdated       = "group.updated"
    TopicTransferState      = "transfer.state"
    TopicTransferProgress   = "transfer.progress"
    TopicTransferDone       = "transfer.done"
    TopicTransferError      = "transfer.error"
    TopicTransferFileFailed = "transfer:file_failed"  // 新增
    TopicNetError           = "net.error"
    TopicConfigChanged      = "config.changed"
)
```

#### 2.2 添加事件结构体
在文件末尾添加：
```go
// TransferFileFailed 文件传输失败事件
type TransferFileFailed struct {
    JobID    string
    PeerID   string
    FileName string
    Reason   string
}
```

### 验证方法
- 运行 `go build ./internal/infra/eventbus` 确保编译通过
- 检查新常量和结构体是否正确导出

### 依赖
- 无前置依赖
- 可与步骤 1 并行执行

---

## 步骤 3: 修改 ChatApp 添加 FileMessageHandler

### 文件路径
`internal/app/chat_app.go`

### 修改内容

#### 3.1 添加 FileMessageHandler 接口定义
在文件开头的 import 之后、ChatApp 结构体之前添加：
```go
// FileMessageHandler 处理文件消息的传输请求
type FileMessageHandler interface {
    HandleFileMessage(peerID identity.NodeID, content message.FileMessageContent) error
}
```

#### 3.2 在 ChatApp 结构体中添加字段
在 ChatApp 结构体中添加：
```go
type ChatApp struct {
    self        identity.NodeID
    store       ports.ChatStore
    conns       ports.ConnManager
    dir         ports.PeerDirectory
    bus         ports.EventBus
    clk         ports.Clock
    lg          *slog.Logger
    fileHandler FileMessageHandler  // 新增

    BaseBackoff time.Duration
    MaxBackoff  time.Duration
}
```

#### 3.3 添加 SetFileHandler 方法
在 NewChatApp 函数之后添加：
```go
// SetFileHandler 设置文件消息处理器
func (a *ChatApp) SetFileHandler(handler FileMessageHandler) {
    a.fileHandler = handler
}
```

### 验证方法
- 运行 `go build ./internal/app` 确保编译通过
- 检查接口和字段是否正确添加

### 依赖
- 步骤 1 必须先完成（需要 message.FileMessageContent）

---

## 步骤 4: 修改 ChatApp.trySend 处理文件消息

### 文件路径
`internal/app/chat_app.go`

### 修改内容

#### 4.1 修改 trySend 函数
在 trySend 函数中，编码协议之前添加文件消息处理逻辑：
```go
func (a *ChatApp) trySend(ctx context.Context, peerID identity.NodeID, msg message.Message) error {
    // 如果是文件消息，转发给 TransferApp 处理
    if msg.MsgType == message.MsgTypeFile {
        if a.fileHandler != nil {
            var content message.FileMessageContent
            if err := json.Unmarshal([]byte(msg.Content), &content); err == nil {
                return a.fileHandler.HandleFileMessage(peerID, content)
            }
        }
        return fmt.Errorf("file message handler not available")
    }

    sess, err := a.session(ctx, peerID)
    if err != nil {
        return err
    }
    // ... 原有的协议编码和发送逻辑保持不变
}
```

#### 4.2 添加必要的 import
确保文件顶部有：
```go
import (
    "encoding/json"  // 新增
    // ... 其他现有 import
)
```

### 验证方法
- 运行 `go build ./internal/app` 确保编译通过
- 手动检查逻辑分支是否正确

### 依赖
- 步骤 1 必须先完成（需要 message.MsgTypeFile 和 FileMessageContent）
- 步骤 3 必须先完成（需要 FileMessageHandler 接口和字段）

---

## 步骤 5: 修改 ChatApp.Start 订阅文件失败事件

### 文件路径
`internal/app/chat_app.go`

### 修改内容

#### 5.1 在 Start 函数中添加订阅逻辑
在 Start 函数的 TopicPeerOnline 订阅之后添加：
```go
func (a *ChatApp) Start(ctx context.Context) {
    if a.bus == nil {
        return
    }

    // 订阅文件失败事件
    a.bus.Subscribe(eventbus.TopicTransferFileFailed, func(payload any) {
        ev, ok := payload.(eventbus.TransferFileFailed)
        if !ok {
            return
        }
        peerID, err := identity.ParseNodeID(ev.PeerID)
        if err != nil {
            return
        }

        // 发送文件失败消息
        failContent := message.FileFailedContent{
            JobID:    ev.JobID,
            FileName: ev.FileName,
            Reason:   ev.Reason,
        }
        contentBytes, _ := json.Marshal(failContent)
        msg := message.Message{
            MsgID:     message.NewID(),
            ConvID:    message.DirectConvID(a.self, peerID),
            SenderID:  a.self,
            Direction: message.DirectionOut,
            Content:   string(contentBytes),
            MsgType:   message.MsgTypeFileFailed,
            SentAt:    a.clk.Now(),
            State:     message.StateSent,
        }
        _ = a.store.AppendOutgoing(msg, a.clk.Now())
    })

    // 原有的 peer.online 订阅保持不变
    a.bus.Subscribe(eventbus.TopicPeerOnline, func(payload any) {
        // ... 现有代码
    })
}
```

### 验证方法
- 运行 `go build ./internal/app` 确保编译通过
- 检查订阅逻辑是否正确处理事件类型转换

### 依赖
- 步骤 1 必须先完成（需要 message.MsgTypeFileFailed 和 FileFailedContent）
- 步骤 2 必须先完成（需要 eventbus.TopicTransferFileFailed 和 TransferFileFailed）
- 步骤 3 必须先完成（需要在 ChatApp 结构体基础上修改）

---

## 步骤 6: 修改 TransferApp 实现 HandleFileMessage

### 文件路径
`internal/app/transfer_app.go`

### 修改内容

#### 6.1 添加 HandleFileMessage 方法
在文件末尾（hashFileChunks 函数之后）添加：
```go
// HandleFileMessage 处理来自 ChatApp 的文件消息传输请求
func (a *TransferApp) HandleFileMessage(peerID identity.NodeID, content message.FileMessageContent) error {
    // 检查文件是否存在
    if _, err := os.Stat(content.FilePath); os.IsNotExist(err) {
        // 发布文件失败事件
        if a.bus != nil {
            a.bus.Publish(eventbus.TopicTransferFileFailed, eventbus.TransferFileFailed{
                JobID:    content.JobID,
                PeerID:   peerID.String(),
                FileName: content.FileName,
                Reason:   "文件已被删除或移动",
            })
        }
        return fmt.Errorf("file not found: %s", content.FilePath)
    }

    // 文件存在，执行传输
    _, err := a.SendFileAs(context.Background(), peerID, content.FilePath, content.JobID)
    if err != nil {
        // 传输失败也发布事件
        if a.bus != nil {
            a.bus.Publish(eventbus.TopicTransferFileFailed, eventbus.TransferFileFailed{
                JobID:    content.JobID,
                PeerID:   peerID.String(),
                FileName: content.FileName,
                Reason:   err.Error(),
            })
        }
    }
    return err
}
```

#### 6.2 添加必要的 import
确保文件顶部有：
```go
import (
    "context"  // 确保已有
    "os"       // 确保已有
    // ... 其他现有 import
)
```

### 验证方法
- 运行 `go build ./internal/app` 确保编译通过
- 检查方法签名是否匹配 FileMessageHandler 接口

### 依赖
- 步骤 1 必须先完成（需要 message.FileMessageContent）
- 步骤 2 必须先完成（需要 eventbus.TopicTransferFileFailed 和 TransferFileFailed）
- 步骤 3 必须先完成（需要实现 FileMessageHandler 接口）

---

## 步骤 7: 修改 bootstrap.Start 注入依赖

### 文件路径
`internal/bootstrap/bootstrap.go`

### 修改内容

#### 7.1 在应用层初始化后注入依赖
在 `n.ChatApp`、`n.TransferApp` 初始化之后，`n.Router` 初始化之前添加：
```go
// ---------------------------------------------------------------- 应用层
n.ChatApp = app.NewChatApp(kp.NodeID(), n.Messages, conn, n.PeerDir, bus, clk, lg)
n.GroupApp = app.NewGroupApp(kp.NodeID(), kp, n.Groups, n.Messages, conn, n.PeerDir, bus, clk, lg)
n.TransferApp = app.NewTransferApp(kp.NodeID(), n.Transfers, sink, conn, n.PeerDir, bus, clk, lg)
n.TransferApp.MaxConcurrent = cfg.Transfer.MaxConcurrent
n.TransferApp.ChunkSize = cfg.Transfer.ChunkSize
n.TransferApp.WindowSize = cfg.Transfer.WindowSize
n.TransferApp.BitmapFlushInterval = cfg.Transfer.BitmapFlushInterval.Std()

// 注入文件消息处理器：ChatApp 通过 TransferApp 处理文件传输
n.ChatApp.SetFileHandler(n.TransferApp)

seedAddrs, seedErrs := cfg.SeedAddrs()
// ... 后续代码保持不变
```

### 验证方法
- 运行 `go build ./internal/bootstrap` 确保编译通过
- 运行完整项目 `go build ./...` 确保整体编译通过
- 检查依赖注入顺序是否正确

### 依赖
- 步骤 3 必须先完成（需要 SetFileHandler 方法）
- 步骤 6 必须先完成（需要 TransferApp 实现 HandleFileMessage）

---

## 步骤 8: 修改前端类型定义

### 文件路径
`frontend/src/api/types.ts`

### 修改内容

#### 8.1 扩展 Message 接口的 msgType
修改 Message 接口的 msgType 字段注释：
```typescript
export interface Message {
  msgId: string
  convId: string
  senderId: string
  direction: 'in' | 'out' | string
  content: string
  msgType: 'text' | 'image' | 'file' | 'file_failed' | string  // 添加 'file_failed'
  fileId?: string
  jobId?: string
  state: 'pending' | 'sent' | 'delivered' | 'failed' | string
  sentAt: number
  recvAt?: number
}
```

#### 8.2 添加 FileFailedContent 接口
在文件末尾添加：
```typescript
/** 文件传输失败内容 */
export interface FileFailedContent {
  jobId: string
  fileName: string
  reason: string
}
```

### 验证方法
- 运行前端构建检查 TypeScript 类型错误
- 检查类型定义是否与后端一致

### 依赖
- 步骤 1 必须先完成（需要对应后端类型）

---

## 步骤 9: 修改前端渲染逻辑

### 文件路径
`frontend/src/components/ChatBubble.vue`

### 修改内容

#### 9.1 添加文件失败消息的计算属性
在 `isFile` 计算属性之后添加：
```typescript
const isFileFailed = computed(() => props.message.msgType === 'file_failed')
```

#### 9.2 添加文件失败内容解析
在 `fileInfo` 计算属性之后添加：
```typescript
// 文件失败消息的内容解析
const fileFailedInfo = computed(() => {
  if (!isFileFailed.value) return null
  try {
    const parsed = JSON.parse(props.message.content)
    return {
      jobId: parsed.job_id || '',
      fileName: parsed.file_name || '未知文件',
      reason: parsed.reason || '传输失败'
    }
  } catch {
    return {
      jobId: '',
      fileName: '文件',
      reason: '传输失败'
    }
  }
})
```

#### 9.3 在模板中添加文件失败消息渲染
在 `template` 中的 `isFile` 模板块之后添加：
```vue
<template v-else-if="isFileFailed">
  <div class="file-failed-message">
    <Icon name="x-circle" :size="32" />
    <div class="file-failed-info">
      <div class="file-failed-name">{{ fileFailedInfo?.fileName || '文件' }}</div>
      <div class="file-failed-reason">{{ fileFailedInfo?.reason || '传输失败' }}</div>
    </div>
  </div>
</template>
```

#### 9.4 添加文件失败消息样式
在 `style` 部分添加：
```css
/* 文件失败消息样式 */
.file-failed-message {
  display: flex;
  align-items: center;
  gap: 12px;
}

.file-failed-info {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.file-failed-name {
  font-size: var(--fs-md);
  font-weight: 500;
}

.file-failed-reason {
  font-size: var(--fs-xs);
  color: var(--t3);
}

.row.out .file-failed-reason {
  color: rgba(255, 255, 255, 0.76);
}
```

### 验证方法
- 运行前端开发服务器检查渲染是否正常
- 手动测试文件失败消息的显示

### 依赖
- 步骤 8 必须先完成（需要类型定义）

---

## 实施差异记录（执行期核实，覆盖本计划中的对应描述）

**后来者请以本节为准。** 2026-09-28 排查「发送文件后聊天框没有文件信息」时，确认了本计划落到代码后的一处结构性缺陷并做了修正：

1. **`trySend` 不再拦截 file 消息（关键修正）**
   计划步骤 4 让 `ChatApp.trySend` 把 file 消息转给 `TransferApp.HandleFileMessage`。它与计划开头「文件消息走 ChatApp 的 outbox 重试机制」的目标直接矛盾：
   - 消息被提前 return，**从不经过 `sess.Send`**，对端的聊天框永远看不到文件卡片；
   - 它也因此永远拿不到 ACK，outbox 会一直重试，而**每次重试都再产生一条假的 `file_failed`**（content 里 FilePath 为空 → `os.Stat("")` → "文件已被删除或移动"），源源不断投递给对端 —— 这正是用户看到的「接收方聊天框堆满裸 JSON」。

   现已删除该分支：file 消息与文本、图片走同一条投递路径。文件是否还在，由 `TransferApp` 在真正传输时判断（失败会发 `transfer:error` 并在传输面板标红）。

2. **`FileMessageContent` 不再含 `FilePath`，并补上 json tag**
   本地路径对接收方毫无用处，且属于发送方隐私；带上去只是把「本地路径可被远端读到」写进协议。字段名统一为 snake_case（`job_id`/`file_name`），与 `protocol.Chat` 的 `msg_type`/`file_id` 一致。

3. **`FileFailedContent` 补上 json tag（snake_case）**
   原设计文档给出的前端接口是 camelCase（`jobId`/`fileName`/`reason`）。实现选择 snake_case：消息 content 属于「落库 + 上网」的数据，全仓库统一 snake_case，camelCase 只用于对外 DTO。前端解析同时兼容 camelCase 与 Go 字段名（历史数据已经在库里了）。

4. **删除 `TransferApp.HandleFileMessage`、`ChatApp.SetFileHandler` / `FileMessageHandler`、`bootstrap` 中的注入**
   失去调用者后它们就是死代码，且 `HandleFileMessage` 正是假失败的来源。

5. **`file_failed` 消息不再由后端产生**
   历史消息仍会被前端渲染成失败卡片（新增分支），但不会再有新条目。发送失败的用户反馈由传输面板承担（`transfer:error` 已有统一出口 + 前端提示）。

6. **前端两个入口收敛为一个实现**
   计划步骤 12 只覆盖了回形针入口；拖放入口（`frontend/src/events/index.ts`）落地时漏了「插文件消息」这一步，于是拖进去的文件只出现在传输列表里、聊天框静悄悄。现抽出 `sendFilesWithMessages(conv, paths)`（`frontend/src/composables/useFileSend.ts`），回形针与拖放共用。同时把 `sendPaths` 里「没有会话就静默 return」改成明确提示。

7. **顺带修复一条被打破的既有测试**
   `ae762d0` 把 `ChatApp.send` 的同步 `trySend` 改成 `go func()` 异步后，`TestDeliveredEventMayArriveBeforeSendReturns` 断言的同步时序不再成立，长期稳定失败。已改名为 `TestDeliveredEventArrivesRegardlessOfSendReturn`：改为「等待事件到达（`waitFor`）+ 加锁防数据竞争」，并保留「返回值永远是 pending 快照」的断言 —— 前端仍必须靠账本收敛两种顺序。

8. **遗留的既有类型错误（未修，非本次引入）**
   `npm run typecheck` 当前有 6 条错误，均来自此前的提交：`frontend/src/api/mock.ts` 缺 `autoStart*`（autoStart 功能未同步 mock），`ChatBubble.vue` 的 `transfers.jobs.value` 与 `import('../api')).then(...)`（`ae762d0` 引入）。它们不影响 `npm run build`（vite 不做类型检查），但应当单独处理。

---

## 步骤 10: 修改 ChatApp.SendFile 携带文件路径

### 文件路径
`internal/app/chat_app.go`

### 修改内容

#### 10.1 修改 SendFile 方法
需要修改 SendFile 方法以携带文件路径，以便离线传输时能找到文件：
```go
// SendFile 发送单聊文件消息。
//
// content 是 JSON: {"job_id":"xxx","file_name":"xxx","file_path":"xxx"}
// file_path 用于离线传输时重新定位文件
func (a *ChatApp) SendFile(ctx context.Context, peerID identity.NodeID, jobID string, fileName string, filePath string) (message.Message, error) {
    payload := fmt.Sprintf(`{"job_id":"%s","file_name":"%s","file_path":"%s"}`, jobID, fileName, filePath)
    return a.send(ctx, peerID, payload, message.MsgTypeFile)
}
```

#### 10.2 同步修改 FileMessageContent 结构体
在 `message.go` 中确保 FilePath 字段已添加（步骤 1 已包含）

### 验证方法
- 运行 `go build ./internal/app` 确保编译通过
- 检查调用方是否需要同步更新

### 依赖
- 步骤 1 必须先完成（需要 FileMessageContent 结构体）
- 需要同步修改 Wails binding 层的接口签名

---

## 步骤 11: 修改 Wails binding 层

### 文件路径
`internal/adapters/wails/services.go` (需要确认具体文件名)

### 修改内容

#### 11.1 修改 SendFileMessage 方法签名
添加 filePath 参数：
```go
func (s *ChatService) SendFileMessage(peerID string, jobID string, fileName string, filePath string) (message.Message, error) {
    pid, err := identity.ParseNodeID(peerID)
    if err != nil {
        return message.Message{}, err
    }
    return s.chat.SendFile(context.Background(), pid, jobID, fileName, filePath)
}
```

### 验证方法
- 运行 `wails build` 确保绑定层编译通过
- 检查前端调用是否需要更新

### 依赖
- 步骤 10 必须先完成

---

## 步骤 12: 前端调用层更新

### 文件路径
`frontend/src/api/bindings.ts` 或相关 API 调用文件

### 修改内容

#### 12.1 更新 sendFileMessage 调用
添加 filePath 参数传递：
```typescript
async sendFileMessage(peerId: string, jobId: string, fileName: string, filePath: string): Promise<Message> {
  return await window.api.main.SendFileMessage(peerId, jobId, fileName, filePath)
}
```

### 验证方法
- 运行前端构建检查 TypeScript 类型错误
- 测试文件发送功能是否正常

### 依赖
- 步骤 11 必须先完成

---

## 测试策略

### 单元测试

#### 12.1 测试 ChatApp.FileMessageHandler 调用
文件：`internal/app/chat_app_test.go`
- 测试文件消息是否正确转发给 handler
- 测试 handler 为 nil 时的错误处理
- 测试 JSON 解析失败时的错误处理

#### 12.2 测试 TransferApp.HandleFileMessage
文件：`internal/app/transfer_app_test.go`
- 测试文件存在时的正常传输
- 测试文件不存在时的事件发布
- 测试传输失败时的事件发布
- 测试文件权限不足、路径无效等边界情况

#### 12.3 测试事件总线
文件：`internal/infra/eventbus/eventbus_test.go`
- 测试 TopicTransferFileFailed 事件的发布和订阅
- 测试事件载荷的正确性

### 集成测试

#### 12.4 离线场景测试
- 场景：对方不在线时发送文件
- 预期：文件消息进入 outbox，对方上线后自动传输
- 验证：检查文件是否成功传输，消息状态是否更新

#### 12.5 文件删除场景测试
- 场景：发送离线文件后删除本地文件，对方上线
- 预期：发送文件失败消息，通知用户文件已被删除
- 验证：检查是否收到 file_failed 类型的消息

#### 12.6 边界情况测试
- 文件权限不足
- 文件是目录而非文件
- 路径包含特殊字符
- 文件路径超长

### 手动测试清单

- [ ] 对方在线时发送文件，验证正常传输
- [ ] 对方离线时发送文件，验证消息进入 outbox
- [ ] 对方上线后，验证文件自动传输
- [ ] 发送离线文件后删除文件，验证失败消息显示
- [ ] 验证文件失败消息在前端正确渲染
- [ ] 验证传输进度和状态更新
- [ ] 验证多条离线文件消息的批量处理

---

## 修改顺序和依赖关系图

```
步骤 1 (message.go) ───┐
                      ├──> 步骤 3 (ChatApp 接口) ───┐
步骤 2 (eventbus.go) ──┘                          │
                                                  ├──> 步骤 4 (trySend)
                                                   │
                                                   ├──> 步骤 5 (Start 订阅)
                                                   │
步骤 6 (TransferApp) <─────────────────────────────┘
     │
     └──> 步骤 7 (bootstrap 注入)
           │
           ├──> 步骤 8 (前端类型)
           │     │
           │     └──> 步骤 9 (前端渲染)
           │
           └──> 步骤 10 (SendFile 签名)
                 │
                 ├──> 步骤 11 (Wails binding)
                 │     │
                 │     └──> 步骤 12 (前端调用)
                 │
                 └──> 步骤 13 (测试)
```

### 关键路径
1 → 3 → 4 → 5 → 6 → 7 → 10 → 11 → 12 → 13

### 可并行执行的步骤
- 步骤 1 和 步骤 2 可以并行
- 步骤 8 和 步骤 9 可以与后端步骤并行（在类型定义稳定后）

---

## 风险点和注意事项

### 风险点
1. **文件路径安全性**：需要确保文件路径不会被恶意利用，建议在 HandleFileMessage 中添加路径验证
2. **内存占用**：大量离线文件消息可能导致 outbox 膨胀，需要考虑清理策略
3. **事件总线性能**：文件失败事件可能频繁触发，需要评估对事件总线的影响
4. **前后端类型同步**：前后端类型定义必须严格一致，否则会导致序列化/反序列化失败

### 注意事项
1. 修改 SendFile 方法签名会影响现有调用，需要全面检查所有调用点
2. 文件失败消息应该有明确的用户提示，避免用户困惑
3. 考虑添加配置选项让用户选择是否启用离线文件传输
4. 失败消息应该提供重试机制或删除选项

---

## 回滚计划

如果实现过程中出现严重问题，可以按以下顺序回滚：

1. 回滚步骤 12（前端调用层）
2. 回滚步骤 11（Wails binding）
3. 回滚步骤 10（SendFile 签名）
4. 回滚步骤 7（bootstrap 注入）
5. 回滚步骤 6（TransferApp）
6. 回滚步骤 5（ChatApp.Start）
7. 回滚步骤 4（ChatApp.trySend）
8. 回滚步骤 3（ChatApp 接口）
9. 回滚步骤 9（前端渲染）
10. 回滚步骤 8（前端类型）
11. 回滚步骤 2（eventbus）
12. 回滚步骤 1（message）

回滚时建议使用 git 的版本控制功能，每个步骤完成后创建一个 commit 便于回滚。
