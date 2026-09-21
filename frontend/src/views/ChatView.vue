<script setup lang="ts">
/**
 * 中栏：聊天。
 *
 * 三件事在这里汇合，边界必须清楚：
 *   1. 渲染消息（含按天分隔、连续消息合并）；
 *   2. 发消息 / 发文件（原生对话框 + 拖放）；
 *   3. 把「正在发文件」这件事通知右栏（面板形态由 ui store 统一裁决）。
 */
import { computed, nextTick, onMounted, ref, watch } from 'vue'

import Avatar from '../components/Avatar.vue'
import ChatBubble from '../components/ChatBubble.vue'
import Icon from '../components/Icon.vue'
import type { Message } from '../api/types'
import { useFileSend } from '../composables/useFileSend'
import { useChatStore } from '../stores/chat'
import { useGroupStore } from '../stores/group'
import { usePeersStore } from '../stores/peers'
import { useUiStore } from '../stores/ui'
import { dateLabel } from '../utils/format'

const chat = useChatStore()
const peers = usePeersStore()
const groups = useGroupStore()
const ui = useUiStore()

const { sending, sendPaths, attach } = useFileSend()

const draft = ref('')
const pathInput = ref('')
const scroller = ref<HTMLElement | null>(null)
const draftEl = ref<HTMLTextAreaElement | null>(null)

const conv = computed(() => chat.activeConversation)
const isGroup = computed(() => chat.activeIsGroup)
const peer = computed(() => (chat.activePeerId ? peers.byId(chat.activePeerId) : undefined))

const title = computed(() => conv.value?.title ?? '')

/**
 * 副标题刻意区分「节点在线」与「连接可用」（ADR-009）。
 * 这两件事在本项目里可以不一致，合并成一个绿灯会让用户看到
 * 「在线却要等一秒才发出去」而不知为何。
 */
const subtitle = computed(() => {
  const c = conv.value
  if (!c) return ''
  if (c.kind === 'group') {
    const g = groups.groups.find((x) => x.groupId === c.convId)
    if (!g) return '群聊'
    return `群聊 · ${groups.activeCount(g)}/${g.members.length} 在线 · 离线成员不接收消息`
  }
  const p = peers.byId(c.peerId ?? '')
  if (!p) return '等待节点信息…'
  const conn = p.connected ? '连接可用' : p.online ? '在线（未连接，发送时按需拨号）' : '离线'
  return `${conn} · ${p.lastAddr || p.subnet || '地址未知'}`
})

// ---------------------------------------------------------------- 消息分组

type Item =
  | { key: string; kind: 'day'; at: number }
  | { key: string; kind: 'msg'; m: Message; tail: boolean; first: boolean }

const RUN_GAP = 5 * 60_000

function dayOf(ms: number): number {
  const d = new Date(ms)
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
}

/** 同一个人、同一方向、间隔 5 分钟内 → 视为一组连续消息。 */
function sameRun(a: Message | undefined, b: Message | undefined): boolean {
  return (
    !!a &&
    !!b &&
    a.senderId === b.senderId &&
    a.direction === b.direction &&
    b.sentAt - a.sentAt < RUN_GAP
  )
}

const items = computed<Item[]>(() => {
  const list = chat.activeMessages
  const out: Item[] = []
  let day = -1
  let prev: Message | undefined

  for (let i = 0; i < list.length; i++) {
    const m = list[i]
    const d = dayOf(m.sentAt)
    if (d !== day) {
      out.push({ key: `day-${d}`, kind: 'day', at: m.sentAt })
      day = d
      prev = undefined
    }
    out.push({
      key: m.msgId,
      kind: 'msg',
      m,
      first: !sameRun(prev, m),
      tail: !sameRun(m, list[i + 1])
    })
    prev = m
  }
  return out
})

// ---------------------------------------------------------------- 滚动

const atBottom = ref(true)

function onScroll(): void {
  const el = scroller.value
  if (!el) return
  atBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight < 64
}

async function toBottom(): Promise<void> {
  await nextTick()
  const el = scroller.value
  if (el) el.scrollTop = el.scrollHeight
}

watch(
  () => chat.activeConvId,
  () => {
    atBottom.value = true
    void toBottom()
  }
)

watch(
  () => chat.activeMessages.length,
  () => {
    // 只有本来就贴着底部时才跟随，否则会把正在翻历史的人拽回去
    if (atBottom.value) void toBottom()
  }
)

onMounted(() => void toBottom())

// ---------------------------------------------------------------- 发送

function autoGrow(): void {
  const el = draftEl.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = `${Math.min(148, el.scrollHeight)}px`
}

watch(draft, () => void nextTick(autoGrow))

async function submit(): Promise<void> {
  const c = conv.value
  const text = draft.value
  if (!c || !text.trim()) return
  draft.value = ''
  await nextTick(autoGrow)
  try {
    await chat.send(c, text)
  } catch (e: any) {
    chat.error = String(e?.message ?? e)
    draft.value = text
  }
  void toBottom()
}

function onKeydown(e: KeyboardEvent): void {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    void submit()
  }
}

// ---------------------------------------------------------------- 发送文件

/** 手动路径兜底（仅当原生对话框不可用时出现）。 */
async function sendTypedPath(): Promise<void> {
  const p = pathInput.value.trim()
  if (!p) return
  pathInput.value = ''
  ui.hidePathInput()
  await sendPaths([p])
}
</script>

<template>
  <!-- data-file-drop-target 是 Wails 拖放的落点标记：
       只有带这个属性的元素才会收到拖入的文件路径（由 Go 侧转发）。 -->
  <main class="pane-chat" data-file-drop-target="chat">
    <header class="chat-head">
      <template v-if="conv">
        <Avatar
          :seed="isGroup ? conv.convId : peer?.nodeId || conv.convId"
          :name="title"
          :size="36"
          :shape="isGroup ? 'square' : 'circle'"
          :online="isGroup ? null : peer ? peer.connected || peer.online : null"
        />
        <div class="head-main">
          <div class="head-title nowrap">{{ title }}</div>
          <div class="head-sub nowrap">{{ subtitle }}</div>
        </div>
      </template>
      <div v-else class="head-main">
        <div class="head-title faint">SwarmLink</div>
        <div class="head-sub">无中心内网 P2P 聊天与文件分享</div>
      </div>
    </header>

    <div v-if="chat.error" class="banner err">{{ chat.error }}</div>

    <div ref="scroller" class="stream scroll" @scroll="onScroll">
      <div v-if="!conv" class="empty welcome">
        <Avatar seed="swarmlink" name="S" :size="64" shape="square" />
        <div class="empty-title">从左侧选择一位联系人开始</div>
        <div>
          同网段的节点会自动出现在列表里。<br />
          跨网段部署需要先在设置中配置种子清单。
        </div>
        <button class="btn btn-soft" @click="ui.openPanel('settings')">
          <Icon name="sliders" :size="14" />打开设置
        </button>
      </div>

      <div v-else-if="!chat.activeMessages.length" class="empty">
        <div v-if="chat.loading">正在加载历史…</div>
        <template v-else>
          <div class="empty-title">还没有消息</div>
          <div>
            把文件直接拖到这里也能发送。
          </div>
        </template>
      </div>

      <template v-for="it in items" :key="it.key">
        <div v-if="it.kind === 'day'" class="day">{{ dateLabel(it.at) }}</div>
        <ChatBubble
          v-else
          :message="it.m"
          :out="it.m.direction === 'out'"
          :show-sender="isGroup"
          :tail="it.tail"
          :first="it.first"
          :sender-name="peers.nameOf(it.m.senderId)"
          :sender-seed="it.m.senderId"
        />
      </template>
    </div>

    <footer class="composer">
      <div class="tools">
        <button
          class="icon-btn"
          :disabled="!conv || isGroup || sending"
          :title="isGroup ? 'v1.0 仅支持单文件点对点发送' : '发送文件'"
          aria-label="发送文件"
          @click="attach"
        >
          <Icon name="paperclip" :size="17" />
        </button>
        <span class="spacer" />
        <span class="hint">Enter 发送 · Shift + Enter 换行</span>
      </div>

      <div class="entry">
        <textarea
          ref="draftEl"
          v-model="draft"
          class="scroll"
          :disabled="!conv"
          rows="1"
          :placeholder="conv ? '输入消息…' : '先选择一个会话'"
          @keydown="onKeydown"
        />
        <button class="send" :disabled="!conv || !draft.trim()" @click="submit">
          <Icon name="send" :size="15" />
          发送
        </button>
      </div>

      <!-- 原生对话框不可用时的兜底（浏览器预览 / 无 GUI 环境） -->
      <div v-if="ui.pathInputVisible" class="fallback">
        <input
          v-model="pathInput"
          class="mono"
          placeholder="文件绝对路径，例如 D:\releases\build.zip"
          @keydown.enter="sendTypedPath"
        />
        <button class="btn btn-primary" @click="sendTypedPath">发送</button>
        <button class="btn btn-soft" @click="ui.hidePathInput()">取消</button>
      </div>
    </footer>

    <!-- 拖放高亮：file-drop-target-active 由 Wails 运行时在拖入时加上 -->
    <div class="drop-overlay">
      <div class="drop-card">
        <Icon name="upload" :size="26" />
        <div class="drop-title">松开即可发送给 {{ title || '当前会话' }}</div>
        <div class="faint">支持一次拖入多个文件</div>
      </div>
    </div>
  </main>
</template>

<style scoped>
/* 中栏是整块玻璃里【最透】的一层：它夹在左右两栏之间，
   让氛围色从中间透出来最多，三栏的色偏差异才拉得开 */
.pane-chat {
  position: relative;
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
  background: var(--glass-chat);
  backdrop-filter: blur(var(--blur)) saturate(110%);
  -webkit-backdrop-filter: blur(var(--blur)) saturate(110%);
  /* 额外向左投一道窄阴影：三栏都是半透明玻璃，只靠 1px 亮缝不足以分开，
     这道「遮挡阴影」让中栏看起来压在左栏之上（渲染顺序天然支持）。
     深色界面要用近黑而不是藏青 —— 藏青投影在深底上等于没画。 */
  box-shadow: var(--shadow-glass), inset 0 1px 0 var(--edge),
    -16px 0 30px -26px rgba(2, 0, 12, 0.85);
}

/* ---------------------------------------------------------------- 顶栏 */

.chat-head {
  display: flex;
  align-items: center;
  gap: 11px;
  flex: 0 0 auto;
  height: var(--head-h);
  padding: 0 16px;
  background: var(--glass-chrome);
  backdrop-filter: blur(10px);
  -webkit-backdrop-filter: blur(10px);
  box-shadow: inset 0 -1px 0 var(--edge);
}

.head-main {
  flex: 1 1 auto;
  min-width: 0;
}

.head-title {
  font-size: var(--fs-lg);
  font-weight: 600;
  letter-spacing: -0.01em;
}

.head-sub {
  font-size: var(--fs-xs);
  color: var(--t3);
}

/* ---------------------------------------------------------------- 消息区 */

.stream {
  flex: 1 1 auto;
  min-height: 0;
  padding: 16px 20px 20px;
}

.day {
  margin: 14px 0 10px;
  text-align: center;
  font-size: var(--fs-xs);
  color: var(--t3);
  user-select: none;
}

.welcome {
  gap: 10px;
  padding-top: 56px;
}

/* ---------------------------------------------------------------- 输入区 */

.composer {
  flex: 0 0 auto;
  padding: 8px 14px 12px;
  background: var(--glass-chrome);
  backdrop-filter: blur(10px);
  -webkit-backdrop-filter: blur(10px);
  box-shadow: inset 0 1px 0 var(--edge);
}

.tools {
  display: flex;
  align-items: center;
  gap: 6px;
  height: 30px;
  margin-bottom: 4px;
}

.spacer {
  flex: 1 1 auto;
}

.hint {
  font-size: var(--fs-xs);
  color: var(--t3);
  user-select: none;
}

.entry {
  display: flex;
  align-items: flex-end;
  gap: 10px;
}

/* 输入框是深色凹槽：白字必须落在足够暗的底上，
   这里【不能】沿用「白霜玻璃」那一套，否则输入时根本看不清自己在打什么 */
.entry textarea {
  flex: 1 1 auto;
  min-height: 34px;
  max-height: 148px;
  padding: 7px 10px;
  background: var(--surface);
  box-shadow: inset 0 0 0 1px var(--line);
  border-radius: var(--r-md);
  font-size: var(--fs-md);
  overflow-y: auto;
}

.entry textarea:hover {
  background: var(--hover);
}

.send {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  flex: 0 0 auto;
  height: 34px;
  padding: 0 15px;
  border-radius: var(--r-md);
  /* 与 .btn-primary 统一用青色「行动色」：粉是身份色（选中/未读），
     两者各管一件事，界面才不会到处都在喊 */
  background: var(--cyan);
  color: var(--t-on-brand);
  font-size: var(--fs-md);
  font-weight: 500;
  transition: background var(--dur-1) var(--ease), transform var(--dur-1) var(--ease);
}

.send:hover:not(:disabled) {
  background: var(--cyan-deep);
}

.send:active:not(:disabled) {
  transform: scale(0.975);
}

/* 用整体透明度表达「禁用」，而不是把字色写死成半透明白 ——
   后者在浅色主题上就是白字压白底，整个按钮消失 */
.send:disabled {
  background: var(--surface);
  color: var(--t1);
  box-shadow: inset 0 0 0 1px var(--line);
  opacity: 0.42;
  cursor: not-allowed;
}

.fallback {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 8px;
  padding: 8px;
  border-radius: var(--r-md);
  background: var(--surface-sunken);
}

.fallback input {
  flex: 1 1 auto;
  background: var(--surface-strong);
}

.fallback .btn {
  flex: 0 0 auto;
}

/* ---------------------------------------------------------------- 拖放 */

.drop-overlay {
  position: absolute;
  inset: 12px;
  z-index: 20;
  display: flex;
  align-items: center;
  justify-content: center;
  border: 2px dashed var(--brand);
  border-radius: var(--r-xl);
  /* 拖放遮罩要彻底压住底下的内容：这里是唯一允许「浓」的地方。
     用深色而不是白色 —— 白色遮罩会把白字标题直接吃掉。 */
  background: rgba(12, 5, 30, 0.74);
  backdrop-filter: blur(var(--blur)) saturate(140%);
  -webkit-backdrop-filter: blur(var(--blur)) saturate(140%);
  opacity: 0;
  pointer-events: none;
  transition: opacity var(--dur-2) var(--ease);
}

/* Wails 运行时在拖入时给落点元素加这个类 */
.pane-chat.file-drop-target-active .drop-overlay {
  opacity: 1;
}

.drop-card {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  color: var(--brand-ink);
  font-size: var(--fs-sm);
}

.drop-title {
  font-size: var(--fs-lg);
  font-weight: 600;
  color: var(--t1);
}

html[data-theme='light'] .drop-overlay {
  background: rgba(255, 255, 255, 0.82);
}
</style>
