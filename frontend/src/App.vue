<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'

import DebugPanel from './views/DebugPanel.vue'
import PeerList from './components/PeerList.vue'
import SettingsView from './views/SettingsView.vue'
import ChatView from './views/ChatView.vue'
import TransferView from './views/TransferView.vue'

import { isMockMode } from './api'
import type { Peer } from './api/types'
import { useChatStore } from './stores/chat'
import { useGroupStore } from './stores/group'
import { usePeersStore } from './stores/peers'
import { useTransferStore } from './stores/transfer'

type Tab = 'transfer' | 'settings' | 'debug'

const peers = usePeersStore()
const chat = useChatStore()
const transfers = useTransferStore()
const groups = useGroupStore()

const tab = ref<Tab>('transfer')
const mock = ref(false)
const preselectedPeer = ref('')

// 群创建表单
const groupName = ref('')
const groupMembers = ref<string[]>([])
const showGroupForm = ref(false)

let timer: number | undefined

onMounted(async () => {
  mock.value = isMockMode() || !(window as any)?.runtime
  await Promise.all([
    peers.refresh(),
    chat.loadConversations(),
    transfers.refresh(),
    groups.refresh()
  ])
  // 诊断数据（连接数/种子）需要周期性拉取；事件流本身是推送的
  timer = window.setInterval(() => {
    void peers.refresh()
  }, 3000)
})

onUnmounted(() => {
  if (timer) window.clearInterval(timer)
})

async function openPeer(p: Peer): Promise<void> {
  await chat.openPeer(p.nodeId, p.displayName)
}

function onSendFile(p: Peer): void {
  preselectedPeer.value = p.nodeId
  tab.value = 'transfer'
}

async function createGroup(): Promise<void> {
  if (!groupName.value.trim()) return
  try {
    await groups.create(groupName.value.trim(), groupMembers.value)
    groupName.value = ''
    groupMembers.value = []
    showGroupForm.value = false
    await chat.loadConversations()
  } catch (e: any) {
    groups.error = String(e?.message ?? e)
  }
}
</script>

<template>
  <div class="app">
    <!-- 左：节点与群 -->
    <aside class="pane sidebar">
      <div class="pane-head">
        <div>
          <div class="pane-title">SwarmLink</div>
          <div class="row-sub mono">
            {{ peers.self?.displayName || '…' }} · {{ peers.self?.nodeId?.slice(0, 8) || '—' }}
          </div>
        </div>
      </div>

      <div v-if="mock" class="banner">
        未连接后端（降级模式）：当前显示的是占位数据。
      </div>

      <div class="pane-body">
        <PeerList @chat="openPeer" @send-file="onSendFile" />

        <div class="section-head">
          <span class="muted">群聊</span>
          <button @click="showGroupForm = !showGroupForm">
            {{ showGroupForm ? '取消' : '新建' }}
          </button>
        </div>

        <div v-if="showGroupForm" class="group-form">
          <input v-model="groupName" placeholder="群名称" />
          <select v-model="groupMembers" multiple size="3">
            <option v-for="p in peers.peers" :key="p.nodeId" :value="p.nodeId">
              {{ p.displayName }}
            </option>
          </select>
          <div class="faint">上限 20 人（v1.0）。可多选初始成员。</div>
          <button class="primary" @click="createGroup">创建</button>
        </div>

        <div v-if="groups.error" class="banner">{{ groups.error }}</div>

        <div
          v-for="g in groups.groups"
          :key="g.groupId"
          class="row"
          @click="chat.openPeer(g.groupId, g.name)"
        >
          <span class="dot online" />
          <div class="row-main">
            <div class="row-title">
              <span>{{ g.name }}</span>
              <span class="tag">{{ groups.activeCount(g) }}/20</span>
            </div>
            <div class="row-sub mono">epoch {{ g.epoch }} · {{ g.groupId.slice(0, 8) }}</div>
          </div>
        </div>
      </div>
    </aside>

    <!-- 中：聊天 -->
    <main class="pane">
      <ChatView />
    </main>

    <!-- 右：传输 / 设置 / 调试 -->
    <section class="pane right">
      <div class="tabs">
        <button :class="{ on: tab === 'transfer' }" @click="tab = 'transfer'">传输</button>
        <button :class="{ on: tab === 'settings' }" @click="tab = 'settings'">设置</button>
        <button :class="{ on: tab === 'debug' }" @click="tab = 'debug'">调试</button>
      </div>

      <TransferView v-if="tab === 'transfer'" :preselected-peer="preselectedPeer" />
      <SettingsView v-else-if="tab === 'settings'" />
      <DebugPanel v-else />
    </section>
  </div>
</template>

<style scoped>
.sidebar .pane-body {
  padding: 8px;
}

.right {
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.tabs {
  display: flex;
  flex: 0 0 auto;
  border-bottom: 1px solid var(--border);
}

.tabs button {
  flex: 1 1 0;
  border: none;
  border-radius: 0;
  background: transparent;
  color: var(--text-dim);
  padding: 8px 0;
}

.tabs button.on {
  color: var(--text);
  background: var(--bg-raised);
  box-shadow: inset 0 -2px 0 var(--accent);
}

.section-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin: 12px 0 6px;
  padding-top: 8px;
  border-top: 1px solid var(--border-soft);
  font-size: 11px;
}

.group-form {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 8px;
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-sm);
  margin-bottom: 8px;
}
</style>
