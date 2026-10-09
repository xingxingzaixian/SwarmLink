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
import { computed, ref, watchEffect } from 'vue'

import Avatar from './Avatar.vue'
import Icon from './Icon.vue'
import { getApi } from '../api'
import type { Message, TransferJob } from '../api/types'
import { bubbleTime, fileSize } from '../utils/format'
import { folderProgress } from '../utils/batch'
import { imageBox, imageSrc, parseImageContent } from '../utils/image'
import { parseFileContent, parseFileFailedContent, parseFolderContent } from '../utils/message'
import { useTransferStore } from '../stores/transfer'
import { useUiStore } from '../stores/ui'

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
const isFileFailed = computed(() => props.message.msgType === 'file_failed')
const isFolder = computed(() => props.message.msgType === 'folder')

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
 * 文件消息与失败消息的内容解析。
 *
 * 解析逻辑放在 utils/message.ts 里有测试兜着：这些字段名来自别人写下的
 * 数据（历史版本用过三种写法），容错不该埋在组件里靠肉眼看。
 */
const fileInfo = computed(() => (isFile.value ? parseFileContent(props.message.content) : null))
const fileFailed = computed(() =>
  isFileFailed.value ? parseFileFailedContent(props.message.content) : null
)

/**
 * 文件夹消息的内容解析。
 *
 * 与文件消息同理：解析失败（旧数据/损坏）要给占位卡片，而不是让整块聊天区崩掉。
 */
const folder = computed(() => (isFolder.value ? parseFolderContent(props.message.content) : null))

// 获取传输任务状态
//
// 注意 transfers.jobs 是【数组】而不是 ref：Pinia 的 setup store 会把
// 返回的 ref 自动解包（store.jobs 等价于 jobs.value）。这里写 `.value`
// 会得到 undefined，进而在渲染函数里抛 TypeError —— 而渲染期的异常会被
// Vue 吞掉并把整个气泡替换成一个空注释节点：文件消息看起来「根本没发出去」，
// 状态文案则永远停在兜底值「正在传输...」。
const transfers = useTransferStore()
const transferJob = computed(() => {
  const jobId = fileInfo.value?.jobId
  if (!jobId) return null
  return transfers.jobs.find((j) => j.jobId === jobId) ?? null
})

// 文件传输状态文本
const transferStatus = computed(() => {
  const job = transferJob.value
  if (!job) return '正在传输...'
  if (job.status === 'done') return '传输完成'
  if (job.status === 'failed') return '传输失败'
  if (job.status === 'cancelled') return '已取消'
  if (job.status === 'paused') return '已暂停'
  // 等待对方上线：判据与传输列表一致（看 waitReason，不看 status）——
  // 等待态与「还没轮到」共用 idle/queued
  if (job.waitReason === 'peer_offline') return '等待对方上线'
  if (job.status === 'queued') return '排队中'
  return '正在传输...'
})

// 文件所在路径
const filePath = computed(() => {
  const job = transferJob.value
  if (!job) return ''
  return job.localPath || ''
})

// --- 文件夹卡片的聚合进度 ---------------------------------------------------
//
// 数据来源有两条，缺一不可：
//   1. store 里的任务 —— 实时（进度事件都落在它上面）；
//   2. 向 ListBatch 补拉一次 —— 重启后 store 是空的，而 List() 又只有 50 条窗口，
//      一个 500 个文件的文件夹会因此算出缺斤少两的进度。

/** 本批次在 store 里的任务。 */
const storeBatchJobs = computed(() => {
  const id = folder.value?.batchId
  if (!id) return []
  return transfers.jobs.filter((j) => j.batchId === id)
})

/** 补拉来的快照（只用来补齐 store 看不见的那些任务）。 */
const fetchedBatchJobs = ref<TransferJob[]>([])

// 只在【看得到的条数少于消息里声明的条目数】时补拉一次：
// 卡片可能同时存在很多张，每张都无条件发一次 ListBatch 会打出几十个 RPC。
let folderFetched = false
watchEffect(() => {
  const f = folder.value
  if (!f || folderFetched) return
  if (f.fileCount === 0 || storeBatchJobs.value.length >= f.fileCount) return
  folderFetched = true
  void (async () => {
    try {
      fetchedBatchJobs.value = await (await getApi()).batchJobs(f.batchId)
    } catch {
      /* 补拉失败就只显示 store 里那部分，不为它弹错误 */
    }
  })()
})

/** 合并后的任务表：store 优先（它比补拉快照新），快照只负责补缺。 */
const batchJobList = computed<TransferJob[]>(() => {
  const byId = new Map<string, TransferJob>()
  for (const j of fetchedBatchJobs.value) byId.set(j.jobId, j)
  for (const j of storeBatchJobs.value) byId.set(j.jobId, j)
  return [...byId.values()]
})

const progress = computed(() => folderProgress(batchJobList.value, folder.value?.batchId ?? ''))

/**
 * 卡片上的进度文案。
 *
 * 三种结局必须分开说。把「N-1/N 完成，1 个失败」压成「已完成」，
 * 会让用户以为整个文件夹都到手了 —— 那正是这个功能最不能出的错。
 */
const folderStatus = computed(() => {
  const p = progress.value
  if (p.total === 0) return '' // 任务还没拿到（刚发起 / 后端还没回）
  if (!p.allFinished) return `已完成 ${p.done}/${p.total}`
  if (p.hasFailure) return `${p.done}/${p.total} 完成，${p.failed} 个失败`
  if (p.cancelled > 0) return `${p.done}/${p.total} 完成，${p.cancelled} 个已取消`
  return `已完成 ${p.done}/${p.total}`
})

/**
 * 还有没结束的任务、且这一批是【我们发出去的】才给「取消全部」。
 *
 * 两个条件缺一不可：
 *   - 全结束了就没什么可取消的；
 *   - 收到的文件夹没有「让对方停下」这种协议（CancelJob 只接受 send 方向），
 *     给一个点了必然报错的按钮，比不给按钮更糟 —— 用户会以为是自己点错了。
 */
const canCancelFolder = computed(
  () => props.out && progress.value.total > 0 && !progress.value.allFinished
)

const ui = useUiStore()

async function cancelFolder(): Promise<void> {
  const id = folder.value?.batchId
  if (!id) return
  try {
    await transfers.cancelBatch(id)
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || '取消失败')
  }
}

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

// 点击文件名打开所在文件夹。
//
// 原写法 `(await import('../api')).then((m) => m.getApi())` 是坏的：命名空间
// 对象上没有 then，拿到的也不是 API 实例 —— 点文件名只会在控制台留下一条
// 「打开文件位置失败」。这里直接用静态导入的 getApi()。
async function openFileLocation() {
  const path = filePath.value
  if (!path) return
  try {
    const api = await getApi()
    await api.revealFile(path)
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

      <div
        class="bubble"
        :class="{
          'bubble-image': isImage,
          'bubble-file': isFile || isFileFailed || isFolder
        }"
      >
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
        <!--
          传输失败：这条消息曾经没有渲染分支，于是它以一段裸 JSON
          （{"JobID":"","FileName":"","Reason":"文件已被删除或移动"}）的形式
          直接糊在聊天框里 —— 既看不懂，也占满了屏幕。
        -->
        <template v-else-if="isFileFailed">
          <div class="file-message failed">
            <Icon name="alert" :size="32" />
            <div class="file-info">
              <div class="file-name">{{ fileFailed?.fileName || '文件' }}</div>
              <div class="file-meta">{{ fileFailed?.reason || '发送失败' }}</div>
            </div>
          </div>
        </template>
        <!--
          文件夹批次：整批只出现【一条】卡片（几百个文件不能塞几百条消息）。
          进度按 batch_id 聚合传输任务，「取消全部」也是整批的动作。
        -->
        <template v-else-if="isFolder">
          <div class="file-message">
            <Icon name="folder" :size="32" />
            <div class="file-info">
              <div class="file-name">{{ folder?.folderName }}/</div>
              <div class="file-meta">
                {{ folder?.fileCount }} 个文件<template v-if="folder?.totalSize"
                  > · {{ fileSize(folder.totalSize) }}</template
                >
              </div>
              <!-- 有失败时用警示色：这正是用户需要停下来看一眼的那种结局 -->
              <div
                v-if="folderStatus"
                class="file-meta"
                :class="{ 'folder-failed': progress.hasFailure }"
              >
                {{ folderStatus }}
              </div>
              <div v-if="folder?.skipped" class="file-meta">
                已跳过 {{ folder.skipped }} 项
              </div>
            </div>
            <button
              v-if="canCancelFolder"
              class="folder-cancel"
              title="停下这一批里还没传完的文件"
              @click="cancelFolder"
            >
              取消全部
            </button>
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

/* 气泡顶部时间。
   收到消息的气泡底是浅色纸面，所以这里必须用纸面墨色而不是 --t2：
   深色主题下 --t2 是白色，白字落在白纸上会让时间戳彻底消失。 */
.bubble-time {
  font-size: 10px;
  color: var(--paper-ink-soft);
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

/* 同 .bubble-time：文件卡的副标题也落在纸面/粉面上，不吃全局 --t3 */
.file-meta {
  font-size: var(--fs-xs);
  color: var(--paper-ink-soft);
}

.row.out .file-meta {
  color: rgba(255, 255, 255, 0.76);
}

/* 传输失败：用警示色与「正在传输」的文件卡片区分开，
   否则用户看到的只是又一个文件条目，不会意识到它没发出去。
   --err-ink 在深浅两套主题下都是「当前底色上的可读色」。 */
.file-message.failed,
.file-message.failed .file-name,
.file-message.failed .file-meta,
.row.out .file-message.failed .file-meta {
  color: var(--err-ink);
}

/* 批次里有失败：只把这一行染红，文件名仍是正常墨色 ——
   整张卡片变红会让「文件夹本身没问题、只是少了一个文件」看起来像整批崩了。 */
.file-meta.folder-failed {
  color: var(--err-ink);
}

/* 「取消全部」：整批只有这一个出口，因此是卡片上的实体按钮，
   而不是传输列表里那种行内图标（那里一条任务只有一个图标位）。 */
.folder-cancel {
  flex: 0 0 auto;
  align-self: flex-start;
  padding: 3px 10px;
  border-radius: 999px;
  background: var(--err-soft);
  color: var(--err-ink);
  font-size: var(--fs-xs);
  transition:
    background var(--dur-1) var(--ease),
    color var(--dur-1) var(--ease);
}

.folder-cancel:hover {
  color: var(--t1);
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
  color: var(--paper-ink-soft);
  font-size: var(--fs-xs);
}

/* 自己的图片消息落在粉面上，纸面墨色在上面会发暗，换回白 */
.row.out .shot-broken {
  color: rgba(255, 255, 255, 0.82);
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
