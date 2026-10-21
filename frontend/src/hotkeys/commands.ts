/**
 * 快捷键命令表 —— 全应用唯一的「有哪些快捷键」清单。
 *
 * 设置页的编辑列表、Ctrl+/ 的一览、dispatcher 的派发全部读这一张表，
 * 不存在第二份手写副本（手写副本必然腐化：加了命令没人记得同步清单）。
 *
 * 两类命令必须分清：
 *   - `window` —— 本文件负责执行，dispatcher 匹配到就跑 `run`；
 *   - `global` —— 由 Go 侧注册系统级热键（应用失焦也生效），这里只负责
 *     「在设置页画出来」，`run` 为空。它们的默认键位在 Go 的
 *     config.GlobalShortcutDefaults，因为应用启动、前端还没加载时就要注册。
 */

import { useChatStore } from '../stores/chat'
import { useUiStore } from '../stores/ui'
import { normalizeAccel } from './keys'

/** 分组：设置页与一览按它分段，顺带决定用户找一条命令时的心理路径。 */
export type HotkeyGroup = 'session' | 'panel' | 'help' | 'global'

/** 命令的作用域。global 由 Go 执行，window 由前端 dispatcher 执行。 */
export type HotkeyScope = 'window' | 'global'

export interface HotkeyCommand {
  id: string
  label: string
  group: HotkeyGroup
  scope: HotkeyScope
  /** 出厂键位。global 命令留空 —— 它们的默认值在 Go 侧。 */
  defaultAccel: string
  /** 固定键位，不允许在设置页修改（目前只有 Esc：它是逃逸键，改掉会把人锁死）。 */
  locked?: boolean
  /**
   * 有任何弹层/浮层打开时禁用这条命令。
   *
   * 针对的是「作用对象被遮住了」的两类动作：切会话、聚焦左栏搜索框。
   * 弹层下面那层列表用户根本看不见，切了也只会让他以为按键失灵。
   * 面板类命令反过来要保留 —— 用 Ctrl+J 直接换一个面板，比先 Esc 再按更顺。
   */
  overlayBlocked?: boolean
  /** window 命令的执行体；global 命令为 undefined。 */
  run?: () => void
}

/**
 * 切到上/下一个会话。
 *
 * 顺序取会话列表的当前顺序（左栏看到什么就是什么），到两端就绕回去 ——
 * 切会话是个高频、可重复的动作，卡在最后一条不动反而让人以为按键失灵。
 * 当前没有打开任何会话时，从第一个（或最后一个）开始。
 */
function stepConversation(delta: 1 | -1): void {
  const chat = useChatStore()
  const list = chat.conversations
  if (!list.length) return

  const i = list.findIndex((c) => c.convId === chat.activeConvId)
  const next = i < 0 ? (delta > 0 ? 0 : list.length - 1) : (i + delta + list.length) % list.length
  void chat.open(list[next])
}

export const COMMANDS: HotkeyCommand[] = [
  {
    id: 'search',
    label: '搜索聊天记录',
    group: 'session',
    scope: 'window',
    defaultAccel: 'Ctrl+K',
    run: () => useUiStore().openSearch('')
  },
  {
    id: 'next_conv',
    label: '下一个会话',
    group: 'session',
    scope: 'window',
    defaultAccel: 'Alt+Down',
    overlayBlocked: true,
    run: () => stepConversation(1)
  },
  {
    id: 'prev_conv',
    label: '上一个会话',
    group: 'session',
    scope: 'window',
    defaultAccel: 'Alt+Up',
    overlayBlocked: true,
    run: () => stepConversation(-1)
  },
  {
    id: 'focus_search',
    label: '定位到左栏搜索框',
    group: 'session',
    scope: 'window',
    defaultAccel: 'Ctrl+F',
    overlayBlocked: true,
    run: () => useUiStore().focusSearchBox()
  },
  {
    id: 'transfers',
    label: '文件传输面板',
    group: 'panel',
    scope: 'window',
    defaultAccel: 'Ctrl+J',
    run: () => useUiStore().openPanel('transfers')
  },
  {
    id: 'share',
    label: '共享中心',
    group: 'panel',
    scope: 'window',
    defaultAccel: 'Ctrl+Shift+H',
    run: () => useUiStore().openPanel('share')
  },
  {
    id: 'settings',
    label: '设置',
    group: 'panel',
    scope: 'window',
    defaultAccel: 'Ctrl+,',
    run: () => useUiStore().openPanel('settings')
  },
  {
    id: 'help',
    label: '快捷键一览',
    group: 'help',
    scope: 'window',
    defaultAccel: 'Ctrl+/',
    run: () => useUiStore().toggleHelp()
  },
  // ---------------------------------------------------------------------
  // 全局热键：id 必须与 Go 侧 wadapter.GlobalShortcutIDs 一致。
  // ---------------------------------------------------------------------
  {
    id: 'screenshot',
    label: '截图（系统级）',
    group: 'global',
    scope: 'global',
    defaultAccel: ''
  },
  {
    id: 'toggle_window',
    label: '唤起 / 收起主窗口（系统级）',
    group: 'global',
    scope: 'global',
    defaultAccel: ''
  }
]

/** 分组标题，用于设置页与一览。 */
export const GROUP_LABELS: Record<HotkeyGroup, string> = {
  global: '全局（应用在后台也生效）',
  session: '会话',
  panel: '面板',
  help: '帮助'
}

/** 只含窗口内命令（dispatcher 与设置页的“窗口内”段用）。 */
export const WINDOW_COMMANDS = COMMANDS.filter((c) => c.scope === 'window')

/**
 * 按加速键找命令。
 *
 * `effective` 是「命令 id → 实际生效键位」（覆盖值 ?? 默认值），由 store 算好。
 * 比较走 normalizeAccel：书写顺序、大小写在匹配时不该有任何影响。
 */
export function commandByAccel(
  effective: Record<string, string>,
  accel: string
): HotkeyCommand | undefined {
  const target = normalizeAccel(accel)
  if (!target) return undefined
  for (const cmd of WINDOW_COMMANDS) {
    const bound = effective[cmd.id]
    if (bound && normalizeAccel(bound) === target) return cmd
  }
  return undefined
}
