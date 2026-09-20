<script setup lang="ts">
import { computed, ref } from 'vue'

import { useDebugStore } from '../stores/debug'
import { usePeersStore } from '../stores/peers'

/**
 * 调试面板（v1.0 就做）。
 *
 * P2P 的失败往往是「谁都没报错，但事情没发生」。把内部事件流摊开给用户看，
 * 是把这类问题从「猜」变成「看」的最短路径 —— 投入产出比极高。
 */
const debug = useDebugStore()
const peers = usePeersStore()

const showSummary = ref(true)

const diag = computed(() => peers.diag)

function time(ms: number): string {
  const d = new Date(ms)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}:${String(
    d.getSeconds()
  ).padStart(2, '0')}`
}
</script>

<template>
  <div class="pane">
    <div class="pane-head">
      <div class="pane-title">调试面板</div>
      <div class="head-actions">
        <button @click="showSummary = !showSummary">{{ showSummary ? '隐藏统计' : '显示统计' }}</button>
        <button @click="debug.togglePause()">{{ debug.paused ? '继续' : '暂停' }}</button>
        <button @click="debug.clear()">清空</button>
      </div>
    </div>

    <div v-if="diag" class="diag">
      <div class="diag-row">
        <span class="faint">节点</span>
        <span>{{ diag.peersTotal }}（在线 {{ diag.peersOnline }}）</span>
      </div>
      <div class="diag-row">
        <span class="faint">常驻连接</span>
        <!-- ADR-009 的可观测断言：空闲时应为 0 -->
        <span>{{ diag.activeSessions }}</span>
      </div>
      <div class="diag-row">
        <span class="faint">活跃传输</span>
        <span>{{ diag.activeTransfers }}</span>
      </div>
      <div class="diag-row">
        <span class="faint">网卡</span>
        <span class="mono">{{ diag.interfaceSummary?.join(', ') || '—' }}</span>
      </div>
      <div v-if="diag.seedDetails?.length" class="diag-row">
        <span class="faint">种子</span>
        <div class="seeds">
          <div v-for="s in diag.seedDetails" :key="s.addr" class="mono seed">
            {{ s.addr }}
            <span v-if="s.failCount > 0" class="tag err">fail={{ s.failCount }}</span>
            <span v-else class="tag ok">ok</span>
          </div>
        </div>
      </div>
    </div>

    <div v-if="showSummary" class="summary">
      <div v-if="!debug.summary.length" class="faint">（暂无事件）</div>
      <div v-for="s in debug.summary" :key="s.topic" class="diag-row">
        <span class="mono">{{ s.topic }}</span>
        <span class="tag">{{ s.count }}</span>
      </div>
    </div>

    <div class="pane-body stream">
      <div v-if="!debug.events.length" class="empty">
        事件流为空。<br />
        <span class="faint">节点发现、消息到达、传输进度都会实时出现在这里。</span>
      </div>
      <div v-for="(e, i) in debug.events" :key="i" class="event mono">
        <span class="faint">{{ time(e.at) }}</span>
        <span class="topic">{{ e.topic }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.head-actions {
  display: flex;
  gap: 4px;
}

.head-actions button {
  padding: 2px 7px;
  font-size: 11px;
}

.diag,
.summary {
  padding: 8px 12px;
  border-bottom: 1px solid var(--border-soft);
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.diag-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.seeds {
  display: flex;
  flex-direction: column;
  gap: 2px;
  align-items: flex-end;
}

.seed {
  display: flex;
  align-items: center;
  gap: 6px;
}

.stream {
  padding: 8px 12px;
}

.event {
  display: flex;
  gap: 8px;
  padding: 1px 0;
  font-size: 11px;
}

.topic {
  color: var(--text-dim);
  word-break: break-all;
}
</style>
