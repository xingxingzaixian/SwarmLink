/**
 * 快捷键派发器 —— 主窗口里【唯一】的 window keydown 监听。
 *
 * 为什么必须收成一个：迁移前各组件各挂一个监听（Modal 一个、表情一个、
 * 更新框一个），层级关系只能靠「注册顺序 + capture/bubble」凑出来。
 * 实际后果是 `UpdatePrompt` 的捕获阶段监听没检查自己可不可见，
 * 把 Esc 永久吞掉了 —— 弹层从此关不掉，而没人能从代码上看出这一点。
 *
 * 现在的模型是显式的：
 *   - 组件用 pushHotkeyLayer 声明自己这一层，Esc 只给「最上面那一层」；
 *   - exclusive 层（更新框 / 表情 / 一览）独占键盘，命令键一律不派发；
 *   - 命令匹配、输入框判定、输入法组合态全部在这一个地方判定。
 *
 * 监听挂在 window 的【冒泡】阶段：组件自己写在元素上的 @keydown（比如输入框
 * 里 @提示的 Esc、编辑群名时的回车）先跑，跑完若调了 preventDefault，
 * 这里就不再插手 —— 局部语义永远优先于全局快捷键。
 */

import { useHotkeysStore } from '../stores/hotkeys'
import { commandByAccel } from './commands'
import {
  eventAccel,
  hasPrimaryModifier,
  isEditableTarget,
  normalizeAccel
} from './keys'

/** 一层键盘上下文。目前只关心「Esc 归谁」以及「是否独占」。 */
export interface HotkeyLayer {
  /** Esc 落到这一层时做什么。没有它，Esc 就继续往下沉。 */
  onEsc?: () => void
  /**
   * 独占：只要这一层在栈里，除 Esc 以外的命令键一律不派发。
   *
   * 用于「必须先把这件事处理掉」的界面（更新确认框）与「正在选择」的
   * 临时浮层（表情面板）：这时候按 Ctrl+K 把搜索弹出来，只会让用户
   * 面对两个都想关闭的层。
   */
  exclusive?: boolean
  /**
   * 独占层仍然放行的命令 id。
   *
   * 有它是因为「打开自己的那个键，应该也能关掉自己」：一览浮层由 Ctrl+/
   * 打开，若连它自己都被独占挡住，用户按第二下会发现没反应 —— 而界面上
   * 唯一的出路只剩 Esc，这不符合任何人的肌肉记忆。
   */
  allow?: string[]
}

interface StackedLayer extends HotkeyLayer {
  /** 用 token 而不是索引标识自己：下层先关会让索引错位。 */
  token: symbol
}

const stack: StackedLayer[] = []

/**
 * 压入一层，返回「弹出这一层」的函数。
 *
 * 组件应在 onMounted / 弹层出现时压入、onBeforeUnmount / 消失时弹出。
 * 返回值是唯一正确的弹出方式 —— 直接按索引删会删掉别人的层。
 */
export function pushHotkeyLayer(layer: HotkeyLayer): () => void {
  const token = Symbol('hotkey-layer')
  stack.push({ ...layer, token })
  return () => {
    const i = stack.findIndex((l) => l.token === token)
    if (i >= 0) stack.splice(i, 1)
  }
}

/** 当前最上层里第一个认领 Esc 的层。 */
function topEscLayer(): StackedLayer | undefined {
  for (let i = stack.length - 1; i >= 0; i--) {
    if (stack[i].onEsc) return stack[i]
  }
  return undefined
}

/** 该命令是否被独占层挡住（独占层的 allow 白名单里的命令不受影响）。 */
function blockedByExclusive(id: string): boolean {
  const exclusive = stack.filter((l) => l.exclusive)
  if (!exclusive.length) return false
  return !exclusive.some((l) => l.allow?.includes(id))
}

function onKeyDown(e: KeyboardEvent): void {
  // 局部处理已经消费了这次按键（输入框里的 @候选、群名编辑的回车…）。
  if (e.defaultPrevented) return

  // 输入法组合期一律不拦：此时回车是选词、Esc 是取消输入，
  // 抢过来就会出现「选了半个字却被当成关闭弹层」。
  if (e.isComposing || e.keyCode === 229) return

  const accel = eventAccel(e)
  if (!accel) return

  if (normalizeAccel(accel) === 'escape') {
    const layer = topEscLayer()
    if (!layer) return // 没有层要关：交给组件自己的局部处理
    e.preventDefault()
    layer.onEsc?.()
    return
  }

  const store = useHotkeysStore()
  const cmd = commandByAccel(store.effective, accel)
  if (!cmd) return

  if (blockedByExclusive(cmd.id)) return

  // 有弹层挡着时，「切会话 / 聚焦左栏」这类命令的对象用户根本看不见。
  // 面板类命令不在此列：Ctrl+J 直接把当前面板换掉，比先 Esc 再按顺手。
  if (cmd.overlayBlocked && stack.length > 0) return

  // 输入框里只放行带主修饰键的组合。
  // 这条规则挡住了「绑一个裸字母键 → 打字时触发命令」这类事故；
  // Esc 与 F1-F12 在配置层就已经被拒绝，因此这里不必再特判。
  if (isEditableTarget(e.target) && !hasPrimaryModifier(accel)) return

  // 长按重复只对「切会话」放行：按住连切是符合直觉的，
  // 而按住连开搜索弹层/连按设置则只会让人以为程序卡了。
  if (e.repeat && cmd.id !== 'next_conv' && cmd.id !== 'prev_conv') return

  e.preventDefault()
  cmd.run?.()
}

let installed = false

/** 安装全局监听（App.vue 挂载时调一次）。重复调用是安全的。 */
export function installHotkeys(): void {
  if (installed) return
  installed = true
  window.addEventListener('keydown', onKeyDown)
}

/** 卸载（App.vue 卸载时调）。 */
export function uninstallHotkeys(): void {
  if (!installed) return
  installed = false
  window.removeEventListener('keydown', onKeyDown)
  // 栈必须一起清掉：热重载后残留的层会让新实例的 Esc 永远落到旧组件上。
  stack.length = 0
}
