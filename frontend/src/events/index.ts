import { useChatStore } from '../stores/chat'
import { useDebugStore } from '../stores/debug'
import { usePeersStore } from '../stores/peers'
import { useTransferStore } from '../stores/transfer'
import { Events } from '@wailsio/runtime'

import { EV, type DebugEvent, type Message, type Peer, type TransferJob } from '../api/types'

type Handler = (data: any) => void

// ---------------------------------------------------------------------------
// 订阅通道
// ---------------------------------------------------------------------------

/**
 * 事件订阅的唯一入口。
 *
 * Wails 环境下走 @wailsio/runtime 的 Events.On（回调收到 WailsEvent，
 * 业务数据在 .data 上）；否则退化为本地 EventTarget，
 * 这样在没有后端的浏览器预览里，组件逻辑依然可被驱动与验证。
 */
export function on(name: string, fn: Handler): void {
  try {
    Events.On(name, (ev: any) => fn(ev?.data ?? ev))
    return
  } catch {
    // 不在 Wails 宿主里：Events.On 无法工作，改用本地事件通道
  }
  window.addEventListener(name, (e) => fn((e as CustomEvent).detail))
}

/** 本地触发（仅调试/预览用）。 */
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

  on(EV.transferProgress, (data: any) => {
    pendingProgress.set(data.jobId, data)
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
