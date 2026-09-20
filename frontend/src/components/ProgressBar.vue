<script setup lang="ts">
import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    percent: number
    status?: string
    height?: number
  }>(),
  { status: 'active', height: 4 }
)

const clamped = computed(() => Math.max(0, Math.min(100, props.percent || 0)))

const tone = computed(() => {
  switch (props.status) {
    case 'done':
      return 'var(--ok)'
    case 'failed':
      return 'var(--err)'
    case 'verifying':
      return 'var(--warn)'
    case 'paused':
      return 'var(--text-faint)'
    default:
      return 'var(--accent)'
  }
})
</script>

<template>
  <div class="track" :style="{ height: height + 'px' }">
    <div class="fill" :style="{ width: clamped + '%', background: tone }" />
  </div>
</template>

<style scoped>
.track {
  width: 100%;
  background: var(--border-soft);
  border-radius: 999px;
  overflow: hidden;
}

.fill {
  height: 100%;
  transition: width 0.15s linear;
}
</style>
