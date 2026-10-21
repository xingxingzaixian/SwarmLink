import { defineStore } from 'pinia'
import { ref } from 'vue'

import type { Screenshot } from '../api/types'

/**
 * 两个截屏模式。
 *
 * 与后端 ports.ScreenshotModeKeepWindow / ScreenshotModeHideWindow 一一对应。
 * 这里写字面量而不是从后端 import：前后端的常量本来就靠约定对齐，
 * 跨语言 import 反而绕远。
 *
 * 截屏是常规功能，没有开关、也没有「记住上次用哪个」—— 两个模式各有
 * 自己的按钮，用户点哪个就是哪个。
 */
export const SHOT_KEEP = 'keep_window'
export const SHOT_HIDE = 'hide_window'

/**
 * 截屏草稿：还没发出去的那一张。
 *
 * 它【必须】在 store 里而不是聊天组件的局部状态：草稿跨窗口 —— 覆盖层
 * 窗口那边提交后由事件送回来，局部状态接不住。
 *
 * 进到草稿就算「截屏完成」：发不发给谁，是用户后面那一下的事。
 */
export const useScreenshotStore = defineStore('screenshot', () => {
  /** 待发送的截图草稿（null = 没有草稿）。 */
  const draft = ref<Screenshot | null>(null)

  /** 放下草稿。 */
  function setDraft(s: Screenshot | null): void {
    draft.value = s
  }

  /** 丢掉草稿。 */
  function clearDraft(): void {
    draft.value = null
  }

  return { draft, setDraft, clearDraft }
})
