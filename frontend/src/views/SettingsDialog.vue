<script setup lang="ts">
/**
 * 全局功能弹层：设置 / 调试 / 文件传输。
 *
 * 这三件事都与「当前会话」无关，所以不该占右栏 —— 右栏要留给
 * 「对方的资料」和「正在发的文件」。它们又都需要整屏空间，
 * 因此用一个带分页的弹层承载。
 *
 * 共享中心【不在这里】：它的形态（左右分栏 + 文件表格 + 一排动作）与这里的
 * 单列表单不是一回事，硬塞进页签组会两边都别扭，因此独立成 ShareDialog。
 */
import { computed } from 'vue'

import Modal from '../components/Modal.vue'
import DebugPanel from './DebugPanel.vue'
import SettingsView from './SettingsView.vue'
import TransferView from './TransferView.vue'
import { useUiStore } from '../stores/ui'

const ui = useUiStore()

/** 本弹层承载的三页。 */
type DialogTab = 'settings' | 'debug' | 'transfers'

const tabs: { key: DialogTab; label: string }[] = [
  { key: 'settings', label: '设置' },
  { key: 'debug', label: '调试面板' },
  { key: 'transfers', label: '文件传输' }
]

/**
 * 当前页。
 *
 * panel 为 'share' 时本组件根本不会被渲染（App 渲染 ShareDialog），
 * 但类型上仍要把这种情况收敛掉 —— 否则 META 会取到 undefined，
 * 一旦将来有人误改渲染条件就是一片白屏。
 */
const active = computed<DialogTab>(() =>
  ui.panel === 'debug' || ui.panel === 'transfers' ? ui.panel : 'settings'
)

const META: Record<DialogTab, { title: string; subtitle: string }> = {
  settings: {
    title: '设置',
    // 副标题不再点名「端口与网卡」：分类栏上用圆点标出了哪一页需要重启，
    // 这里改成解释那个圆点 —— 否则用户看到一个点也不知道它是什么意思。
    subtitle: '标了圆点的分类（网络）改完需重启应用才生效'
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
