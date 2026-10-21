/**
 * 快捷键的「键位语言」：事件 → 加速键字符串 → 展示文案。
 *
 * 三条必须守住的决定（改这里之前先读一遍）：
 *
 * 1. 【按物理键 e.code 匹配，不按字符 e.key】。
 *    e.key 在大写锁定、Shift+1（结果是 "!"）、中文输入法下都会漂移，
 *    而快捷键一旦偶尔失灵，用户根本没法描述自己遇到了什么。
 *    代价是非 QWERTY 布局（法语 AZERTY）下显示的是物理位置 ——
 *    本项目只有中英文环境，接受这个代价。
 *
 * 2. 【存储格式与 Wails 的加速键语法一致】，只有一点不同：主修饰键一律写
 *    `Ctrl`（macOS 上它代表 ⌘）。不用 Wails 的 `CmdOrCtrl` 是因为它在
 *    Windows 上规范化的结果就是 "Ctrl" —— 存别名、读回来是另一个词，
 *    设置页会显示出用户从没选过的键位。
 *
 * 3. 【键名必须是 Wails 能解析的名字】（`keys.go` 的 namedKeys）：
 *    `up` / `page up` / `escape` / `f1`… 全局热键要原样交给 Go 去注册，
 *    这里写错一个词，表现是「前端显示得好好的，全局键永远绑不上」。
 */

/** 当前平台是否 macOS。只影响展示与主修饰键取 ⌘ 还是 Ctrl。 */
export const isMac =
  typeof navigator !== 'undefined' && /mac/i.test(navigator.platform || navigator.userAgent)

/**
 * e.code → 加速键里的键名（全部小写，与 Wails 的 namedKeys 对齐）。
 *
 * `+` 刻意不在表里：它是加速键的分隔符，一个主键为 `+` 的快捷键
 * 既没法存也没法解析 —— 与其产生一个永远匹配不上的条目，不如不支持。
 */
const CODE_KEYS: Record<string, string> = {
  Escape: 'escape',
  Enter: 'enter',
  NumpadEnter: 'enter',
  Tab: 'tab',
  Backspace: 'backspace',
  Delete: 'delete',
  Space: 'space',
  Home: 'home',
  End: 'end',
  PageUp: 'page up',
  PageDown: 'page down',
  ArrowUp: 'up',
  ArrowDown: 'down',
  ArrowLeft: 'left',
  ArrowRight: 'right',

  Comma: ',',
  Period: '.',
  Slash: '/',
  Backslash: '\\',
  Semicolon: ';',
  Quote: "'",
  BracketLeft: '[',
  BracketRight: ']',
  Minus: '-',
  Equal: '=',
  Backquote: '`'
}

// 字母 / 数字 / 功能键：批量生成，避免手写 26 + 10 + 24 行。
for (let i = 0; i < 26; i++) {
  CODE_KEYS[`Key${String.fromCharCode(65 + i)}`] = String.fromCharCode(97 + i)
}
for (let i = 0; i <= 9; i++) {
  CODE_KEYS[`Digit${i}`] = String(i)
}
for (let i = 1; i <= 24; i++) {
  CODE_KEYS[`F${i}`] = `f${i}`
}

/** 修饰键的规范顺序：比较两个加速键时不能受书写顺序影响。 */
const MODIFIER_ORDER = ['ctrl', 'alt', 'shift'] as const

/**
 * 从键盘事件推出加速键字符串；推不出来（例如纯修饰键、未知键）时返回空串。
 *
 * 主修饰键的取值规则：macOS 上 ⌘ 与 Ctrl 都算「Ctrl」。
 * 这与迁移前的 `e.ctrlKey || e.metaKey`（App.vue 里的 Ctrl+K）保持同一语义 ——
 * 若分成两个键，用户从 Windows 换到 mac 时所有快捷键都会失效。
 */
export function eventAccel(e: KeyboardEvent): string {
  const key = keyFromEvent(e)
  if (!key) return ''

  const mods: string[] = []
  if (isMac ? e.metaKey || e.ctrlKey : e.ctrlKey) mods.push('Ctrl')
  if (e.altKey) mods.push('Alt')
  if (e.shiftKey) mods.push('Shift')
  mods.push(key)
  return mods.join('+')
}

/** 取主键名：物理键优先，未知物理键时退回字符。 */
function keyFromEvent(e: KeyboardEvent): string {
  const byCode = CODE_KEYS[e.code]
  if (byCode) return byCode

  // Numpad 数字：e.code 是 Numpad1，物理位置明确，按数字键处理。
  const numpad = /^Numpad([0-9])$/.exec(e.code)
  if (numpad) return numpad[1]

  // 兜底：单字符键（虚拟键盘、输入法注入的事件可能没有 code）。
  if (e.key && e.key.length === 1) return e.key.toLowerCase()

  return ''
}

/**
 * 规范化加速键，用于【比较】而不是存储。
 *
 * 做三件事：小写化、修饰键按固定顺序重排、把各种别名收敛成 ctrl/alt/shift。
 * 不做的话，配置文件里手写一个 `Shift+Ctrl+H` 就会匹配不上默认的 `Ctrl+Shift+H`。
 */
export function normalizeAccel(accel: string): string {
  if (!accel) return ''
  const parts = accel
    .split('+')
    .map((p) => p.trim().toLowerCase())
    .filter(Boolean)
  if (parts.length < 1) return ''

  const key = parts.pop() as string
  const mods = new Set<string>()
  for (const m of parts) {
    if (m === 'cmd' || m === 'command' || m === 'cmdorctrl' || m === 'control') mods.add('ctrl')
    else if (m === 'option' || m === 'optionoralt') mods.add('alt')
    else mods.add(m)
  }
  const ordered = MODIFIER_ORDER.filter((m) => mods.has(m))
  return [...ordered, key].join('+')
}

/** 是否含主修饰键（Ctrl / Alt）。只有 Shift 的快捷键等同裸键。 */
export function hasPrimaryModifier(accel: string): boolean {
  const n = normalizeAccel(accel)
  return n.startsWith('ctrl+') || n.includes('+ctrl+') || n.startsWith('alt+') || n.includes('+alt+')
}

/** 主键是否是 Esc 或 F1-F24（这两类键在输入框里不会与打字冲突）。 */
export function isBareAllowedKey(accel: string): boolean {
  const n = normalizeAccel(accel)
  const key = n.split('+').pop() ?? ''
  return key === 'escape' || /^f([1-9]|1[0-9]|2[0-4])$/.test(key)
}

/** 主键名（展示与校验用）。 */
export function accelKey(accel: string): string {
  const n = normalizeAccel(accel)
  return n.split('+').pop() ?? ''
}

/** 两条加速键是否相同（忽略书写顺序与大小写）。 */
export function sameAccel(a: string, b: string): boolean {
  const na = normalizeAccel(a)
  return na !== '' && na === normalizeAccel(b)
}

/**
 * 展示文案：Windows 是 `Ctrl+Shift+A`，macOS 是 `⌘⇧A`。
 *
 * 只用于界面，不参与匹配 —— 展示层改文案不该影响任何行为。
 */
export function formatAccel(accel: string): string {
  if (!accel) return '未绑定'
  const n = normalizeAccel(accel)
  const parts = n.split('+')
  const key = parts.pop() ?? ''
  const mods = new Set(parts)

  const labels: string[] = []
  if (mods.has('ctrl')) labels.push(isMac ? '⌘' : 'Ctrl')
  if (mods.has('alt')) labels.push(isMac ? '⌥' : 'Alt')
  if (mods.has('shift')) labels.push(isMac ? '⇧' : 'Shift')
  labels.push(formatKey(key))
  return labels.join(isMac ? '' : '+')
}

/** 主键的展示名。方向键与空白键用符号，比 `Page up` 这类文字好认。 */
function formatKey(key: string): string {
  switch (key) {
    case 'up':
      return '↑'
    case 'down':
      return '↓'
    case 'left':
      return '←'
    case 'right':
      return '→'
    case 'escape':
      return 'Esc'
    case 'enter':
      return 'Enter'
    case 'space':
      return 'Space'
    case 'page up':
      return 'PgUp'
    case 'page down':
      return 'PgDn'
    case 'backspace':
      return 'Backspace'
    case 'delete':
      return 'Delete'
    default:
      return key.length === 1 ? key.toUpperCase() : key.charAt(0).toUpperCase() + key.slice(1)
  }
}

/**
 * 事件目标是否是一个「正在被打字」的输入区域。
 *
 * 用 e.target 而不是 document.activeElement：keydown 的 target 就是当前
 * 聚焦元素，而 activeElement 在弹层里可能还停在别处（弹层会把焦点给 dialog）。
 */
export function isEditableTarget(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null
  if (!el || !el.tagName) return false
  const tag = el.tagName.toLowerCase()
  if (tag === 'input' || tag === 'textarea' || tag === 'select') return true
  return el.isContentEditable === true
}
