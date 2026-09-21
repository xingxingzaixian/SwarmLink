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
}

/** 前端可调用的后端能力。 */
export interface SwarmApi {
  // 设置
  getSettings(): Promise<Settings>
  saveSettings(s: Settings): Promise<string[]>
  listInterfaces(): Promise<string[]>
  downloadDir(): Promise<string>

  // 单聊
  sendMessage(peerId: string, text: string): Promise<Message>
  history(peerId: string, limit: number, before: number): Promise<Message[]>
  conversations(): Promise<Conversation[]>

  // 传输
  sendFile(peerId: string, path: string): Promise<string>
  transfers(): Promise<TransferJob[]>
  /**
   * 在系统文件管理器中定位已传输的文件（Windows/macOS 会选中文件本身）。
   * 失败时 reject，由调用方提示 —— 例如文件已被用户手动删除。
   */
  revealFile(path: string): Promise<void>
  /** 清空已结束的传输记录，返回删除条数。 */
  clearTransfers(): Promise<number>

  // 节点与诊断
  self(): Promise<Self>
  peerList(): Promise<Peer[]>
  diagnostics(): Promise<Diagnostics>

  // 群聊
  groupList(): Promise<Group[]>
  groupCreate(name: string, memberIds: string[]): Promise<Group>
  groupAddMember(groupId: string, memberId: string): Promise<Group>
  groupSendMessage(groupId: string, text: string): Promise<Message>
  groupHistory(groupId: string, limit: number, before: number): Promise<Message[]>
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
  netError: 'net:error',
  configChanged: 'config:changed',
  debug: 'debug:event',
  /** 拖入窗口的文件路径（由桌面外壳转发，非领域事件）。 */
  fileDropped: 'file:dropped'
} as const

/** `file:dropped` 的载荷。 */
export interface FilesDroppedEvent {
  paths: string[]
  /** 命中的 data-file-drop-target 属性值。 */
  target: string
}

export interface DebugEvent {
  topic: string
  at: number
}
