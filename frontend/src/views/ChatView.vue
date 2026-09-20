<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'

import MessageBubble from '../components/MessageBubble.vue'
import { useChatStore } from '../stores/chat'
import { usePeersStore } from '../stores/peers'

const chat = useChatStore()
const peers = usePeersStore()

const draft = ref('')
const scroller = ref<HTMLElement | null>(null)

const conv = computed(() => chat.activeConversation)

const title = computed(() => conv.value?.title ?? '未选择会话')

const subtitle = computed(() => {
  const c = conv.value
  if (!c) return ''
  if (c.kind === 'group') return '群聊 · 离线成员不会收到（v1.0 不暂存）'
  const p = c.peerId ? peers.byId(c.peerId) : undefined
  if (!p) return c.state ?? ''
  // 明确区分「节点在线」与「连接可用」
  return `${p.nodeId} · ${p.connected ? '已连接' : p.online ? '在线（未连接，发送时将按需拨号）' : '离线'}`
})

async function scrollToBottom(): Promise<void> {
  await nextTick()
  const el = scroller.value
  if (el) el.scrollTop = el.scrollHeight
}

watch(
  () => [chat.activeConvId, chat.activeMessages.length],
  () => void scrollToBottom()
)

async function submit(): Promise<void> {
  const c = conv.value
  const text = draft.value
  if (!c || !text.trim()) return
  draft.value = ''
  try {
    await chat.send(c, text)
  } catch (e: any) {
    chat.error = String(e?.message ?? e)
    draft.value = text
  }
  await scrollToBottom()
}

function onKeydown(e: KeyboardEvent): void {
  // Enter 发送，Shift+Enter 换行
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    void submit()
  }
}
</script>

<template>
  <div class="pane">
    <div class="pane-head">
      <div>
        <div class="pane-title">{{ title }}</div>
        <div class="row-sub">{{ subtitle }}</div>
      </div>
    </div>

    <div v-if="chat.error" class="banner">{{ chat.error }}</div>

    <div ref="scroller" class="pane-body messages">
      <div v-if="!conv" class="empty">
        从左侧选择一个节点或群开始对话。
      </div>
      <div v-else-if="!chat.activeMessages.length" class="empty">
        <span v-if="chat.loading">正在加载历史…</span>
        <span v-else>还没有消息。</span>
      </div>
      <MessageBubble
        v-for="m in chat.activeMessages"
        :key="m.msgId"
        :message="m"
        :show-sender="conv?.kind === 'group'"
        :sender-name="peers.nameOf(m.senderId)"
      />
    </div>

    <div class="composer">
      <textarea
        v-model="draft"
        :disabled="!conv"
        rows="2"
        placeholder="输入消息，Enter 发送，Shift+Enter 换行"
        @keydown="onKeydown"
      />
      <button class="primary" :disabled="!conv || !draft.trim()" @click="submit">发送</button>
    </div>
  </div>
</template>

<style scoped>
.messages {
  padding: 12px 16px;
}

.composer {
  display: flex;
  gap: 8px;
  padding: 10px 12px;
  border-top: 1px solid var(--border);
  align-items: flex-end;
}

textarea {
  flex: 1 1 auto;
  resize: none;
  font-family: inherit;
  font-size: 13px;
  padding: 7px 9px;
  border-radius: var(--radius-sm);
  border: 1px solid var(--border);
  background: var(--bg);
  color: var(--text);
}

textarea:focus {
  outline: none;
  border-color: var(--accent);
}
</style>
