<script setup lang="ts">
/**
 * 传输任务列表（右栏「传输」页与设置里的「文件传输」共用）。
 *
 * 关键取舍：进度条只承担「还剩多少」，文字承担「还要多久」。
 * 速率与 ETA 来自 progress 事件（4 Hz），刷新页面后可能暂时为空 ——
 * 因此它们的存在与否不能影响布局（用同一行内的可选片段，不换行占位）。
 */
import { computed } from 'vue'

import Icon from './Icon.vue'
import ProgressBar from './ProgressBar.vue'
import { getApi } from '../api'
import type { TransferJob } from '../api/types'
import { isActive } from '../stores/transfer'
import { usePeersStore } from '../stores/peers'
import { useUiStore } from '../stores/ui'
import { etaText, fileSize, isSending, speedText, transferStatus } from '../utils/format'

const props = withDefaults(
  defineProps<{
    jobs: TransferJob[]
    /** 是否显示「对方」名字（全局列表需要，单会话面板不需要）。 */
    showPeer?: boolean
  }>(),
  { showPeer: false }
)

const peers = usePeersStore()
const ui = useUiStore()

/** 在系统文件管理器里定位已传输的文件（Windows/macOS 会选中文件本身）。 */
async function reveal(path: string): Promise<void> {
  try {
    await (await getApi()).revealFile(path)
  } catch (e: any) {
    // 最常见的原因：文件已被手动删除或移动
    ui.notify(String(e?.message ?? e) || '无法定位该文件')
  }
}

/** 进行中的排前面，其余按更新时间倒序 —— 用户关心的是「还在跑的」。 */
const ordered = computed(() =>
  [...props.jobs].sort((a, b) => {
    const aa = isActive(a.status) ? 0 : 1
    const bb = isActive(b.status) ? 0 : 1
    if (aa !== bb) return aa - bb
    return b.updatedAt - a.updatedAt
  })
)

const running = computed(() => props.jobs.filter((j) => isActive(j.status)).length)

function percentOf(j: TransferJob): number {
  return Math.max(0, Math.min(100, j.percent || 0))
}
</script>

<template>
  <div class="wrap">
    <div v-if="!ordered.length" class="empty">
      <div class="empty-title">还没有传输记录</div>
      <div>在与某人的聊天里点「回形针」或把文件拖进聊天区即可发起。</div>
    </div>

    <template v-else>
      <div v-if="running" class="sum">
        <Icon name="activity" :size="13" />
        <span>{{ running }} 个任务进行中</span>
      </div>

      <article v-for="j in ordered" :key="j.jobId" class="job">
        <header class="head">
          <span class="dir" :class="isSending(j.direction) ? 'up' : 'down'">
            <Icon :name="isSending(j.direction) ? 'upload' : 'download'" :size="15" />
          </span>

          <div class="main">
            <div class="name nowrap" :title="j.fileName">
              {{ j.fileName || '正在获取文件名…' }}
            </div>
            <div class="meta">
              <span v-if="showPeer" class="nowrap">{{ peers.nameOf(j.peerId) }}</span>
              <span>{{ isSending(j.direction) ? '发送' : '接收' }}</span>
              <span v-if="j.fileSize">{{ fileSize(j.fileSize) }}</span>
              <span v-if="isActive(j.status)" class="pct">{{ percentOf(j).toFixed(0) }}%</span>
            </div>
          </div>

          <span class="tag" :class="transferStatus(j.status).cls">
            {{ transferStatus(j.status).text }}
          </span>
        </header>

        <ProgressBar :percent="j.percent" :status="j.status" />

        <div class="foot">
          <span v-if="speedText(j.speed)" class="mono faint">{{ speedText(j.speed) }}</span>
          <span v-if="etaText(j.etaMs)" class="faint">{{ etaText(j.etaMs) }}</span>
          <span class="spacer" />
          <span v-if="j.completed && j.fileSize" class="mono faint">
            {{ fileSize(j.completed) }} / {{ fileSize(j.fileSize) }}
          </span>
          <button
            v-if="j.status === 'done' && j.localPath"
            class="reveal"
            :title="`定位到：${j.localPath}`"
            aria-label="在文件管理器中显示该文件"
            @click="reveal(j.localPath)"
          >
            <Icon name="folder" :size="13" />
          </button>
        </div>

        <div v-if="j.error" class="err">{{ j.error }}</div>
      </article>
    </template>
  </div>
</template>

<style scoped>
.wrap {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 12px 14px 18px;
}

.sum {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--t2);
  font-size: var(--fs-sm);
}

.job {
  display: flex;
  flex-direction: column;
  gap: 7px;
  padding: 11px 12px;
  border-radius: var(--r-lg);
  background: var(--surface);
  box-shadow: inset 0 0 0 1px var(--edge), 0 8px 20px -16px rgba(2, 0, 12, 0.9);
}

.head {
  display: flex;
  align-items: flex-start;
  gap: 9px;
}

.dir {
  display: flex;
  align-items: center;
  justify-content: center;
  flex: 0 0 auto;
  width: 26px;
  height: 26px;
  border-radius: var(--r-sm);
}

.dir.up {
  background: var(--brand-soft);
  color: var(--brand-ink);
}

.dir.down {
  background: var(--ok-soft);
  color: var(--ok-ink);
}

.main {
  flex: 1 1 auto;
  min-width: 0;
}

.name {
  font-size: var(--fs-md);
  font-weight: 500;
}

.meta {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 1px;
  font-size: var(--fs-xs);
  color: var(--t3);
}

.pct {
  color: var(--t2);
  font-variant-numeric: tabular-nums;
}

.foot {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: var(--fs-xs);
  min-height: 14px;
}

.spacer {
  flex: 1 1 auto;
}

.err {
  color: var(--err-ink);
  font-size: var(--fs-xs);
  line-height: 1.5;
}

/* 定位按钮：只有图标。路径不占版面 —— 它就在 title 里，
   真要复制的人悬停即可；大多数时候用户只想知道「传到哪了」，点一下最实在。 */
.reveal {
  flex: 0 0 auto;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  padding: 0;
  border-radius: var(--r-sm);
  background: var(--surface-sunken);
  box-shadow: inset 0 0 0 1px var(--line);
  color: var(--t2);
  transition: background var(--dur-1) var(--ease), color var(--dur-1) var(--ease);
}

.reveal:hover {
  background: var(--hover);
  color: var(--t1);
}
</style>
