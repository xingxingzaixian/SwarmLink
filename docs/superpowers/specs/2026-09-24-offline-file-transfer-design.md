# 离线文件传输设计

## 目标

实现离线文件传输功能：当接收方不在线时，文件发送请求被缓存到本地，等接收方上线后自动传输。如果文件已被删除，则记录发送失败并通知用户。

## 背景

当前系统已有离线消息缓存机制（ChatApp 的 outbox），但文件传输需要双方同时在线建立持续连接。用户希望在接收方离线时也能发起文件传输，系统自动处理重试。

## 方案概述

采用混合方案（方案C）：文件消息走 ChatApp 的 outbox 重试机制，实际传输由 TransferApp 负责。

- 复用现有 outbox 重试机制，保持消息统一性
- 传输逻辑仍在 TransferApp，保持职责分离
- UI 层统一显示，用户体验一致

## 架构设计

### 数据结构

#### 扩展文件消息内容
```go
type FileMessageContent struct {
    JobID    string // 传输任务ID
    FileName string // 文件名
    FilePath string // 完整路径（发送方本地）
}
```

#### 新增消息类型
```go
const MsgTypeFileFailed = "file_failed"

type FileFailedContent struct {
    JobID    string // 失败的任务ID
    FileName string // 文件名
    Reason   string // 失败原因
}
```

### 组件协作

#### ChatApp 扩展
```go
// FileMessageHandler 处理文件消息的传输请求
type FileMessageHandler interface {
    HandleFileMessage(peerID NodeID, content FileMessageContent) error
}

type ChatApp struct {
    // ... 现有字段
    fileHandler FileMessageHandler
}

func (a *ChatApp) SetFileHandler(handler FileMessageHandler) {
    a.fileHandler = handler
}
```

#### TransferApp 实现
```go
func (a *TransferApp) HandleFileMessage(peerID identity.NodeID, content FileMessageContent) error {
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

### 事件总线

#### 新增事件类型
```go
const TopicTransferFileFailed = "transfer:file_failed"

type TransferFileFailed struct {
    JobID    string
    PeerID   string
    FileName string
    Reason   string
}
```

#### ChatApp 订阅失败事件
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
        failContent := FileFailedContent{
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
    
    // 原有的 peer.online 订阅...
}
```

### ChatApp trySend 修改

> **【已于 2026-09-28 撤销】下面这段「把 file 消息转给 TransferApp」的实现是缺陷源头，现已删除。**
>
> 它与本文开头「文件消息走 ChatApp 的 outbox 重试机制」的目标直接矛盾：被提前 return 的消息从不经过 `sess.Send`，于是
> 1. 对端的聊天框永远看不到文件卡片；
> 2. 它永远拿不到 ACK，outbox 无限重试，而每次重试都会再产生一条假的 `file_failed`（content 里的 FilePath 为空 → `os.Stat("")` → 「文件已被删除或移动」），并作为普通消息投递给对端 —— 用户看到的就是接收方聊天框堆满裸 JSON。
>
> 现在 file 消息与文本、图片走同一条投递路径；文件是否还在由 TransferApp 在真正传输时判断（失败发 `transfer:error`）。`FileMessageContent` 也不再包含发送方本地路径 `FilePath`。详见实施计划末尾的「实施差异记录」。

```go
// 【已删除，保留仅为对照】
func (a *ChatApp) trySend(ctx context.Context, peerID identity.NodeID, msg message.Message) error {
    // 如果是文件消息，转发给 TransferApp 处理
    if msg.MsgType == message.MsgTypeFile {
        if a.fileHandler != nil {
            var content FileMessageContent
            if err := json.Unmarshal([]byte(msg.Content), &content); err == nil {
                return a.fileHandler.HandleFileMessage(peerID, content)
            }
        }
        return fmt.Errorf("file message handler not available")
    }
    // 原有的文本/图片消息发送逻辑...
}
```

## 组合根装配

### bootstrap/changeset 修改
```go
type Changeset struct {
    // ... 现有字段
    ChatApp    *app.ChatApp
    TransferApp *app.TransferApp
}

func Start(ctx context.Context, opts Options) (*Changeset, error) {
    // ... 现有初始化代码
    
    chatApp := app.NewChatApp(kp.NodeID(), msgStore, conn, peerDir, bus, clk, lg)
    transferApp := app.NewTransferApp(kp.NodeID(), transferRepo, sink, conn, peerDir, bus, clk, lg)
    
    // 注入文件消息处理器
    chatApp.SetFileHandler(transferApp)
    
    return &Changeset{
        ChatApp:    chatApp,
        TransferApp: transferApp,
    }, nil
}
```

## 前端适配

### 类型定义
```typescript
export interface Message {
  msgType: 'text' | 'image' | 'file' | 'file_failed' | string
}

export interface FileFailedContent {
  jobId: string
  fileName: string
  reason: string
}
```

### 渲染逻辑
```vue
<template v-if="msg.msgType === 'file_failed'">
  <div class="file-failed">
    <Icon name="x-circle" />
    <span>文件发送失败：{{ fileName }}</span>
    <span class="reason">{{ reason }}</span>
  </div>
</template>
```

## 边界情况和错误处理

### 文件检查边界情况
- 文件不存在：返回错误，发送失败消息
- 文件权限不足：返回错误，发送失败消息
- 文件是目录：返回错误，发送失败消息
- 路径无效：返回错误，发送失败消息

### 传输失败处理
- 连接失败：文件消息会保留在 outbox，下次对方上线时重试
- 传输中断：由 TransferApp 现有的重试机制处理
- 对方拒绝：标记传输失败，不重试

### 清理策略
- 传输任务完成后，对应的文件消息标记为 delivered
- 失败消息保留在聊天记录中，但不会无限重试
- 可考虑在设置中添加"清理失败的离线文件消息"选项

## 测试策略

### 单元测试
- 测试 ChatApp.FileMessageHandler 调用
- 测试 TransferApp.HandleFileMessage 的文件检查逻辑
- 测试事件总线的发布和订阅

### 集成测试
- 测试离线场景：对方不在线时发送文件，对方上线后自动传输
- 测试文件删除场景：发送离线文件后删除文件，上线后发送失败消息
- 测试边界情况：权限不足、路径无效等

## 实现步骤

1. 扩展 message.go 添加 `MsgTypeFileFailed` 和 `FileFailedContent`
2. 扩展 eventbus.go 添加 `TopicTransferFileFailed` 和 `TransferFileFailed`
3. 修改 ChatApp 添加 `FileMessageHandler` 接口和实现
4. 修改 ChatApp.trySend 处理文件消息
5. 修改 ChatApp.Start 订阅文件失败事件
6. 修改 TransferApp 实现 `HandleFileMessage`
7. 修改 bootstrap.Start 注入依赖
8. 修改前端类型定义和渲染逻辑
9. 编写单元测试和集成测试
