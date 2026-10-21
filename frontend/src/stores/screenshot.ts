import { defineStore } from 'pinia'
import { ref } from 'vue'

import { getApi } from '../api'
import type { Screenshot } from '../api/types'

/**
 * 截屏模式。
 *
 * 与后端 ports.ScreenshotModeInteractive / ScreenshotModeKeepWindow /
 * ScreenshotModeHideWindow 一一对应。
 * 这里写字面量而不是从后端 import：前后端的常量本来就靠约定对齐，
 * 跨语言 import 反而绕远。
 */
export const SHOT_INTERACTIVE = 'interactive'
export const SHOT_KEEP = 'keep_window'
export const SHOT_HIDE = 'hide_window'

/** 三个合法模式。 */
const ALL_MODES: readonly string[] = [SHOT_INTERACTIVE, SHOT_KEEP, SHOT_HIDE]

/**
 * 截屏的偏好设置：允不允许截屏、上次用哪种模式。
 *
 * 这两个值有写者也有读者（设置页写「允不允许」，聊天页写「上次哪种」），
 * 放进一个 store，两边就不会各读一份、各写一份而互相覆盖。
 *
 * 草稿（还没发出去的那张截图）刻意不放这里 —— 它是「给某一个会话看的」，
 * 切换会话时应当丢弃，留在聊天组件的局部状态里更安全。
 */
export const useScreenshotStore = defineStore('screenshot', () => {
  /** 总开关：设置页里的「允许截屏」。 */
  const enabled = ref(true)
  /** 上次使用的模式（interactive | keep_window | hide_window）。 */
  const mode = ref(SHOT_INTERACTIVE)
  /**
   * 待发送的截图草稿（null = 没有草稿）。
   *
   * 它【必须】在 store 里而不是聊天组件的局部状态：草稿有两个来源 ——
   * 一键整屏截图（同一个窗口内直接拿到）与交互式截图（覆盖层窗口
   * 那边提交后由事件送回来）。后者跨窗口，局部状态接不住。
   */
  const draft = ref<Screenshot | null>(null)

  /** 放下草稿。 */
  function setDraft(s: Screenshot | null): void {
    draft.value = s
  }

  /** 丢掉草稿。 */
  function clearDraft(): void {
    draft.value = null
  }

  /**
   * 从设置里同步这两个值。
   *
   * 读不到就保持当前值：后端还有一道总开关检查，
   * 真正的拒绝会给出一句明确原因，而不是静默失败。
   */
  async function syncFromSettings(): Promise<void> {
    try {
      const api = await getApi()
      const s = await api.getSettings()
      enabled.value = s.screenshotEnabled !== false
      if (ALL_MODES.includes(s.screenshotLastMode)) {
        mode.value = s.screenshotLastMode
      }
    } catch {
      // 保持当前值不变
    }
  }

  /**
   * 记住用户显式选择的模式。
   *
   * 失败不抛：模式没记住只是「下次又要选一遍」，不该打断当前这次截屏。
   */
  async function setMode(m: string): Promise<void> {
    const next = ALL_MODES.includes(m) ? m : SHOT_INTERACTIVE
    mode.value = next
    try {
      const api = await getApi()
      await api.rememberScreenshotMode(next)
    } catch {
      // 忽略：模式没落盘不影响本次使用
    }
  }

  return { enabled, mode, draft, syncFromSettings, setMode, setDraft, clearDraft }
})
