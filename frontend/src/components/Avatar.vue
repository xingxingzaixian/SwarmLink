<script setup lang="ts">
/**
 * 确定性头像。
 *
 * 本项目【没有】头像上传，也不该给每个节点用同一张灰色占位图 ——
 * 局域网里同时看到 10 个节点时，靠名字首字 + 稳定的颜色区分是最快的。
 * 颜色由 node_id 哈希决定：同一个节点在任何位置（列表 / 气泡 / 资料卡）
 * 永远是同一个颜色，用户会自然地把它当成身份标识。
 *
 * 调色板是【手选】的 8 组同明度渐变，用 OKLCH 定义。两条硬约束：
 *   - 器内【没有绿色】：强调色是电信号绿，绿色头像会被误读成「在线」，
 *     而在线与否由右下角的点表达，两者不能撞语义；
 *   - 各组明度相同（L 差 ≤ 2%）：明度不齐时，深色那几组上的白字会先糊掉。
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
    /** 选中态：给头像加一圈强调色描边（列表里的选中项 / 资料卡大头像）。 */
    active?: boolean
  }>(),
  { seed: '', name: '', size: 38, shape: 'circle', online: null, active: false }
)

const PALETTE: [string, string][] = [
  ['oklch(64% 0.15 255)', 'oklch(55% 0.16 258)'], // 蓝
  ['oklch(62% 0.15 275)', 'oklch(53% 0.16 278)'], // 靛
  ['oklch(64% 0.15 300)', 'oklch(55% 0.16 303)'], // 紫
  ['oklch(66% 0.16 340)', 'oklch(56% 0.17 343)'], // 品红
  ['oklch(65% 0.16 15)', 'oklch(55% 0.17 18)'], // 玫红
  ['oklch(72% 0.14 70)', 'oklch(62% 0.15 72)'], // 琥珀
  ['oklch(64% 0.12 195)', 'oklch(54% 0.13 198)'], // 青
  ['oklch(62% 0.03 255)', 'oklch(52% 0.03 255)'] // 石板
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

const dotSize = computed(() => Math.max(9, Math.round(props.size * 0.28)))
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
        borderWidth: Math.max(2, dotSize * 0.22) + 'px'
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
  border-radius: 50%;
  color: #fff;
  font-family: var(--font-display);
  font-weight: 600;
  letter-spacing: 0.01em;
  user-select: none;
  /* 静默的一圈边：渐变直接贴在面板色上时边缘会「发毛」，
     但**不用强调色** —— 一列联系人都镶绿边会把 ≤10% 的强调色预算烧光，
     那圈绿留给选中项（.ring）与资料卡大头像。 */
  box-shadow: 0 0 0 2px var(--divider);
}

/* 选中描边用强调色。底色不写死面板色：头像会出现在面板、弹层、列表行上，
   写死就会到处露馅。 */
.avatar.ring {
  box-shadow: 0 0 0 2px var(--divider), 0 0 0 4px var(--accent-ring);
}

/* 在线点：右下角的缺口环 + 向外扩散的脉冲。
   环色用 --bg-panel：头像主要出现在左右栏面板上。 */
.state {
  position: absolute;
  right: -1px;
  bottom: -1px;
  border-style: solid;
  border-color: var(--bg-panel);
  border-radius: 50%;
}

.state.on {
  background: var(--accent);
}

/* 扩散环用伪元素，不能直接放在 .state 上 ——
   box-shadow 与上面的 border 会互相打架。 */
.state.on::after {
  content: '';
  position: absolute;
  inset: 0;
  border-radius: 50%;
  background: var(--accent);
  animation: pulse 2s var(--ease) infinite;
}

.state.off {
  background: var(--text-muted);
}
</style>
