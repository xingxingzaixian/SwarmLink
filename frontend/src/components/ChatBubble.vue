<script setup lang="ts">
/**
 * 聊天气泡。
 *
 * 「连续消息」的处理是这个组件的主要复杂度：同一个人 5 分钟内连发多条时，
 * 只在该组的【末条】显示头像，并收紧组内的行距。这不是装饰 ——
 * 一屏十条消息各带一个头像会把有效的阅读区域吃掉一大半。
 *
 * 送达状态只对【自己发的】显示，且只显示异常/未完成态：
 * 每条都挂一个「已送达」标签是纯噪音（QQ 也不显示）。
 */
import { computed } from 'vue'

import Avatar from './Avatar.vue'
import Icon from './Icon.vue'
import type { Message } from '../api/types'
import { clockTime } from '../utils/format'

const props = withDefaults(
  defineProps<{
    message: Message
    /** 是否本人发出（决定靠右、配色、头像位置）。 */
    out: boolean
    senderName?: string
    senderSeed?: string
    /** 群聊里需要在气泡上方标出说话人。 */
    showSender?: boolean
    /** 是否为连续消息的末条：只有末条显示头像。 */
    tail?: boolean
    /** 是否为连续消息的首条：只有首条显示说话人名字。 */
    first?: boolean
  }>(),
  { senderName: '', senderSeed: '', showSender: false, tail: true, first: true }
)

const time = computed(() => clockTime(props.message.sentAt))

/** 未送达/失败才给视觉提示；pending 用一个呼吸点表示「在路上」。 */
const stateIcon = computed(() => {
  if (!props.out) return ''
  if (props.message.state === 'failed') return 'alert'
  if (props.message.state === 'pending') return 'clock'
  return ''
})

const stateText = computed(() => {
  switch (props.message.state) {
    case 'pending':
      return '发送中'
    case 'sent':
      return '已发送'
    case 'delivered':
      return '已送达'
    case 'failed':
      return '发送失败（对方可能已离线，消息会在重连后重试）'
    default:
      return props.message.state
  }
})
</script>

<template>
  <div class="row" :class="{ out, tail }" :title="stateText">
    <Avatar
      class="ava"
      :class="{ hidden: !tail }"
      :seed="senderSeed || message.senderId"
      :name="senderName"
      :size="32"
      :online="null"
    />

    <div class="col">
      <div v-if="showSender && !out && first" class="who">{{ senderName }}</div>

      <div class="bubble">
        <div class="text">{{ message.content }}</div>
        <div class="meta">
          <span class="time">{{ time }}</span>
          <Icon
            v-if="stateIcon"
            :name="stateIcon"
            :size="12"
            :class="['state', message.state]"
          />
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.row {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  margin-top: 2px;
}

/* 连续消息之间收紧；一组结束后留出节奏感 */
.row:not(.tail) {
  margin-top: 2px;
}

.row.tail {
  margin-top: 10px;
}

.row.out {
  flex-direction: row-reverse;
}

.ava {
  /* 用 visibility 而不是 v-if：占位必须保留，否则气泡会左右跳 */
  margin-top: 1px;
}

.ava.hidden {
  visibility: hidden;
}

.col {
  display: flex;
  flex-direction: column;
  min-width: 0;
  max-width: min(568px, 68%);
}

.row.out .col {
  align-items: flex-end;
}

.who {
  margin: 0 0 3px 2px;
  font-size: var(--fs-xs);
  color: var(--t3);
}

/* 收到消息 = 浅色「纸面」+ 深字。
   这是深色玻璃界面上唯一能把「别人的话」和「背景」彻底分开的做法：
   若也做成半透明白霜，长文本会跟着背景色漂移，越读越累。
   白纸在饱和玻璃上也最好看 —— 它和背景的明度差最大。 */
.bubble {
  position: relative;
  padding: 8px 12px 6px;
  border-radius: var(--r-lg);
  background: var(--paper);
  color: var(--paper-ink);
  box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.55),
    0 10px 24px -16px rgba(4, 0, 18, 0.85);
  /* 靠头像那一角收小：这是气泡「指向说话人」的暗示，比画三角形稳 */
  border-top-left-radius: var(--r-xs);
  word-break: break-word;
  white-space: pre-wrap;
}

/* 发出消息 = 粉。
   注意这里用的是【加深版】的 #EC4899：白字直接落在 #EC4899 上只有 3.5:1，
   压深到 #C93A82 起才有 4.8:1 —— 色相没变，只是把明度让给了可读性。 */
.row.out .bubble {
  border-top-left-radius: var(--r-lg);
  border-top-right-radius: var(--r-xs);
  background: linear-gradient(135deg, #c93a82, #9c1f60);
  color: #fff;
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.26),
    0 10px 24px -14px rgba(156, 31, 96, 0.95);
}

.text {
  font-size: var(--fs-md);
  line-height: 1.58;
}

.meta {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 4px;
  margin-top: 2px;
}

/* 纸面上的时间戳要用深灰，不能沿用深色面板上的白字档 */
.time {
  font-size: 10px;
  color: #5b5273;
  font-variant-numeric: tabular-nums;
  user-select: none;
}

.row.out .time {
  color: rgba(255, 255, 255, 0.76);
}

.state {
  opacity: 0.8;
}

.state.pending {
  animation: breathe 1.4s var(--ease) infinite;
}

/* 失败标记落在白纸上，用深红；语义色 #FB7185 是给深色底用的，在纸上读不出 */
.state.failed {
  color: #e11d48;
  opacity: 1;
}

@keyframes breathe {
  0%,
  100% {
    opacity: 0.35;
  }
  50% {
    opacity: 0.9;
  }
}
</style>
