// 与 internal/adapters/wails/dto.go 一一对应的前端类型。
// 修改 DTO 时两处必须同步（这是绑定的契约面）。

export interface Peer {
  nodeId: string
  shortId: string
  displayName: string
  state: string
  lastAddr: string
  subnet: string
  source: string
  lastSeen: number
  /** 节点在线（由 UDP announce 维护） */
  online: boolean
  /** 连接可用（由 TCP 会话维护）—— 与 online 是两件事，UI 必须分开显示 */
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
  debug: 'debug:event'
} as const

export interface DebugEvent {
  topic: string
  at: number
}
