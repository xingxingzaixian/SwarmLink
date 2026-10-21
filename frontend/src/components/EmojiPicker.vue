<script setup lang="ts">
/**
 * emoji 选择面板。
 *
 * 不用 Modal：弹层不该抢走输入框的焦点 —— 用户选完表情还要接着打字，
 * 若每选一次都要重新点一下输入框，这个面板就等于白做。
 */
import { onBeforeUnmount, onMounted, ref } from 'vue'

import { EMOJI_GROUPS } from '../constants/emoji'
import { pushHotkeyLayer } from '../hotkeys/dispatcher'

const emit = defineEmits<{ (e: 'pick', ch: string): void; (e: 'close'): void }>()

const root = ref<HTMLElement | null>(null)

/**
 * 点面板外部关闭。用捕获阶段，否则被输入区自身的点击处理挡住。
 *
 * 但触发按钮自己不算「外部」：否则点按钮时捕获阶段先关闭、按钮的
 * toggle 再把它打开，净效果是「再点一次关不掉」。调用方用
 * `data-emoji-toggle` 标记触发按钮即可。
 */
function onDocClick(e: MouseEvent): void {
  const target = e.target as HTMLElement | null
  if (target?.closest?.('[data-emoji-toggle]')) return
  if (!root.value?.contains(e.target as Node)) emit('close')
}

/**
 * Esc 关面板。
 *
 * 声明成独占层而不是自己听 keydown：面板开着时按 Ctrl+K 会把搜索弹层
 * 叠在输入区上面，而用户本来只想选个表情 —— 这类「两个层都想关」的
 * 处境由派发器统一挡住。焦点仍留在输入框（本组件不抢焦点）。
 */
let popLayer: (() => void) | null = null

onMounted(() => {
  document.addEventListener('click', onDocClick, true)
  popLayer = pushHotkeyLayer({ exclusive: true, onEsc: () => emit('close') })
})

onBeforeUnmount(() => {
  document.removeEventListener('click', onDocClick, true)
  popLayer?.()
  popLayer = null
})
</script>

<template>
  <div ref="root" class="picker scroll">
    <div v-for="g in EMOJI_GROUPS" :key="g.name" class="group">
      <div class="gname">{{ g.name }}</div>
      <div class="grid">
        <button
          v-for="ch in g.items"
          :key="ch"
          class="cell"
          type="button"
          :title="ch"
          @click="emit('pick', ch)"
        >
          {{ ch }}
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.picker {
  width: 268px;
  max-height: 232px;
  overflow-y: auto;
  padding: 8px;
  border-radius: var(--r-md);
  background: var(--bg-elevated);
  border: 1px solid var(--divider-strong);
  box-shadow: var(--shadow-lg);
}

.gname {
  margin: 2px 0 4px;
  color: var(--text-muted);
}

.grid {
  display: grid;
  grid-template-columns: repeat(8, 1fr);
  gap: 2px;
  margin-bottom: 6px;
}

.cell {
  display: flex;
  align-items: center;
  justify-content: center;
  height: 28px;
  padding: 0;
  font-size: 17px;
  line-height: 1;
  border-radius: var(--r-xs);
  background: transparent;
  transition: background var(--dur-1) var(--ease);
}

.cell:hover {
  background: var(--bg-hover);
}
</style>
