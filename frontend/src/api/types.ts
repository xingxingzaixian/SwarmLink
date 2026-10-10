// 与 internal/adapters/wails/dto.go 一一对应的前端类型。
// 修改 DTO 时两处必须同步（这是绑定的契约面）。

export interface Peer {
  nodeId: string
  shortId: string
  displayName: string
  /** 领域状态：unknown | discovered | online | offline | blocked */
  state: string
  lastAddr: string
  subnet: string
  source: string
  lastSeen: number
  /**
   * 已握手过（后端为 `state == 'online'`，由 TCP 连接层置位）。
   *
   * ⚠ 它【不是】「节点在不在」—— 后者由 UDP announce 维护，状态是
   * `discovered`。早期前端注释把它写成 announce 维护，导致分组口径一直
   * 用错：刚被广播发现、还没聊过天的节点因此被当成离线。
   */
  online: boolean
  /**
   * 此刻已有 TCP 会话：首条消息零跳数，立刻发得出。
   * 与 online 高度相关但不等价 —— 会话可能被空闲回收，而 state 仍为 online。
   */
  connected: boolean
}

export interface Message {
  msgId: string
  convId: string
  senderId: string
  direction: 'in' | 'out' | string
  content: string
  msgType: string
  fileId?: string
  jobId?: string
  state: 'pending' | 'sent' | 'delivered' | 'failed' | string
  sentAt: number
  recvAt?: number
}

export interface Conversation {
  convId: string
  kind: 'direct' | 'group' | string
  title: string
  peerId?: string
  state?: string
  isOnline: boolean
  lastTime: number
}

export interface TransferJob {
  jobId: string
  peerId: string
  fileName: string
  fileSize: number
  direction: 'send' | 'recv' | string
  localPath: string
  /**
   * 非空表示这条任务的等待原因：
   *   - `peer_offline`：在等对方上线；
   *   - `batch_queued`：文件夹批次里「排在队列里，轮到就传」。
   *
   * 判据必须看它而不是 status：等待中的任务 status 是 idle（DB 里是 queued），
   * 与「还没轮到」共用同一个状态，光看状态分不出来。
   * 两者【语义不同】，文案也不能混：一个是等对端，一个是等前一个文件传完。
   */
  waitReason?: string
  /**
   * 文件夹批次：batchId 非空时 relPath 是它在目录树里的位置（含根名，
   * 如 `照片/2024/a.jpg`）。三者都是零值 = 单文件传输（既有语义不变）。
   *
   * 传输列表靠 relPath 区分同名文件（a/dup.txt 与 b/dup.txt 只显示文件名
   * 就变成两行一模一样）；isDir 的条目是【空目录】占位，没有内容也没有分块。
   */
  batchId?: string
  relPath?: string
  isDir?: boolean
  /**
   * 已确认的【分块数】，不是字节数（后端 Job.Completed ← Bitmap.CompletedCount）。
   * 展示已传字节请用 utils/format 的 transferredBytes：直接 fileSize(completed)
   * 会把 15MB 的文件显示成「30 B」。
   */
  completed: number
  totalChunks: number
  percent: number
  status: string
  error?: string
  updatedAt: number
  /**
   * 传输速率（字节/秒，滑动窗口值）。
   * 只随 `transfer:progress` 事件到达；TransferService.List() 落库的 DTO 里没有，
   * 因此刷新后可能短暂为空 —— UI 必须容忍。
   */
  speed?: number
  /** 预计剩余毫秒数。来源同上，仅事件携带。 */
  etaMs?: number
}

/**
 * 一次文件夹发送的摘要（与绑定里的 FolderDTO 一一对应）。
 *
 * 数字由【发送端枚举后】给出并随消息投递给对端：让接收端自己数，两端
 * 就会显示两个不同的「共 N 个文件」（接收端可能还没收全）。
 */
export interface FolderSummary {
  batchId: string
  folderName: string
  /** 条目数（含空目录）。 */
  fileCount: number
  /** 总字节数（空目录算 0）。 */
  totalSize: number
  /** 被跳过的项（符号链接）。必须是发送端给的数字，否则用户不知道「少了几项」。 */
  skipped: number
}

export interface GroupMember {
  nodeId: string
  displayName: string
  role: string
  state: string
}

export interface Group {
  groupId: string
  name: string
  ownerId: string
  epoch: number
  isOwner: boolean
  members: GroupMember[]
  createdAt: number
}

export interface Self {
  nodeId: string
  displayName: string
  tcpPort: number
  udpPort: number
  subnet: string
}

export interface SeedDetail {
  addr: string
  failCount: number
  nodeId: string
  tcpPort: number
  subnet: string
}

export interface Diagnostics {
  peersTotal: number
  peersOnline: number
  activeSessions: number
  activeTransfers: number
  seedCount: number
  seedDetails: SeedDetail[]
  interfaceSummary: string[]
}

/** 部署前置条件（P-1 / P-2）自检结果。 */
export interface SelfCheck {
  /** false = 未配置种子，自检跳过（单网段部署本就无需自检）。 */
  performed: boolean
  seedCount: number
  /** 本轮通过种子学到的跨网段目录条数。 */
  learned: number
  p1OK: boolean
  p1Detail: string
  p2Overlap: boolean
  p2Detail: string
  seeds: SeedDetail[]
}

export interface Settings {
  displayName: string
  downloadDir: string
  autoOpen: boolean
  tcpPort: number
  udpPort: number
  interfaceMode: string
  allowInterfaces: string[]
  denyInterfaces: string[]
  seeds: string[]
  seedRefreshSec: number
  seedsPerRefresh: number
  maxConcurrent: number
  chunkSize: number
  windowSize: number
  maxActiveConns: number
  idleTimeoutSec: number
  requireAuth: boolean
  encryption: string
  /** 新消息 / 传输事件是否发声。 */
  notifySound: boolean
  /** 点关闭按钮时把窗口收进托盘而不是退出。 */
  notifyMinimizeToTray: boolean
  /** 开机自启动。 */
  autoStart: boolean
}

/** 前端可调用的后端能力。 */
export interface SwarmApi {
  // 设置
  getSettings(): Promise<Settings>
  saveSettings(s: Settings): Promise<string[]>
  listInterfaces(): Promise<string[]>
  downloadDir(): Promise<string>

  /**
   * 上报「用户当前正在看哪个会话」（没有打开会话时传空串）。
   *
   * 提示决策需要它来区分两种情形：正看着这个会话来消息 → 不打扰；
   * 别的会话来消息 → 响铃 + 闪烁。窗口的可见性与焦点由 Go 侧直接读，
   * 只有这一项前端才知道。
   */
  setActiveConversation(convId: string): Promise<void>
  /**
   * 部署前置条件自检（P-1 / P-2）。
   *
   * 会真实发起一轮种子探测与目录拉取，因此最坏要等约 3 秒 ——
   * 「跨网段发现不到人」的根因九成在这两条前提上，静态提示没用，必须实探。
   */
  runSelfCheck(): Promise<SelfCheck>

  // 开机自启动
  autoStartEnable(): Promise<void>
  autoStartDisable(): Promise<void>
  autoStartIsEnabled(): Promise<boolean>

  // 单聊
  sendMessage(peerId: string, text: string): Promise<Message>
  history(peerId: string, limit: number, before: number): Promise<Message[]>
  conversations(): Promise<Conversation[]>
  /** 发送本地图片文件（后端负责压缩与内联编码）。 */
  sendImage(peerId: string, path: string): Promise<Message>
  /** 发送剪贴板图片：只有字节没有路径，dataB64 是标准 base64。 */
  sendImageBytes(peerId: string, name: string, dataB64: string): Promise<Message>
  /** 把一条图片消息另存到指定路径。 */
  saveImage(msgId: string, destPath: string): Promise<void>
  /** 发送文件消息（通知聊天窗口有文件传输）。 */
  sendFileMessage(peerId: string, jobId: string, fileName: string): Promise<Message>
  /**
   * 发送文件夹消息（让两端聊天框出现同一张聚合卡片）。
   *
   * 条目数与总大小由发送端给出（见 FolderSummary）：一个几百个文件的文件夹
   * 不能往聊天框里塞几百条消息，这条消息本身也不携带文件、不含本地路径。
   */
  sendFolderMessage(
    peerId: string,
    batchId: string,
    folderName: string,
    fileCount: number,
    totalSize: number,
    skipped: number
  ): Promise<Message>

  /**
   * 清空一个会话的聊天区。
   *
   * 「清空」不是删除：只是记下一个清空点，早于它的消息不再显示。
   * 因此这个操作随时可以被 restoreConversation 撤销。
   */
  clearConversation(convId: string): Promise<void>
  /** 撤销清空，让被隐藏的消息重新显示。 */
  restoreConversation(convId: string): Promise<void>
  /**
   * 会话清空点的毫秒时间戳；**0 表示未清空**。
   *
   * 前端用它做两件事：过滤聊天区消息、决定「查看历史」按钮是否可点。
   */
  clearedAt(convId: string): Promise<number>

  // 传输
  sendFile(peerId: string, path: string): Promise<string>
  /**
   * 发送一个文件夹。
   *
   * 枚举与落库在返回前完成（后端只 stat、不哈希 —— 一个 10GB 的文件夹不该
   * 为了「开始发送」先读一整遍磁盘），随后由后端的排队机制驱动，因此返回后
   * 传输列表里会立刻出现一批「排队中」的任务。
   */
  sendFolder(peerId: string, path: string): Promise<FolderSummary>
  transfers(): Promise<TransferJob[]>
  /**
   * 一个批次的全部任务（文件夹卡片的聚合进度用）。
   *
   * 不能拿 transfers() 代替：它走后端 ListRecent(50) 的窗口，几百个文件的
   * 文件夹重启后会算出缺斤少两的进度。
   */
  batchJobs(batchId: string): Promise<TransferJob[]>
  /** 取消整个批次里未结束的任务（含正在传的），返回取消条数。 */
  cancelBatch(batchId: string): Promise<number>
  /**
   * 在系统文件管理器中定位已传输的文件（Windows/macOS 会选中文件本身）。
   * 失败时 reject，由调用方提示 —— 例如文件已被用户手动删除。
   */
  revealFile(path: string): Promise<void>
  /** 清空已结束的传输记录，返回删除条数。 */
  clearTransfers(): Promise<number>
  /**
   * 取消一条传输：排队/等待中的直接置取消，正在传的由发送路径干净退出。
   * 半截临时文件会保留，下次重发仍可按位图续传。
   */
  cancelTransfer(jobId: string): Promise<void>
  /** 立刻重试一条等待中的传输（不等退避，也不要求对端在目录里在线）。 */
  retryTransfer(jobId: string): Promise<void>

  // 节点与诊断
  self(): Promise<Self>
  peerList(): Promise<Peer[]>
  /** 从本机列表移除一个节点（只在本机生效；该节点再次上线会自动回来）。 */
  removePeer(nodeId: string): Promise<void>
  diagnostics(): Promise<Diagnostics>

  // 群聊
  groupList(): Promise<Group[]>
  groupCreate(name: string, memberIds: string[]): Promise<Group>
  groupAddMember(groupId: string, memberId: string): Promise<Group>
  groupSendMessage(groupId: string, text: string): Promise<Message>
  groupHistory(groupId: string, limit: number, before: number): Promise<Message[]>
  groupSendImage(groupId: string, path: string): Promise<Message>
  groupSendImageBytes(groupId: string, name: string, dataB64: string): Promise<Message>
  groupSendFile(groupId: string, jobId: string, fileName: string): Promise<Message>
}

/** 后端推送的事件名（与 internal/adapters/wails/eventbridge.go 保持一致）。 */
export const EV = {
  peerUpdated: 'peer:updated',
  chatNewMessage: 'chat:new-message',
  chatDelivered: 'chat:delivered',
  groupUpdated: 'group:updated',
  transferProgress: 'transfer:progress',
  transferDone: 'transfer:done',
  transferError: 'transfer:error',
  /** 任务状态变化（载荷 { jobId, status, reason }）。 */
  transferState: 'transfer:state',
  netError: 'net:error',
  configChanged: 'config:changed',
  debug: 'debug:event',
  /** 拖入窗口的文件路径（由桌面外壳转发，非领域事件）。 */
  fileDropped: 'file:dropped',
  /** 用户点击了系统通知（由桌面外壳转发，非领域事件）。 */
  notifyActivated: 'notify:activated'
} as const

/** `file:dropped` 的载荷。 */
export interface FilesDroppedEvent {
  paths: string[]
  /** 命中的 data-file-drop-target 属性值。 */
  target: string
}

/** `notify:activated` 的载荷。 */
export interface SystemNotifyEvent {
  /** 点击的那条通知对应的会话；取不到时为空串。 */
  convId: string
}

export interface DebugEvent {
  topic: string
  at: number
}
