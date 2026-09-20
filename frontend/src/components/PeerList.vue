<script setup lang="ts">
import { computed, ref } from 'vue'

import { usePeersStore } from '../stores/peers'
import type { Peer } from '../api'

const emit = defineEmits<{
  (e: 'chat', peer: Peer): void
  (e: 'send-file', peer: Peer): void
}>()

const peers = usePeersStore()
const filter = ref('')

const visible = computed(() => {
  const q = filter.value.trim().toLowerCase()
  const list = peers.peers
  if (!q) return list
  return list.filter(
    (p) =>
      p.displayName.toLowerCase().includes(q) ||
      p.nodeId.toLowerCase().includes(q) ||
      p.lastAddr.toLowerCase().includes(q)
  )
})

/**
 * 「在线」与「已连接」是两个不同的事实（ADR-009）：
 *  - online    由 UDP announce 维护，代价趋零
 *  - connected 由 TCP 会话维护，按需建立
 * 因此这里分别显示，避免用户把「在线」误读为「随时可发」。
 */
function stateTag(p: Peer): { text: string; cls: string } {
  if (p.connected) return { text: '已连接', cls: 'info' }
  if (p.online) return { text: '在线', cls: 'ok' }
  if (p.state === 'offline') return { text: '离线', cls: 'err' }
  return { text: '已发现', cls: '' }
}

function dotClass(p: Peer): string {
  if (p.connected) return 'connected'
  if (p.online) return 'online'
  if (p.state === 'offline') return 'offline'
  return ''
}
</script>

<template>
  <div class="peer-list">
    <input v-model="filter" placeholder="过滤节点…" />

    <div v-if="!visible.length" class="empty">
      <div v-if="peers.loading">正在加载…</div>
      <div v-else-if="filter">无匹配节点</div>
      <div v-else>
        尚未发现节点。<br />
        <span class="faint">跨网段部署请检查设置里的种子清单。</span>
      </div>
    </div>

    <div
      v-for="p in visible"
      :key="p.nodeId"
      class="row"
      :title="`NodeID: ${p.nodeId}\n地址: ${p.lastAddr || '未知'}\n子网: ${p.subnet || '未知'}`"
    >
      <span class="dot" :class="dotClass(p)" />
      <div class="row-main">
        <div class="row-title">
          <span>{{ p.displayName }}</span>
          <span class="tag" :class="stateTag(p).cls">{{ stateTag(p).text }}</span>
        </div>
        <div class="row-sub mono">{{ p.shortId }} · {{ p.lastAddr || '—' }}</div>
      </div>
      <div class="actions">
        <button title="聊天" @click="emit('chat', p)">聊</button>
        <button title="发送文件" @click="emit('send-file', p)">传</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.peer-list {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.actions {
  display: flex;
  gap: 4px;
  opacity: 0;
  transition: opacity 0.12s;
}

.row:hover .actions {
  opacity: 1;
}

.actions button {
  padding: 2px 7px;
  font-size: 11px;
}
</style>
