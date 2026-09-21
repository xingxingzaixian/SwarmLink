import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { getApi, type TransferJob } from '../api'
import { baseName } from '../utils/format'

/** 事件载荷：只带「易变」的进度，落地元数据（文件名/大小）靠 List() 补齐。 */
interface ProgressPayload {
  jobId: string
  peerId?: string
  percent: number
  status?: string
  speed?: number
  etaMs?: number
}

/** 已结束的状态。注意 failed 也算结束 —— 它不会自己再动起来。 */
const TERMINAL = new Set(['done', 'failed', 'cancelled'])

export function isActive(status: string): boolean {
  return !TERMINAL.has(status)
}

/**
 * 传输任务状态。
 *
 * 进度的数据来源有两条，职责必须分清：
 *   - `transfer:progress` 事件（后端 4 Hz）：只给 percent / speed / eta —— 高频、轻量；
 *   - `TransferService.List()`：给 fileName / fileSize / localPath 等落地元数据。
 *
 * 因此遇到「事件里出现了没见过的 job」时，不能只造一个空壳就完事，
 * 否则进度条上会挂着一个没有文件名的任务（v1.0 老界面的问题）。
 * 这里改成造壳 + 触发一次节流后的 List() 补齐。
 */
export const useTransferStore = defineStore('transfer', () => {
  const jobs = ref<TransferJob[]>([])
  const error = ref('')

  /** 每个 peer 的任务（收发都算），按列表原有顺序。 */
  function forPeer(peerId: string): TransferJob[] {
    if (!peerId) return []
    return jobs.value.filter((j) => j.peerId === peerId)
  }

  /** 某个 peer 正在进行中的任务数。右栏据此决定要不要自动切到进度页。 */
  function activeCountOf(peerId: string): number {
    return forPeer(peerId).filter((j) => isActive(j.status)).length
  }

  async function refresh(): Promise<void> {
    try {
      const list = await (await getApi()).transfers()
      // 保留事件带来的速率/ETA：List() 不返回它们，直接覆盖会让速度显示闪断
      const prev = new Map(jobs.value.map((j) => [j.jobId, j]))
      const merged = list.map((j) => {
        const old = prev.get(j.jobId)
        return old?.speed === undefined ? j : { ...j, speed: old.speed, etaMs: old.etaMs }
      })
      // 已结束的本地条目也要留住：后端只保留最近若干条，更早的会被挤出去，
      // 而用户此刻正看着它 —— 让它凭空消失比多留几条糟糕得多。
      const seen = new Set(list.map((j) => j.jobId))
      const kept = jobs.value.filter((j) => !seen.has(j.jobId) && !isActive(j.status))
      jobs.value = [...merged, ...kept]
    } catch (e: any) {
      error.value = String(e?.message ?? e)
    }
  }

  // --- 元数据补齐（节流：一次突发只拉一次列表） -----------------------------
  let hydrateTimer: number | undefined

  function scheduleHydrate(): void {
    if (hydrateTimer !== undefined) return
    hydrateTimer = window.setTimeout(() => {
      hydrateTimer = undefined
      void refresh()
    }, 400)
  }

  function applyProgress(p: ProgressPayload): void {
    const i = jobs.value.findIndex((j) => j.jobId === p.jobId)
    if (i < 0) {
      // 新任务：先造壳让进度条立刻动起来，再补元数据
      jobs.value.unshift({
        jobId: p.jobId,
        peerId: p.peerId ?? '',
        fileName: '',
        fileSize: 0,
        direction: 'send',
        localPath: '',
        completed: 0,
        totalChunks: 0,
        percent: p.percent,
        status: p.status ?? 'active',
        speed: p.speed,
        etaMs: p.etaMs,
        updatedAt: Date.now()
      })
      scheduleHydrate()
      return
    }
    const prev = jobs.value[i]
    jobs.value[i] = {
      ...prev,
      percent: p.percent,
      status: p.status ?? prev.status,
      speed: p.speed ?? prev.speed,
      etaMs: p.etaMs ?? prev.etaMs,
      updatedAt: Date.now()
    }
    // 名字由发起方从本地路径填好了，大小/分块数只能向后端要。
    // 此时任务一定已落在后端仓储里（进度事件以它为前提），拉一次是安全的。
    if (!prev.fileSize) scheduleHydrate()
  }

  function applyDone(d: { jobId: string; peerId?: string; path: string }): void {
    const i = jobs.value.findIndex((j) => j.jobId === d.jobId)
    if (i < 0) {
      // 完成事件先于任何进度事件到达时必须【自己建条目】，不能只 hydrate：
      //   - 小文件：可能一个进度事件都没发出就 done 了；
      //   - 接收端：根本没有 registerPending 这一步，done 往往是第一个事件。
      // 过去这里只调 scheduleHydrate() 就返回，于是「文件传完了，
      // 但列表里什么记录都没有」—— 而 List() 当时还排除已完成的任务。
      jobs.value.unshift({
        jobId: d.jobId,
        peerId: d.peerId ?? '',
        fileName: baseName(d.path),
        fileSize: 0, // 由 List() 补齐（含收发方向）
        direction: 'send',
        localPath: d.path,
        completed: 0,
        totalChunks: 0,
        percent: 100,
        status: 'done',
        updatedAt: Date.now()
      })
      scheduleHydrate()
      return
    }
    jobs.value[i] = {
      ...jobs.value[i],
      percent: 100,
      status: 'done',
      localPath: d.path,
      speed: undefined,
      etaMs: undefined
    }
  }

  function applyError(d: { jobId: string; error: string }): void {
    const i = jobs.value.findIndex((j) => j.jobId === d.jobId)
    if (i < 0) {
      // 与 done 同理：失败也可能先于任何进度事件（例如刚发起就拿不到会话）
      jobs.value.unshift({
        jobId: d.jobId,
        peerId: '',
        fileName: '',
        fileSize: 0,
        direction: 'send',
        localPath: '',
        completed: 0,
        totalChunks: 0,
        percent: 0,
        status: 'failed',
        error: d.error,
        updatedAt: Date.now()
      })
      scheduleHydrate()
      return
    }
    jobs.value[i] = {
      ...jobs.value[i],
      status: 'failed',
      error: d.error,
      speed: undefined,
      etaMs: undefined
    }
    // 失败也可能是「还没发出第一块」——此时元数据仍是空的，补一次
    if (!jobs.value[i].fileSize) scheduleHydrate()
  }

  /**
   * 用「刚发出的任务」建一条本地条目。
   *
   * 文件名从用户选中的本地路径取：这是发起方唯一确定知道、
   * 而进度事件又不会携带的信息。先把名字填上，列表里就不会出现
   * 一个只有进度条、没有名字的空壳（v1.0 老界面的样子）。
   */
  function registerPending(peerId: string, jobId: string, path: string): void {
    const i = jobs.value.findIndex((j) => j.jobId === jobId)
    if (i >= 0) {
      jobs.value[i] = { ...jobs.value[i], peerId, fileName: jobs.value[i].fileName || baseName(path) }
      return
    }
    jobs.value = [
      {
        jobId,
        peerId,
        fileName: baseName(path),
        fileSize: 0, // 由后端 List() 补齐
        direction: 'send',
        localPath: '',
        completed: 0,
        totalChunks: 0,
        percent: 0,
        // 「排队中」是诚实的：真正开始前还要等并发额度与文件哈希
        status: 'queued',
        updatedAt: Date.now()
      },
      ...jobs.value
    ]
  }

  /**
   * 发起发送，返回后端分配的真实 job_id。
   *
   * 后端已经在【建任务之前】生成 job_id 并随返回值给出（见
   * TransferService.SendFile / TransferApp.SendFileAs），因此这个 id 会原样
   * 出现在后续的 transfer:progress / done / error 事件里 —— 可以放心用它
   * 先建条目，进度会自动落到同一条上。
   *
   * 这里【不】顺手 refresh()：任务此刻可能还没落库（文件哈希在后台跑），
   * 拉列表反而会把刚建好的条目冲掉。等第一个进度事件再说。
   */
  async function sendFile(peerId: string, path: string): Promise<string> {
    const jobId = await (await getApi()).sendFile(peerId, path)
    if (jobId) registerPending(peerId, jobId, path)
    return jobId
  }

  /** 串行多选发送：后端一次只接一个路径，串行也避免多文件抢满并发额度。 */
  async function sendFiles(peerId: string, paths: string[]): Promise<number> {
    let ok = 0
    for (const p of paths) {
      try {
        await sendFile(peerId, p)
        ok++
      } catch (e: any) {
        error.value = String(e?.message ?? e)
      }
    }
    return ok
  }

  /** 全局进行中的任务（左栏底部/弹层里的角标用）。 */
  const activeJobs = computed(() => jobs.value.filter((j) => isActive(j.status)))

  return {
    jobs,
    error,
    activeJobs,
    forPeer,
    activeCountOf,
    refresh,
    applyProgress,
    applyDone,
    applyError,
    registerPending,
    sendFile,
    sendFiles
  }
})
