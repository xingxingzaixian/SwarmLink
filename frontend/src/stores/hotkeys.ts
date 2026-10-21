import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { getApi, isMockMode, type GlobalShortcutState, type ShortcutsSnapshot } from '../api'
import { COMMANDS, WINDOW_COMMANDS, type HotkeyCommand } from '../hotkeys/commands'
import { formatAccel, normalizeAccel } from '../hotkeys/keys'

/**
 * 快捷键状态：用户覆盖值 + 全局热键的注册现状。
 *
 * 这里刻意【不存默认键位】：默认值属于命令表（窗口内）与 Go 的
 * config.GlobalShortcutDefaults（全局）。只存「用户改过的」这一半，
 * 升级后默认键位变了才能传导到没改过的用户身上。
 */
export const useHotkeysStore = defineStore('hotkeys', () => {
  /** 命令 id → 加速键（空串 = 用户主动解绑）。只含改过的项。 */
  const overrides = ref<Record<string, string>>({})
  /** 全局热键的注册现状（由 Go 侧返回，含“被其他程序占用”这类失败）。 */
  const global = ref<GlobalShortcutState[]>([])

  const ready = ref(false)
  const loading = ref(false)
  /** 浏览器预览：没有 Go，全局热键不可能注册成功，界面要如实说明。 */
  const mock = ref(false)
  const error = ref('')

  /** 窗口内命令的生效键位（覆盖值优先，其次命令表默认值）。 */
  const effective = computed<Record<string, string>>(() => {
    const map: Record<string, string> = {}
    for (const cmd of WINDOW_COMMANDS) {
      const v = overrides.value[cmd.id]
      map[cmd.id] = v !== undefined ? v : cmd.defaultAccel
    }
    return map
  })

  /** 某条命令当前绑的键（global 命令的值来自 Go）。 */
  function boundOf(cmd: HotkeyCommand): string {
    if (cmd.scope === 'global') {
      return global.value.find((g) => g.id === cmd.id)?.accelerator ?? ''
    }
    return effective.value[cmd.id] ?? ''
  }

  /** 某条全局命令的注册失败原因（窗口内命令恒为空）。 */
  function errorOf(id: string): string {
    return global.value.find((g) => g.id === id)?.error ?? ''
  }

  /**
   * 查冲突：返回第一个与该键重复的命令 id。
   *
   * 必须在保存前查 —— 后端不做跨命令去重（它只知道全局那两个），
   * 真要绑成一样，表现是「按下去只有一个命令响应」，用户完全无从判断。
   */
  function conflictOf(accel: string, exceptId: string): HotkeyCommand | null {
    const target = normalizeAccel(accel)
    if (!target) return null
    for (const cmd of COMMANDS) {
      if (cmd.id === exceptId || cmd.locked) continue
      if (normalizeAccel(boundOf(cmd)) === target) return cmd
    }
    return null
  }

  /**
   * 给按钮 title 加键位后缀（`发送文件（Ctrl+O）`）。
   *
   * 没绑定就不加后缀 —— 空括号比没有提示更让人困惑。
   * 展示文案随平台变（mac 是 ⌘），因此不能在模板里硬编码字符串。
   */
  function withHint(label: string, id: string): string {
    const cmd = COMMANDS.find((c) => c.id === id)
    const accel = cmd ? boundOf(cmd) : ''
    return accel ? `${label}（${formatAccel(accel)}）` : label
  }

  function applySnapshot(snap: ShortcutsSnapshot): void {
    overrides.value = { ...snap.overrides }
    global.value = snap.global
  }

  /** 读一次配置（App 启动时调，dispatcher 靠它拿到覆盖值）。 */
  async function load(): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      const api = await getApi()
      mock.value = isMockMode()
      applySnapshot(await api.readShortcuts())
      ready.value = true
    } catch (e: any) {
      // 读不到就只能用默认键位：快捷键是附加能力，不该挡住应用启动。
      error.value = String(e?.message ?? e) || '读取快捷键设置失败'
    } finally {
      loading.value = false
    }
  }

  async function persist(next: Record<string, string>): Promise<boolean> {
    loading.value = true
    error.value = ''
    try {
      const api = await getApi()
      applySnapshot(await api.applyShortcuts(next))
      return true
    } catch (e: any) {
      error.value = String(e?.message ?? e) || '保存快捷键失败'
      return false
    } finally {
      loading.value = false
    }
  }

  /**
   * 改一条命令的键位。
   *
   * `accel` 为空串 = 解绑（要落盘，否则重启后默认键又回来了）；
   * 等于默认值 = 删掉覆盖项（这样将来默认值变了他能收到）。
   */
  async function setAccel(id: string, accel: string): Promise<boolean> {
    const cmd = COMMANDS.find((c) => c.id === id)
    if (!cmd) return false
    const next = { ...overrides.value }
    if (accel === '' || accel === cmd.defaultAccel) {
      delete next[id]
      if (accel === '') next[id] = ''
    } else {
      next[id] = accel
    }
    return persist(next)
  }

  /** 清空全部覆盖值，回到出厂键位。 */
  async function resetAll(): Promise<boolean> {
    loading.value = true
    error.value = ''
    try {
      const api = await getApi()
      applySnapshot(await api.resetShortcuts())
      return true
    } catch (e: any) {
      error.value = String(e?.message ?? e) || '恢复默认快捷键失败'
      return false
    } finally {
      loading.value = false
    }
  }

  return {
    overrides,
    global,
    ready,
    loading,
    mock,
    error,
    effective,
    boundOf,
    errorOf,
    conflictOf,
    withHint,
    load,
    setAccel,
    resetAll
  }
})
