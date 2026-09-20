<script setup lang="ts">
import { computed } from 'vue'

import type { Message } from '../api'

const props = defineProps<{
  message: Message
  /** 群聊里需要显示发送者名字；单聊不需要。 */
  showSender?: boolean
  senderName?: string
}>()

const outgoing = computed(() => props.message.direction === 'out')

const stateLabel = computed(() => {
  switch (props.message.state) {
    case 'pending':
      return '发送中'
    case 'sent':
      return '已发送'
    case 'delivered':
      return '已送达'
    case 'failed':
      return '失败'
    default:
      return props.message.state
  }
})

const stateClass = computed(() => {
  if (props.message.state === 'failed') return 'err'
  if (props.message.state === 'delivered') return 'ok'
  return ''
})

function time(ms: number): string {
  if (!ms) return ''
  const d = new Date(ms)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}
</script>

<template>
  <div class="bubble-row" :class="{ out: outgoing }">
    <div class="bubble">
      <div v-if="showSender && !outgoing" class="sender">{{ senderName }}</div>
      <div class="content">{{ message.content }}</div>
      <div class="meta">
        <span class="faint">{{ time(message.sentAt) }}</span>
        <span v-if="outgoing" class="tag" :class="stateClass">{{ stateLabel }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.bubble-row {
  display: flex;
  margin: 6px 0;
}

.bubble-row.out {
  justify-content: flex-end;
}

.bubble {
  max-width: 70%;
  padding: 7px 10px;
  border-radius: var(--radius);
  background: var(--bg-raised);
  border: 1px solid var(--border-soft);
  word-break: break-word;
  white-space: pre-wrap;
}

.bubble-row.out .bubble {
  background: var(--accent-soft);
  border-color: rgba(79, 140, 255, 0.35);
}

.sender {
  font-size: 11px;
  color: var(--info);
  margin-bottom: 2px;
}

.meta {
  display: flex;
  align-items: center;
  gap: 6px;
  justify-content: flex-end;
  margin-top: 3px;
  font-size: 10px;
}
</style>
