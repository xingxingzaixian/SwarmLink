<script setup lang="ts">
/**
 * 右栏：资料 ⇄ 传输进度。
 *
 * 自动切换的规则（这是本次改版的核心交互）：
 *   - 默认显示「资料」；
 *   - 一旦当前会话产生进行中的传输，自动切到「传输」；
 *   - 传输结束【不】自动切回资料 —— 用户刚发完文件就被顶回资料页，
 *     等于这次传输的结果他一眼都没看到（小文件尤其明显：一闪就过去了）；
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
    // 只接管一次，不负责把面板还回去。
    if (n > 0) ui.focusTransfer()
  },
  { immediate: true }
)
</script>

<template>
  <section class="side">
    <!-- 两个等宽标签页（下划线指示选中）。主题开关不在这里 ——
         它属于「整个窗口」而不属于右栏，已挪到聊天区顶栏。 -->
    <div class="tabs" role="tablist">
      <button
        class="tab"
        role="tab"
        :aria-selected="mode === 'info'"
        :class="{ active: mode === 'info' }"
        @click="ui.focusInfo()"
      >
        资料
      </button>
      <button
        v-if="canTransfer"
        class="tab"
        role="tab"
        :aria-selected="mode === 'transfer'"
        :class="{ active: mode === 'transfer' }"
        @click="ui.focusTransfer()"
      >
        传输
        <span v-if="running" class="count">{{ running }}</span>
      </button>
    </div>

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

      <!-- 空态由 TransferList 自己画：右栏不再保留第二套空态文案，
           否则 job 到达前的那一帧会闪出一句不同的提示。 -->
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
  background: var(--bg-panel);
  border-left: 1px solid var(--divider);
}

/* 标签页的按钮形态：全局 .tab 是按 div 写的，这里补齐按钮语义所需的复位
   （宽度靠 flex:1，点击区整块，键盘可 Tab） */
.tab {
  border: 0;
  background: none;
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
}

.side-body {
  flex: 1 1 auto;
  min-height: 0;
}

.side-foot {
  flex: 0 0 auto;
  padding: 10px 14px;
  border-top: 1px solid var(--divider);
}
</style>
