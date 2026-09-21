<script setup lang="ts">
/**
 * 右栏「资料」：当前会话对象的身份与网络事实。
 *
 * 这里承担一个产品判断：对一个 P2P 排障工具来说，
 * 「对方在哪个子网、地址是什么、什么时候最后出现」比「个性签名」有用得多。
 * 所以资料卡的元素是网络事实，而不是社交资料。
 */
import { computed, ref } from 'vue'

import Avatar from './Avatar.vue'
import Icon from './Icon.vue'
import type { Group, Peer } from '../api/types'
import { useGroupStore } from '../stores/group'
import { ago, fullTime } from '../utils/format'

const props = defineProps<{
  convId: string
  peer?: Peer
  group?: Group
}>()

const emit = defineEmits<{ (e: 'send-file'): void }>()

const groups = useGroupStore()
const copied = ref('')

async function copy(text: string, what: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    copied.value = what
    window.setTimeout(() => (copied.value = ''), 1500)
  } catch {
    // WebView 未授予剪贴板权限时静默失败：用户仍可手动选中文本
  }
}

/** 状态标签：把「在线」与「连接可用」分开说，避免用户误以为随时可发。 */
const presence = computed(() => {
  const p = props.peer
  if (!p) return { text: '群聊', cls: 'brand' }
  if (p.connected) return { text: '连接可用', cls: 'ok' }
  if (p.online) return { text: '在线（未连接）', cls: 'brand' }
  if (p.state === 'offline') return { text: '离线', cls: 'err' }
  return { text: '已发现', cls: '' }
})
</script>

<template>
  <div class="card">
    <div class="hero">
      <Avatar
        :seed="group ? group.groupId : peer?.nodeId || convId"
        :name="group ? group.name : peer?.displayName || ''"
        :size="70"
        :shape="group ? 'square' : 'circle'"
        :online="group ? null : peer ? peer.connected || peer.online : null"
      />
      <div class="name">{{ group ? group.name : peer?.displayName || '未知节点' }}</div>
      <span class="tag" :class="presence.cls">{{ presence.text }}</span>
    </div>

    <!-- 单聊：发文件是这个面板的主要动作，因此放在最显眼的位置 -->
    <div v-if="peer" class="actions">
      <button class="btn btn-primary btn-block" @click="emit('send-file')">
        <Icon name="paperclip" :size="14" />发送文件
      </button>
    </div>

    <div v-if="peer" class="info">
      <div class="row">
        <span class="k">节点 ID</span>
        <button class="v mono copyable" :title="peer.nodeId" @click="copy(peer.nodeId, 'id')">
          <span class="nowrap">{{ peer.nodeId }}</span>
          <Icon :name="copied === 'id' ? 'check' : 'copy'" :size="12" />
        </button>
      </div>
      <div class="row">
        <span class="k">地址</span>
        <span class="v mono">{{ peer.lastAddr || '未知' }}</span>
      </div>
      <div class="row">
        <span class="k">子网</span>
        <span class="v mono">{{ peer.subnet || '未知' }}</span>
      </div>
      <div class="row">
        <span class="k">发现方式</span>
        <span class="v">{{ peer.source || '—' }}</span>
      </div>
      <div class="row">
        <span class="k">最后出现</span>
        <span class="v" :title="fullTime(peer.lastSeen)">{{ ago(peer.lastSeen) }}</span>
      </div>
    </div>

    <template v-if="group">
      <div class="info">
        <div class="row">
          <span class="k">群 ID</span>
          <button class="v mono copyable" :title="group.groupId" @click="copy(group.groupId, 'id')">
            <span class="nowrap">{{ group.groupId }}</span>
            <Icon :name="copied === 'id' ? 'check' : 'copy'" :size="12" />
          </button>
        </div>
        <div class="row">
          <span class="k">我的角色</span>
          <span class="v">{{ group.isOwner ? '群主' : '成员' }}</span>
        </div>
        <div class="row">
          <span class="k">成员版本</span>
          <span class="v mono">epoch {{ group.epoch }}</span>
        </div>
        <div class="row">
          <span class="k">在线</span>
          <span class="v">{{ groups.activeCount(group) }} / {{ group.members.length }}</span>
        </div>
      </div>

      <div class="section">成员</div>
      <div class="members">
        <div v-for="m in group.members" :key="m.nodeId" class="member">
          <Avatar :seed="m.nodeId" :name="m.displayName" :size="28" :online="null" />
          <span class="m-name nowrap">{{ m.displayName }}</span>
          <span v-if="m.role === 'owner'" class="tag brand">群主</span>
          <span v-if="m.state !== 'active'" class="tag">离线</span>
        </div>
      </div>
    </template>

    <p v-if="peer && !peer.connected" class="tip">
      当前没有可用连接。发消息时会按需拨号（ADR-009：空闲连接会被回收，
      因此首条消息可能多等一个 RTT）。
    </p>
  </div>
</template>

<style scoped>
.card {
  display: flex;
  flex-direction: column;
  padding: 22px 16px 24px;
}

.hero {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 9px;
  padding-bottom: 18px;
}

.name {
  font-size: var(--fs-xl);
  font-weight: 600;
  text-align: center;
  word-break: break-word;
}

.actions {
  padding: 0 4px 14px;
}

.info {
  display: flex;
  flex-direction: column;
  gap: 1px;
  padding: 12px 0;
  border-top: 1px solid var(--line-soft);
}

.row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  min-height: 26px;
  font-size: var(--fs-sm);
}

.k {
  flex: 0 0 auto;
  color: var(--t3);
}

.v {
  min-width: 0;
  text-align: right;
  color: var(--t2);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.copyable {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  max-width: 168px;
  color: var(--t1);
  cursor: pointer;
  border-radius: var(--r-xs);
  transition: color var(--dur-1) var(--ease);
}

.copyable:hover {
  color: var(--brand-ink);
}

.section {
  padding: 10px 0 6px;
  box-shadow: inset 0 1px 0 var(--edge);
  font-size: var(--fs-xs);
  font-weight: 600;
  letter-spacing: 0.04em;
  color: var(--t3);
}

.members {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.member {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 5px 6px;
  border-radius: var(--r-sm);
  font-size: var(--fs-md);
}

.member:hover {
  background: var(--hover);
}

.m-name {
  flex: 1 1 auto;
  min-width: 0;
}

.tip {
  margin: 14px 0 0;
  padding: 9px 11px;
  border-radius: var(--r-md);
  background: var(--surface-sunken);
  box-shadow: inset 0 0 0 1px var(--edge);
  color: var(--t2);
  font-size: var(--fs-xs);
  line-height: 1.55;
}
</style>
