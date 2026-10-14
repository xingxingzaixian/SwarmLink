<script setup lang="ts">
/**
 * 群信息面板：成员列表 + 群主操作（加人 / 移除 / 改名 / 解散）。
 *
 * 为什么单独一个组件：这些操作都围绕「一个群的当前状态」，与会话列表、
 * 聊天区都无关；塞进任一处都会让那处同时承担两个职责。
 *
 * 权限边界（与后端一致，UI 只是把它显出来）：
 *   - 只有群主能改成员与群名、能解散；
 *   - 群主不能被移除（后端会拒绝，按钮也不给）；
 *   - 任何成员变更都会让 epoch+1 并广播，因此面板读的是 groups store 的
 *     最新快照（group:updated 事件会刷新它）。
 */
import { computed, ref } from 'vue'

import Avatar from './Avatar.vue'
import Icon from './Icon.vue'
import Modal from './Modal.vue'
import type { Group, GroupMember } from '../api/types'
import { useGroupStore } from '../stores/group'
import { usePeersStore } from '../stores/peers'
import { useUiStore } from '../stores/ui'

const props = defineProps<{ group: Group }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const groups = useGroupStore()
const peers = usePeersStore()
const ui = useUiStore()

const selfId = computed(() => peers.self?.nodeId ?? '')

/** 在群人数（用后端算好的 active，而不是 state 缓存）。 */
const memberCount = computed(() => props.group.members.filter((m) => m.active).length)

/** 在线成员数（按节点目录的口径，与会话状态行一致）。 */
const onlineCount = computed(
  () => props.group.members.filter((m) => m.active && isOnline(m)).length
)

/** 本机在该群的成员记录（可能不存在：还没同步到这个群）。 */
const me = computed(() => groups.meIn(props.group, selfId.value))

/** 本机是否仍在群。取不到自己时按「不在」处理：宁可少给操作入口。 */
const inGroup = computed(() => !!me.value?.active)

function isOnline(m: GroupMember): boolean {
  const p = peers.byId(m.nodeId)
  return !!p && (p.online || p.connected)
}

function isSelf(m: GroupMember): boolean {
  return m.nodeId === selfId.value
}

function roleLabel(m: GroupMember): string {
  if (m.nodeId === props.group.ownerId) return '群主'
  if (m.role === 'admin') return '管理员'
  return '成员'
}

/**
 * 成员状态标签统一走 store 里的那一份实现。
 *
 * 这里曾经有一份本地版本，与联系人卡片各写一遍 —— 那条判据（撕票号 >= 票号，
 * 且「是谁撕的」决定文案）分叉之后会表现为「面板说已退出、卡片说不在群」。
 */
const stateLabel = (m: GroupMember): string => groups.memberStateLabel(m)

// ---------------------------------------------------------------- 改名
const editingName = ref(false)
const nameDraft = ref('')
const busy = ref(false)
const error = ref('')

function startRename(): void {
  nameDraft.value = props.group.name
  editingName.value = true
  error.value = ''
}

async function submitRename(): Promise<void> {
  const name = nameDraft.value.trim()
  if (!name) return
  busy.value = true
  error.value = ''
  try {
    await groups.rename(props.group.groupId, name)
    editingName.value = false
  } catch (e: any) {
    error.value = String(e?.message ?? e)
  } finally {
    busy.value = false
  }
}

// ---------------------------------------------------------------- 加人
/** 可加入的候选：本机已知、且不在群里的 active 节点。 */
const adding = ref(false)
const candidates = computed(() =>
  peers.peers.filter((p) => !props.group.members.some((m) => m.nodeId === p.nodeId))
)

async function addMember(nodeId: string): Promise<void> {
  busy.value = true
  error.value = ''
  try {
    await groups.addMember(props.group.groupId, nodeId)
  } catch (e: any) {
    error.value = String(e?.message ?? e)
  } finally {
    busy.value = false
  }
}

// ---------------------------------------------------------------- 移除
async function removeMember(m: GroupMember): Promise<void> {
  busy.value = true
  error.value = ''
  try {
    await groups.removeMember(props.group.groupId, m.nodeId)
  } catch (e: any) {
    error.value = String(e?.message ?? e)
  } finally {
    busy.value = false
  }
}

// ---------------------------------------------------------------- 解散
const confirmingDissolve = ref(false)

async function dissolve(): Promise<void> {
  busy.value = true
  error.value = ''
  try {
    await groups.dissolve(props.group.groupId)
    confirmingDissolve.value = false
    ui.notify('群已解散')
    emit('close')
  } catch (e: any) {
    error.value = String(e?.message ?? e)
  } finally {
    busy.value = false
  }
}

// ---------------------------------------------------------------- 退出
//
// 退出权属于本人（ADR-012）：本地立即生效，**不要求群主在线**。
// 因此这里不需要「等群主确认」之类的等待态 —— 调用返回即已退出。
const confirmingLeave = ref(false)

async function leaveGroup(): Promise<void> {
  busy.value = true
  error.value = ''
  try {
    await groups.leave(props.group.groupId)
    confirmingLeave.value = false
    ui.notify('已退出该群')
    emit('close')
  } catch (e: any) {
    error.value = String(e?.message ?? e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Modal
    :title="group.name"
    :subtitle="`${onlineCount}/${memberCount} 人在线 · epoch ${group.epoch}`"
    :width="460"
    :height="560"
    @close="emit('close')"
  >
    <template #head>
      <button v-if="group.isOwner && !editingName" class="btn btn-soft" @click="startRename">
        <Icon name="edit" :size="14" />改名
      </button>
    </template>

    <div class="form">
      <!-- 改名：就地编辑，避免再弹一层 -->
      <div v-if="editingName" class="rename">
        <input
          v-model="nameDraft"
          placeholder="群名称"
          @keydown.enter="submitRename"
          @keydown.esc="editingName = false"
        />
        <button class="btn btn-primary" :disabled="busy" @click="submitRename">保存</button>
      </div>

      <div v-if="group.state === 'dissolved'" class="banner warn">
        该群已由群主解散，不能再发送消息。
      </div>

      <div class="section-head">
        <span>成员 · {{ memberCount }}</span>
        <button
          v-if="group.isOwner"
          class="icon-btn mini"
          title="添加成员"
          aria-label="添加成员"
          @click="adding = !adding"
        >
          <Icon name="plus" :size="15" />
        </button>
      </div>

      <!-- 加人：从「本机已知且不在群里」的节点里挑 -->
      <div v-if="adding" class="picks">
        <div v-if="!candidates.length" class="faint small">没有可添加的节点。</div>
        <button
          v-for="p in candidates"
          :key="p.nodeId"
          class="pick"
          :disabled="busy"
          @click="addMember(p.nodeId)"
        >
          <Avatar :seed="p.nodeId" :name="p.displayName" :size="26" :online="null" />
          <span class="nowrap">{{ p.displayName }}</span>
          <span class="spacer" />
          <Icon name="plus" :size="14" />
        </button>
      </div>

      <div class="members">
        <div
          v-for="m in group.members"
          :key="m.nodeId"
          class="member"
          :class="{ inactive: !m.active }"
        >
          <Avatar
            :seed="m.nodeId"
            :name="m.displayName || m.nodeId.slice(0, 8)"
            :size="34"
            :online="m.active ? isOnline(m) : false"
          />
          <div class="member-main">
            <div class="member-name nowrap">
              {{ m.displayName || m.nodeId.slice(0, 8) }}
              <span v-if="isSelf(m)" class="tag">我</span>
              <span v-if="stateLabel(m)" class="tag warn">{{ stateLabel(m) }}</span>
            </div>
            <div class="member-sub">
              {{ roleLabel(m) }} ·
              {{ !m.active ? stateLabel(m) : isOnline(m) ? '在线' : '离线' }}
            </div>
          </div>
          <button
            v-if="group.isOwner && m.active && m.nodeId !== group.ownerId"
            class="icon-btn mini"
            title="移出群"
            aria-label="移出群"
            :disabled="busy"
            @click="removeMember(m)"
          >
            <Icon name="x" :size="13" />
          </button>
        </div>
      </div>

      <div v-if="error" class="err">{{ error }}</div>
    </div>

    <!--
      底部只放一个「离开这个群」的出口：
        - 群主 → 解散群（群主退出会让群进入无人能变更的死状态，因此不给这个选项）
        - 其他在群成员 → 退出群
        - 已退出 → 什么也不给（重复退出没有意义）
      两者都不可撤销，所以都走二次确认。
    -->
    <template #footer>
      <button
        v-if="group.isOwner && group.state !== 'dissolved'"
        class="btn btn-danger"
        :disabled="busy"
        @click="confirmingDissolve = true"
      >
        <Icon name="trash" :size="14" />解散群
      </button>
      <button
        v-else-if="inGroup && group.state !== 'dissolved'"
        class="btn btn-danger"
        :disabled="busy"
        @click="confirmingLeave = true"
      >
        <Icon name="x" :size="14" />退出群
      </button>
    </template>
  </Modal>

  <!-- 退出：二次确认，文案说清「退出后还能不能被拉回来」 -->
  <Modal
    v-if="confirmingLeave"
    title="退出群聊"
    subtitle="退出后不再接收该群消息"
    :width="440"
    :height="280"
    @close="confirmingLeave = false"
  >
    <div class="form">
      <div>将退出「{{ group.name }}」。</div>
      <div class="faint small">
        退出会立即在本机生效，不需要群主在线。你仍会保留该群的历史消息，
        但它不会再接收新消息；群主之后可以再次把你拉回群。
        其他成员会看到你已退出。
      </div>
      <div v-if="error" class="err">{{ error }}</div>
    </div>
    <template #head>
      <button class="btn btn-danger" :disabled="busy" @click="leaveGroup">
        {{ busy ? '退出中…' : '确认退出' }}
      </button>
    </template>
  </Modal>

  <Modal
    v-if="confirmingDissolve"
    title="解散群聊"
    subtitle="此操作不可撤销"
    :width="420"
    :height="260"
    @close="confirmingDissolve = false"
  >
    <div class="form">
      <div>将解散「{{ group.name }}」。</div>
      <div class="faint small">
        解散后所有成员会收到通知，且都不能再向该群发送消息。成员记录与历史消息会保留，
        但群不再接收新的消息。
      </div>
    </div>
    <template #head>
      <button class="btn btn-danger" :disabled="busy" @click="dissolve">
        {{ busy ? '解散中…' : '确认解散' }}
      </button>
    </template>
  </Modal>
</template>

<style scoped>
.form {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 16px 20px 24px;
}

.rename {
  display: flex;
  gap: 8px;
}

.rename input {
  flex: 1 1 auto;
  height: 34px;
  padding: 0 10px;
  border-radius: var(--r-sm);
  background: var(--bg-elevated);
  border: 1px solid var(--divider);
  color: var(--text-primary);
  font-size: var(--fs-md);
}

.banner.warn {
  padding: 8px 10px;
  border-radius: var(--r-sm);
  background: var(--danger-soft);
  color: var(--danger);
  font-size: var(--fs-sm);
}

.section-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-family: var(--font-display);
  font-size: var(--fs-sm);
  color: var(--text-secondary);
}

.members {
  display: flex;
  flex-direction: column;
}

.member {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 7px 6px;
}

.member + .member {
  border-top: 1px solid var(--divider);
}

/* 已退出 / 已移出：整行降透明度。只降一点点 ——
   降太多会让「已退出」看起来像「已删除」，而记录其实是刻意保留的。 */
.member.inactive {
  opacity: 0.55;
}

.member-main {
  flex: 1 1 auto;
  min-width: 0;
}

.member-name {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: var(--fs-md);
  color: var(--text-primary);
}

.member-sub {
  font-size: var(--fs-xs);
  color: var(--text-muted);
}

.tag.warn {
  background: var(--danger-soft);
  color: var(--danger);
}

.picks {
  display: flex;
  flex-direction: column;
  gap: 2px;
  max-height: 200px;
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
}

.pick:hover {
  background: var(--bg-hover);
}

.spacer {
  flex: 1 1 auto;
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

/* 危险动作：整屏只有「解散群」用红色实心块 —— 它是唯一不可撤销的动作。 */
.btn-danger {
  background: var(--danger-soft);
  color: var(--danger);
  border: 1px solid transparent;
}

.btn-danger:hover {
  background: color-mix(in oklch, var(--danger) 26%, transparent);
}
</style>
