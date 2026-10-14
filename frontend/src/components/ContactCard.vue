<script setup lang="ts">
/**
 * 右栏「资料」：当前会话对象的身份与网络事实。
 *
 * 这里承担一个产品判断：对一个 P2P 排障工具来说，
 * 「对方在哪个子网、地址是什么、什么时候最后出现、连得好不好」
 * 比「个性签名」有用得多。所以资料卡的元素是网络事实，而不是社交资料。
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

/**
 * 连接质量分档 —— 全部来自真实数据，没有数据就说没有数据。
 *
 * 后端只在该节点有 TCP 会话时才谈得上 RTT，而心跳要跑完一轮才有第一个值。
 * 因此「没会话」与「刚连上还没测到」必须分开说：前者是「无连接」，
 * 后者是「测量中」。把两者都显示成 0ms 是编造。
 */
const quality = computed(() => {
  const p = props.peer
  if (!p || !p.connected) return { level: 0, text: '无连接' }
  const ms = p.rttMs
  if (ms <= 0) return { level: 0, text: '测量中' }
  if (ms <= 30) return { level: 5, text: `优 · ${ms}ms` }
  if (ms <= 80) return { level: 4, text: `良 · ${ms}ms` }
  if (ms <= 200) return { level: 3, text: `一般 · ${ms}ms` }
  return { level: 2, text: `较差 · ${ms}ms` }
})
</script>

<template>
  <div class="card">
    <div class="profile">
      <Avatar
        :seed="group ? group.groupId : peer?.nodeId || convId"
        :name="group ? group.name : peer?.displayName || ''"
        :size="92"
        :shape="group ? 'square' : 'circle'"
        :online="group ? null : peer ? peer.connected || peer.online : null"
        active
      />
      <div class="profile__name">{{ group ? group.name : peer?.displayName || '未知节点' }}</div>
      <span class="profile__status" :class="presence.cls">
        <span class="dot dot--static" />
        {{ presence.text }}
      </span>

      <!-- 单聊：发文件是这个面板的主要动作，做成整宽的强调色主按钮 -->
      <button v-if="peer" class="send-file-btn" @click="emit('send-file')">
        <Icon name="upload" :size="16" />发送文件
      </button>
    </div>

    <!-- 连接质量：右栏的视觉锚点，也是「连得好不好」的唯一量化表达 -->
    <div v-if="peer" class="signal-card">
      <div class="signal-card__top">
        <span class="signal-card__label micro">连接质量</span>
        <span class="signal-card__val">{{ quality.text }}</span>
      </div>
      <div class="signal" :class="{ idle: quality.level === 0 }">
        <span v-for="n in 5" :key="n" :class="{ dim: n > quality.level }" />
      </div>
    </div>

    <div v-if="peer" class="section">
      <div class="section__title micro">节点信息</div>
      <div class="kv">
        <span class="kv__k">节点 ID</span>
        <span class="kv__v">
          <span class="nowrap">{{ peer.nodeId }}</span>
          <button
            class="kv__copy"
            :title="`复制节点 ID`"
            :aria-label="`复制节点 ID`"
            @click="copy(peer.nodeId, 'id')"
          >
            <Icon :name="copied === 'id' ? 'check' : 'copy'" :size="14" />
          </button>
        </span>
      </div>
      <div class="kv">
        <span class="kv__k">地址</span>
        <span class="kv__v">{{ peer.lastAddr || '未知' }}</span>
      </div>
      <div class="kv">
        <span class="kv__k">子网</span>
        <span class="kv__v">{{ peer.subnet || '未知' }}</span>
      </div>
      <div class="kv">
        <span class="kv__k">发现方式</span>
        <span class="kv__v">{{ peer.source || '—' }}</span>
      </div>
      <div class="kv">
        <span class="kv__k">最后出现</span>
        <span class="kv__v" :title="fullTime(peer.lastSeen)">{{ ago(peer.lastSeen) }}</span>
      </div>
    </div>

    <div v-if="peer" class="section">
      <div class="section__title micro">安全</div>
      <div class="kv">
        <span class="kv__k">身份认证</span>
        <span class="kv__v on">{{ peer.connected ? 'Ed25519 已通过' : 'Ed25519（握手时校验）' }}</span>
      </div>
      <!-- 不写成「已加密」：协议里 Flags.ENCRYPTED 与 KEY_EXCHANGE 只是预留的接缝，
           真的加密通道 v1.1 才启用。这种话界面宁可说得保守。 -->
      <div class="kv">
        <span class="kv__k">传输加密</span>
        <span class="kv__v" title="协议已预留 Flags.ENCRYPTED 与 KEY_EXCHANGE，v1.1 启用">
          v1.1 计划启用
        </span>
      </div>
    </div>

    <template v-if="group">
      <div class="section">
        <div class="section__title micro">群信息</div>
        <div class="kv">
          <span class="kv__k">群 ID</span>
          <span class="kv__v">
            <span class="nowrap">{{ group.groupId }}</span>
            <button class="kv__copy" title="复制群 ID" aria-label="复制群 ID" @click="copy(group.groupId, 'id')">
              <Icon :name="copied === 'id' ? 'check' : 'copy'" :size="14" />
            </button>
          </span>
        </div>
        <div class="kv">
          <span class="kv__k">我的角色</span>
          <span class="kv__v">{{ group.isOwner ? '群主' : '成员' }}</span>
        </div>
        <div class="kv">
          <span class="kv__k">成员版本</span>
          <span class="kv__v">epoch {{ group.epoch }}</span>
        </div>
        <div class="kv">
          <span class="kv__k">在线</span>
          <span class="kv__v">{{ groups.activeCount(group) }} / {{ groups.memberCount(group) }}</span>
        </div>
      </div>

      <div class="section">
        <div class="section__title micro">成员</div>
        <div class="members">
          <div v-for="m in group.members" :key="m.nodeId" class="member">
            <Avatar :seed="m.nodeId" :name="m.displayName" :size="28" :online="null" />
            <span class="m-name nowrap">{{ m.displayName }}</span>
            <span v-if="m.role === 'owner'" class="tag brand">群主</span>
            <span v-if="groups.memberStateLabel(m)" class="tag">
              {{ groups.memberStateLabel(m) }}
            </span>
          </div>
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
  padding: 24px 20px;
}

/* ---------------------------------------------------------------- 资料头 */

.profile {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
}

.profile__name {
  font-family: var(--font-display);
  font-size: 20px;
  font-weight: 600;
  text-align: center;
  word-break: break-word;
}

/* 状态胶囊：默认强调色（「好」的状态一律用信号绿），
   否则按 presence 给的语义类退回中性/警示色。 */
.profile__status {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  padding: 5px 14px;
  border-radius: 999px;
  background: var(--accent-soft);
  color: var(--accent);
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
}

/* 胶囊里的小圆点跟着胶囊的字色走（currentColor）：
   连接可用是绿的、在线未连接是灰的、离线是红的 —— 一处规则三种色调。 */
.profile__status .dot {
  background: currentColor;
}

.profile__status.brand {
  background: var(--bg-elevated);
  color: var(--text-secondary);
}

.profile__status.err {
  background: var(--danger-soft);
  color: var(--danger);
}

.send-file-btn {
  width: 100%;
  margin-top: 8px;
  padding: 12px;
  border-radius: var(--r-md);
  background: linear-gradient(135deg, var(--accent), var(--accent-strong));
  color: var(--accent-ink);
  font-family: var(--font-display);
  font-size: var(--fs-lg);
  font-weight: 600;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  transition: transform var(--dur-1) var(--ease), box-shadow var(--dur-2) var(--ease);
}

.send-file-btn:hover {
  transform: translateY(-2px);
  box-shadow: var(--shadow-glow);
}

/* ---------------------------------------------------------------- 成员列表 */

.members {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.member {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 6px 8px;
  border-radius: var(--r-sm);
  font-size: var(--fs-md);
  transition: background var(--dur-1) var(--ease);
}

.member:hover {
  background: var(--bg-hover);
}

.m-name {
  flex: 1 1 auto;
  min-width: 0;
}

/* ---------------------------------------------------------------- 提示 */

.tip {
  margin: 20px 0 0;
  padding: 9px 11px;
  border-radius: var(--r-sm);
  background: var(--bg-sunken);
  border: 1px solid var(--divider);
  color: var(--text-secondary);
  font-size: var(--fs-xs);
  line-height: 1.55;
}

/* 分档之外的条压暗：五条一起亮就分不出「优」与「一般」了。
   动画保留 —— 压暗的条仍在轻轻起伏，读起来是「这条信道上没有信号」。 */
.signal span.dim {
  opacity: 0.22;
  animation: none;
  transform: scaleY(0.4);
}
</style>
