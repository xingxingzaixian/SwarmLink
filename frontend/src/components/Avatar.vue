<script setup lang="ts">
/**
 * 确定性头像。
 *
 * 本项目【没有】头像上传，也不该给每个节点用同一张灰色占位图 ——
 * 局域网里同时看到 10 个节点时，靠名字首字 + 稳定的颜色区分是最快的。
 * 颜色由 node_id 哈希决定：同一个节点在任何位置（列表 / 气泡 / 资料卡）
 * 永远是同一个颜色，用户会自然地把它当成身份标识。
 *
 * 调色板是【手选】的 8 组同明度渐变，不是随机 HSL ——
 * 随机 HSL 会产出脏黄、荧光绿这类破坏浅色主题的颜色。
 */
import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    /** 用于取色的稳定标识（node_id / group_id）。 */
    seed?: string
    name?: string
    size?: number
    /** 人用圆形，群用圆角方形 —— 沿用 IM 的通用约定。 */
    shape?: 'circle' | 'square'
    /** null 表示不显示在线点。 */
    online?: boolean | null
    /** 选中态：给头像加一圈品牌色描边。 */
    active?: boolean
  }>(),
  { seed: '', name: '', size: 38, shape: 'circle', online: null, active: false }
)

const PALETTE: [string, string][] = [
  ['#5b8def', '#3667d6'],
  ['#19b8f0', '#0b91cc'],
  ['#8a7cf5', '#6455e0'],
  ['#2ec4a6', '#159a80'],
  ['#f0a04b', '#d47c22'],
  ['#ef7396', '#d2486f'],
  ['#8fbf4a', '#6b9a2c'],
  ['#7d8ea8', '#5c6d87']
]

/** djb2：够短，且对 node_id 这类 hex 串分布良好。 */
function hash(s: string): number {
  let h = 5381
  for (let i = 0; i < s.length; i++) h = ((h << 5) + h + s.charCodeAt(i)) >>> 0
  return h
}

const tones = computed(() => PALETTE[hash(props.seed || props.name) % PALETTE.length])

/** 中文取首字，拉丁取首字母大写；都没有则退回问号。 */
const label = computed(() => {
  const s = (props.name || '').trim()
  if (!s) return '?'
  const ch = s[0]
  return /[a-z]/i.test(ch) ? ch.toUpperCase() : ch
})

const radius = computed(() =>
  props.shape === 'circle' ? '50%' : `${Math.max(6, Math.round(props.size * 0.28))}px`
)

const dotSize = computed(() => Math.max(8, Math.round(props.size * 0.28)))
</script>

<template>
  <div
    class="avatar"
    :class="{ ring: active }"
    :style="{
      width: size + 'px',
      height: size + 'px',
      borderRadius: radius,
      background: `linear-gradient(135deg, ${tones[0]}, ${tones[1]})`,
      fontSize: Math.round(size * 0.42) + 'px'
    }"
  >
    <span>{{ label }}</span>
    <span
      v-if="online !== null"
      class="state"
      :class="online ? 'on' : 'off'"
      :style="{
        width: dotSize + 'px',
        height: dotSize + 'px',
        borderWidth: Math.max(1.5, dotSize * 0.2) + 'px'
      }"
    />
  </div>
</template>

<style scoped>
.avatar {
  position: relative;
  flex: 0 0 auto;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #fff;
  font-weight: 600;
  letter-spacing: 0.01em;
  user-select: none;
  /* 渐变上的白字要压一点内阴影才不显薄 */
  box-shadow: inset 0 -1px 0 rgba(0, 0, 0, 0.08);
}

/* 选中描边用粉（身份色）。底色写成半透明深色而不是某个具体面板色：
   头像会出现在玻璃栏、弹层、列表行上，写死底色就会到处露馅 */
.avatar.ring {
  box-shadow: inset 0 -1px 0 rgba(0, 0, 0, 0.08),
    0 0 0 2px var(--ring), 0 0 0 4px var(--brand);
}

/* 在线点的「环」= 头像底色的缺口。
   深色界面上要用深环；用白环会变成一个刺眼的光圈。 */
.state {
  position: absolute;
  right: -1px;
  bottom: -1px;
  border-style: solid;
  border-color: var(--ring);
  border-radius: 50%;
}

.state.on {
  background: var(--ok);
}

.state.off {
  background: #c6ccd6;
}
</style>
