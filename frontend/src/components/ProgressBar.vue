<script setup lang="ts">
import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    percent: number
    status?: string
    /** 校验中等「不确定进度」阶段用流动条纹表达。 */
    height?: number
  }>(),
  { status: 'active', height: 6 }
)

const clamped = computed(() => Math.max(0, Math.min(100, props.percent || 0)))

const tone = computed(() => {
  switch (props.status) {
    case 'done':
      // 成功一律用强调色：这套设计里只有「绿 = 好」一种正向语义
      return 'var(--accent)'
    case 'failed':
      return 'var(--danger)'
    case 'verifying':
      return 'var(--warn)'
    case 'paused':
      return 'var(--text-muted)'
    case 'cancelled':
      return 'var(--divider-strong)'
    default:
      // 进行中：强调色（与发送键、自己的气泡同色系）
      return 'var(--accent)'
  }
})

/** 条纹在「正在动」的状态下才有意义；已完成还给条纹会显得还在跑。 */
const striped = computed(
  () => props.status !== 'done' && props.status !== 'failed' && props.status !== 'cancelled'
)
</script>

<template>
  <div
    class="track"
    :style="{ height: height + 'px' }"
    role="progressbar"
    :aria-valuenow="Math.round(clamped)"
    aria-valuemin="0"
    aria-valuemax="100"
  >
    <!-- 用 backgroundColor 而不是 background 简写：
         简写会把类里的 background-image（流动条纹）一并重置掉，
         行内样式的优先级高于类，条纹就再也显示不出来了。 -->
    <div
      class="fill"
      :class="{ striped }"
      :style="{ width: clamped + '%', backgroundColor: tone }"
    />
  </div>
</template>

<style scoped>
.track {
  width: 100%;
  background: var(--bg-sunken);
  border: 1px solid var(--divider);
  border-radius: 999px;
  overflow: hidden;
}

.fill {
  height: 100%;
  border-radius: 999px;
  transition: width 0.2s var(--ease), background-color var(--dur-2) var(--ease);
}

/* 斜纹沿 X 轴缓慢平移：表达「在动」，但不制造错误的进度预期。
   条纹用强调色墨色而不是白 —— 白条纹落在浅色主题的亮底上会消失。 */
.striped {
  background-image: linear-gradient(
    100deg,
    color-mix(in oklch, var(--accent-ink) 26%, transparent) 25%,
    transparent 25%,
    transparent 50%,
    color-mix(in oklch, var(--accent-ink) 26%, transparent) 50%,
    color-mix(in oklch, var(--accent-ink) 26%, transparent) 75%,
    transparent 75%
  );
  background-size: 14px 14px;
  animation: flow 0.9s linear infinite;
}

@keyframes flow {
  from {
    background-position: 0 0;
  }
  to {
    background-position: 14px 0;
  }
}
</style>
