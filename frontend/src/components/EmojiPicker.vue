<script setup lang="ts">
/**
 * emoji 选择面板。
 *
 * 不用 Modal：弹层不该抢走输入框的焦点 —— 用户选完表情还要接着打字，
 * 若每选一次都要重新点一下输入框，这个面板就等于白做。
 */
import { onBeforeUnmount, onMounted, ref } from 'vue'

import { EMOJI_GROUPS } from '../constants/emoji'

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

function onKey(e: KeyboardEvent): void {
  if (e.key === 'Escape') emit('close')
}

onMounted(() => {
  document.addEventListener('click', onDocClick, true)
  document.addEventListener('keydown', onKey)
})

onBeforeUnmount(() => {
  document.removeEventListener('click', onDocClick, true)
  document.removeEventListener('keydown', onKey)
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
  border-radius: var(--r-lg);
  background: var(--glass-float);
  backdrop-filter: blur(var(--blur-lg)) saturate(140%);
  -webkit-backdrop-filter: blur(var(--blur-lg)) saturate(140%);
  box-shadow: inset 0 0 0 1px var(--edge), 0 18px 40px -22px rgba(2, 0, 12, 0.95);
}

.gname {
  margin: 2px 0 4px;
  font-size: var(--fs-xs);
  color: var(--t3);
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
  border-radius: var(--r-sm);
  background: transparent;
}

.cell:hover {
  background: var(--hover);
}
</style>
