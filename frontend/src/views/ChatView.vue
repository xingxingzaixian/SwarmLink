<script setup lang="ts">
/**
 * 中栏：聊天。
 *
 * 三件事在这里汇合，边界必须清楚：
 *   1. 渲染消息（含按天分隔、连续消息合并）；
 *   2. 发消息 / 发文件（原生对话框 + 拖放）；
 *   3. 把「正在发文件」这件事通知右栏（面板形态由 ui store 统一裁决）。
 */
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'

import Avatar from '../components/Avatar.vue'
import ChatBubble from '../components/ChatBubble.vue'
import EmojiPicker from '../components/EmojiPicker.vue'
import GroupInfoPanel from '../components/GroupInfoPanel.vue'
import Icon from '../components/Icon.vue'
import ImageLightbox from '../components/ImageLightbox.vue'
import type { Message } from '../api/types'
import { useFileSend } from '../composables/useFileSend'
import { useChatStore } from '../stores/chat'
import { useGroupStore } from '../stores/group'
import { usePeersStore } from '../stores/peers'
import { useUiStore } from '../stores/ui'
import { canClear, canRestore } from '../utils/clearView'
import { dateLabel } from '../utils/format'
import { previewText } from '../utils/message'

const chat = useChatStore()
const peers = usePeersStore()
const groups = useGroupStore()
const ui = useUiStore()

const { sending, sendPaths, attach, attachFolder, attachImage } = useFileSend()

const draft = ref('')
const pathInput = ref('')
const scroller = ref<HTMLElement | null>(null)
const draftEl = ref<HTMLTextAreaElement | null>(null)

/** 正在放大的图片消息（null = 关闭查看器）。 */
const zoomed = ref<Message | null>(null)
const emojiOpen = ref(false)

const conv = computed(() => chat.activeConversation)
const isGroup = computed(() => chat.activeIsGroup)
const peer = computed(() => (chat.activePeerId ? peers.byId(chat.activePeerId) : undefined))

const title = computed(() => conv.value?.title ?? '')

/**
 * 状态行刻意区分「节点在线」与「连接可用」（ADR-009）。
 *
 * 这两件事在本项目里可以不一致，合并成一个绿灯会让用户看到
 * 「在线却要等一秒才发出去」而不知为何。
 *
 * 拆成三段而不是拼成一个字符串：地址要单独走等宽字体，而「亮/灭」
 * 只由连通状态决定 —— 拼在一起就只能整行一个颜色，群聊那行还会整行变绿。
 */
const statusText = computed(() => {
  const c = conv.value
  if (!c) return '无中心内网 P2P 聊天与文件分享'
  if (c.kind === 'group') {
    const g = groups.groups.find((x) => x.groupId === c.convId)
    if (!g) return '群聊'
    return `群聊 · ${groups.activeCount(g)}/${groups.memberCount(g)} 在线 · 离线成员不接收消息`
  }
  const p = peers.byId(c.peerId ?? '')
  if (!p) return '等待节点信息…'
  return p.connected ? '连接可用' : p.online ? '在线（未连接，发送时按需拨号）' : '离线'
})

/** 状态行的「亮」与否：决定那行字是信号绿还是中性灰。 */
const statusLive = computed(() => {
  const c = conv.value
  if (!c) return false
  if (c.kind === 'group') {
    const g = groups.groups.find((x) => x.groupId === c.convId)
    return !!g && groups.activeCount(g) > 0
  }
  const p = peers.byId(c.peerId ?? '')
  return !!p && (p.connected || p.online)
})

/** 状态行右半：可拨号地址（等宽）。群聊没有单一地址，返回空串。 */
const statusAddr = computed(() => {
  const c = conv.value
  if (!c || c.kind === 'group') return ''
  const p = peers.byId(c.peerId ?? '')
  return p?.lastAddr || p?.subnet || ''
})

// ---------------------------------------------------------------- 群信息
const infoOpen = ref(false)

/** 当前群（群会话时有值；单聊为 undefined）。 */
const activeGroup = computed(() =>
  isGroup.value ? groups.byId(conv.value?.convId ?? '') : undefined
)

/** 群已解散：禁用输入并展示横幅。取不到群时按未解散处理，避免误禁用。 */
const groupDissolved = computed(() => activeGroup.value?.state === 'dissolved')

function openInfo(): void {
  if (activeGroup.value) infoOpen.value = true
}

// ---------------------------------------------------------------- @提及
//
// 只在群会话里工作。做法是「从光标前的文本推断」而不是维护一份输入状态机：
// @ 是用户随时可能手打的普通字符，维护状态机反而会在退格/粘贴时出错。
const mentionQuery = ref<string | null>(null)

const mentionCandidates = computed(() => {
  if (mentionQuery.value === null || !activeGroup.value) return []
  const q = mentionQuery.value.toLowerCase()
  const selfId = peers.self?.nodeId ?? ''
  return activeGroup.value.members
    // 只能在群成员里 @：判据用 active（票号），否则会 @ 到已退出的人。
    .filter((m) => m.active && m.nodeId !== selfId)
    .map((m) => ({ nodeId: m.nodeId, name: m.displayName || m.nodeId.slice(0, 8) }))
    .filter((m) => !q || m.name.toLowerCase().includes(q))
})

/** 依据光标位置刷新 @前缀（没有在提人时置 null）。 */
function refreshMention(): void {
  const el = draftEl.value
  if (!el || !isGroup.value) {
    mentionQuery.value = null
    return
  }
  const caret = el.selectionStart ?? draft.value.length
  const before = draft.value.slice(0, caret)
  const at = before.lastIndexOf('@')
  if (at < 0) {
    mentionQuery.value = null
    return
  }
  const frag = before.slice(at + 1)
  // 片段里出现空白或又一个 @，说明这个 @ 已经结束（或不是提及）
  if (/[\s@]/.test(frag)) {
    mentionQuery.value = null
    return
  }
  mentionQuery.value = frag
}

/** 选中某成员：把光标前的 @片段替换成 `@名字 `。 */
async function pickMention(name: string): Promise<void> {
  const el = draftEl.value
  const caret = el?.selectionStart ?? draft.value.length
  const before = draft.value.slice(0, caret)
  const at = before.lastIndexOf('@')
  if (at < 0) return
  const insert = `@${name} `
  draft.value = before.slice(0, at) + insert + draft.value.slice(caret)
  mentionQuery.value = null
  await nextTick()
  el?.focus()
  const pos = at + insert.length
  el?.setSelectionRange(pos, pos)
  void autoGrow()
}

// ---------------------------------------------------------------- 引用回复
//
// 引用走【纯文本约定】而不是新的消息信封：以 `> ` 开头的行就是引用块。
// 这样托盘悬停预览（只取首行）拿到的是「> 张三：原文」这样一句人话，
// 而不是一段裸 JSON —— 后者正是这个项目里反复出现过的提示事故。
async function quoteMessage(m: Message): Promise<void> {
  const who = m.direction === 'out' ? '我' : peers.nameOf(m.senderId)
  const line = `> ${who}：${previewText(m)}`
  draft.value = `${line}\n${draft.value}`
  await nextTick()
  void autoGrow()
  draftEl.value?.focus()
}

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

// ---------------------------------------------------------------- 跳转定位

/**
 * 消费「跳到这一条」：滚到视口中央 + 临时高亮 2s。
 *
 * 为什么在这里而不是在 store 的 openAt 里（§4.6 原写法是 store 内 querySelector）：
 * 窗口要等下一次渲染才进 DOM，store 里同步查必然为空；而滚动本来就是视图职责。
 * store 只置 anchorMsgId 这份意图，这里在渲染后执行，执行完（含高亮期）清掉它。
 *
 * 时序上有两处必须显式处理：
 *   1. `atBottom` 要先置 false —— 会话历史那个 length 监听会在贴底时把视图
 *      拽到底部，把刚定位好的位置毁掉；
 *   2. 再让出一个宏任务 —— toBottom 走的是 nextTick 的微任务链，这里必须
 *      排在它之后，否则「先滚到底」会在我们的滚动之后生效。
 */
let anchorTimer: number | undefined

watch(
  () => chat.anchorMsgId,
  async (id) => {
    if (anchorTimer) window.clearTimeout(anchorTimer)
    if (!id) return

    atBottom.value = false
    await nextTick()
    await new Promise<void>((resolve) => setTimeout(resolve, 0))
    // 再压一次：刚才那次「滚到底」会异步派发 scroll 事件、由 onScroll 把
    // atBottom 置回 true。不压的话，紧接着来一条新消息就会把视图拽到底部。
    atBottom.value = false

    // 不用 `[data-msg-id="…"]` 属性选择器：msg_id 是外部数据，拼进选择器
    // 得先做 CSS 转义（引号、反斜杠都会把语法写坏）。扫一遍再比对，
    // 一屏最多百来条的代价可以忽略。
    const target = Array.from(scroller.value?.querySelectorAll('[data-msg-id]') ?? []).find(
      (n) => (n as HTMLElement).dataset.msgId === id
    )
    target?.scrollIntoView({ block: 'center' })

    // 高亮是「你要找的是这条」的一次性指示，不是常驻状态 —— 2s 后撤掉。
    anchorTimer = window.setTimeout(() => chat.clearAnchor(), 2000)
  }
)

onBeforeUnmount(() => {
  if (anchorTimer) window.clearTimeout(anchorTimer)
})

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
  // 输入法组合态下的 Enter 是「确认候选词」，不是「发送」。
  // 少了这个判断，中文用户每选一次词就会把半成品发出去。
  if (e.isComposing) return

  // @提及下拉打开时：Enter 选中第一个候选，Esc 关掉它 —— 都不能落到「发送」上，
  // 否则用户想选人却把半截 @ 发了出去。
  if (mentionCandidates.value.length) {
    if (e.key === 'Enter') {
      e.preventDefault()
      const first = mentionCandidates.value[0]
      if (first) void pickMention(first.name)
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      mentionQuery.value = null
      return
    }
  }

  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    void submit()
  }
}

/**
 * 中文输入法的组合态。
 *
 * 组合期间 textarea 的 selectionStart 指向的是「上一个已提交的位置」，
 * 此刻插入会把表情切进正在拼音的字符中间。因此组合态下先排队，
 * 等 compositionend（Vue 也在这一刻才把输入同步进 v-model）再插。
 */
const composing = ref(false)
let pendingEmoji = ''

function onCompositionStart(): void {
  composing.value = true
}

function onCompositionEnd(): void {
  composing.value = false
  if (!pendingEmoji) return
  const ch = pendingEmoji
  pendingEmoji = ''
  void insertEmoji(ch)
}

/**
 * 把表情插入到光标处。
 *
 * 必须手动记录并恢复光标：v-model 写回 draft 之后，Vue 重新渲染
 * 会把 textarea 的选区重置到末尾 —— 表现为「在句子中间插一个表情，光标跳到结尾」。
 */
async function insertEmoji(ch: string): Promise<void> {
  if (composing.value) {
    pendingEmoji += ch
    return
  }
  const el = draftEl.value
  const start = el?.selectionStart ?? draft.value.length
  const end = el?.selectionEnd ?? draft.value.length
  draft.value = draft.value.slice(0, start) + ch + draft.value.slice(end)

  await nextTick()
  el?.focus()
  const caret = start + ch.length
  el?.setSelectionRange(caret, caret)
  void autoGrow()
}

/**
 * 粘贴图片。
 *
 * 剪贴板里的 File 只有 Blob、没有本地路径，因此走 base64 字节入口
 * （与「选文件」不同：那条路有真实路径，可以让 Go 侧直接读盘）。
 * 纯文本粘贴不拦截，交给浏览器默认行为。
 */
async function onPaste(e: ClipboardEvent): Promise<void> {
  const c = conv.value
  if (!c) return
  const files = Array.from(e.clipboardData?.files ?? []).filter((f) => f.type.startsWith('image/'))
  if (!files.length) return

  e.preventDefault()
  for (const f of files) {
    try {
      const bytes = new Uint8Array(await f.arrayBuffer())
      await chat.sendImageBytes(c, f.name || 'clipboard.png', toBase64(bytes))
    } catch (err: any) {
      ui.notify(String(err?.message ?? err) || '粘贴图片失败')
    }
  }
  void toBottom()
}

/** Uint8Array → base64。分块转换：一次性展开大数组会顶爆调用栈。 */
function toBase64(bytes: Uint8Array): string {
  const CHUNK = 0x8000
  let binary = ''
  for (let i = 0; i < bytes.length; i += CHUNK) {
    binary += String.fromCharCode(...bytes.subarray(i, i + CHUNK))
  }
  return btoa(binary)
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

// ------------------------------------------------------------ 清空 / 查看历史

/**
 * 清空当前会话的聊天区。
 *
 * 刻意不做二次确认：这个动作【不删数据】，且「查看历史」就是它的撤销键。
 * 加确认框反而会让用户以为自己在做危险操作。
 */
async function clearView(): Promise<void> {
  const c = conv.value
  if (!c) return
  try {
    await chat.clearConversation(c)
    ui.notify('已清空当前聊天区（消息未删除，点「查看历史」可恢复）')
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || '清空失败')
  }
}

/** 撤销清空：被隐藏的消息立刻回来（本地已有数据，不需要重新请求）。 */
async function restoreView(): Promise<void> {
  const c = conv.value
  if (!c) return
  try {
    await chat.restoreConversation(c)
    ui.notify('已恢复显示被清空的消息')
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || '恢复显示失败')
  }
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
          :size="40"
          :shape="isGroup ? 'square' : 'circle'"
          :online="isGroup ? null : peer ? peer.connected || peer.online : null"
        />
        <div class="head-main" :class="{ clickable: isGroup }" @click="openInfo">
          <div class="head-title nowrap">
            {{ title }}
            <span v-if="isGroup" class="head-hint">查看群信息</span>
          </div>
          <div class="head-sub nowrap" :class="{ live: statusLive }">
            <span class="dot dot--static" />
            <span class="nowrap">{{ statusText }}</span>
            <span v-if="statusAddr" class="ip mono nowrap">{{ statusAddr }}</span>
          </div>
        </div>
      </template>
      <div v-else class="head-main">
        <div class="head-title">蜂群</div>
        <div class="head-sub">
          <span class="dot dot--static" />
          <span>{{ statusText }}</span>
        </div>
      </div>
      <div class="head-actions">
        <button
          v-if="isGroup"
          class="icon-btn"
          title="群信息 / 成员管理"
          aria-label="群信息"
          @click="openInfo"
        >
          <Icon name="info" :size="18" />
        </button>
        <!-- 共享中心入口：与其他全局功能（设置/调试/传输）不同，这个是【日常会用】
             的功能，藏在头像菜单里等于没有 —— 因此常驻顶栏，与主题切换并排。
             带文字而不是纯图标：共享/上传类的图标在中文语境里认不出是「共享文件」，
             而「认不出」正是它原来没人发现的原因。 -->
        <button
          class="btn btn-soft share-entry"
          :class="{ on: ui.panel === 'share' }"
          title="共享中心：发布目录给可见范围内的人，或下载他人的共享"
          aria-label="打开共享中心"
          @click="ui.openPanel('share')"
        >
          <Icon name="share" :size="15" />
          <span>共享中心</span>
        </button>
        <button
          class="icon-btn"
          :title="ui.theme === 'light' ? '切换到深色主题' : '切换到浅色主题'"
          :aria-label="ui.theme === 'light' ? '切换到深色主题' : '切换到浅色主题'"
          @click="ui.toggleTheme()"
        >
          <Icon :name="ui.theme === 'light' ? 'moon' : 'sun'" :size="18" />
        </button>
      </div>
    </header>

    <div v-if="chat.error" class="banner err">{{ chat.error }}</div>
    <div v-if="groupDissolved" class="banner warn">该群已由群主解散，不能再发送消息。</div>
    <!-- 跳转失败的回退提示：这类事不报错（消息被清空是正常路径），
         但绝不能静默 —— 否则用户点完搜索结果只会觉得「点错了」。 -->
    <div v-if="chat.notice" class="banner warn">{{ chat.notice }}</div>

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
        <!--
          「已清空」与「还没有消息」必须分开说：两者都是空屏，但前者
          用户刚刚亲手清空过（他需要知道消息还在、以及怎么找回来），
          后者则是一个从没聊过的新会话。
        -->
        <template v-else-if="conv && chat.isCleared(conv.convId)">
          <div class="empty-title">已清空</div>
          <div>
            消息没有被删除 · 点上方「查看历史」可恢复显示
          </div>
        </template>
        <template v-else>
          <div class="empty-title">还没有消息</div>
          <div>
            把文件直接拖到这里也能发送。
          </div>
        </template>
      </div>

      <template v-for="it in items" :key="it.key">
        <div v-if="it.kind === 'day'" class="day-sep">
          <span class="micro">{{ dateLabel(it.at) }}</span>
        </div>
        <!-- data-msg-id 是「跳转定位」的锚点：滚动时靠它在 DOM 里找回这条消息
             （不能按顺序索引 —— 消息列表随时被 upsert/筛选改变）。
             anchor 决定要不要上那 2s 的目标高亮。 -->
        <ChatBubble
          v-else
          :data-msg-id="it.m.msgId"
          :anchor="chat.anchorMsgId === it.m.msgId"
          :message="it.m"
          :out="it.m.direction === 'out'"
          :show-sender="isGroup"
          :tail="it.tail"
          :first="it.first"
          :sender-name="peers.nameOf(it.m.senderId)"
          :sender-seed="it.m.senderId"
          @zoom="(m) => (zoomed = m)"
          @quote="quoteMessage"
        />
      </template>
    </div>

    <footer class="composer">
      <div class="tools">
        <button
          class="icon-btn"
          :disabled="!conv || sending || groupDissolved"
          :title="groupDissolved ? '群已解散' : '发送文件'"
          aria-label="发送文件"
          @click="attach"
        >
          <Icon name="paperclip" :size="17" />
        </button>

        <button
          class="icon-btn"
          :disabled="!conv || sending || groupDissolved"
          :title="groupDissolved ? '群已解散' : '发送文件夹'"
          aria-label="发送文件夹"
          @click="attachFolder"
        >
          <Icon name="folder" :size="17" />
        </button>

        <button
          class="icon-btn"
          :disabled="!conv || groupDissolved"
          :title="groupDissolved ? '群已解散' : '发送图片'"
          aria-label="发送图片"
          @click="attachImage"
        >
          <Icon name="image" :size="17" />
        </button>

        <div class="emoji-wrap">
          <button
            class="icon-btn"
            data-emoji-toggle
            :disabled="!conv"
            title="表情"
            aria-label="插入表情"
            @click="emojiOpen = !emojiOpen"
          >
            <Icon name="smile" :size="17" />
          </button>
          <EmojiPicker
            v-if="emojiOpen"
            class="emoji-pop"
            @pick="insertEmoji"
            @close="emojiOpen = false"
          />
        </div>

        <button
          class="icon-btn"
          :disabled="!conv || !canClear(chat.activeMessages.length)"
          title="清空聊天区（不删除消息，可随时查看历史）"
          aria-label="清空聊天区"
          @click="clearView"
        >
          <Icon name="trash" :size="17" />
        </button>

        <button
          class="icon-btn"
          :disabled="!conv || !canRestore(chat.clearedAtOf(conv.convId))"
          title="查看历史（恢复显示被清空的消息）"
          aria-label="查看历史"
          @click="restoreView"
        >
          <Icon name="clock" :size="17" />
        </button>

        <span class="spacer" />
      </div>

      <div class="entry">
        <!-- @提及候选：贴着输入框上沿弹出，不占据正文空间 -->
        <div v-if="mentionCandidates.length" class="mention-pop">
          <button
            v-for="m in mentionCandidates"
            :key="m.nodeId"
            class="mention-item"
            @click="pickMention(m.name)"
          >
            <Avatar :seed="m.nodeId" :name="m.name" :size="22" :online="null" />
            <span class="nowrap">{{ m.name }}</span>
          </button>
        </div>

        <textarea
          ref="draftEl"
          v-model="draft"
          class="scroll"
          :disabled="!conv || groupDissolved"
          rows="1"
          :placeholder="
            !conv
              ? '先选择一个会话'
              : groupDissolved
                ? '该群已解散'
                : '输入消息…（Enter 发送，Shift+Enter 换行）'
          "
          @keydown="onKeydown"
          @input="refreshMention"
          @click="refreshMention"
          @paste="onPaste"
          @compositionstart="onCompositionStart"
          @compositionend="onCompositionEnd"
        />
        <button
          class="send"
          :disabled="!conv || groupDissolved || !draft.trim()"
          title="发送"
          aria-label="发送"
          @click="submit"
        >
          <Icon name="send" :size="18" />
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

    <ImageLightbox v-if="zoomed" :message="zoomed" @close="zoomed = null" />

    <!-- 群信息面板（成员管理 / 改名 / 解散） -->
    <GroupInfoPanel
      v-if="infoOpen && activeGroup"
      :group="activeGroup"
      @close="infoOpen = false"
    />

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
.pane-chat {
  position: relative;
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
  background-color: var(--bg-base);
  /* 网格点阵：这版设计里「网络拓扑」的底纹。
     只铺中栏 —— 左右栏是实心面板，铺上去只会发脏。 */
  background-image:
    linear-gradient(var(--grid) 1px, transparent 1px),
    linear-gradient(90deg, var(--grid) 1px, transparent 1px);
  background-size: 26px 26px;
  background-position: center;
}

/* ---------------------------------------------------------------- 顶栏 */

.chat-head {
  position: relative;
  display: flex;
  align-items: center;
  gap: 13px;
  flex: 0 0 auto;
  height: var(--head-h);
  padding: 0 20px;
  /* 半透明 + 模糊：底下的网格点阵透过来一点点，滚动时不会「一块实心板压过去」 */
  background: color-mix(in oklch, var(--bg-base) 80%, transparent);
  backdrop-filter: blur(6px);
  -webkit-backdrop-filter: blur(6px);
  border-bottom: 1px solid var(--divider);
}

/* 头部下方 130px 的强调线：整屏唯一一处「带方向」的装饰，
   把视线引向左边的头像与标题 */
.chat-head::after {
  content: '';
  position: absolute;
  left: 0;
  bottom: -1px;
  width: 130px;
  height: 2px;
  background: linear-gradient(90deg, var(--accent), transparent);
}

.head-main {
  flex: 1 1 auto;
  min-width: 0;
}

/* 群会话时整块标题可点开群信息：用指针 + hover 提示「点得动」，
   否则用户永远发现不了这个入口。 */
.head-main.clickable {
  cursor: pointer;
}

.head-main.clickable:hover .head-title {
  color: var(--accent);
}

.head-hint {
  margin-left: 8px;
  font-family: var(--font-body);
  font-size: var(--fs-xs);
  font-weight: 400;
  color: var(--text-muted);
  opacity: 0;
  transition: opacity var(--dur-1) var(--ease);
}

.head-main.clickable:hover .head-hint {
  opacity: 1;
}

.head-title {
  font-family: var(--font-display);
  font-size: var(--fs-xl);
  font-weight: 600;
  letter-spacing: -0.01em;
}

/* 状态行：绿点 + 状态词 + 等宽地址。
   只有「真的连上了」才整行变绿（.live），否则中性灰 ——
   群聊那行永远不该是绿的，它的成分太杂。 */
.head-sub {
  display: flex;
  align-items: center;
  gap: 6px;
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  color: var(--text-muted);
}

.head-sub.live {
  color: var(--accent);
}

.head-sub .ip {
  color: var(--text-muted);
  font-size: var(--fs-xs);
}

/* 状态点跟着那行字的颜色走：连上时整行（含点）是绿的，否则是灰的。
   用 currentColor 而不是再写一份 .dot.online —— 一处改动两处同步。 */
.head-sub .dot {
  background: currentColor;
}

.head-actions {
  display: flex;
  align-items: center;
  gap: 6px;
}

/* 共享中心入口：高度与旁边的图标按钮对齐（.btn 默认 30px，这里统一成 34px）。 */
.share-entry {
  height: 34px;
  padding: 0 11px;
}

.share-entry.on {
  background: var(--accent-soft);
  color: var(--accent);
}

/* ---------------------------------------------------------------- 消息区 */

.stream {
  flex: 1 1 auto;
  min-height: 0;
  padding: 22px 26px;
}

.welcome {
  gap: 10px;
  padding-top: 56px;
}

/* ---------------------------------------------------------------- 输入区 */

.composer {
  flex: 0 0 auto;
  padding: 14px 20px 18px;
  border-top: 1px solid var(--divider);
  background: var(--bg-base);
}

.tools {
  display: flex;
  align-items: center;
  gap: 4px;
  margin-bottom: 10px;
}

.spacer {
  flex: 1 1 auto;
}

.emoji-wrap {
  position: relative;
}

/* 面板向上弹出：它挂在输入框上方，向下弹会盖住输入区 */
.emoji-pop {
  position: absolute;
  bottom: calc(100% + 8px);
  left: 0;
  z-index: 20;
}

/* 输入容器：边框与光晕都落在容器上（里面的 textarea 是裸的），
   否则会出现双层边框、且 focus 时只有文字框发光 */
.entry {
  position: relative; /* @提及候选框相对它定位 */
  display: flex;
  align-items: flex-end;
  gap: 10px;
  padding: 9px 11px;
  background: var(--bg-elevated);
  border: 1.5px solid var(--divider-strong);
  border-radius: var(--r-md);
  transition: border-color var(--dur-2) var(--ease), box-shadow var(--dur-2) var(--ease);
}

/* ---------------------------------------------------------------- @提及 */
.mention-pop {
  position: absolute;
  left: 0;
  right: 0;
  bottom: calc(100% + 8px);
  z-index: 20;
  max-height: 220px;
  overflow-y: auto;
  padding: 5px;
  border-radius: var(--r-md);
  background: var(--bg-elevated);
  border: 1px solid var(--divider-strong);
  box-shadow: var(--shadow-lg);
}

.mention-item {
  display: flex;
  align-items: center;
  gap: 9px;
  width: 100%;
  padding: 6px 8px;
  border-radius: var(--r-xs);
  font-size: var(--fs-md);
  color: var(--text-primary);
  text-align: left;
}

.mention-item:hover {
  background: var(--bg-hover);
  color: var(--accent);
}

.entry:focus-within {
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
}

.entry textarea {
  flex: 1 1 auto;
  min-height: 24px;
  max-height: 148px;
  padding: 0;
  border: 0;
  background: none;
  border-radius: 0;
  font-size: var(--fs-lg);
  overflow-y: auto;
}

.entry textarea:hover,
.entry textarea:focus {
  background: none;
  border-color: transparent;
  box-shadow: none;
}

/* 发送键：42×42 的渐变方块。整屏只有它与自己的气泡用这条渐变。 */
.send {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex: 0 0 auto;
  width: 42px;
  height: 42px;
  border-radius: 12px;
  background: linear-gradient(135deg, var(--accent), var(--accent-strong));
  color: var(--accent-ink);
  transition: transform var(--dur-1) var(--ease), box-shadow var(--dur-2) var(--ease),
    opacity var(--dur-1) var(--ease);
}

.send:hover:not(:disabled) {
  transform: scale(1.06);
  box-shadow: var(--shadow-glow);
}

.send:active:not(:disabled) {
  transform: scale(0.94);
}

/* 用整体透明度表达「禁用」，而不是把底色写死 ——
   写死底色在浅色主题上就是白字压白底，整个按钮消失 */
.send:disabled {
  opacity: 0.38;
  cursor: not-allowed;
}

.fallback {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 8px;
  padding: 8px;
  border-radius: var(--r-sm);
  background: var(--bg-sunken);
}

.fallback input {
  flex: 1 1 auto;
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
  border: 2px dashed var(--accent);
  border-radius: var(--r-lg);
  /* 拖放遮罩要彻底压住底下的内容：这里是唯一允许「浓」的地方。
     color-mix 让它在两套主题下都成立，不必再写一个 light 分支。 */
  background: color-mix(in oklch, var(--bg-base) 88%, transparent);
  backdrop-filter: blur(6px);
  -webkit-backdrop-filter: blur(6px);
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
  color: var(--accent);
  font-size: var(--fs-sm);
}

.drop-title {
  font-family: var(--font-display);
  font-size: var(--fs-lg);
  font-weight: 600;
  color: var(--text-primary);
}
</style>
