<script setup lang="ts">
/**
 * 图标集（内联 SVG，零依赖）。
 *
 * 为什么不用图标字体 / 图标库：
 *  - 字体图标在中文界面里会出现基线偏移，且无法按需 tree-shake；
 *  - 这里只有 20 来个图标，内联的代价远小于一个依赖。
 *
 * 全部图标统一 24 网格、1.7 描边、圆头圆角，因此任意尺寸下粗细观感一致。
 * 颜色一律走 currentColor —— 图标不该自己决定颜色。
 */
import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    name: string
    size?: number
    stroke?: number
  }>(),
  { size: 16, stroke: 1.7 }
)

/** 圆用两段圆弧表达，这样可以和普通路径共存于同一个数组。 */
const ICONS: Record<string, string[]> = {
  search: ['M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14', 'M16.2 16.2 21 21'],
  paperclip: [
    'M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48'
  ],
  send: ['M21.5 2.5 10.5 13.5', 'M21.5 2.5 14.5 21.5 10.5 13.5 2.5 9.5 21.5 2.5'],
  sliders: [
    'M4 21v-7',
    'M4 10V3',
    'M12 21v-9',
    'M12 8V3',
    'M20 21v-5',
    'M20 12V3',
    'M1 14h6',
    'M9 8h6',
    'M17 16h6'
  ],
  activity: ['M22 12h-4l-3 9L9 3l-3 9H2'],
  folder: ['M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z'],
  users: [
    'M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2',
    'M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8',
    'M23 21v-2a4 4 0 0 0-3-3.87',
    'M16 3.13a4 4 0 0 1 0 7.75'
  ],
  user: ['M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2', 'M12 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8'],
  x: ['M18 6 6 18', 'M6 6l12 12'],
  back: ['M15 18l-6-6 6-6'],
  copy: [
    'M11 9h9a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2h-9a2 2 0 0 1-2-2v-9a2 2 0 0 1 2-2',
    'M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1'
  ],
  check: ['M20 6 9 17l-5-5'],
  refresh: [
    'M23 4v6h-6',
    'M1 20v-6h6',
    'M3.51 9a9 9 0 0 1 14.85-3.36L23 10',
    'M1 14l4.64 4.36A9 9 0 0 0 20.49 15'
  ],
  plus: ['M12 5v14', 'M5 12h14'],
  info: ['M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20', 'M12 16v-4', 'M12 8h.01'],
  file: ['M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z', 'M14 2v6h6'],
  download: ['M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4', 'M7 10l5 5 5-5', 'M12 15V3'],
  upload: ['M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4', 'M17 8l-5-5-5 5', 'M12 3v12'],
  menu: ['M3 6h18', 'M3 12h18', 'M3 18h18'],
  alert: [
    'M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z',
    'M12 9v4',
    'M12 17h.01'
  ],
  signal: [
    'M5 12.55a11 11 0 0 1 14.08 0',
    'M1.42 9a16 16 0 0 1 21.16 0',
    'M8.53 16.11a6 6 0 0 1 6.95 0',
    'M12 20h.01'
  ],
  clock: ['M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20', 'M12 6v6l4 2'],
  hash: ['M4 9h16', 'M4 15h16', 'M10 3 8 21', 'M16 3l-2 18'],
  chevron: ['M6 9l6 6 6-6'],
  sun: ['M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8', 'M12 1v2', 'M12 21v2', 'M1 12h2', 'M21 12h2'],
  moon: ['M20 14.5A8.5 8.5 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5'],
  image: [
    'M4 3h16a1 1 0 0 1 1 1v16a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z',
    'M8.5 10a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3',
    'M21 15.5 16 11 5 21'
  ],
  smile: [
    'M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20',
    'M8 14s1.5 2 4 2 4-2 4-2',
    'M9 9h.01',
    'M15 9h.01'
  ]
}

const paths = computed(() => ICONS[props.name] ?? [])
</script>

<template>
  <svg
    class="icon"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    :stroke-width="stroke"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    <path v-for="(d, i) in paths" :key="i" :d="d" />
  </svg>
</template>

<style scoped>
.icon {
  display: block;
  flex: 0 0 auto;
}
</style>
