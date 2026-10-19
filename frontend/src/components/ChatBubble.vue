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
import { computed, onBeforeUnmount, ref, watch, watchEffect } from 'vue'

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
    /**
     * 是否是「搜索跳转」的目标消息：短暂高亮一下。
     *
     * 由 ChatView 按 store 的 anchorMsgId 比对后传入，2s 后自动撤回 ——
     * 高亮本身不该由气泡自己计时，否则同一条消息被连点两次会留下两个定时器。
     */
    anchor?: boolean
  }>(),
  { senderName: '', senderSeed: '', showSender: false, tail: true, first: true, anchor: false }
)

const emit = defineEmits<{ (e: 'zoom', m: Message): void; (e: 'quote', m: Message): void }>()

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

/**
 * 群消息的未送达提示。
 *
 * 只对【自己发的】且确实还有人没收到的消息显示。后端在全员送达后会清掉统计，
 * 因此这里的 0 就是「都到了」，不必再区分 null / 0。
 */
const pendingText = computed(() => {
  if (!props.out) return ''
  const n = props.message.pendingCount ?? 0
  if (n <= 0) return ''
  const total = props.message.memberCount ?? 0
  return total > 0 ? `${n}/${total} 人未送达` : `${n} 人未送达`
})

// ---------------------------------------------------------------- 右键菜单
//
// 只放「复制」与「引用」两项：这两件是看消息时最常做的，且都不改数据。
// 转发 / 删除涉及跨会话与不可逆语义，等有明确需求再做。
const menu = ref<{ x: number; y: number } | null>(null)

function onContextMenu(e: MouseEvent): void {
  e.preventDefault()
  menu.value = { x: e.clientX, y: e.clientY }
}

function closeMenu(): void {
  menu.value = null
}

function onDocClick(): void {
  closeMenu()
}

watch(menu, (open) => {
  if (open) document.addEventListener('click', onDocClick, true)
  else document.removeEventListener('click', onDocClick, true)
})

onBeforeUnmount(() => document.removeEventListener('click', onDocClick, true))

/** 可复制的文本：图片/文件/文件夹的 content 是 JSON 信封，透出去没人看得懂。 */
function copyableText(): string {
  if (isImage.value) return '[图片]'
  if (isFile.value) return fileInfo.value?.fileName || '文件'
  if (isFileFailed.value) return fileFailed.value?.fileName || '文件'
  if (isFolder.value) return `${folder.value?.folderName ?? '文件夹'}/`
  return props.message.content
}

async function copyMessage(): Promise<void> {
  const text = copyableText()
  closeMenu()
  try {
    await navigator.clipboard.writeText(text)
    ui.notify('已复制')
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || '复制失败')
  }
}

function quoteMessage(): void {
  closeMenu()
  emit('quote', props.message)
}

// ---------------------------------------------------------------- 文本渲染
//
// 两件事在渲染层完成，都不改消息模型：
//   1. `@名字` 高亮（提及）；
//   2. 以 `> ` 开头的行渲染成引用块（见 ChatView 的 quoteMessage）。
interface TextSeg {
  t: 'text' | 'mention'
  v: string
}
interface TextLine {
  quote: boolean
  segs: TextSeg[]
}

function splitMentions(s: string): TextSeg[] {
  const out: TextSeg[] = []
  const re = /@([^\s@]+)/g
  let last = 0
  let m: RegExpExecArray | null
  while ((m = re.exec(s)) !== null) {
    if (m.index > last) out.push({ t: 'text', v: s.slice(last, m.index) })
    out.push({ t: 'mention', v: m[0] })
    last = m.index + m[0].length
  }
  if (last < s.length) out.push({ t: 'text', v: s.slice(last) })
  return out
}

const textLines = computed<TextLine[]>(() =>
  (props.message.content ?? '').split('\n').map((raw) => {
    const quote = raw.startsWith('> ')
    return { quote, segs: splitMentions(quote ? raw.slice(2) : raw) }
  })
)

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
  <div class="row" :class="{ out, tail, anchor }" :title="stateText">
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
        @contextmenu="onContextMenu"
      >
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
            <div class="file__icon"><Icon name="file" :size="18" /></div>
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
            <div class="file__icon"><Icon name="alert" :size="18" /></div>
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
            <div class="file__icon"><Icon name="folder" :size="18" /></div>
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
        <!--
          文本：按行渲染，`> ` 开头的行是引用块、`@名字` 高亮。
          行尾补一个换行字符而不是 <br> —— 气泡是 pre-wrap，加 <br> 会变成空两行。
        -->
        <div v-else class="text">
          <template v-for="(line, li) in textLines" :key="li">
            <span :class="{ 'quote-line': line.quote }">
              <template v-for="(s, si) in line.segs" :key="si">
                <span v-if="s.t === 'mention'" class="mention">{{ s.v }}</span>
                <template v-else>{{ s.v }}</template>
              </template>
              <template v-if="li < textLines.length - 1">{{ '\n' }}</template>
            </span>
          </template>
        </div>
      </div>

      <!-- 时间与送达状态都放在气泡【外面】。放里面有两个问题：
           正文会被挤出一条对不齐的窄行；而自己的气泡是深色渐变，
           时间戳在那里只能靠调透明度凑对比度。 -->
      <div class="meta">
        <span class="stamp">{{ time }}</span>
        <span v-if="pendingText" class="pending">{{ pendingText }}</span>
        <Icon
          v-if="stateIcon"
          :name="stateIcon"
          :size="12"
          :class="['state', message.state]"
        />
      </div>
    </div>

    <!-- 右键菜单：固定定位，因为它在气泡的 overflow 之外 -->
    <Teleport to="body">
      <div
        v-if="menu"
        class="ctx-menu"
        :style="{ left: menu.x + 'px', top: menu.y + 'px' }"
        @click.stop
      >
        <button class="ctx-item" @click="copyMessage">
          <Icon name="copy" :size="14" />复制
        </button>
        <button class="ctx-item" @click="quoteMessage">
          <Icon name="back" :size="14" />引用
        </button>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.row {
  display: flex;
  align-items: flex-start;
  gap: 11px;
  /* 入场：新消息逐条上浮显形。已存在的行不会重播 ——
     Vue 复用 DOM，只有新插入的那一行才走到这段动画。 */
  animation: rise 0.55s var(--ease) both;
}

/* 连续消息之间收紧；一组结束后留出节奏感（20px 是设计系统定的消息间距） */
.row:not(.tail) {
  margin-top: 2px;
}

.row.tail {
  margin-top: 20px;
}

.row.out {
  flex-direction: row-reverse;
}

.ava {
  /* 用 visibility 而不是 v-if：占位必须保留，否则气泡会左右跳 */
  margin-top: 2px;
}

.ava.hidden {
  visibility: hidden;
}

/* 自己的头像：与 Avatar 的「我」保持同一套渐变（accent → warm） */
.self-avatar {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 38px;
  height: 38px;
  margin-top: 2px;
  border-radius: 50%;
  background: linear-gradient(135deg, var(--accent), var(--warm));
  color: var(--accent-ink);
  font-family: var(--font-display);
  font-size: 15px;
  font-weight: 600;
  flex-shrink: 0;
}

.col {
  display: flex;
  flex-direction: column;
  min-width: 0;
  max-width: min(568px, 74%);
}

.row.out .col {
  align-items: flex-end;
}

.who {
  margin: 0 0 5px 2px;
  font-family: var(--font-display);
  font-size: var(--fs-sm);
  color: var(--text-muted);
}

/* 对方的气泡：浮起的中性卡片（--bg-elevated + 一道亮边）。
   这是深色界面上把「别人的话」与背景分开的标准做法，
   而且是靠明度分层，不是靠透明度 —— 长文本不会跟着背景漂。 */
.bubble {
  position: relative;
  padding: 11px 15px;
  border-radius: var(--r-md);
  font-size: var(--fs-lg);
  line-height: 1.55;
  word-break: break-word;
  white-space: pre-wrap;
  background: var(--bg-elevated);
  border: 1px solid var(--divider-strong);
  color: var(--text-primary);
  /* 靠头像那一角收小：气泡「指向说话人」的暗示，比画三角形稳 */
  border-top-left-radius: 4px;
  transition: transform var(--dur-2) var(--ease);
}

.bubble:hover {
  transform: translateY(-1px);
}

/* 自己的气泡 = 信号绿渐变 + 深绿字。
   整屏只有它与发送键用这条渐变 —— 强调色的预算全花在「我发的」上。 */
.row.out .bubble {
  border-top-left-radius: var(--r-md);
  border-top-right-radius: 4px;
  background: linear-gradient(135deg, var(--accent), var(--accent-strong));
  border-color: transparent;
  color: var(--accent-ink);
  box-shadow: 0 8px 24px -10px oklch(86% 0.19 140 / 0.55);
}

/* ---------------------------------------------------------------- 跳转高亮 */
/*
 * 搜索跳转到的目标消息：2s 的描边呼吸。
 *
 * 用 box-shadow 描边而不是改底色：气泡有三套底色（对方卡片 / 自己的绿渐变 /
 * 图片无底），改底色就得写三份并各自调对比度；描边对三者都成立。
 * 颜色全部走 var()，浅色主题下不用另写一份。
 */
.row.anchor .bubble {
  animation: anchor-flash 2s var(--ease) both;
}

@keyframes anchor-flash {
  0%,
  55% {
    box-shadow: 0 0 0 2px var(--accent), 0 0 0 7px var(--accent-soft);
  }
  100% {
    box-shadow: 0 0 0 2px transparent, 0 0 0 7px transparent;
  }
}

/*
 * 减少动态效果时「只变色不闪」：去掉动画，保留一圈静态描边，
 * 2s 后随 anchor 类一起撤掉 —— 定位提示仍然看得见，但没有任何运动。
 */
@media (prefers-reduced-motion: reduce) {
  .row.anchor .bubble {
    animation: none;
    box-shadow: 0 0 0 2px var(--accent), 0 0 0 7px var(--accent-soft);
  }
}

.text {
  font-size: var(--fs-lg);
  line-height: 1.55;
}

/* 引用块：左侧一条竖线 + 稍微压暗。它必须一眼可辨，
   否则「引用」和「复制了一段话」在界面上完全一样。 */
.quote-line {
  display: inline;
  border-left: 2px solid currentColor;
  margin-right: 2px;
  padding-left: 8px;
  opacity: 0.82;
}

/* @提及：强调色 + 极淡底。深色自己气泡上换成墨色，避免绿底叠绿字。 */
.mention {
  color: var(--accent);
  font-weight: 600;
}

.row.out .mention {
  color: var(--accent-ink);
  text-decoration: underline;
  text-underline-offset: 2px;
}

/* 未送达计数：与时间戳同排，但要更显眼一点（它是「有东西没到」） */
.pending {
  font-family: var(--font-mono);
  font-size: 10.5px;
  color: var(--warn);
}

.row.out .pending {
  color: color-mix(in oklch, var(--accent-ink) 82%, transparent);
}

/* 右键菜单：固定定位在光标处（Teleport 到 body 以逃出气泡的裁剪） */
.ctx-menu {
  position: fixed;
  z-index: 200;
  min-width: 132px;
  padding: 5px;
  border-radius: var(--r-sm);
  background: var(--bg-elevated);
  border: 1px solid var(--divider-strong);
  box-shadow: var(--shadow-lg);
}

.ctx-item {
  display: flex;
  align-items: center;
  gap: 9px;
  width: 100%;
  height: 30px;
  padding: 0 9px;
  border-radius: var(--r-xs);
  font-size: var(--fs-md);
  color: var(--text-primary);
  text-align: left;
  transition: background var(--dur-1) var(--ease), color var(--dur-1) var(--ease);
}

.ctx-item:hover {
  background: var(--bg-hover);
  color: var(--accent);
}

/* 图片气泡：无底色、无内边距、无边框。
   图片自带留白，再套一层卡片就是「框中框」；白边还会吃掉缩略图的一圈。
   注意 pre-wrap 要关掉：气泡里没有文字，留着它会把模板换行渲染成空隙。 */
.bubble-image {
  padding: 0;
  background: none;
  border: 0;
  box-shadow: none;
  white-space: normal;
}

/* 文件 / 文件夹 / 失败卡：左 38px 图标块 + 名称与元信息 */
.bubble-file {
  padding: 10px 14px;
}

.file-message {
  display: flex;
  align-items: center;
  gap: 12px;
}

/* 图标块：对方用 accent-soft（暗示「这是一份内容」），
   自己用深绿半透明 —— 绿渐变底上没有对比空间再放绿图标。 */
.file__icon {
  width: 38px;
  height: 38px;
  flex-shrink: 0;
  border-radius: var(--r-sm);
  display: grid;
  place-items: center;
  background: var(--accent-soft);
  color: var(--accent);
}

.row.out .file__icon {
  background: color-mix(in oklch, var(--accent-ink) 18%, transparent);
  color: var(--accent-ink);
}

.file-info {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.file-name {
  font-weight: 600;
}

.file-name.clickable {
  text-decoration: underline;
  text-underline-offset: 2px;
  cursor: pointer;
}

.file-name.clickable:hover {
  opacity: 0.82;
}

.file-meta {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-muted);
}

/* 绿底上不能用中性灰（会发脏），用强调色墨色压一档透明度 */
.row.out .file-meta {
  color: color-mix(in oklch, var(--accent-ink) 72%, transparent);
}

/* 传输失败：整块换警示色，与「正在传输」的文件卡彻底区分开 ——
   否则用户看到的只是又一个文件条目，不会意识到它没发出去。 */
.file-message.failed,
.file-message.failed .file-name,
.file-message.failed .file-meta {
  color: var(--danger);
}

.file-message.failed .file__icon {
  background: var(--danger-soft);
  color: var(--danger);
}

/* 批次里有失败：只把这一行染红，文件名仍是正常墨色 ——
   整张卡片变红会让「文件夹本身没问题、只是少了一个文件」看起来像整批崩了。 */
.file-meta.folder-failed {
  color: var(--danger);
}

/* 「取消全部」：整批只有这一个出口，因此是卡片上的实体按钮，
   而不是传输列表里那种行内图标（那里一条任务只有一个图标位）。 */
.folder-cancel {
  flex: 0 0 auto;
  align-self: flex-start;
  padding: 3px 10px;
  border-radius: 999px;
  background: var(--danger-soft);
  color: var(--danger);
  font-family: var(--font-body);
  font-size: var(--fs-xs);
  transition: background var(--dur-1) var(--ease);
}

/* hover 只加浓底色，不换字色：白字落在浅色主题的深红上是达标的，
   但落在深色主题的亮红上会糊，换字色属于「两套主题里必然坏一套」。 */
.folder-cancel:hover {
  background: color-mix(in oklch, var(--danger) 28%, transparent);
}

.shot {
  display: block;
  max-width: 220px;
  border-radius: var(--r-md);
  cursor: zoom-in;
  object-fit: contain;
  transition: transform var(--dur-2) var(--ease);
}

.shot:hover {
  transform: scale(1.02);
}

.shot-broken {
  display: flex;
  align-items: center;
  justify-content: center;
  max-width: 220px;
  border-radius: var(--r-md);
  background: var(--bg-elevated);
  border: 1px solid var(--divider);
  color: var(--text-muted);
  font-size: var(--fs-xs);
}

/* 时间 + 送达状态：气泡下方一行等宽小字 */
.meta {
  display: flex;
  align-items: center;
  gap: 5px;
  margin-top: 5px;
}

.stamp {
  font-family: var(--font-mono);
  font-size: 10.5px;
  color: var(--text-muted);
  user-select: none;
}

.state {
  opacity: 0.9;
}

.state.pending {
  animation: breathe 1.4s var(--ease) infinite;
}

.state.failed {
  color: var(--danger);
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
