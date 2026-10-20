<script setup lang="ts">
/**
 * 应用外壳：三栏 + 全局浮层。
 *
 *   左 = 联系人（含本人入口）  中 = 聊天  右 = 资料 / 传输
 *
 * 背景不再是「氛围层 + 毛玻璃」两层，而是 body 上的 mesh 光晕 + 中栏自己的
 * 网格点阵（见 style.css 与 ChatView.vue）。因此这里只负责一件事：
 * 三栏的栅格与降级。布局不做任何业务判断，跨栏联动全部走 store。
 */
import { onMounted, onUnmounted, ref, watch } from 'vue'

import ChatView from './views/ChatView.vue'
import ContactPanel from './components/ContactPanel.vue'
import SidePanel from './components/SidePanel.vue'
import TransferQueueBar from './components/TransferQueueBar.vue'
import UpdatePrompt from './components/UpdatePrompt.vue'
import SearchPanel from './views/SearchPanel.vue'
import SettingsDialog from './views/SettingsDialog.vue'
import ShareDialog from './views/ShareDialog.vue'
import { getApi, isMockMode } from './api'
import { useChatStore } from './stores/chat'
import { useGroupStore } from './stores/group'
import { usePeersStore } from './stores/peers'
import { useTransferStore } from './stores/transfer'
import { useUiStore } from './stores/ui'
import { useUpdateStore } from './stores/update'
import { useAppearanceStore } from './stores/appearance'

const peers = usePeersStore()
const chat = useChatStore()
const transfers = useTransferStore()
const groups = useGroupStore()
const ui = useUiStore()
const update = useUpdateStore()
const appearance = useAppearanceStore()

const mock = ref(false)

let timer: number | undefined

onMounted(async () => {
  // 先探测后端：决定用真实绑定还是降级到 mock，再拉数据
  await getApi()
  mock.value = isMockMode()

  await Promise.all([
    peers.refresh(),
    chat.loadConversations(),
    transfers.refresh(),
    groups.refresh(),
    // 更新状态主动拉一次：事件只在变化时推，而「下载在上次运行时就完成了、
    // 用户刚打开应用」这种情况不会有事件 —— 不拉就永远不弹框。
    // 失败静默：更新是附加能力，不能挡住启动。
    update.refresh().catch(() => {}),
    // 背景图要在首帧之后尽快贴上，否则用户会看到「先灰底、再跳到图片」。
    // 失败静默：没有背景图是正常状态，不是错误。
    appearance.load().catch(() => {})
  ])

  // 节点目录的变化主体靠 peer:updated 事件推送，这里只做兜底轮询：
  // 「连接可用」与「连接质量（RTT）」这两类由会话层维护的状态没有单独事件源，
  // 需要周期性对账 —— 心跳每轮都会更新 RTT，事件却只在建连/断连时发一次。
  timer = window.setInterval(() => {
    void peers.refresh()
  }, 3000)
})

/**
 * 把「用户正在看哪个会话」告诉 Go。
 *
 * 提示决策靠它区分「正看着这个会话来消息（不打扰）」与「别的会话来消息
 * （响铃 + 闪烁）」—— 只有前端知道用户在看哪个会话，窗口焦点 Go 侧自己能读。
 *
 * 用 watch 而不是在 open() 里调用：切会话的路径不止一条（点联系人、点会话
 * 列表、对方先开口自动建会话），挂在状态变化上就不会漏。
 * 上报失败只当没发生：提示是附加能力，绝不能影响聊天本身。
 */
watch(
  () => chat.activeConvId,
  async (convId) => {
    try {
      const api = await getApi()
      await api.setActiveConversation(convId ?? '')
    } catch {
      /* 忽略：没有托盘/提示决策的环境（浏览器 mock）也会走到这里 */
    }
  },
  { immediate: true }
)

/**
 * 全局快捷键：Ctrl/⌘ + K 打开「搜索聊天记录」。
 *
 * 挂在 window 而不是某个输入框上：它是全局动作，焦点在哪（包括正在写草稿）
 * 都应该能用。`altKey` 要排除掉 —— macOS 的 ⌘⌥K 是别的意思，
 * 抢过来会让用户觉得「这个应用乱吃快捷键」。
 *
 * preventDefault 不是可选的：WebView 里 Ctrl+K 在部分平台有默认行为
 * （聚焦到浏览器层 / 清空当前行），不拦会出现「面板开了但焦点丢了」。
 */
function onShortcut(e: KeyboardEvent): void {
  if (!(e.ctrlKey || e.metaKey) || e.altKey) return
  if (e.key.toLowerCase() !== 'k') return
  e.preventDefault()
  ui.openSearch('')
}

onMounted(() => window.addEventListener('keydown', onShortcut))

onUnmounted(() => {
  if (timer) window.clearInterval(timer)
  window.removeEventListener('keydown', onShortcut)
})
</script>

<template>
  <div class="app" :class="{ 'with-queue': transfers.activeJobs.length > 0 }">
    <ContactPanel :mock="mock" />
    <ChatView />
    <SidePanel />

    <Transition name="toast">
      <div v-if="ui.toast" class="toast">{{ ui.toast }}</div>
    </Transition>

    <!-- 全局传输队列浮层（规格 1.1）。有没有它由它自己那一个条件决定，
         外壳只负责「有它时让出 --queue-h」—— 判据同源（store.activeJobs），
         所以不会出现留了空间却没有浮层的空档。 -->
    <TransferQueueBar />
  </div>

  <!-- 搜索聊天记录也是独立弹框：它是「整窗两栏」（左筛选 + 右结果），
       塞进设置那种页签组里没有意义；而且它需要一个初始关键词。 -->
  <SearchPanel v-if="ui.panel === 'search'" />

  <!-- 共享中心是独立弹框：它的形态（左右分栏 + 文件表格 + 一排动作）与
       「设置 / 调试 / 传输」那种单列表单完全不同，塞进同一个页签组里只会两边都别扭。 -->
  <ShareDialog v-else-if="ui.panel === 'share'" />
  <!-- 兜底必须排在最后，且是「还有别的面板」而不是「panel 非空」：
       SettingsDialog 自己不认 'search'，会把未知值当成 'settings' 渲染成一片设置页。 -->
  <SettingsDialog v-else-if="ui.panel" />

  <!-- 更新确认框与其它弹层【并列】而不是二选一：它由后端状态驱动（下载完成），
       而不是由 ui.panel 驱动；用户正在设置页里也可能收到它。 -->
  <UpdatePrompt />
</template>

<style scoped>
.app {
  display: grid;
  /* 中栏用 minmax(0,1fr)：不写 min 的话，长消息会把栅格撑破 */
  grid-template-columns: var(--rail-w) minmax(0, 1fr) var(--side-w);
  height: 100%;
  overflow: hidden;
}

/* 中等窗口先压缩两侧，保证聊天区不被挤到没法读 */
@media (max-width: 1180px) {
  .app {
    grid-template-columns: 252px minmax(0, 1fr) 286px;
  }
}

/* 再窄就只保留「人 + 对话」这两件最核心的事（桌面端正常不会走到这里） */
@media (max-width: 940px) {
  .app {
    grid-template-columns: 232px minmax(0, 1fr);
  }

  .app > :nth-child(3) {
    display: none;
  }
}

/* 底部浮层占位：不让出这一档内边距，左栏底部的「加好友」、右栏底部的按钮
   和输入框都会有一段被浮层压住（浮层是 fixed，不参与栅格）。 */
.app.with-queue {
  padding-bottom: var(--queue-h);
}

/* 轻提示默认贴在视口底部 30px —— 浮层出现时它正好落在浮层上，跟着抬上去。
   `--queue-h` 是两者唯一的耦合点（见 style.css 的布局尺寸段）。 */
.app.with-queue .toast {
  bottom: calc(30px + var(--queue-h));
}

/* 轻提示只负责进出场动画；外观在 style.css 的 .toast 里（全局只此一份） */
.toast-enter-active,
.toast-leave-active {
  transition: opacity var(--dur-3) var(--ease), transform var(--dur-3) var(--ease);
}

.toast-enter-from,
.toast-leave-to {
  opacity: 0;
  transform: translateX(-50%) translateY(8px);
}
</style>
