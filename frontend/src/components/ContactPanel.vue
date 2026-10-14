<script setup lang="ts">
/**
 * 左栏：联系人 / 群组。
 *
 * 顶部用「联系人 / 群组」两个图标页签切换，而不是把二者纵向堆在一起：
 * 它们不是同一类条目（人 vs 会话容器），混排时用户得自己找分隔线。
 *
 * 联系人页签内部仍分「在线 / 离线」两段 —— 这个分组不是为了好看：
 * 本项目里「在线」由 UDP announce 维护，「已连接」由 TCP 会话维护，
 * 两者可以不一致（ADR-009）。把能立刻发消息的人聚在上面，
 * 用户就不用自己判断哪个绿点算数。
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
import { previewText } from '../utils/message'

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

// 移除节点（只在本机生效；二次确认后再调后端）
const removing = ref<Peer | null>(null)
const removeBusy = ref(false)
const removeError = ref('')

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

/** 视图标签：联系人与群组分开，一个列表里不再混排两种不同性质的条目。 */
const tab = ref<'contacts' | 'groups'>('contacts')

/** 标签计数：联系人用全部节点数（与「在线 + 离线」两段之和一致），群组用群数。 */
const contactsCount = computed(() => peers.peers.length)

/** 搜索命中按标签各自判断：在联系人页搜不到群名，不该在联系人页报错。 */
const noPeerMatch = computed(() => searching.value && !searchedPeers.value.length)
const noGroupMatch = computed(() => searching.value && !matchedGroups.value.length)

/** 联系人页空态：用 ready 而不是 !loading，否则 3 秒一次的轮询会让空态反复闪。 */
const noPeers = computed(() => peers.ready && !peers.peers.length)

const searchPlaceholder = computed(() =>
  tab.value === 'groups' ? '搜索群组…' : '搜索会话 / 节点…'
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
  // 不能直接透 m.content：文件/图片消息的 content 是 JSON 信封，
  // 透出去左栏就是一段「{"job_id":"01a0ed92-…」的裸 JSON。
  if (m) return previewText(m)
  return p.lastAddr || '尚未通信'
}

function groupSub(g: Group): string {
  const m = chat.previewOf(g.groupId)
  if (m) return previewText(m)
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

// ---------------------------------------------------------------- 移除节点

function askRemove(p: Peer): void {
  removing.value = p
  removeError.value = ''
}

/**
 * 确认移除。
 *
 * 三条边界与后端一致，也写在弹窗文案里（避免用户以为「删了就彻底断了」）：
 * 只在本机生效、不是拉黑、对方再次被验证到会自动回到列表。
 */
async function confirmRemove(): Promise<void> {
  const p = removing.value
  if (!p) return
  removeBusy.value = true
  removeError.value = ''
  try {
    await peers.remove(p.nodeId)
    // 只在弹窗仍是这个节点时才关闭：用户可能在等待期间关掉弹窗、又对另一个节点点了移除，
    // 无条件清空会把那个新弹窗一起关掉。
    if (removing.value?.nodeId === p.nodeId) removing.value = null
  } catch (e: any) {
    removeError.value = String(e?.message ?? e)
    return
  } finally {
    removeBusy.value = false
  }

  // 重算会话状态：删的若正是当前会话，右侧不该继续显示它。
  // 单独收尾而不是放进上面的 try：它失败与「移除失败」是两回事，不能混进同一个错误通道。
  try {
    await chat.loadConversations()
  } catch {
    // 忽略：会话状态是一次可重算的投影，失败不该影响移除这个已完成的动作
  }
}

// ---------------------------------------------------------------- 本人菜单

function toggleMenu(): void {
  menuOpen.value = !menuOpen.value
}

function pick(kind: 'settings' | 'debug' | 'transfers' | 'share'): void {
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

/**
 * 状态条上的节点数：用 reachable（正在广播或已连接）而不是在线数。
 *
 * 与列表的「在线」分组同一口径 —— 两处用不同判据时，用户会看到
 * 「右上写着 3 NODES 但列表里只有 1 个在线」，那种不一致最难解释。
 */
const reachableCount = computed(() => peers.peers.filter(reachable).length)
</script>

<template>
  <aside class="rail">
    <!-- 品牌区：Logo 的脉冲环是「本机是一个活跃节点」的唯一视觉暗示。
         整个界面只有这一处脉冲环（信号卡的律动是另一码事）。 -->
    <div class="brand">
      <div class="brand__logo">S</div>
      <div>
        <div class="brand__name">SwarmLink</div>
        <div class="brand__sub micro">P2P Mesh Net</div>
      </div>
    </div>

    <!-- 网络状态条：第一眼说明「这是个网络工具」。
         文案随真实状态走 —— 一个节点都没有时还说 LIVE 就是撒谎。 -->
    <div class="netstat">
      <span class="netstat__left">
        <span class="dot dot--static" :class="{ online: reachableCount > 0 }" />
        {{ reachableCount > 0 ? 'NETWORK LIVE' : 'NETWORK IDLE' }}
      </span>
      <span class="netstat__right">{{ reachableCount }} NODES</span>
    </div>

    <header class="rail-head">
      <label class="search">
        <Icon name="search" :size="14" />
        <input
          v-model="query"
          type="text"
          :placeholder="searchPlaceholder"
          spellcheck="false"
        />
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

      <!-- 视图切换：联系人与群组改成图标 + 标签两个页签，而不是在同一列表里纵向排布。
           两者是不同性质的条目（人 vs 会话容器），混排时用户得自己找分隔线；
           计数并入标签，切换前就能知道另一侧有没有内容。 -->
      <div class="tabs" role="tablist" aria-label="切换联系人与群组">
        <button
          class="tab"
          :class="{ on: tab === 'contacts' }"
          role="tab"
          :aria-selected="tab === 'contacts'"
          @click="tab = 'contacts'"
        >
          <Icon name="user" :size="15" />
          <span>联系人</span>
          <span v-if="contactsCount" class="tab-count">{{ contactsCount }}</span>
        </button>
        <button
          class="tab"
          :class="{ on: tab === 'groups' }"
          role="tab"
          :aria-selected="tab === 'groups'"
          @click="tab = 'groups'"
        >
          <Icon name="users" :size="15" />
          <span>群组</span>
          <span v-if="groups.groups.length" class="tab-count">{{ groups.groups.length }}</span>
        </button>
      </div>
    </header>

    <div v-if="mock" class="banner">
      未连接后端：当前是降级预览数据，发送文件只会演示进度。
    </div>

    <div class="rail-body scroll">
      <!-- 群组页签 -->
      <template v-if="tab === 'groups'">
        <!-- 即使为空也保留分区头，否则「新建群聊」这个唯一入口就消失了 -->
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
        <div v-if="noGroupMatch" class="empty">
          <div class="empty-title">没有匹配的群聊</div>
          <div>换个关键词，或者清空搜索看全部。</div>
        </div>
        <div v-else-if="!matchedGroups.length" class="hint">还没有群聊，点右上角 + 建一个。</div>
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

      <!-- 联系人页签 -->
      <template v-else>

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
            <!-- 放在 .row-meta 之外当同级元素：.row-meta 是列式布局，
                 塞进去会让「带未读角标的行」比其它行更高 -->
            <button
              class="icon-btn mini"
              title="从本机列表移除"
              aria-label="从本机列表移除"
              @click.stop="askRemove(p)"
            >
              <Icon name="trash" :size="13" />
            </button>
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
            <button
              class="icon-btn mini"
              title="从本机列表移除"
              aria-label="从本机列表移除"
              @click.stop="askRemove(p)"
            >
              <Icon name="trash" :size="13" />
            </button>
            <div class="row-meta">
              <span class="row-time">{{ when(peerConvId(p), p.lastSeen) }}</span>
            </div>
          </div>
        </template>

        <div v-if="noPeerMatch" class="empty">
          <div class="empty-title">没有匹配的联系人</div>
          <div>试试 NodeID 的前几位，或者直接搜地址。</div>
        </div>

        <!-- 用 ready 而不是 !loading：后台轮询每 3 秒跑一次，
             用 loading 会让这个空状态被反复卸载再挂载，肉眼就是定时闪一下。
             ready 一旦为真就不再回落，空状态也就不会再动。 -->
        <div v-else-if="noPeers" class="empty">
          <div class="empty-title">还没有发现任何节点</div>
          <div>同网段会自动广播发现；跨网段需要在设置里配置种子清单。</div>
          <button class="btn btn-soft" @click="ui.openPanel('settings')">
            <Icon name="sliders" :size="14" />打开设置
          </button>
        </div>
      </template>
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
        title="设置 / 共享 / 调试 / 传输"
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
      <button class="menu-item" @click="pick('share')">
        <Icon name="share" :size="15" />共享中心
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

    <!-- 移除节点：文案必须把三条边界说清楚（只在本机、不是拉黑、会随对方上线恢复），
         否则用户会以为「删了就彻底断了」—— 后端的实际语义见设计文档 §5 -->
    <Modal
      v-if="removing"
      title="从列表移除节点"
      subtitle="只在本机生效，不会通知对方"
      :width="440"
      :height="300"
      @close="removing = null"
    >
      <div class="form">
        <div>将把「{{ removing.displayName }}」从本机列表中移除。</div>
        <div class="faint small">
          对方不会收到任何通知，别人的列表也不变，历史消息也不会被删除。
          这也不是拉黑：不会拒绝对方的握手与连接，只是让它不再出现在你的列表里。
          该节点若再次上线（或与它重新握手），会自动回到列表 ——
          因此它适合清理已经失效的旧身份，不适合屏蔽一个还在使用的节点。
        </div>
        <div v-if="removeError" class="err">{{ removeError }}</div>
      </div>

      <template #head>
        <button class="btn btn-primary" :disabled="removeBusy" @click="confirmRemove">
          {{ removeBusy ? '移除中…' : '移除' }}
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
  background: var(--bg-panel);
  border-right: 1px solid var(--divider);
}

/* ---------------------------------------------------------------- 顶部 */

.rail-head {
  flex: 0 0 auto;
  padding: 4px 16px 8px;
}

.search {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 0 10px;
  border-radius: var(--r-sm);
  background: var(--bg-elevated);
  border: 1px solid var(--divider);
  color: var(--text-muted);
  transition: border-color var(--dur-2) var(--ease), box-shadow var(--dur-2) var(--ease),
    color var(--dur-2) var(--ease);
}

.search:focus-within {
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
  color: var(--text-secondary);
}

.search input {
  flex: 1 1 auto;
  height: 34px;
  padding: 0;
  border: 0;
  background: none;
  font-size: var(--fs-md);
  color: var(--text-primary);
}

/* 输入框在容器里是「裸」的：边框与光晕都由 .search 承担，
   否则会出现双层边框 */
.search input:hover,
.search input:focus {
  background: none;
  border-color: transparent;
  box-shadow: none;
}

.clear {
  width: 20px;
  height: 20px;
  border: 0;
  color: var(--text-muted);
}

/* ---------------------------------------------------------------- 视图切换 */

/* 两枚等宽页签装在一条凹槽里：凹槽给出「同一组里的互斥选项」这一层语义，
   选中项浮到 elevated 面上，未选中项留在槽里。 */
.tabs {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 3px;
  margin-top: 8px;
  padding: 3px;
  border-radius: var(--r-sm);
  background: var(--bg-sunken);
  border: 1px solid var(--divider);
}

.tab {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 7px;
  height: 30px;
  padding: 0 8px;
  border-radius: var(--r-xs);
  font-size: var(--fs-sm);
  font-weight: 500;
  color: var(--text-secondary);
  transition: background var(--dur-2) var(--ease), color var(--dur-2) var(--ease),
    box-shadow var(--dur-2) var(--ease);
}

.tab:hover:not(.on) {
  color: var(--text-primary);
}

.tab.on {
  background: var(--bg-elevated);
  color: var(--accent);
  box-shadow: 0 1px 3px oklch(0% 0 0 / 0.28);
}

/* 计数做成小药丸：只有真有内容时才出现，0 不值得占一格。 */
.tab-count {
  min-width: 17px;
  height: 17px;
  padding: 0 5px;
  border-radius: 999px;
  background: var(--bg-hover);
  color: var(--text-muted);
  font-family: var(--font-mono);
  font-size: 10px;
  font-weight: 600;
  line-height: 17px;
  text-align: center;
  font-variant-numeric: tabular-nums;
}

.tab.on .tab-count {
  background: var(--accent-soft);
  color: var(--accent);
}

/* ---------------------------------------------------------------- 列表 */

.rail-body {
  flex: 1 1 auto;
  min-height: 0;
  padding: 6px 10px;
}

.icon-btn.mini {
  width: 24px;
  height: 24px;
  border-radius: var(--r-xs);
}

/* 离线联系人：头像去色 + 整行降透明度。只降一点点 ——
   降太多会让「离线」看起来像「已删除」。 */
.list-row.offline :deep(.avatar) {
  filter: grayscale(0.6);
}

.row-meta {
  flex: 0 0 auto;
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 3px;
}

.row-time {
  font-family: var(--font-mono);
  font-size: 10.5px;
  color: var(--text-muted);
  font-variant-numeric: tabular-nums;
}

.hint {
  padding: 4px 12px 6px;
  font-size: var(--fs-sm);
  color: var(--text-muted);
}

/* ---------------------------------------------------------------- 本人 */

/* 本机身份做成一张凹面小卡（原型 .me）而不是整条 chrome 栏：
   它和上面的联系人不是同一类东西，视觉上分开才不会看成一伙。 */
.self-bar {
  flex: 0 0 auto;
  display: flex;
  align-items: center;
  gap: 11px;
  margin: 10px 14px;
  padding: 11px;
  border-radius: var(--r-md);
  background: var(--bg-sunken);
  border: 1px solid var(--divider);
}

.self-main {
  flex: 1 1 auto;
  min-width: 0;
}

.self-name {
  font-family: var(--font-display);
  font-size: var(--fs-md);
  font-weight: 600;
}

/* 从触发它的那个按钮「长出来」，因此原点在左下角 */
.self-menu {
  position: absolute;
  left: 14px;
  bottom: 78px;
  z-index: 30;
  min-width: 168px;
  padding: 6px;
  border-radius: var(--r-md);
  background: var(--bg-elevated);
  border: 1px solid var(--divider-strong);
  box-shadow: var(--shadow-lg);
  transform-origin: bottom left;
  animation: sl-pop var(--dur-2) var(--ease) both;
}

.menu-item {
  display: flex;
  align-items: center;
  gap: 9px;
  width: 100%;
  height: 32px;
  padding: 0 10px;
  border-radius: var(--r-xs);
  font-size: var(--fs-md);
  color: var(--text-primary);
  transition: background var(--dur-1) var(--ease), color var(--dur-1) var(--ease);
}

.menu-item:hover {
  background: var(--bg-hover);
  color: var(--accent);
}

/* ---------------------------------------------------------------- 建群 / 移除 */

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
  color: var(--text-secondary);
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
  color: var(--text-primary);
  cursor: pointer;
  transition: background var(--dur-1) var(--ease);
}

.pick:hover {
  background: var(--bg-hover);
}

.pick input {
  width: 15px;
  height: 15px;
  flex: 0 0 auto;
}

.err {
  padding: 8px 10px;
  border-radius: var(--r-sm);
  background: var(--danger-soft);
  color: var(--danger);
  font-size: var(--fs-sm);
}

.small {
  font-size: var(--fs-sm);
}
</style>
