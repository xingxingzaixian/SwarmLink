<script setup lang="ts">
/**
 * 右栏：资料 ⇄ 传输进度。
 *
 * 自动切换的规则（这是本次改版的核心交互）：
 *   - 默认显示「资料」；
 *   - 一旦当前会话产生进行中的传输，自动切到「传输」；
 *   - 传输全部结束后切回「资料」；
 *   - 用户手动切到「资料」后就不再抢 —— 除非用户自己又发起了一次传输。
 *
 * 第 4 条是关键：进度条把人正在看的资料顶掉是很难受的，
 * 但用户主动点了发送却又看不到进度，同样难受。区分「自动」与「主动」即可。
 */
import { computed, watch } from 'vue'

import ContactCard from './ContactCard.vue'
import Icon from './Icon.vue'
import TransferList from './TransferList.vue'
import { useChatStore } from '../stores/chat'
import { useGroupStore } from '../stores/group'
import { usePeersStore } from '../stores/peers'
import { isActive, useTransferStore } from '../stores/transfer'
import { useUiStore } from '../stores/ui'
import { useFileSend } from '../composables/useFileSend'

const chat = useChatStore()
const peers = usePeersStore()
const groups = useGroupStore()
const transfers = useTransferStore()
const ui = useUiStore()
const { attach } = useFileSend()

const conv = computed(() => chat.activeConversation)
const isGroup = computed(() => chat.activeIsGroup)
const peerId = computed(() => chat.activePeerId)

const peer = computed(() => (peerId.value ? peers.byId(peerId.value) : undefined))
const group = computed(() =>
  conv.value?.kind === 'group' ? groups.groups.find((g) => g.groupId === conv.value?.convId) : undefined
)

/** 群聊不支持点对点传文件（v1.0），因此不做「传输」页。 */
const canTransfer = computed(() => !isGroup.value && !!peerId.value)

const jobs = computed(() => (peerId.value ? transfers.forPeer(peerId.value) : []))
const running = computed(() => jobs.value.filter((j) => isActive(j.status)).length)

/** 群聊 / 无会话时强制停在资料页。 */
const mode = computed(() => (canTransfer.value ? ui.sideMode : 'info'))

watch(
  () => chat.activeConvId,
  () => ui.resetSide(running.value > 0)
)

watch(
  running,
  (n) => {
    if (!canTransfer.value || ui.pinned) return
    if (n > 0) ui.focusTransfer()
    else ui.resetSide(false)
  },
  { immediate: true }
)
</script>

<template>
  <section class="side">
    <header class="side-head">
      <div class="seg" role="tablist">
        <button
          role="tab"
          :aria-selected="mode === 'info'"
          :class="{ on: mode === 'info' }"
          @click="ui.focusInfo()"
        >
          资料
        </button>
        <button
          v-if="canTransfer"
          role="tab"
          :aria-selected="mode === 'transfer'"
          :class="{ on: mode === 'transfer' }"
          @click="ui.focusTransfer()"
        >
          传输
          <span v-if="running" class="count">{{ running }}</span>
        </button>
      </div>
      <span class="spacer" />
      <button
        class="icon-btn"
        :title="ui.theme === 'dark' ? '切换到浅色主题' : '切换到深色主题'"
        aria-label="切换主题"
        @click="ui.toggleTheme()"
      >
        <Icon :name="ui.theme === 'dark' ? 'sun' : 'moon'" :size="16" />
      </button>
    </header>

    <div class="side-body scroll">
      <div v-if="!conv" class="empty">
        <div class="empty-title">未选择会话</div>
        <div>选中左侧的联系人后，这里会显示对方的资料与传输进度。</div>
      </div>

      <ContactCard
        v-else-if="mode === 'info'"
        :conv-id="conv.convId"
        :peer="peer"
        :group="group"
        @send-file="attach"
      />

      <div v-else-if="!jobs.length" class="empty">
        <div class="empty-title">还没有传输记录</div>
        <div>点聊天区的回形针，或把文件拖进聊天区即可发起。</div>
      </div>

      <TransferList v-else :jobs="jobs" />
    </div>

    <footer v-if="canTransfer && mode === 'transfer'" class="side-foot">
      <button class="btn btn-ghost btn-block" @click="ui.focusInfo()">
        <Icon name="back" :size="14" />返回资料
      </button>
    </footer>
  </section>
</template>

<style scoped>
.side {
  position: relative;
  display: flex;
  flex-direction: column;
  min-height: 0;
  background: var(--glass-side);
  backdrop-filter: blur(var(--blur)) saturate(110%);
  -webkit-backdrop-filter: blur(var(--blur)) saturate(110%);
  box-shadow: var(--shadow-glass), inset 0 1px 0 var(--edge),
    -16px 0 30px -26px rgba(2, 0, 12, 0.85);
}

.side-head .spacer {
  flex: 1 1 auto;
}

.side-head {
  display: flex;
  align-items: center;
  flex: 0 0 auto;
  height: var(--head-h);
  padding: 0 14px;
  background: var(--glass-chrome);
  backdrop-filter: blur(10px);
  -webkit-backdrop-filter: blur(10px);
  box-shadow: inset 0 -1px 0 var(--edge);
}

/* 分段控件：浅底 + 白卡片表示选中，比两个 tab 下划线更适合窄栏。
   轨道必须靠【一点灰】描边才成立 —— 纯白描边在白色玻璃上等于没画，
   未选中的那一项会看起来像浮在外面的孤立文字。 */
.seg {
  display: flex;
  gap: 2px;
  padding: 2px;
  border-radius: var(--r-md);
  background: var(--surface-sunken);
  box-shadow: inset 0 0 0 1px var(--line);
}

.seg button {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  height: 26px;
  padding: 0 12px;
  border-radius: var(--r-sm);
  font-size: var(--fs-sm);
  color: var(--t2);
  transition: background var(--dur-1) var(--ease), color var(--dur-1) var(--ease);
}

.seg button:hover {
  color: var(--t1);
}

.seg button.on {
  background: var(--surface-strong);
  color: var(--t1);
  font-weight: 600;
  box-shadow: 0 2px 8px -4px rgba(4, 0, 18, 0.7), inset 0 1px 0 rgba(255, 255, 255, 0.22);
}

.count {
  min-width: 16px;
  height: 16px;
  padding: 0 4px;
  border-radius: 999px;
  background: var(--brand);
  color: #fff;
  color: #fff;
  font-size: 10px;
  font-weight: 600;
  line-height: 16px;
  text-align: center;
}

.side-body {
  flex: 1 1 auto;
  min-height: 0;
}

.side-foot {
  flex: 0 0 auto;
  padding: 10px 14px;
  background: var(--glass-chrome);
  backdrop-filter: blur(10px);
  -webkit-backdrop-filter: blur(10px);
  box-shadow: inset 0 1px 0 var(--edge);
}
</style>
