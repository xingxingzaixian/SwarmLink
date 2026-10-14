<script lang="ts">
/**
 * 已打开弹层的顺序（后进先出）。
 *
 * 必须在模块作用域：弹框里再开弹框时，两层都监听 window 的 keydown，
 * 不区分的话按一次 Esc 会把两层一起关掉（`stopPropagation` 拦不住同样挂在
 * window 上的另一个监听器）—— 用户以为自己只退了当前这层。
 */
const openDialogs: object[] = []
</script>

<script setup lang="ts">
/**
 * 通用弹层。
 *
 * 设置 / 调试 / 传输 / 共享文件共用它 —— 它们都是「与当前会话无关的全局功能」，
 * 放进右栏会和「用户信息 / 文件进度」抢位置。
 */
import { onBeforeUnmount, onMounted, ref } from 'vue'

import Icon from './Icon.vue'

const props = withDefaults(
  defineProps<{
    title: string
    subtitle?: string
    width?: number
    height?: number
  }>(),
  { subtitle: '', width: 760, height: 600 }
)

const emit = defineEmits<{ (e: 'close'): void }>()

const dialog = ref<HTMLElement | null>(null)

/** 本实例在弹层栈里的身份（用对象而不是索引：索引会随下层关闭而错位）。 */
const self = {}

/** Esc 关闭：桌面应用的基本礼仪，缺了会让人以为弹层卡住。只关最上层那一个。 */
function onKey(e: KeyboardEvent): void {
  if (e.key !== 'Escape') return
  if (openDialogs[openDialogs.length - 1] !== self) return
  e.stopPropagation()
  emit('close')
}

onMounted(() => {
  openDialogs.push(self)
  window.addEventListener('keydown', onKey)
  dialog.value?.focus()
})

onBeforeUnmount(() => {
  const i = openDialogs.indexOf(self)
  if (i >= 0) openDialogs.splice(i, 1)
  window.removeEventListener('keydown', onKey)
})
</script>

<template>
  <Teleport to="body">
    <div class="overlay" @click.self="emit('close')">
      <section ref="dialog" class="dialog anim-pop" role="dialog" aria-modal="true" tabindex="-1"
        :style="{ width: width + 'px', height: height + 'px' }">
        <header class="head">
          <div class="titles">
            <h2>{{ title }}</h2>
            <p v-if="subtitle" class="faint">{{ subtitle }}</p>
          </div>
          <slot name="head" />
          <button class="icon-btn" title="关闭 (Esc)" aria-label="关闭" @click="emit('close')">
            <Icon name="x" :size="17" />
          </button>
        </header>

        <div v-if="$slots.tabs" class="tabs">
          <slot name="tabs" />
        </div>

        <div class="body scroll">
          <slot />
        </div>

        <!-- 吸底操作条：可选。有它在，滚动到内容任何位置都能落最终动作。 -->
        <footer v-if="$slots.footer" class="foot">
          <slot name="footer" />
        </footer>
      </section>
    </div>
  </Teleport>
</template>

<style scoped>
.overlay {
  position: fixed;
  inset: 0;
  z-index: 100;
  display: flex;
  align-items: center;
  justify-content: center;
  /* 遮罩压暗一档就够，不再叠模糊：弹层本身已是实心面板，
     再加一层玻璃只会让背景「糊成一片」却没有任何信息价值。 */
  background: color-mix(in oklch, var(--bg-sunken) 62%, transparent);
  animation: sl-fade var(--dur-2) var(--ease) both;
}

.dialog {
  display: flex;
  flex-direction: column;
  max-width: calc(100vw - 48px);
  max-height: calc(100vh - 48px);
  background: var(--bg-panel);
  border: 1px solid var(--divider-strong);
  border-radius: var(--r-lg);
  box-shadow: var(--shadow-lg);
  overflow: hidden;
}

.dialog:focus {
  outline: none;
}

.head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex: 0 0 auto;
  padding: 16px 16px 14px 20px;
  border-bottom: 1px solid var(--divider);
}

.titles {
  flex: 1 1 auto;
  min-width: 0;
}

h2 {
  margin: 0;
  font-family: var(--font-display);
  font-size: var(--fs-xl);
  font-weight: 600;
  letter-spacing: -0.01em;
}

.titles p {
  margin: 2px 0 0;
  font-size: var(--fs-xs);
}

.tabs {
  display: flex;
  gap: 4px;
  flex: 0 0 auto;
  padding: 0 20px;
  border-bottom: 1px solid var(--divider);
}

.tabs :deep(button) {
  position: relative;
  height: 34px;
  padding: 0 12px;
  font-family: var(--font-display);
  font-size: var(--fs-md);
  color: var(--text-secondary);
  transition: color var(--dur-1) var(--ease);
}

.tabs :deep(button:hover) {
  color: var(--text-primary);
}

.tabs :deep(button.on) {
  color: var(--text-primary);
  font-weight: 600;
}

/* 下划线用一个伪元素做，避免切换时布局跳动 */
.tabs :deep(button.on)::after {
  content: '';
  position: absolute;
  left: 8px;
  right: 8px;
  bottom: -1px;
  height: 2px;
  border-radius: 2px 2px 0 0;
  background: var(--accent);
}

.body {
  flex: 1 1 auto;
  min-height: 0;
  background: var(--bg-panel);
  padding: 14px 16px;
}

.foot {
  flex: 0 0 auto;
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 10px;
  padding: 12px 20px;
  border-top: 1px solid var(--divider);
  background: var(--bg-panel);
}
</style>
