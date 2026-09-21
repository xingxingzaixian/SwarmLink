<script setup lang="ts">
/**
 * 调试面板（弹层的「调试」页）。
 *
 * P2P 的失败往往是「谁都没报错，但事情没发生」。把内部事件流摊开给用户看，
 * 是把这类问题从「猜」变成「看」的最短路径 —— 投入产出比极高。
 *
 * 改成弹层内容后【不删任何信息】：只把原来的顶栏按钮下移成工具条，
 * 统计区从常驻改为可折叠（它首屏有用，之后就是占地方）。
 */
import { computed, ref, watch } from 'vue'

import Icon from '../components/Icon.vue'
import { useDebugStore } from '../stores/debug'
import { usePeersStore } from '../stores/peers'
import { useTransferStore } from '../stores/transfer'
import { clockTime } from '../utils/format'

const debug = useDebugStore()
const peers = usePeersStore()
const transfers = useTransferStore()

const showSummary = ref(false)
const stick = ref(true)

const diag = computed(() => peers.diag)

/** 与常驻连接数并列展示：诊断面板的价值就是把这两条断言摆在明面上。 */
const activeTransfers = computed(() => transfers.activeJobs.length)

async function refreshDiag(): Promise<void> {
  await Promise.all([peers.refresh(), transfers.refresh()])
}

// 事件流跟随底部，但要尊重用户往上翻的动作
const stream = ref<HTMLElement | null>(null)

function onScroll(): void {
  const el = stream.value
  if (!el) return
  stick.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}

watch(
  () => debug.events.length,
  () => {
    if (!stick.value) return
    void Promise.resolve().then(() => {
      const el = stream.value
      if (el) el.scrollTop = el.scrollHeight
    })
  }
)
</script>

<template>
  <div class="debug">
    <div class="bar">
      <button class="btn btn-soft" @click="refreshDiag">
        <Icon name="refresh" :size="14" />刷新统计
      </button>
      <button class="btn btn-soft" @click="debug.togglePause()">
        <Icon :name="debug.paused ? 'activity' : 'clock'" :size="14" />
        {{ debug.paused ? '继续' : '暂停' }}
      </button>
      <button class="btn btn-soft" @click="debug.clear()">
        <Icon name="x" :size="14" />清空
      </button>
      <span class="spacer" />
      <button class="btn btn-soft" @click="showSummary = !showSummary">
        {{ showSummary ? '隐藏主题统计' : '主题统计' }}
      </button>
    </div>

    <div v-if="diag" class="stats">
      <div class="stat">
        <span class="k">节点</span>
        <span class="v">{{ diag.peersTotal }}</span>
        <span class="u">在线 {{ diag.peersOnline }}</span>
      </div>
      <div class="stat">
        <span class="k">常驻连接</span>
        <!-- ADR-009 的可观测断言：空闲时应为 0 -->
        <span class="v">{{ diag.activeSessions }}</span>
        <span class="u">空闲应为 0</span>
      </div>
      <div class="stat">
        <span class="k">活跃传输</span>
        <span class="v">{{ activeTransfers }}</span>
      </div>
      <div class="stat wide">
        <span class="k">网卡</span>
        <span class="v mono">{{ diag.interfaceSummary?.join('、') || '—' }}</span>
      </div>
      <div v-if="diag.seedDetails?.length" class="stat wide">
        <span class="k">种子</span>
        <div class="seeds">
          <span v-for="s in diag.seedDetails" :key="s.addr" class="seed mono">
            {{ s.addr }}
            <span class="tag" :class="s.failCount > 0 ? 'err' : 'ok'">
              {{ s.failCount > 0 ? `失败 ${s.failCount}` : '正常' }}
            </span>
          </span>
        </div>
      </div>
    </div>

    <div v-if="showSummary" class="summary">
      <div v-if="!debug.summary.length" class="faint">（暂无事件）</div>
      <div v-for="s in debug.summary" :key="s.topic" class="srow">
        <span class="mono nowrap">{{ s.topic }}</span>
        <span class="tag">{{ s.count }}</span>
      </div>
    </div>

    <div ref="stream" class="stream scroll" @scroll="onScroll">
      <div v-if="!debug.events.length" class="empty">
        <div class="empty-title">事件流为空</div>
        <div>节点发现、握手、消息到达、传输进度都会实时出现在这里。</div>
      </div>
      <div v-for="(e, i) in debug.events" :key="i" class="event">
        <span class="t mono">{{ clockTime(e.at) }}</span>
        <span class="topic">{{ e.topic }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.debug {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
}

.bar {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: 0 0 auto;
  padding: 12px 20px;
}

.spacer {
  flex: 1 1 auto;
}

/* 用「1px 间隙露出底色」画格线：比给每格加边框少一半规则，
   而且在半透明底上不会出现边框叠加变深的问题 */
.stats {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 1px;
  flex: 0 0 auto;
  margin: 0 20px 12px;
  border-radius: var(--r-lg);
  overflow: hidden;
  background: var(--line);
  box-shadow: inset 0 0 0 1px var(--edge);
}

.stat {
  display: flex;
  flex-direction: column;
  gap: 1px;
  padding: 10px 12px;
  background: var(--surface-sunken);
}

.stat.wide {
  grid-column: 1 / -1;
}

.k {
  font-size: var(--fs-xs);
  color: var(--t3);
}

.v {
  font-size: var(--fs-xl);
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}

.stat.wide .v {
  font-size: var(--fs-sm);
  font-weight: 400;
}

.u {
  font-size: 10px;
  color: var(--t3);
}

.seeds {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.seed {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.summary {
  display: flex;
  flex-direction: column;
  gap: 2px;
  flex: 0 0 auto;
  max-height: 132px;
  overflow-y: auto;
  margin: 0 20px 12px;
  padding: 8px 10px;
  border-radius: var(--r-md);
  background: var(--surface-sunken);
  box-shadow: inset 0 0 0 1px var(--edge);
}

.srow {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  font-size: var(--fs-xs);
  color: var(--t2);
}

.stream {
  flex: 1 1 auto;
  min-height: 0;
  padding: 6px 20px 18px;
}

.event {
  display: flex;
  gap: 10px;
  padding: 2px 0;
  font-size: var(--fs-xs);
  line-height: 1.6;
}

.t {
  flex: 0 0 auto;
  color: var(--t3);
}

.topic {
  color: var(--t2);
  word-break: break-all;
}
</style>
