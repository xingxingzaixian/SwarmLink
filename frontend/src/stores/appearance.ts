import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { getApi, type Background } from '../api'

/**
 * 自定义背景图。
 *
 * 图片本身【不由前端保存】：它落在数据目录的 backgrounds/ 下，由后端负责
 * 校验（文件头嗅探）、限大小、随配置一起持久化。前端只做两件事：
 *   1. 拿到 data URL 后把它写进 CSS 变量，命中 body.has-app-bg；
 *   2. 把用户选的图读成 base64 交给后端。
 *
 * 为什么不塞 localStorage：背景图常态是 2–8MB，localStorage 配额只有 5MB
 * 且是 base64 存储（再膨胀 33%）；更要命的是它跟着浏览器配置走，
 * 换机器、清缓存就没了 —— 而背景是「用户设置」，应该跟 config.toml 同寿命。
 */
export const useAppearanceStore = defineStore('appearance', () => {
  const state = ref<Background>({
    dataUrl: '',
    fileName: '',
    dim: 45,
    blur: 6,
    maxBytes: 8 << 20
  })

  /** 是否已经加载过（未加载时不要急着渲染「移除背景」这类按钮）。 */
  const ready = ref(false)
  const busy = ref(false)
  const hasBackground = computed(() => state.value.dataUrl !== '')

  /** 正在拖滑杆时不必每一帧都写盘，收手 400ms 后再存。 */
  let lookTimer: ReturnType<typeof window.setTimeout> | null = null

  /**
   * 把当前状态投影到 DOM。
   *
   * 用 body 上的 CSS 变量而不是给每个组件传 prop：面板、气泡、弹层全部
   * 消费 --bg-* 变量，改一处就等于整窗生效，组件不需要知道背景这回事。
   */
  function apply(): void {
    const body = document.body
    const has = state.value.dataUrl !== ''

    body.classList.toggle('has-app-bg', has)

    if (has) {
      body.style.setProperty('--app-bg-image', `url("${state.value.dataUrl}")`)
    } else {
      body.style.removeProperty('--app-bg-image')
    }
    body.style.setProperty('--app-bg-dim', String(state.value.dim / 100))
    body.style.setProperty('--app-bg-blur', `${state.value.blur}px`)
  }

  function set(next: Background): void {
    state.value = next
    apply()
  }

  /** 应用启动时拉一次（没有就什么都不做）。 */
  async function load(): Promise<void> {
    try {
      const api = await getApi()
      set(await api.getBackground())
    } finally {
      // 即使失败也把 ready 置上：否则设置页会一直显示「加载中」
      ready.value = true
    }
  }

  /**
   * 选一张本地图片设为背景。
   *
   * 前端只做「读字节 + 粗校验大小」，类型与真伪交给后端按文件头判定 ——
   * 前端看到的 file.type 是浏览器按扩展名猜的，不足为凭。
   */
  async function chooseFile(file: File): Promise<void> {
    const max = state.value.maxBytes || 8 << 20
    if (file.size > max) {
      throw new Error(`图片太大（${(file.size / 1024 / 1024).toFixed(1)} MB，上限 ${max >> 20} MB）`)
    }
    if (file.type && !file.type.startsWith('image/')) {
      throw new Error('请选择 PNG / JPEG / WebP / GIF 图片')
    }

    busy.value = true
    try {
      const buf = await file.arrayBuffer()
      const ext = (file.name.match(/\.[^.]+$/)?.[0] ?? '.png').toLowerCase()
      const api = await getApi()
      set(await api.saveBackground(toBase64(buf), ext))
    } finally {
      busy.value = false
    }
  }

  async function clear(): Promise<void> {
    busy.value = true
    try {
      const api = await getApi()
      set(await api.clearBackground())
    } finally {
      busy.value = false
    }
  }

  /**
   * 调整遮罩/模糊：本地立即生效，落盘防抖。
   *
   * 拖滑杆时每一帧都发一次 RPC 是没必要的（而且每次都要重写 config.toml）。
   */
  function setLook(dim: number, blur: number): void {
    state.value.dim = clamp(Math.round(dim), 0, 100)
    state.value.blur = clamp(Math.round(blur), 0, 40)
    apply()

    if (lookTimer) window.clearTimeout(lookTimer)
    lookTimer = window.setTimeout(() => {
      lookTimer = null
      void persistLook()
    }, 400)
  }

  /** 真正把遮罩/模糊写回配置（通常由 setLook 的防抖触发）。 */
  async function persistLook(): Promise<void> {
    const api = await getApi()
    set(await api.setBackgroundLook(state.value.dim, state.value.blur))
  }

  return {
    state,
    ready,
    busy,
    hasBackground,
    load,
    chooseFile,
    clear,
    setLook,
    persistLook
  }
})

/**
 * ArrayBuffer → base64。
 *
 * 不能用 `String.fromCharCode(...new Uint8Array(buf))`：5MB 的图有 500 万个
 * 参数，直接爆栈（Chrome 上限约 12 万）。按 32KB 分块是标准做法。
 */
function toBase64(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf)
  const chunk = 0x8000
  let bin = ''
  for (let i = 0; i < bytes.length; i += chunk) {
    bin += String.fromCharCode(...bytes.subarray(i, i + chunk))
  }
  return btoa(bin)
}

function clamp(v: number, lo: number, hi: number): number {
  if (Number.isNaN(v)) return lo
  return Math.min(hi, Math.max(lo, v))
}
