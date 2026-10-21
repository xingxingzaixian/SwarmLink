import { defineStore } from 'pinia'
import { ref } from 'vue'

/** 全局功能面板。它们与「当前会话」无关，因此不占右栏，改用弹层。 */
export type PanelKind = '' | 'settings' | 'debug' | 'transfers' | 'share' | 'search'

/** 右栏的两种形态。 */
export type SideMode = 'info' | 'transfer'

/** 视觉主题。dark = 饱和渐变玻璃；light = QQ 式浅色玻璃。 */
export type Theme = 'dark' | 'light'

const THEME_KEY = 'swarmlink.theme'

/**
 * 界面外壳状态：弹层、右栏形态、轻提示。
 *
 * 单独一个 store 是因为这三件事都需要被【相隔较远的组件】同时读写：
 * 左栏底部的头像菜单要开弹层，聊天区的「发送文件」要把右栏切到传输，
 * 而右栏自己要根据是否有活跃任务自动切换 —— 靠组件间传参会把这条链路绕得很长。
 */
/**
 * 读取上次选择的主题。
 * 默认 dark（项目的默认观感），只有明确存过 light 才切过去。
 * 存储不可用（隐私模式 / 被禁）时静默退回默认 —— 主题失效不该挡住应用启动。
 */
function storedTheme(): Theme {
  try {
    return window.localStorage.getItem(THEME_KEY) === 'light' ? 'light' : 'dark'
  } catch {
    // 隐私模式 / 存储被禁：退回默认，不影响使用
    return 'dark'
  }
}

/** 把主题写到 <html data-theme> —— 全应用【唯一】改这个属性的地方。 */
function applyTheme(t: Theme): void {
  document.documentElement.dataset.theme = t
}

/** 记住选择。失败不影响主题生效。 */
function persistTheme(t: Theme): void {
  try {
    window.localStorage.setItem(THEME_KEY, t)
  } catch {
    /* 记不住就算了，本次会话仍然生效 */
  }
}

export const useUiStore = defineStore('ui', () => {
  const panel = ref<PanelKind>('')
  const sideMode = ref<SideMode>('info')

  /**
   * 用户是否手动接管了右栏。
   *
   * 一旦手动切回「资料」，后续的自动切换就不再把面板抢走 ——
   * 否则用户正在看节点信息时被进度条顶掉，是很烦人的体验。
   * 只有「主动发起一次传输」才会重新解除接管。
   */
  const pinned = ref(false)

  /**
   * 手动填文件路径的兜底输入是否可见。
   *
   * 放在 store 而不是聊天组件里：触发它的入口有两个（聊天工具栏的回形针、
   * 右栏资料卡的「发送文件」），但输入框只画在聊天区一处 ——
   * 状态必须比组件活得久，否则从右栏触发时输入框根本不会出现。
   */
  const pathInputVisible = ref(false)

  /**
   * 主题。
   *
   * 关键：在 store 创建时就【同步】把属性写到 <html> 上，而不是等某个组件
   * mount 后由 watch 去写。早期版本正是后者，于是主题成了「组件生命周期 +
   * 响应式」的派生结果 —— 一旦两者不同步（HMR 保留旧 store 状态、组件重挂载），
   * 就会表现成「localStorage 说 dark、页面是 light，点一次没反应、点两次才对」：
   * 第一次点击只是把 store 追平到 DOM，第二次才真正发生改变。
   *
   * 现在 store 是唯一写者，状态变更与 DOM 写入在同一处完成，不可能错位。
   */
  const theme = ref<Theme>(storedTheme())
  applyTheme(theme.value)

  const toast = ref('')
  let toastTimer: number | undefined

  /**
   * 历史搜索弹层的初始关键词。
   *
   * 左栏顶部那个输入框既是「联系人筛选」，也可以当作「去聊天记录里找」的入口
   * （按回车）。两条路共用一个输入值，而弹层是 Teleport 到 body 的、与左栏没有
   * 父子关系 —— 这个值只能从 store 递过去。
   */
  const searchSeed = ref('')

  /**
   * 「把焦点送到左栏搜索框」的请求计数。
   *
   * 快捷键（Ctrl+F）与左栏是两个没有父子关系的组件，前端也没有跨组件的
   * 焦点 API，因此用一个自增计数当信号：ContactPanel 盯它一眼，看到变化
   * 就 focus 自己的输入框。比把 input 的 ref 提到 store 里干净得多。
   */
  const searchFocusTick = ref(0)

  function focusSearchBox(): void {
    searchFocusTick.value++
  }

  /**
   * 快捷键一览浮层是否可见（Ctrl+/）。
   *
   * 它不走 panel：panel 是「设置 / 传输 / 共享」那种有归属的功能弹层，
   * 一览是随时可开、Esc 即走的参考页，压在什么上面都合理。
   */
  const helpVisible = ref(false)

  function toggleHelp(force?: boolean): void {
    helpVisible.value = force ?? !helpVisible.value
  }

  /**
   * 配置被「非设置页」的入口改过的次数。
   *
   * 存在的理由：提示开关有两个入口（设置页、托盘菜单）。托盘改完后端会推
   * config:changed，但设置页持有的是自己那份快照 —— 不重读就会出现
   * 「设置页显示已关、实际还在响」，而用户接着点一次「保存」又会把托盘那次
   * 改动覆盖回去。用一个自增计数把这次变化告诉设置页，比引入一个全局
   * settings store 便宜得多。
   */
  const configRevision = ref(0)

  function bumpConfigRevision(): void {
    configRevision.value++
  }

  function openPanel(kind: Exclude<PanelKind, ''>): void {
    panel.value = kind
  }

  /**
   * 打开「搜索聊天记录」弹层。
   *
   * 与 openPanel 分开是因为它需要一个初始关键词；而且所有入口（左栏搜索框回车、
   * Ctrl+K、本人菜单）都必须走它 —— 只要有一个入口忘了给它，就会读到上一次
   * 残留的关键词，表现为「按 Ctrl+K 出来的是上次搜的词」。
   */
  function openSearch(seed: string): void {
    searchSeed.value = seed.trim()
    panel.value = 'search'
  }

  function setTheme(next: Theme): void {
    theme.value = next
    applyTheme(next)
    persistTheme(next)
  }

  /**
   * 切换主题。
   *
   * 基准取「DOM 上当前实际生效的值」，而不是 store 的值：
   * 这样即使 data-theme 被外部改过（调试、浏览器扩展、遗留状态），
   * 第一次点击也一定产生可见变化，而不是先空点一次去做对齐。
   */
  function toggleTheme(): void {
    const applied: Theme = document.documentElement.dataset.theme === 'light' ? 'light' : 'dark'
    setTheme(applied === 'dark' ? 'light' : 'dark')
  }

  function closePanel(): void {
    panel.value = ''
    // 顺手清掉搜索的初始关键词：弹层已经卸载，留着它只会在下次
    // 由别的入口打开时重新冒出来。
    searchSeed.value = ''
  }

  /** 展示传输：由「发送文件」等主动行为触发，因此解除手动接管。 */
  function focusTransfer(): void {
    sideMode.value = 'transfer'
    pinned.value = false
  }

  /** 用户手动切回资料：接管右栏，交由用户决定看什么。 */
  function focusInfo(): void {
    sideMode.value = 'info'
    pinned.value = true
  }

  /** 切换会话时重置右栏：没有活跃任务就不该停在传输页。 */
  function resetSide(hasActiveTransfer: boolean): void {
    pinned.value = false
    sideMode.value = hasActiveTransfer ? 'transfer' : 'info'
  }

  function showPathInput(): void {
    pathInputVisible.value = true
  }

  function hidePathInput(): void {
    pathInputVisible.value = false
  }

  /** 轻提示。只用于「动作已生效」这类无需用户决策的反馈。 */
  function notify(text: string): void {
    toast.value = text
    if (toastTimer) window.clearTimeout(toastTimer)
    toastTimer = window.setTimeout(() => {
      toast.value = ''
    }, 2200)
  }

  return {
    panel,
    sideMode,
    pinned,
    theme,
    setTheme,
    toggleTheme,
    pathInputVisible,
    toast,
    searchSeed,
    searchFocusTick,
    helpVisible,
    toggleHelp,
    focusSearchBox,
    configRevision,
    bumpConfigRevision,
    openPanel,
    openSearch,
    closePanel,
    focusTransfer,
    focusInfo,
    resetSide,
    showPathInput,
    hidePathInput,
    notify
  }
})
