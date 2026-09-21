<script setup lang="ts">
/**
 * 通用弹层。
 *
 * 设置 / 调试 / 传输三个面板共用它 —— 它们都是「与当前会话无关的全局功能」，
 * 放进右栏会和「用户信息 / 文件进度」抢位置（这正是本次改版要解决的问题）。
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

/** Esc 关闭：桌面应用的基本礼仪，缺了会让人以为弹层卡住。 */
function onKey(e: KeyboardEvent): void {
  if (e.key === 'Escape') {
    e.stopPropagation()
    emit('close')
  }
}

onMounted(() => {
  window.addEventListener('keydown', onKey)
  dialog.value?.focus()
})

onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
</script>

<template>
  <Teleport to="body">
    <div class="overlay" @click.self="emit('close')">
      <section
        ref="dialog"
        class="dialog anim-pop"
        role="dialog"
        aria-modal="true"
        tabindex="-1"
        :style="{ width: width + 'px', height: height + 'px' }"
      >
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
  /* 深色界面上的遮罩要压得比浅色更狠：否则后面那片饱和渐变会跟弹层抢注意力 */
  background: rgba(6, 2, 18, 0.5);
  backdrop-filter: blur(10px) saturate(120%);
  -webkit-backdrop-filter: blur(10px) saturate(120%);
  animation: sl-fade var(--dur-2) var(--ease) both;
}

.dialog {
  display: flex;
  flex-direction: column;
  max-width: calc(100vw - 48px);
  max-height: calc(100vh - 48px);
  background: var(--glass-float);
  backdrop-filter: blur(var(--blur-lg)) saturate(140%);
  -webkit-backdrop-filter: blur(var(--blur-lg)) saturate(140%);
  border-radius: var(--r-2xl);
  box-shadow: var(--shadow-modal);
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
}

.titles {
  flex: 1 1 auto;
  min-width: 0;
}

h2 {
  margin: 0;
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
  box-shadow: inset 0 -1px 0 var(--edge);
}

.tabs :deep(button) {
  position: relative;
  height: 34px;
  padding: 0 12px;
  font-size: var(--fs-md);
  color: var(--t2);
  transition: color var(--dur-1) var(--ease);
}

.tabs :deep(button:hover) {
  color: var(--t1);
}

.tabs :deep(button.on) {
  color: var(--t1);
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
  background: var(--brand);
}

html[data-theme='light'] .overlay {
  background: rgba(24, 34, 54, 0.28);
}

.body {
  flex: 1 1 auto;
  min-height: 0;
  /* 透明：让弹层本身的玻璃材质一直贯到底部，中间不出现「换了一层」的接缝 */
  background: transparent;
}
</style>
