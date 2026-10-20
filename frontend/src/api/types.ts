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
  /**
   * 最近一次心跳测得的往返延迟（毫秒）。**0 表示没有数据**：
   * 没有会话，或会话刚建立、心跳还没跑完一轮。
   *
   * 界面必须把 0 显示成「测量中」—— 把「没测到」显示成「0ms」是编造数据。
   */
  rttMs: number
  /**
   * 对方自述的应用版本，缺省表示「对方没报」（旧版本客户端）。
   *
   * 只用于「局域网更新」判断谁更新，不可当真：它随 UDP 广播传播、不参与签名，
   * 真正的包版本由会话内的 UPDATE_INFO_RESP 给出。
   */
  appVersion?: string
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
  /**
   * 群消息的送达统计（单聊为 0/缺省）。
   *
   * pendingCount > 0 表示还有成员没确认送达（离线或尚未连接），
   * 气泡据此显示「N 人未送达」。消息全员送达后后端会清掉统计，这里回到 0。
   */
  memberCount?: number
  pendingCount?: number
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
  /**
   * 【本次尝试的起点】百分比：>0 表示这条是从上次的半截续传的（界面显示
   * 「续传自 XX%」），缺省/0 表示从头传。
   *
   * 与 percent 并列而不是同一个数：percent 答「现在到哪儿了」（一路上涨），
   * 它答「这次是从哪儿接上的」（定下就不动）。
   *
   * 两个来源都带它：TransferService.List() 落库的 DTO（含重启后的陈行），
   * 以及 `transfer:progress` 事件 —— 续传起点要等 FILE_META_ACK 才定下来，
   * 那一刻没有别的可用事件。
   */
  resumeFromPercent?: number
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

/** 共享可见性（与 dto.go 的 ShareVisibilityDTO 一一对应）。 */
export interface ShareVisibility {
  /** all | users | group */
  type: string
  /** users 时是 node_id 列表；group 时是群 id 列表。 */
  allowed?: string[]
}

/**
 * 一条共享（与 dto.go 的 ShareDTO 一一对应）。
 *
 * 本机发布的与他人发布的共用这一种形状：界面渲染的是同一张卡片，
 * 差别只有「能不能下载 / 能不能取消」两个按钮位。
 */
export interface Share {
  shareId: string
  name: string
  ownerId: string
  ownerName: string
  fileCount: number
  totalSize: number
  visibility: ShareVisibility
  /** 后端算好的可见性文案（「全部」「指定人(2)」「仅群组」）。 */
  visibilityText: string
  /** true = 本机发布的。 */
  mine: boolean
  /** 仅本机共享有值：本地根路径。 */
  rootPath?: string
  /** 仅他人共享有意义：发布者此刻是否可达。 */
  online: boolean
  /**
   * 「现在点下载会成功吗」——**由后端算好**。
   *
   * 它同时取决于 mine / online / 可见性。前端不要自己组合这三个字段：
   * 那样等于把一条判定规则复制到第二个地方，两处迟早不一致。
   */
  downloadable: boolean
  createdAt: number
  updatedAt: number
}

/** 共享里的一条文件。 */
export interface ShareItem {
  /** 相对共享根的路径（斜杠分隔）。下载请求按它标识文件。 */
  path: string
  name: string
  size: number
  /** 空目录占位项（零字节，下载时只建目录）。 */
  isDir?: boolean
  mtime?: number
}

/** 一次共享下载的受理结果（进度走 transfer:* 事件）。 */
export interface ShareDownload {
  /** 批次标识：界面用它把所有相关任务聚合成一张卡片、也用它整批取消。 */
  batchId: string
  accepted: number
  /** 被共享方跳过的条目及原因（文件已删除、路径非法等）。 */
  rejected?: string[]
}

export interface GroupMember {
  nodeId: string
  displayName: string
  role: string
  state: string
  /**
   * 是否在群 —— **权威判据**。
   *
   * 由后端按「票号」算好（`admitSeq > kickSeq && admitSeq > leftSeq`）。
   * 前端不要用 `state` 自行推导：state 只是派生缓存，
   * 一旦与票号不同步，就会出现「后端说已退出、界面仍显示在群」。
   */
  active: boolean
  /** 票号：群主最近一次「发票」的编号。 */
  admitSeq: number
  /** 群主撕票号（被踢）。 */
  kickSeq: number
  /** 本人撕票号（主动退出），自签。 */
  leftSeq: number
}

export interface Group {
  groupId: string
  name: string
  ownerId: string
  epoch: number
  /** 群主的群级发票计数器（票号来源），仅用于展示与排障。 */
  mintSeq: number
  /** 群状态：active | dissolved。解散后禁用输入并展示横幅。 */
  state: string
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
  /** 接收侧的单文件上限（字节），0 表示不限制。前端只用来提前拦截，真正的判定在接收端。 */
  maxFileSize: number
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

  /** 局域网更新总开关（关闭 = 本机不再拉取，但仍可作为别人的更新源）。 */
  updateEnabled: boolean
  /** `prompt` 下载完弹框询问 | `auto` 自动装 | `notify` 只提示不装。 */
  updateMode: string
  /** 弹框后多少小时无操作自动安装；**0 = 永不**。 */
  updateAutoInstallHours: number
}

/**
 * 局域网更新的阶段。
 *
 * 与后端 app.UpdatePhase 一一对应。前端按它决定显示什么，
 * 不要自己从 percent / scheduledAt 反推阶段 —— 两个判据迟早不一致。
 */
export type UpdatePhase =
  | 'idle'
  | 'checking'
  | 'downloading'
  | 'ready'
  | 'scheduled'
  | 'applying'
  | 'failed'
  | 'unsupported'

/** 一个版本更高的候选更新源。 */
export interface UpdatePeer {
  peerId: string
  displayName: string
  addr: string
  version: string
}

/** 局域网更新的完整状态快照（后端只有这一个更新事件）。 */
export interface UpdateState {
  phase: UpdatePhase | string
  currentVersion: string
  targetVersion?: string
  sourcePeerId?: string
  sourceName?: string
  sourceAddr?: string
  size?: number
  downloaded?: number
  percent?: number
  sha256?: string
  /** 排定的安装时刻（毫秒）；0/缺省表示没有排期。 */
  scheduledAt?: number
  /** 到点自动安装的时刻（毫秒）；0/缺省表示不会自动安装。 */
  autoInstallAt?: number
  /** 为真时前端应弹「立即更新 / 选择时间」确认框。 */
  promptPending: boolean
  error?: string
  /** 当前平台是否支持自动安装（目前仅 Windows）。 */
  platformSupported: boolean
  /** 本机是否持有与当前版本一致的安装包（能否作为别人的更新源）。 */
  canDistribute: boolean
  installerPath?: string
  lastCheckAt?: number
  /** 局域网里版本更高的节点。 */
  higherPeers?: UpdatePeer[]
  /** 数据目录（关于信息）。 */
  dataDir: string
  /** 日志文件路径（关于信息）。 */
  logPath: string
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

  // 局域网更新
  /** 读取更新状态（设置页打开时拉一次；之后靠 update:state 事件推送）。 */
  getUpdateState(): Promise<UpdateState>
  /** 触发一次后台检查（不等结果，结果通过事件回来）。 */
  checkUpdateNow(): Promise<UpdateState>
  /**
   * 立即安装并重启。
   *
   * 后端会拉起安装器并退出进程，因此这个 Promise 正常返回后大概率再也
   * 收不到后续 —— 前端不要在这之后依赖任何状态。
   */
  applyUpdateNow(): Promise<UpdateState>
  /** 把安装安排到 atMillis（毫秒时间戳）。 */
  scheduleUpdate(atMillis: number): Promise<UpdateState>
  /** 稍后提醒（30 分钟后重新弹框），不取消更新。 */
  snoozeUpdate(): Promise<UpdateState>
  /** 放弃本次更新（删除已下载的包）。 */
  cancelUpdate(): Promise<UpdateState>

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

  // 历史搜索（ADR-013）
  /**
   * 搜索本机消息历史（新 → 旧，遵守会话清空点）。
   *
   * 四条契约必须原样传下去，界面【不能】自己在调用侧加工：
   *   1. `text` 的空白由后端去掉，空串直接返回空数组（不查库）；
   *   2. `convIds` 为空 = 后端派生的「可见会话集合」（与 conversations() 同源），
   *      不是「全库」—— 本地已移除的对端因此不会出现在结果里。
   *      传 `[convId]` 即收窄到某个会话（左栏筛选走它）；
   *   3. `beforeMsgId` 是游标：传上一页最后一条的 msgId，首页传空串。
   *      后端内部把它换算成写入序，因此不要拿 sentAt 自己造游标 ——
   *      同一毫秒里连发几条时，按时间跳会漏掉边界上的命中。
   *   4. 后端【不返回命中总数】（20 万条消息下 count(*) 是 213ms、
   *      按会话分组是 1.9s），所以界面只能显示「还有更早的」，
   *      不能显示「共 N 条」—— 那个数字没有便宜来源。
   */
  searchHistory(text: string, convIds: string[], limit: number, beforeMsgId: string): Promise<Message[]>
  /**
   * 取「以某条消息为中心」的一页（新 → 旧），用于点搜索结果后定位。
   *
   * 返回空数组表示该消息不存在 / 不属于该会话 / 已被清空点隐藏 ——
   * 那是正常的用户路径（应回退到会话最新一页并提示），不是错误。
   */
  jumpContext(convId: string, msgId: string, before: number, after: number): Promise<Message[]>

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
  /**
   * 把同一个文件夹发给多个对端（群聊用）：全批共享一个 batch_id，
   * 聊天框只出一张卡片、进度按「文件数」而不是「文件数 × 成员数」算。
   */
  sendFolderTo(peerIds: string[], path: string): Promise<FolderSummary>
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
   * 取消一条传输：排队/等待中、已暂停、正在传的都能取消 —— 前两种直接置
   * 取消，正在传的由发送路径干净退出。半截临时文件会保留，下次重发仍可按
   * 位图续传。
   *
   * 只对本机发出的任务可用：接收侧没有取消出口（后端会拒绝）。
   */
  cancelTransfer(jobId: string): Promise<void>
  /** 立刻重试一条等待中的传输（不等退避，也不要求对端在目录里在线）。 */
  retryTransfer(jobId: string): Promise<void>
  /**
   * 暂停一条正在传输的任务：进度与半截文件全部保留，对端也会跟着停下。
   * 只有「正在传」的能暂停，等待态（等对端 / 批次排队）会返回错误。
   */
  pauseTransfer(jobId: string): Promise<void>
  /**
   * 恢复一条已暂停的任务。发送侧重走元数据协商，接收侧直接续写；
   * 对方还按着时会返回错误，等对方恢复后自动继续。
   */
  resumeTransfer(jobId: string): Promise<void>

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
  /** 群主移除成员（epoch+1 并广播）。 */
  groupRemoveMember(groupId: string, memberId: string): Promise<Group>
  /** 退出群（本人）：本地立即生效，不依赖群主在线。 */
  groupLeave(groupId: string): Promise<Group>
  /** 群主改名（epoch+1 并广播）。 */
  groupRename(groupId: string, name: string): Promise<Group>
  /** 群主解散群（epoch+1 并广播；成员侧停止收发）。 */
  groupDissolve(groupId: string): Promise<Group>
  groupSendMessage(groupId: string, text: string): Promise<Message>
  groupHistory(groupId: string, limit: number, before: number): Promise<Message[]>
  groupSendImage(groupId: string, path: string): Promise<Message>
  groupSendImageBytes(groupId: string, name: string, dataB64: string): Promise<Message>
  groupSendFile(groupId: string, jobId: string, fileName: string): Promise<Message>
  /** 群发文件夹卡片（文件本身由前端逐个在线成员 P2P 发起）。 */
  groupSendFolder(
    groupId: string,
    batchId: string,
    folderName: string,
    fileCount: number,
    totalSize: number,
    skipped: number
  ): Promise<Message>

  // 文件共享（FR-14）
  /**
   * 共享清单：本机发布的在前，他人发布的在后。
   *
   * 一次返回两组而不是分两个方法：界面同一屏要展示它们，分两次调用会
   * 出现两批数据来自不同时刻的经典问题（「我发布的没了」那种一闪）。
   */
  shareList(): Promise<Share[]>
  /**
   * 发布一条共享。
   *
   * 可见性拆成 (visType, allowed) 两个参数：两者的取值有强耦合规则
   * （all 忽略 allowed、group 只允许一个），拆开后调用点一眼能看出这条规则。
   */
  sharePublish(
    rootPath: string,
    name: string,
    visType: string,
    allowed: string[]
  ): Promise<Share>
  shareUnpublish(shareId: string): Promise<void>
  /**
   * 修改一条本机共享的可见性。
   *
   * 不换 share_id（别人列表里的条目不会闪、历史下载的批次仍对得上），
   * 后端会立刻重新广播摘要 —— 否则「改成指定人」在对方列表里要等下一轮
   * 周期通告（最长 45s）才生效。
   */
  shareSetVisibility(
    shareId: string,
    visType: string,
    allowed: string[]
  ): Promise<Share>
  /** 重新扫描共享根目录（磁盘内容变了之后用）。 */
  shareRefresh(shareId: string): Promise<Share>
  /**
   * 浏览一条共享的文件清单（自己的或他人的）。
   *
   * 他人的清单是现拉现取的，因此共享方不在线时这里会 reject ——
   * 界面必须把失败原因原样展示（「共享方不在线」是用户唯一能行动的线索）。
   */
  shareBrowse(shareId: string): Promise<ShareItem[]>
  /**
   * 下载共享里的指定文件（异步：返回即代表共享方已受理）。
   *
   * 返回的 batchId 会出现在随后相关任务的 transfer:* 事件里。
   */
  shareDownload(shareId: string, paths: string[]): Promise<ShareDownload>
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
  /**
   * 本机共享清单变化（载荷 { shareId, action }）。
   *
   * 他人共享的变化不走它：那是目录条目的变化，已经由 peer:updated 承担。
   */
  shareUpdated: 'share:updated',
  /**
   * 局域网更新状态变化（载荷是完整快照 UpdateState）。
   *
   * 只有这一个更新事件：状态机有 8 个阶段，用增量事件拼装迟早会出现
   * 「显示已下载完、后端已经进入安装」这种自相矛盾的画面。
   */
  updateState: 'update:state',
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
