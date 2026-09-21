<script setup lang="ts">
/**
 * 全局功能弹层：设置 / 调试 / 文件传输。
 *
 * 这三件事都与「当前会话」无关，所以不该占右栏 —— 右栏要留给
 * 「对方的资料」和「正在发的文件」。它们又都需要整屏空间，
 * 因此用一个带分页的弹层承载。
 */
import { computed } from 'vue'

import Modal from '../components/Modal.vue'
import DebugPanel from './DebugPanel.vue'
import SettingsView from './SettingsView.vue'
import TransferView from './TransferView.vue'
import type { PanelKind } from '../stores/ui'
import { useUiStore } from '../stores/ui'

const ui = useUiStore()

const tabs: { key: Exclude<PanelKind, ''>; label: string }[] = [
  { key: 'settings', label: '设置' },
  { key: 'debug', label: '调试面板' },
  { key: 'transfers', label: '文件传输' }
]

const active = computed(() => (ui.panel || 'settings') as Exclude<PanelKind, ''>)

const META: Record<string, { title: string; subtitle: string }> = {
  settings: {
    title: '设置',
    subtitle: '端口与网卡相关项需重启应用生效'
  },
  debug: {
    title: '调试面板',
    subtitle: 'P2P 排障靠的是「看见」，不是「猜」'
  },
  transfers: {
    title: '文件传输',
    subtitle: '发送入口在聊天区：回形针按钮，或直接把文件拖进去'
  }
}

const meta = computed(() => META[active.value])
</script>

<template>
  <Modal :title="meta.title" :subtitle="meta.subtitle" :width="820" :height="620" @close="ui.closePanel()">
    <template #tabs>
      <button
        v-for="t in tabs"
        :key="t.key"
        :class="{ on: active === t.key }"
        @click="ui.openPanel(t.key)"
      >
        {{ t.label }}
      </button>
    </template>

    <SettingsView v-if="active === 'settings'" />
    <DebugPanel v-else-if="active === 'debug'" />
    <TransferView v-else />
  </Modal>
</template>
