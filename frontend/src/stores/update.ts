import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { getApi, type UpdateState } from '../api'

/**
 * 局域网更新状态。
 *
 * 状态的权威副本在 Go 侧（app.UpdateApp），前端只做两件事：
 *   1. 保存后端推来的快照（事件 update:state）；
 *   2. 把用户动作转成一次 RPC。
 *
 * 刻意不在前端维护「已下载百分比」之类的派生状态：后端每 256 KiB 推一帧
 * 完整快照，两处各算一遍只会出现「进度条 100% 但阶段还是 downloading」。
 */
export const useUpdateStore = defineStore('update', () => {
  const state = ref<UpdateState | null>(null)
  const working = ref(false)

  /** 阶段（未加载时为 idle，避免模板里到处写可选链）。 */
  const phase = computed(() => state.value?.phase ?? 'idle')

  /** 是否该弹确认框（后端已经处理了「稍后提醒」的抑制窗口）。 */
  const promptVisible = computed(() => state.value?.promptPending === true)

  /** 当前平台是否支持自动安装（目前仅 Windows）。 */
  const supported = computed(() => state.value?.platformSupported !== false)

  /** 局域网里版本最高的那个候选源。 */
  const topPeer = computed(() => {
    const list = state.value?.higherPeers ?? []
    return list.length ? list[0] : null
  })

  /** 正在进行中的阶段（设置页据此显示进度与按钮）。 */
  const busy = computed(() =>
    ['checking', 'downloading', 'applying'].includes(String(phase.value))
  )

  function apply(next: UpdateState): void {
    state.value = next
  }

  /** 主动拉一次状态（应用启动、打开设置页时用）。 */
  async function refresh(): Promise<void> {
    const api = await getApi()
    state.value = await api.getUpdateState()
  }

  /** 触发一次后台检查（结果通过事件回来）。 */
  async function checkNow(): Promise<void> {
    if (working.value) return
    working.value = true
    try {
      const api = await getApi()
      state.value = await api.checkUpdateNow()
    } finally {
      working.value = false
    }
  }

  /**
   * 立即安装并重启。
   *
   * 成功后进程很快就会退出，因此不再对返回值做后续处理。
   */
  async function applyNow(): Promise<void> {
    working.value = true
    try {
      const api = await getApi()
      state.value = await api.applyUpdateNow()
    } finally {
      working.value = false
    }
  }

  /** 安排到指定时刻安装（毫秒时间戳）。 */
  async function scheduleAt(atMillis: number): Promise<void> {
    const api = await getApi()
    state.value = await api.scheduleUpdate(atMillis)
  }

  /** 稍后提醒（30 分钟）。 */
  async function snooze(): Promise<void> {
    const api = await getApi()
    state.value = await api.snoozeUpdate()
  }

  /** 放弃本次更新（删除已下载的包）。 */
  async function cancel(): Promise<void> {
    const api = await getApi()
    state.value = await api.cancelUpdate()
  }

  return {
    state,
    working,
    phase,
    promptVisible,
    supported,
    topPeer,
    busy,
    apply,
    refresh,
    checkNow,
    applyNow,
    scheduleAt,
    snooze,
    cancel
  }
})
