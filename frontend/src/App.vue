<script setup lang="ts">
/**
 * 应用外壳：氛围层 + 三栏 + 全局浮层。
 *
 *   左 = 联系人（含本人入口）  中 = 聊天  右 = 资料 / 传输进度
 *
 * 氛围层是整块玻璃的「内容」：三栏是半透明面板，没有背后这片渐变，
 * 磨砂就退化成一坨灰。构图有两条约束：
 *   1. 粉（#EC4899）与青（#06B6D4）放在【对角】——它们最亮，
 *      压住面板角落没问题，但不能铺在文字最密的中栏正中；
 *   2. 中栏底下留紫 / 蓝（深色），白字在这里最容易达到 4.5:1。
 * 换句话说，这个布局是为了对比度排的，不只是为了好看。
 *
 * 布局本身只决定「什么占据主视线的哪一块」，不做任何业务判断。
 * 所有跨栏联动（发文件后右栏切进度、左栏点人开会话）都走 store。
 */
import { onMounted, onUnmounted, ref } from 'vue'

import ChatView from './views/ChatView.vue'
import ContactPanel from './components/ContactPanel.vue'
import SidePanel from './components/SidePanel.vue'
import SettingsDialog from './views/SettingsDialog.vue'
import { getApi, isMockMode } from './api'
import { useChatStore } from './stores/chat'
import { useGroupStore } from './stores/group'
import { usePeersStore } from './stores/peers'
import { useTransferStore } from './stores/transfer'
import { useUiStore } from './stores/ui'

const peers = usePeersStore()
const chat = useChatStore()
const transfers = useTransferStore()
const groups = useGroupStore()
const ui = useUiStore()

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
    groups.refresh()
  ])

  // 节点目录的变化主体靠 peer:updated 事件推送，这里只做兜底轮询：
  // 「连接可用」这类由会话层维护的状态没有单独事件，需要周期性对账。
  timer = window.setInterval(() => {
    void peers.refresh()
  }, 3000)
})

onUnmounted(() => {
  if (timer) window.clearInterval(timer)
})
</script>

<template>
  <!-- 氛围层：装饰性，对辅助技术隐藏，且不接收指针事件 -->
  <div class="ambient" aria-hidden="true">
    <!-- 细颗粒：玻璃之所以像「材质」而不像「半透明 div」，一半靠这层噪点 -->
    <span class="grain" />
  </div>

  <div class="app">
    <ContactPanel :mock="mock" />
    <ChatView />
    <SidePanel />

    <Transition name="toast">
      <div v-if="ui.toast" class="toast">{{ ui.toast }}</div>
    </Transition>
  </div>

  <SettingsDialog v-if="ui.panel" />
</template>

<style scoped>
/* ---------------------------------------------------------------- 氛围层 */

/* 用渐变层而不是「几个 blur 过的大圆」：
   径向渐变的长衰减本身就是柔和的，省掉大半径 filter: blur 的合成开销。 */
.ambient {
  position: fixed;
  inset: 0;
  z-index: 0;
  overflow: hidden;
  pointer-events: none;
  background:
    /* 渐变1 的粉：只当右上角的一束光。铺太开会把整屏推成粉紫海报 */
    radial-gradient(58% 58% at 97% 0%, var(--g1-pink) 0%, rgba(236, 72, 153, 0) 58%),
    /* 渐变2 的青：左下角同理 */
    radial-gradient(56% 56% at 0% 100%, var(--g2-cyan) 0%, rgba(6, 182, 212, 0) 56%),
    /* 底：渐变1 的紫 → 渐变2 的蓝，收在更深的靛蓝上 */
    linear-gradient(135deg, var(--g1-purple) 0%, var(--g2-blue) 56%, var(--g-ink) 100%);
}

/* 深色薄纱：把整片渐变压深一档。
   这是「高级」与「海报」的分界线 —— 规格给的四色都偏亮，
   原样铺满屏会亮到让面板失去存在感（第一版就是这样）。
   压深后色相仍在，只是不再喧宾夺主。 */
.ambient::after {
  content: '';
  position: absolute;
  inset: 0;
  background: rgba(8, 3, 22, 0.45);
}

/* 浅色主题下薄纱要反过来洗白，而不是继续压暗 */
html[data-theme='light'] .ambient::after {
  background: rgba(255, 255, 255, 0.52);
}

html[data-theme='light'] .grain {
  opacity: 0.03;
}

/* 颗粒放在薄纱之上，才不会被洗掉 */
.grain {
  position: absolute;
  inset: 0;
  opacity: 0.05;
  background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='180' height='180'%3E%3Cfilter id='n'%3E%3CfeTurbulence type='fractalNoise' baseFrequency='0.85' numOctaves='3'/%3E%3C/filter%3E%3Crect width='180' height='180' filter='url(%23n)'/%3E%3C/svg%3E");
  background-size: 180px 180px;
}

/* ---------------------------------------------------------------- 三栏 */

.app {
  position: relative;
  /* 明确压在氛围层之上，不依赖 DOM 顺序 —— 顺序一改就会全盘错位 */
  z-index: 1;
  display: grid;
  grid-template-columns: var(--rail-w) minmax(0, 1fr) var(--side-w);
  height: 100%;
  /* 自身必须透明，否则氛围层被盖住，玻璃就失去内容 */
  background: transparent;
}

/* 栏间用【亮】色缝分隔：玻璃接缝处的光，比灰色分隔线更贴合材质 */
.app > :nth-child(2),
.app > :nth-child(3) {
  border-left: 1px solid var(--edge);
}

/* 中等窗口先压缩两侧，保证聊天区不被挤到没法读 */
@media (max-width: 1120px) {
  .app {
    grid-template-columns: 248px minmax(0, 1fr) 268px;
  }
}

/* 再窄就只保留「人 + 对话」这两件最核心的事（桌面端正常不会走到这里） */
@media (max-width: 900px) {
  .app {
    grid-template-columns: 224px minmax(0, 1fr);
  }

  .app > :nth-child(3) {
    display: none;
  }
}

/* ---------------------------------------------------------------- 轻提示 */

.toast {
  position: fixed;
  left: 50%;
  bottom: 34px;
  z-index: 200;
  transform: translateX(-50%);
  max-width: 60vw;
  padding: 9px 16px;
  border-radius: 999px;
  background: rgba(12, 5, 30, 0.78);
  backdrop-filter: blur(16px) saturate(150%);
  -webkit-backdrop-filter: blur(16px) saturate(150%);
  box-shadow: var(--shadow-pop);
  color: #fff;
  font-size: var(--fs-sm);
  pointer-events: none;
}

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
