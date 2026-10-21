<script setup lang="ts">
/**
 * 全局传输队列浮层（规格 1.1）。
 *
 * 与传输列表的分工是刻意的：列表回答「哪些任务、各自到哪了」，浮层只回答
 * 四个数字（总进度 / 并发数 / 总网速 / 未结束任务数）和一个全局动作
 * （全部暂停 ⇄ 全部恢复），再把「要看细账」这件事交回列表。
 * 只在有未结束任务时出现 —— 没事的时候它不该占一条屏幕。
 *
 * 为什么数字都在前端算，而不是加一个后端聚合接口：
 *   - 速率只随 `transfer:progress` 事件到达（落库的 DTO 里没有速率），
 *     后端根本没有「整队速率」这个可查的量；
 *   - 未结束的任务总在列表窗口的前列（后端按 updated_at 倒序取 50 条，
 *     正在跑的每条 ≤2 秒就会刷新一行），所以这里的口径与用户在列表里
 *     看到的完全一致 —— 加接口只会多一份可能分叉的真相。
 */
import { computed, onMounted, ref, watch } from 'vue'

import Icon from './Icon.vue'
import { getApi } from '../api'
import type { TransferJob } from '../api/types'
import { useHotkeysStore } from '../stores/hotkeys'
import { canPause, isPaused, useTransferStore } from '../stores/transfer'
import { useUiStore } from '../stores/ui'
import { speedText, transferredBytes } from '../utils/format'

const ui = useUiStore()
const transfers = useTransferStore()
const hotkeys = useHotkeysStore()

/** 与传输列表同一口径：DTO 的 percent 可能是空的（刚建出来的壳）。 */
function percentOf(j: TransferJob): number {
  return Math.max(0, Math.min(100, j.percent || 0))
}

/**
 * 未结束的任务：浮层的每个数字都只描述这一批。
 *
 * 直接复用 store 里那份 `activeJobs`（左栏角标与它同源），而不是在这里再
 * filter 一次 —— App 外壳要靠同一个集合决定「底部给浮层留多少空间」，
 * 两处各算各的就会出现「留了空间却没有浮层」或者反过来压住输入框。
 */
const active = computed(() => transfers.activeJobs)

/**
 * 「正在传」的条数 —— 判据就是后端 `PauseJob` 认的那两种状态（meta_exchange /
 * transferring），因此它同时是「能被暂停的」与「正在占额度」的那一批。
 */
const running = computed(() => active.value.filter((j) => canPause(j)))
const paused = computed(() => active.value.filter((j) => isPaused(j)))

/**
 * 总进度按【字节】加权，不让一个大文件和一个小文件各算 50%。
 *
 * 分母只算「拿得到大小」的任务：刚建出来的壳还带着 fileSize=0，
 * 把它算进去会让百分比被 0 稀释。
 */
const metered = computed(() => active.value.filter((j) => j.fileSize > 0))
const totalBytes = computed(() => metered.value.reduce((s, j) => s + j.fileSize, 0))
const doneBytes = computed(() =>
  metered.value.reduce((s, j) => s + transferredBytes(j.fileSize, percentOf(j), false), 0)
)
const percent = computed(() =>
  totalBytes.value ? Math.min(100, Math.round((doneBytes.value / totalBytes.value) * 100)) : 0
)

/** 总网速 = 各任务速率之和。等待中与已暂停的任务没有速率，天然贡献 0。 */
const speed = computed(() => active.value.reduce((s, j) => s + (j.speed ?? 0), 0))

/**
 * 全局并发上限（只约束本机发起的传输：接收侧由对端驱动，不占这个额度）。
 *
 * 读一次设置，并在「配置被别处改过」时回读 —— `ui.configRevision` 正是为
 * 这种事准备的（托盘菜单也能改配置，设置页与本浮层都不能拿着旧快照显示）。
 */
const maxConcurrent = ref(0)

async function loadSettings(): Promise<void> {
  try {
    maxConcurrent.value = (await (await getApi()).getSettings()).maxConcurrent || 0
  } catch {
    maxConcurrent.value = 0 // 读不到就不显示上限，而不是显示一个编出来的 3
  }
}

onMounted(loadSettings)
watch(() => ui.configRevision, loadSettings)

const busy = ref(false)

/**
 * 全部暂停：逐一走既有的 `pauseTransfer`（后端 PauseJob 已有完整的在途中断、
 * 对端信令与状态广播），这里只负责「对哪些任务、按哪个顺序、失败怎么算」。
 *
 * 只暂停【正在传】的那些，与 D-07 划下的边界一致：等待对方上线、批次排队
 * 中的任务本来就还没开始跑，它们有自己的出口（立即重试 / 取消）。因此在一个
 * 文件夹批次里点这个按钮，停住的是此刻在传的那个文件，队列里下一个仍会照常
 * 开始 —— 要连队列一起停，用批次卡片上的「全部取消」。
 */
async function pauseAll(): Promise<void> {
  const targets = running.value
  if (!targets.length || busy.value) return
  busy.value = true
  const api = await getApi()
  let ok = 0
  // 串行而不是 Promise.all：一条一条来才对得上用户的直觉（点一下 = 停一个），
  // 也让失败点可归属。数量本来就受并发上限约束，不值得为它并发。
  for (const j of targets) {
    try {
      await api.pauseTransfer(j.jobId)
      ok++
    } catch {
      /* 单条失败不打断整批：用户点的是「都停下」，尽力而为比半途而废好 */
    }
  }
  busy.value = false
  ui.notify(
    ok === targets.length
      ? `已暂停 ${ok} 个任务`
      : `已暂停 ${ok} 个，${targets.length - ok} 个未能暂停（见传输列表）`
  )
}

/**
 * 全部恢复：规格只写了「全部暂停」，这一半是必须补的 ——
 * 一个只能单向停下的全局按钮会把用户送进死胡同（队里几十条要一条条点回来）。
 *
 * 与暂停同理地忽略单条失败：最典型的失败是「对方也按着暂停」，
 * 那时正确的动作就是什么都不做（对方恢复后会自动继续）。
 */
async function resumeAll(): Promise<void> {
  const targets = paused.value
  if (!targets.length || busy.value) return
  busy.value = true
  const api = await getApi()
  let ok = 0
  for (const j of targets) {
    try {
      await api.resumeTransfer(j.jobId)
      ok++
    } catch {
      /* 同上：交给对端与列表去解释这一条为什么没动 */
    }
  }
  busy.value = false
  ui.notify(
    ok === targets.length
      ? `已恢复 ${ok} 个任务`
      : `已恢复 ${ok} 个，${targets.length - ok} 个未恢复（可能被对方暂停）`
  )
}
</script>

<template>
  <Transition name="queue">
    <div v-if="active.length" class="queue" role="status">
      <!-- 顶部一条 2px 的整队进度：浮层里唯一允许用强调色的地方，
           它要能被余光扫到，又不该像按钮那样抢注意力。 -->
      <div class="queue-track" aria-hidden="true">
        <div class="queue-fill" :style="{ transform: `scaleX(${percent / 100})` }" />
      </div>

      <Icon name="activity" :size="14" class="queue-ico" />
      <span class="queue-title">{{ active.length }} 个任务进行中</span>

      <span
        class="queue-chip"
        :title="`正在传 ${running.length} 个 / 全局并发上限 ${maxConcurrent || '未设置'}（上限只约束本机发起的传输，接收由对方驱动）`"
      >
        并发 <b>{{ running.length }}</b><template v-if="maxConcurrent">/{{ maxConcurrent }}</template>
      </span>

      <span v-if="speedText(speed)" class="queue-chip" title="所有任务速率之和">
        {{ speedText(speed) }}
      </span>

      <span class="spacer" />

      <span v-if="totalBytes" class="queue-pct" title="当前未结束任务的字节加权进度">
        {{ percent }}%
      </span>

      <button
        v-if="running.length"
        class="btn btn-soft"
        :disabled="busy"
        title="暂停所有正在传输的任务（等待中与批次排队中的不受影响）"
        @click="pauseAll"
      >
        <Icon name="pause" :size="13" />暂停全部
      </button>
      <button
        v-else-if="paused.length"
        class="btn btn-soft"
        :disabled="busy"
        title="恢复所有已暂停的任务（被对方暂停的那些会跳过）"
        @click="resumeAll"
      >
        <Icon name="play" :size="13" />全部恢复
      </button>

      <button class="btn btn-ghost" :title="hotkeys.withHint('打开完整传输列表', 'transfers')"
        @click="ui.openPanel('transfers')">
        查看
      </button>
    </div>
  </Transition>
</template>

<style scoped>
.queue {
  position: fixed;
  left: 0;
  right: 0;
  bottom: 0;
  /* 低于 Modal 的 100：弹层打开时它（连同它压暗的背景）不该浮在弹层之上 */
  z-index: 90;
  height: var(--queue-h);
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 0 16px;
  /* 比面板再亮一档：它是一条浮在所有列之上的层，不该和左右栏融成一片 */
  background: var(--bg-elevated);
  border-top: 1px solid var(--divider-strong);
  font-size: var(--fs-sm);
  color: var(--text-secondary);
}

.queue-track {
  position: absolute;
  top: -1px;
  left: 0;
  right: 0;
  height: 2px;
  background: var(--bg-hover);
}

.queue-fill {
  width: 100%;
  height: 100%;
  background: var(--accent);
  transform-origin: left center;
  /* 缩放而不是改宽度：进度每来一帧都要动一次，走 transform 不触发布局 */
  transition: transform var(--dur-3) var(--ease);
}

.queue-ico {
  flex: 0 0 auto;
  color: var(--accent);
}

/* 允许被压缩：窗口窄的时候先让这句话省略，而不是把右侧的按钮挤出屏幕 */
.queue-title {
  flex: 0 1 auto;
  min-width: 0;
  color: var(--text-primary);
  font-weight: 500;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* 数字一律 mono：它们是「机器说的话」，与列表里的速率/大小同一口径 */
.queue-chip,
.queue-pct {
  flex: 0 0 auto;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-muted);
}

.queue-chip b {
  color: var(--text-secondary);
  font-weight: 600;
}

.queue-pct {
  font-size: var(--fs-sm);
  color: var(--text-primary);
  font-variant-numeric: tabular-nums;
}

.spacer {
  flex: 1 1 auto;
}

/* 进出场：从下方推上来。用 transform 而不是高度，避免每帧重排整页 */
.queue-enter-active,
.queue-leave-active {
  transition: transform var(--dur-3) var(--ease), opacity var(--dur-3) var(--ease);
}

.queue-enter-from,
.queue-leave-to {
  transform: translateY(100%);
  opacity: 0;
}
</style>
