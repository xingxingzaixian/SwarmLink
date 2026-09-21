<script setup lang="ts">
/**
 * 左栏：联系人。
 *
 * 分三组而不是一个大列表：
 *   群聊 / 在线好友 / 离线好友
 *
 * 这个分组不是为了好看 —— 本项目里「在线」由 UDP announce 维护，
 * 「已连接」由 TCP 会话维护，两者可以不一致（ADR-009）。
 * 把能立刻发消息的人聚在上面，用户就不用自己判断哪个绿点算数。
 */
import { computed, onBeforeUnmount, ref, watch } from 'vue'

import Avatar from './Avatar.vue'
import Icon from './Icon.vue'
import Modal from './Modal.vue'
import { directConvId, isGroupId } from '../api/ids'
import type { Group, Peer } from '../api/types'
import { useChatStore } from '../stores/chat'
import { useGroupStore } from '../stores/group'
import { reachable, usePeersStore } from '../stores/peers'
import { useUiStore } from '../stores/ui'
import { listTime } from '../utils/format'

defineProps<{ mock?: boolean }>()

const peers = usePeersStore()
const chat = useChatStore()
const groups = useGroupStore()
const ui = useUiStore()

const query = ref('')
const menuOpen = ref(false)
const showCreate = ref(false)

const groupName = ref('')
const memberIds = ref<string[]>([])
const creating = ref(false)
const createError = ref('')

// ---------------------------------------------------------------- 检索

const q = computed(() => query.value.trim().toLowerCase())

const matchedGroups = computed(() =>
  q.value ? groups.groups.filter((g) => g.name.toLowerCase().includes(q.value)) : groups.groups
)

function matchPeer(p: Peer): boolean {
  return (
    p.displayName.toLowerCase().includes(q.value) ||
    p.nodeId.toLowerCase().includes(q.value) ||
    p.lastAddr.toLowerCase().includes(q.value)
  )
}

const searching = computed(() => q.value.length > 0)
const searchedPeers = computed(() => peers.peers.filter(matchPeer))
const onlinePeers = computed(() => (searching.value ? searchedPeers.value : peers.onlinePeers))
const offlinePeers = computed(() => (searching.value ? [] : peers.offlinePeers))

const nothingFound = computed(
  () => searching.value && !matchedGroups.value.length && !searchedPeers.value.length
)

// ---------------------------------------------------------------- 行数据

function peerConvId(p: Peer): string {
  return directConvId(peers.self?.nodeId ?? '', p.nodeId)
}

function isActive(convId: string): boolean {
  return chat.activeConvId === convId
}

/** 有消息就显示消息，没有就显示地址 —— 空列表里「什么都没有」最没用。 */
function peerSub(p: Peer): string {
  const m = chat.previewOf(peerConvId(p))
  if (m) return m.content
  return p.lastAddr || '尚未通信'
}

function groupSub(g: Group): string {
  const m = chat.previewOf(g.groupId)
  if (m) return m.content
  return `${groups.activeCount(g)}/${groups.MAX_MEMBERS} 人在线`
}

function when(convId: string, fallback: number): string {
  return listTime(chat.previewOf(convId)?.sentAt ?? fallback)
}

function openPeer(p: Peer): void {
  void chat.openPeer(p.nodeId, p.displayName)
}

function openGroup(g: Group): void {
  void chat.openPeer(g.groupId, g.name)
}

// ---------------------------------------------------------------- 建群

watch(showCreate, (v) => {
  if (!v) {
    groupName.value = ''
    memberIds.value = []
    createError.value = ''
  }
})

async function submitCreate(): Promise<void> {
  const name = groupName.value.trim()
  if (!name) {
    createError.value = '请填写群名称'
    return
  }
  creating.value = true
  createError.value = ''
  try {
    const g = await groups.create(name, memberIds.value)
    showCreate.value = false
    await chat.loadConversations()
    void chat.openPeer(g.groupId, g.name)
  } catch (e: any) {
    createError.value = String(e?.message ?? e)
  } finally {
    creating.value = false
  }
}

// ---------------------------------------------------------------- 本人菜单

function toggleMenu(): void {
  menuOpen.value = !menuOpen.value
}

function pick(kind: 'settings' | 'debug' | 'transfers'): void {
  menuOpen.value = false
  ui.openPanel(kind)
}

/** 点击别处收起菜单。用 capture 是为了在行点击之前就拿到事件。 */
function onDocClick(e: MouseEvent): void {
  const el = e.target as HTMLElement | null
  if (!el?.closest('.self-bar, .self-menu')) menuOpen.value = false
}

watch(menuOpen, (open) => {
  if (open) document.addEventListener('click', onDocClick, true)
  else document.removeEventListener('click', onDocClick, true)
})

onBeforeUnmount(() => document.removeEventListener('click', onDocClick, true))

const self = computed(() => peers.self)
</script>

<template>
  <aside class="rail">
    <header class="rail-head">
      <label class="search">
        <Icon name="search" :size="14" />
        <input v-model="query" type="text" placeholder="搜索好友、群聊或地址" spellcheck="false" />
        <button
          v-if="query"
          class="icon-btn clear"
          title="清除"
          aria-label="清除搜索"
          @click="query = ''"
        >
          <Icon name="x" :size="13" />
        </button>
      </label>
    </header>

    <div v-if="mock" class="banner">
      未连接后端：当前是降级预览数据，发送文件只会演示进度。
    </div>

    <div class="rail-body scroll">
      <!-- 群聊：即使为空也保留分区头，否则「新建群聊」这个唯一入口就消失了 -->
      <template v-if="matchedGroups.length || !searching">
        <div class="section-head">
          <span>群聊 · {{ matchedGroups.length }}</span>
          <button
            v-if="!searching"
            class="icon-btn mini"
            title="新建群聊"
            aria-label="新建群聊"
            @click="showCreate = true"
          >
            <Icon name="plus" :size="15" />
          </button>
        </div>
        <div v-if="!matchedGroups.length" class="hint">还没有群聊，点右上角 + 建一个。</div>
        <div
          v-for="g in matchedGroups"
          :key="g.groupId"
          class="list-row"
          :class="{ active: isActive(g.groupId) }"
          @click="openGroup(g)"
        >
          <Avatar :seed="g.groupId" :name="g.name" :size="38" shape="square" :online="null" />
          <div class="list-row-main">
            <div class="list-row-title nowrap">{{ g.name }}</div>
            <div class="list-row-sub">{{ groupSub(g) }}</div>
          </div>
          <div class="row-meta">
            <span class="row-time">{{ when(g.groupId, g.createdAt) }}</span>
            <span v-if="chat.unreadOf(g.groupId)" class="badge">
              {{ chat.unreadOf(g.groupId) > 99 ? '99+' : chat.unreadOf(g.groupId) }}
            </span>
          </div>
        </div>
      </template>

      <!-- 在线好友 -->
      <template v-if="onlinePeers.length">
        <div class="section-head">
          <span>{{ searching ? '好友' : '在线' }} · {{ onlinePeers.length }}</span>
        </div>
        <div
          v-for="p in onlinePeers"
          :key="p.nodeId"
          class="list-row"
          :class="{ active: isActive(peerConvId(p)) }"
          :title="`NodeID ${p.nodeId}\n地址 ${p.lastAddr || '未知'}\n子网 ${p.subnet || '未知'}`"
          @click="openPeer(p)"
        >
          <!-- 在线点用 reachable：正在广播、还没握手的节点也算「在」，
               差别由下面的「未连接」标签表达，而不是把点画成灰色 -->
          <Avatar :seed="p.nodeId" :name="p.displayName" :size="38" :online="reachable(p)" />
          <div class="list-row-main">
            <div class="list-row-title nowrap">
              <span class="nowrap">{{ p.displayName }}</span>
              <!-- 只给「在线但未连接」加标识：它意味着首条消息要多等一个 RTT -->
              <span v-if="!p.connected" class="tag">未连接</span>
            </div>
            <div class="list-row-sub">{{ peerSub(p) }}</div>
          </div>
          <div class="row-meta">
            <span class="row-time">{{ when(peerConvId(p), p.lastSeen) }}</span>
            <span v-if="chat.unreadOf(peerConvId(p))" class="badge">
              {{ chat.unreadOf(peerConvId(p)) > 99 ? '99+' : chat.unreadOf(peerConvId(p)) }}
            </span>
          </div>
        </div>
      </template>

      <!-- 离线好友 -->
      <template v-if="offlinePeers.length">
        <div class="section-head"><span>离线 · {{ offlinePeers.length }}</span></div>
        <div
          v-for="p in offlinePeers"
          :key="p.nodeId"
          class="list-row offline"
          :class="{ active: isActive(peerConvId(p)) }"
          @click="openPeer(p)"
        >
          <Avatar :seed="p.nodeId" :name="p.displayName" :size="38" :online="false" />
          <div class="list-row-main">
            <div class="list-row-title nowrap">{{ p.displayName }}</div>
            <div class="list-row-sub">{{ peerSub(p) }}</div>
          </div>
          <div class="row-meta">
            <span class="row-time">{{ when(peerConvId(p), p.lastSeen) }}</span>
          </div>
        </div>
      </template>

      <div v-if="nothingFound" class="empty">
        <div class="empty-title">没有匹配的联系人</div>
        <div>试试 NodeID 的前几位，或者直接搜地址。</div>
      </div>

      <!-- 用 ready 而不是 !loading：后台轮询每 3 秒跑一次，
           用 loading 会让这个空状态被反复卸载再挂载，肉眼就是定时闪一下。
           ready 一旦为真就不再回落，空状态也就不会再动。 -->
      <div v-else-if="peers.ready && !peers.peers.length && !groups.groups.length" class="empty">
        <div class="empty-title">还没有发现任何节点</div>
        <div>
          同网段会自动广播发现；跨网段需要在设置里配置种子清单。
        </div>
        <button class="btn btn-soft" @click="ui.openPanel('settings')">
          <Icon name="sliders" :size="14" />打开设置
        </button>
      </div>
    </div>

    <!-- 本人 + 菜单入口 -->
    <footer class="self-bar">
      <Avatar
        :seed="self?.nodeId ?? 'self'"
        :name="self?.displayName ?? ''"
        :size="34"
        :online="true"
      />
      <div class="self-main">
        <div class="self-name nowrap">{{ self?.displayName || '正在读取本机身份…' }}</div>
        <div class="mono faint nowrap">
          {{ self?.subnet || '—' }}
          <template v-if="self?.tcpPort"> · TCP {{ self.tcpPort }}</template>
        </div>
      </div>
      <button
        class="icon-btn"
        :class="{ on: menuOpen }"
        title="设置 / 调试 / 传输"
        aria-label="打开功能菜单"
        @click="toggleMenu"
      >
        <Icon name="menu" :size="17" />
      </button>
    </footer>

    <div v-if="menuOpen" class="self-menu">
      <button class="menu-item" @click="pick('settings')">
        <Icon name="sliders" :size="15" />设置
      </button>
      <button class="menu-item" @click="pick('debug')">
        <Icon name="activity" :size="15" />调试面板
      </button>
      <button class="menu-item" @click="pick('transfers')">
        <Icon name="folder" :size="15" />文件传输
      </button>
    </div>

    <!-- 建群 -->
    <Modal v-if="showCreate" title="新建群聊" subtitle="v1.0 上限 20 人" :width="420" :height="480" @close="showCreate = false">
      <div class="form">
        <label class="field">
          <span>群名称</span>
          <input v-model="groupName" placeholder="例如：三号线联调" @keydown.enter="submitCreate" />
        </label>

        <div class="field">
          <span>初始成员（可多选，不含自己）</span>
          <div v-if="!peers.peers.length" class="faint small">暂无可选节点。</div>
          <div v-else class="picks">
            <label v-for="p in peers.peers" :key="p.nodeId" class="pick">
              <input v-model="memberIds" type="checkbox" :value="p.nodeId" />
              <Avatar :seed="p.nodeId" :name="p.displayName" :size="26" :online="null" />
              <span class="nowrap">{{ p.displayName }}</span>
              <span v-if="!p.online && !p.connected" class="tag">离线</span>
            </label>
          </div>
        </div>

        <div v-if="createError" class="err">{{ createError }}</div>
      </div>

      <template #head>
        <button class="btn btn-primary" :disabled="creating" @click="submitCreate">
          {{ creating ? '创建中…' : '创建' }}
        </button>
      </template>
    </Modal>
  </aside>
</template>

<style scoped>
.rail {
  position: relative;
  display: flex;
  flex-direction: column;
  min-height: 0;
  background: var(--glass-rail);
  backdrop-filter: blur(var(--blur)) saturate(110%);
  -webkit-backdrop-filter: blur(var(--blur)) saturate(110%);
  /* 外圈大而散的投影 + 顶边 1px 高光：前者让面板「离开」背景，
     后者模拟玻璃厚度上的折射。缺任何一个都会变成「没画完的界面」 */
  box-shadow: var(--shadow-glass), inset 0 1px 0 var(--edge);
}

/* ---------------------------------------------------------------- 顶部 */

.rail-head {
  flex: 0 0 auto;
  padding: 12px 12px 8px;
}

.search {
  display: flex;
  align-items: center;
  gap: 7px;
  height: 32px;
  padding: 0 8px 0 10px;
  border-radius: var(--r-md);
  background: var(--surface-sunken);
  box-shadow: inset 0 0 0 1px var(--line);
  color: var(--t3);
  transition: background var(--dur-1) var(--ease), box-shadow var(--dur-1) var(--ease);
}

.search:focus-within {
  background: var(--surface-strong);
  color: var(--cyan);
  box-shadow: inset 0 0 0 1px var(--line), 0 0 0 3px rgba(6, 182, 212, 0.32);
}

.search input {
  flex: 1 1 auto;
  height: 100%;
  padding: 0;
  border: 0;
  background: none;
  font-size: var(--fs-sm);
  color: var(--t1);
}

.search input:hover,
.search input:focus {
  background: none;
  box-shadow: none;
}

.clear {
  width: 20px;
  height: 20px;
  color: var(--t3);
}

/* ---------------------------------------------------------------- 列表 */

.rail-body {
  flex: 1 1 auto;
  min-height: 0;
  padding: 0 8px 10px;
}

.icon-btn.mini {
  width: 22px;
  height: 22px;
}

.list-row.offline :deep(.avatar) {
  filter: grayscale(0.55);
  opacity: 0.72;
}

.row-meta {
  flex: 0 0 auto;
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 3px;
}

.row-time {
  font-size: 10.5px;
  color: var(--t3);
  font-variant-numeric: tabular-nums;
}

.hint {
  padding: 4px 12px 6px;
  font-size: var(--fs-sm);
  color: var(--t3);
}

/* ---------------------------------------------------------------- 本人 */

.self-bar {
  flex: 0 0 auto;
  display: flex;
  align-items: center;
  gap: 10px;
  height: var(--head-h);
  padding: 0 12px 0 14px;
  background: var(--glass-chrome);
  backdrop-filter: blur(10px);
  -webkit-backdrop-filter: blur(10px);
  box-shadow: inset 0 1px 0 var(--edge);
}

.self-main {
  flex: 1 1 auto;
  min-width: 0;
}

.self-name {
  font-size: var(--fs-md);
  font-weight: 600;
}

.self-menu {
  position: absolute;
  left: 12px;
  bottom: calc(var(--head-h) + 8px);
  z-index: 30;
  min-width: 156px;
  padding: 6px;
  border-radius: var(--r-lg);
  background: var(--glass-float);
  backdrop-filter: blur(var(--blur-lg)) saturate(140%);
  -webkit-backdrop-filter: blur(var(--blur-lg)) saturate(140%);
  box-shadow: var(--shadow-pop);
  /* 从触发它的那个按钮「长出来」，因此原点在左下角 */
  transform-origin: bottom left;
  animation: sl-pop-up var(--dur-2) var(--ease) both;
}

.menu-item {
  display: flex;
  align-items: center;
  gap: 9px;
  width: 100%;
  height: 32px;
  padding: 0 10px;
  border-radius: var(--r-sm);
  font-size: var(--fs-md);
  color: var(--t1);
  transition: background var(--dur-1) var(--ease);
}

.menu-item:hover {
  background: var(--hover);
}

/* ---------------------------------------------------------------- 建群 */

.form {
  display: flex;
  flex-direction: column;
  gap: 18px;
  padding: 18px 20px 24px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 7px;
  font-size: var(--fs-sm);
  color: var(--t2);
}

.picks {
  display: flex;
  flex-direction: column;
  gap: 2px;
  max-height: 240px;
  overflow-y: auto;
}

.pick {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 6px 8px;
  border-radius: var(--r-sm);
  font-size: var(--fs-md);
  color: var(--t1);
  cursor: pointer;
}

.pick:hover {
  background: var(--hover);
}

.pick input {
  width: 15px;
  height: 15px;
  flex: 0 0 auto;
  accent-color: var(--brand);
}

.err {
  padding: 8px 10px;
  border-radius: var(--r-sm);
  background: var(--err-soft);
  color: var(--err-ink);
  font-size: var(--fs-sm);
}

.small {
  font-size: var(--fs-sm);
}
</style>
