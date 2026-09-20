import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { getApi, type TransferJob } from '../api'

/** 传输任务状态。进度由 transfer:progress 事件驱动（后端 4 Hz + 前端 rAF 合并）。 */
export const useTransferStore = defineStore('transfer', () => {
  const jobs = ref<TransferJob[]>([])
  const error = ref('')

  const active = computed(() =>
    jobs.value.filter((j) => j.status !== 'done' && j.status !== 'cancelled')
  )

  async function refresh(): Promise<void> {
    try {
      jobs.value = await (await getApi()).transfers()
    } catch (e: any) {
      error.value = String(e?.message ?? e)
    }
  }

  function applyProgress(p: { jobId: string; percent: number; status?: string; peerId?: string }): void {
    const i = jobs.value.findIndex((j) => j.jobId === p.jobId)
    if (i < 0) {
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
        updatedAt: Date.now()
      })
      return
    }
    const prev = jobs.value[i]
    jobs.value[i] = {
      ...prev,
      percent: p.percent,
      status: p.status ?? prev.status,
      updatedAt: Date.now()
    }
  }

  function applyDone(d: { jobId: string; path: string }): void {
    const i = jobs.value.findIndex((j) => j.jobId === d.jobId)
    if (i < 0) return
    jobs.value[i] = { ...jobs.value[i], percent: 100, status: 'done', localPath: d.path }
  }

  function applyError(d: { jobId: string; error: string }): void {
    const i = jobs.value.findIndex((j) => j.jobId === d.jobId)
    if (i < 0) return
    jobs.value[i] = { ...jobs.value[i], status: 'failed', error: d.error }
  }

  async function sendFile(peerId: string, path: string): Promise<string> {
    return (await getApi()).sendFile(peerId, path)
  }

  return { jobs, active, error, refresh, applyProgress, applyDone, applyError, sendFile }
})
