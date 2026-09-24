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
import { computed, ref } from 'vue'

import Avatar from './Avatar.vue'
import Icon from './Icon.vue'
import type { Message } from '../api/types'
import { bubbleTime } from '../utils/format'
import { imageBox, imageSrc, parseImageContent } from '../utils/image'
import { useTransferStore } from '../stores/transfer'

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

const emit = defineEmits<{ (e: 'zoom', m: Message): void }>()

const time = computed(() => bubbleTime(props.message.sentAt))

const isImage = computed(() => props.message.msgType === 'image')
const isFile = computed(() => props.message.msgType === 'file')

/**
 * 图片消息的内容解析。
 *
 * 解析失败（旧数据 / 损坏）时给占位块，而不是让整块聊天区崩掉 ——
 * 一条坏消息不该毁掉整段历史。
 */
const image = computed(() => (isImage.value ? parseImageContent(props.message.content) : null))

const box = computed(() =>
  image.value ? imageBox(image.value, 220) : { width: 220, height: 140 }
)

/**
 * 文件消息的内容解析。
 *
 * content 是 JSON: {"job_id":"xxx","file_name":"xxx"}
 */
const fileInfo = computed(() => {
  if (!isFile.value) return null
  try {
    const parsed = JSON.parse(props.message.content)
    return {
      jobId: parsed.job_id || '',
      fileName: parsed.file_name || '未知文件'
    }
  } catch {
    return {
      jobId: '',
      fileName: '文件消息'
    }
  }
})

// 获取传输任务状态
const transfers = useTransferStore()
const transferJob = computed(() => {
  if (!fileInfo.value?.jobId) return null
  return transfers.jobs.value.find((j) => j.jobId === fileInfo.value.jobId)
})

// 文件传输状态文本
const transferStatus = computed(() => {
  const job = transferJob.value
  if (!job) return '正在传输...'
  if (job.status === 'done') return '传输完成'
  if (job.status === 'failed') return '传输失败'
  if (job.status === 'cancelled') return '已取消'
  if (job.status === 'paused') return '已暂停'
  if (job.status === 'queued') return '排队中'
  return '正在传输...'
})

// 文件所在路径
const filePath = computed(() => {
  const job = transferJob.value
  if (!job) return ''
  return job.localPath || ''
})

/**
 * 解码失败（b64 合法但不是图片、或数据被截断）也要落到占位块。
 *
 * 只判断 parse 是不够的：JSON 合法 + base64 合法的垃圾数据依然无法解码，
 * 那时浏览器会画一个破图图标，比明确的占位块难懂得多。
 */
const broken = ref(false)

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

// 点击文件名打开所在文件夹
async function openFileLocation() {
  if (!filePath.value) return
  try {
    const api = (await import('../api')).then((m) => m.getApi())
    const apiInstance = await api()
    await apiInstance.revealFile(filePath.value)
  } catch (e: any) {
    console.error('打开文件位置失败:', e)
  }
}
</script>

<template>
  <div class="row" :class="{ out, tail }" :title="stateText">
    <!-- 自己的消息显示"我"字头像 -->
    <div v-if="out" class="ava self-avatar" :class="{ hidden: !tail }">我</div>
    <Avatar
      v-else
      class="ava"
      :class="{ hidden: !tail }"
      :seed="senderSeed || message.senderId"
      :name="senderName"
      :size="32"
      :online="null"
    />

    <div class="col">
      <div v-if="showSender && !out && first" class="who">{{ senderName }}</div>

      <div class="bubble" :class="{ 'bubble-image': isImage, 'bubble-file': isFile }">
        <!-- 时间显示在内容上面 -->
        <div class="bubble-time">{{ time }}</div>

        <template v-if="isImage">
          <img
            v-if="image && !broken"
            class="shot"
            :src="imageSrc(image)"
            :width="box.width"
            :height="box.height"
            alt="图片消息"
            @error="broken = true"
            @click="emit('zoom', message)"
          />
          <div v-else class="shot-broken" :style="{ width: box.width, height: box.height }">
            图片显示失败
          </div>
        </template>
        <template v-else-if="isFile">
          <div class="file-message">
            <Icon name="file" :size="32" />
            <div class="file-info">
              <div 
                class="file-name" 
                :class="{ clickable: filePath }"
                @click="openFileLocation"
              >
                {{ fileInfo?.fileName || '文件' }}
              </div>
              <div class="file-meta">{{ transferStatus }}</div>
            </div>
          </div>
        </template>
        <div v-else class="text">{{ message.content }}</div>
        <div class="meta">
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

/* 自己的头像：显示"我"字 */
.self-avatar {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  margin-top: 1px;
  border-radius: 50%;
  background: linear-gradient(135deg, #c93a82, #9c1f60);
  color: #fff;
  font-size: 14px;
  font-weight: 500;
  flex-shrink: 0;
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

/* 气泡顶部时间 */
.bubble-time {
  font-size: 10px;
  color: var(--t2);
  margin-bottom: 4px;
  user-select: none;
}

.row.out .bubble-time {
  color: rgba(255, 255, 255, 0.9);
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

/* 图片气泡：padding 收紧到 4px —— 图片自带留白，
   沿用文字的 padding 会让它看起来被「框」住了。 */
.bubble-image {
  padding: 4px 4px 2px;
  /* 文字气泡靠 pre-wrap 保留换行；图片气泡里没有文字，
     留着它只会把模板换行渲染成额外空隙。 */
  white-space: normal;
}

/* 文件气泡：展示文件图标和文件名 */
.bubble-file {
  padding: 10px 14px;
}

.file-message {
  display: flex;
  align-items: center;
  gap: 12px;
}

.file-info {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.file-name {
  font-size: var(--fs-md);
  font-weight: 500;
}

.file-name.clickable {
  text-decoration: underline;
  cursor: pointer;
  color: var(--brand);
}

.file-name.clickable:hover {
  opacity: 0.8;
}

.row.out .file-name.clickable {
  color: rgba(255, 255, 255, 0.9);
}

.file-meta {
  font-size: var(--fs-xs);
  color: var(--t3);
}

.row.out .file-meta {
  color: rgba(255, 255, 255, 0.76);
}

.shot {
  display: block;
  max-width: 220px;
  border-radius: var(--r-md);
  cursor: zoom-in;
  object-fit: contain;
  background: rgba(0, 0, 0, 0.08);
}

.shot-broken {
  display: flex;
  align-items: center;
  justify-content: center;
  max-width: 220px;
  border-radius: var(--r-md);
  background: rgba(0, 0, 0, 0.06);
  color: var(--t3);
  font-size: var(--fs-xs);
}

.meta {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 4px;
  margin-top: 2px;
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
