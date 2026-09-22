import { useChatStore } from '../stores/chat'
import { useDebugStore } from '../stores/debug'
import { usePeersStore } from '../stores/peers'
import { useTransferStore } from '../stores/transfer'
import { useUiStore } from '../stores/ui'
import { isImagePath } from '../utils/image'
import { Events } from '@wailsio/runtime'

import {
  EV,
  type DebugEvent,
  type FilesDroppedEvent,
  type Message,
  type Peer,
  type TransferJob
} from '../api/types'

/**
 * 拖放文件 → 发送。
 *
 * 路径来自桌面外壳（WebView 的 File 对象没有本地路径），落点由
 * data-file-drop-target 决定。这里只认聊天区：用户把文件拖到联系人列表上
 * 多半是误操作，静默忽略比「猜他想发给谁」安全。
 */
async function handleFilesDropped(data: FilesDroppedEvent): Promise<void> {
  if (data?.target !== 'chat' || !data.paths?.length) return

  const chat = useChatStore()
  const transfers = useTransferStore()
  const ui = useUiStore()

  const conv = chat.activeConversation
  if (!conv) {
    ui.notify('先打开一个会话，再拖入文件')
    return
  }

  // 图片走消息链路（气泡内显示、不进传输列表），其余走文件传输。
  // 这里按扩展名分流只是为了少一次往返：真实格式仍由后端按魔数判定，
  // 因此「扩展名骗人」的后果是一个明确的错误提示，而不是错进了传输列表。
  //
  // 注意分流必须发生在 peerId 检查【之前】：群会话没有 peerId，
  // 若先卡 peerId，群里的图片会被一句「先打开与某个联系人的会话」挡回去 ——
  // 而群里发图本来是支持的（走扇出）。
  const images = data.paths.filter(isImagePath)
  const files = data.paths.filter((p) => !isImagePath(p))

  for (const p of images) {
    try {
      await chat.sendImage(conv, p)
    } catch (e: any) {
      ui.notify(String(e?.message ?? e) || '图片发送失败')
    }
  }

  if (!files.length) return

  const peerId = chat.activePeerId
  if (!peerId) {
    ui.notify('文件传输暂不支持群聊，请改用图片或在单聊里发送')
    return
  }

  ui.focusTransfer()
  const ok = await transfers.sendFiles(peerId, files)
  ui.notify(
    ok === files.length
      ? `已开始发送 ${ok} 个文件`
      : `已发起 ${ok}/${files.length} 个文件，失败的请看右侧`
  )
}

type Handler = (data: any) => void

// ---------------------------------------------------------------------------
// 订阅通道
// ---------------------------------------------------------------------------

/**
 * 事件订阅的唯一入口。
 *
 * 两条通道【同时】订阅：
 *   - Wails 宿主：@wailsio/runtime 的 Events.On（回调收到 WailsEvent，业务数据在 .data 上）；
 *   - 浏览器预览：本地 CustomEvent（配合 emitLocal，让降级模式也能被真实驱动）。
 *
 * 为什么不做「探测宿主再二选一」：Events.On 在没有原生 runtime 时【不会抛错】，
 * 它只是把回调登记进一个永远不会被触发的 map —— 靠 try/catch 判断宿主是失效的。
 * 而两条通道各自沉默的代价是零（各多一个不会被调用的监听器）。
 */
export function on(name: string, fn: Handler): void {
  try {
    Events.On(name, (ev: any) => fn(ev?.data ?? ev))
  } catch {
    /* 没有原生 runtime 时忽略 */
  }
  window.addEventListener(name, (e) => fn((e as CustomEvent).detail))
}

/**
 * 本地触发（降级预览 / 调试用）。
 *
 * 它驱动的是【真实的 store 更新路径】，不是另做一套假数据 ——
 * 因此在浏览器里 `npm run dev` 就能验证进度合并、未读、气泡状态这些逻辑。
 */
export function emitLocal(name: string, data: any): void {
  window.dispatchEvent(new CustomEvent(name, { detail: data }))
}

// ---------------------------------------------------------------------------
// 进度合并（rAF）
// ---------------------------------------------------------------------------

/**
 * 进度事件的合并队列。
 *
 * 后端已按 job 节流到 4 Hz；前端再合并一层是为了应对
 * 「多个任务同时推进」的情况：把同一帧内的多个更新收敛成一次 store 写入，
 * 避免高频 patch 触发大量重渲染（P1-10）。
 */
const pendingProgress = new Map<string, any>()
let flushScheduled = false

function scheduleProgressFlush(): void {
  if (flushScheduled) return
  flushScheduled = true

  const run = () => {
    flushScheduled = false
    const transfers = useTransferStore()
    for (const [, payload] of pendingProgress) {
      transfers.applyProgress(payload)
    }
    pendingProgress.clear()
  }

  if (typeof requestAnimationFrame === 'function') {
    requestAnimationFrame(run)
  } else {
    setTimeout(run, 16)
  }
}

// ---------------------------------------------------------------------------
// 集中注册
// ---------------------------------------------------------------------------

let registered = false

/**
 * 集中注册全部事件订阅。
 *
 * 【必须】在应用启动时调用一次，且不要在组件里各自订阅：
 * 散落订阅会在组件卸载时泄漏监听并导致重复更新。
 */
export function registerEvents(): void {
  if (registered) return
  registered = true

  const peers = usePeersStore()
  const chat = useChatStore()
  const transfers = useTransferStore()
  const debug = useDebugStore()

  on(EV.debug, (data: DebugEvent) => debug.push(data))

  on(EV.peerUpdated, (data: Partial<Peer> & { nodeId: string }) => {
    peers.applyUpdate(data)
  })

  on(EV.chatNewMessage, (m: Message) => chat.receive(m))

  on(EV.chatDelivered, (data: { msgId: string }) => chat.markDelivered(data.msgId))

  // 进度载荷在【这一处】翻译成 store 的命名：后端发的是 Go 的 JSON key
  // （speed / eta），前端用 etaMs 明确单位是毫秒，避免和秒混用。
  on(EV.transferProgress, (data: any) => {
    pendingProgress.set(data.jobId, {
      jobId: data.jobId,
      peerId: data.peerId,
      percent: data.percent,
      status: data.status,
      speed: data.speed,
      etaMs: data.eta
    })
    scheduleProgressFlush()
  })

  on(EV.transferDone, (data: { jobId: string; peerId: string; path: string }) => {
    transfers.applyDone(data)
  })

  on(EV.transferError, (data: { jobId: string; error: string }) => {
    transfers.applyError(data)
  })

  on(EV.groupUpdated, (data: { groupId: string; name: string; epoch: number }) => {
    // 群元数据只推摘要；成员详情由 GroupService.List() 拉取
    void chat.loadConversations()
    debug.push({ topic: `group.updated → epoch=${data.epoch}`, at: Date.now() })
  })

  on(EV.fileDropped, (data: FilesDroppedEvent) => {
    void handleFilesDropped(data)
  })

  on(EV.netError, (data: { op: string; peerId: string; error: string }) => {
    debug.push({ topic: `net.error(${data.op}): ${data.error}`, at: Date.now() })
  })

  on(EV.configChanged, () => {
    debug.push({ topic: 'config.changed', at: Date.now() })
  })
}

/** 测试/预览辅助：构造一个传输进度载荷。 */
export function mockProgress(jobId: string, percent: number): TransferJob {
  return {
    jobId,
    peerId: '',
    fileName: '',
    fileSize: 0,
    direction: 'send',
    localPath: '',
    completed: 0,
    totalChunks: 0,
    percent,
    status: 'active',
    updatedAt: Date.now()
  }
}
