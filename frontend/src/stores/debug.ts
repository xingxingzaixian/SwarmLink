import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import type { DebugEvent } from '../api'

/**
 * 调试面板数据源。
 *
 * 之所以在 v1.0 就做：P2P 排障极难，能实时看到内部事件流
 * 是这个项目投入产出比最高的一个功能（架构书 3.5）。
 */
export const useDebugStore = defineStore('debug', () => {
  const CAP = 500

  const events = ref<DebugEvent[]>([])
  const paused = ref(false)

  function push(e: DebugEvent): void {
    if (paused.value) return
    events.value.push(e)
    if (events.value.length > CAP) {
      events.value.splice(0, events.value.length - CAP)
    }
  }

  function clear(): void {
    events.value = []
  }

  function togglePause(): void {
    paused.value = !paused.value
  }

  /** 按主题聚合计数，便于一眼看出「谁在刷屏」。 */
  const summary = computed(() => {
    const counts = new Map<string, number>()
    for (const e of events.value) {
      counts.set(e.topic, (counts.get(e.topic) ?? 0) + 1)
    }
    return [...counts.entries()]
      .map(([topic, count]) => ({ topic, count }))
      .sort((a, b) => b.count - a.count)
  })

  return { events, paused, summary, push, clear, togglePause }
})
