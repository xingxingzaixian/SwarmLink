<script setup lang="ts">
/**
 * 贴图窗口：把一块截下来的画面「钉」在桌面上。
 *
 * 它是一个无边框、置顶、独立的窗口，与聊天窗口没有关系 —— 用户截完图
 * 顺手按「贴图」，那块画面就留在原地，可以一边看一边操作别的窗口。
 *
 * 交互约定（与常见的贴图工具一致）：
 *   - 在图上按住拖动 = 移动窗口（靠 CSS 的 --wails-draggable: drag，
 *     由 Wails 运行时把鼠标位移转成窗口移动，不经过 Go）；
 *   - 滚轮 = 等比缩放（改窗口尺寸，图不变形）；
 *   - Esc / 关闭按钮 = 收起这张贴图；
 *   - 另存为 = 存成文件（字节是原图，不重新压缩）。
 *
 * 为什么不支持「复制到剪贴板」：Wails v3 的剪贴板目前只有文本
 * （Clipboard.SetText / Text），图片要等上游支持。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Window } from '@wailsio/runtime'

import Icon from '../components/Icon.vue'
import { getApi } from '../api'
import { saveFile } from '../api/dialog'

/** 贴图 id（后端开窗时写在 URL 上）。 */
const id = Number(new URLSearchParams(window.location.search).get('id') ?? '0')

/** 图片的 data URL（显示用）。 */
const src = ref('')
/** 图片原始 base64（另存为用；与 src 分开存，省掉一次字符串切割）。 */
const rawB64 = ref('')
/** 1:1 显示时的窗口尺寸（DIP），缩放都以此为基准。 */
const base = ref({ w: 0, h: 0 })
/** 当前缩放比。 */
const zoom = ref(1)
/** 鼠标是否在窗口上（决定工具条显不显示）。 */
const hover = ref(false)
/** 失败提示。 */
const err = ref('')
/** 短暂提示（保存成功之类）。 */
const toast = ref('')
let toastTimer = 0

const zoomText = computed(() => `${Math.round(zoom.value * 100)}%`)

function notify(msg: string): void {
  toast.value = msg
  window.clearTimeout(toastTimer)
  toastTimer = window.setTimeout(() => {
    toast.value = ''
  }, 1800)
}

/**
 * 改窗口尺寸来实现缩放。
 *
 * 为什么不缩放 <img> 的 CSS 尺寸而保持窗口不变：窗口比图大时，多出来的
 * 透明区域会挡住底下的点击（置顶窗口是「实心」的，透明不穿透）。
 * 让窗口始终贴合图片，才不会挡住无关的地方。
 */
async function applyZoom(next: number): Promise<void> {
  if (!base.value.w || !base.value.h) return
  const z = Math.min(4, Math.max(0.2, Math.round(next * 100) / 100))
  if (z === zoom.value) return
  try {
    await Window.SetSize(Math.round(base.value.w * z), Math.round(base.value.h * z))
    zoom.value = z
  } catch (e: any) {
    notify(String(e?.message ?? e) || '调整大小失败')
  }
}

function onWheel(e: WheelEvent): void {
  void applyZoom(zoom.value + (e.deltaY < 0 ? 0.1 : -0.1))
}

async function close(): Promise<void> {
  try {
    const api = await getApi()
    await api.closePin(id)
  } catch {
    // 后端关不掉时自己关：贴图窗口必须能被用户收起来。
    try {
      await Window.Close()
    } catch {
      /* 已经没有可关的窗口了 */
    }
  }
}

async function saveAs(): Promise<void> {
  if (!rawB64.value) return
  const d = new Date()
  const p = (n: number) => String(n).padStart(2, '0')
  const name = `贴图-${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}.png`
  const dest = await saveFile(name)
  if (!dest) return
  try {
    const api = await getApi()
    await api.saveOverlay(rawB64.value, dest)
    notify('已保存')
  } catch (e: any) {
    notify(String(e?.message ?? e) || '保存失败')
  }
}

function onKey(e: KeyboardEvent): void {
  if (e.key === 'Escape') {
    e.preventDefault()
    void close()
  } else if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
    e.preventDefault()
    void saveAs()
  }
}

onMounted(async () => {
  // 贴图窗口是透明的：全局样式给 body 的底色必须去掉，
  // 否则图片四周会出现一块不透明的方块。
  document.documentElement.style.background = 'transparent'
  document.body.style.background = 'transparent'
  window.addEventListener('keydown', onKey)

  try {
    const api = await getApi()
    const p = await api.getPinImage(id)
    rawB64.value = p.dataBase64
    base.value = { w: p.width, h: p.height }
    src.value = `data:${p.mime};base64,${p.dataBase64}`
  } catch (e: any) {
    err.value = String(e?.message ?? e) || '贴图加载失败'
  }
})

onBeforeUnmount(() => {
  window.clearTimeout(toastTimer)
  window.removeEventListener('keydown', onKey)
})
</script>

<template>
  <div class="pin" @pointerenter="hover = true" @pointerleave="hover = false">
    <!-- 图片本身就是拖动区：--wails-draggable 由 Wails 运行时识别。 -->
    <img v-if="src" class="pin__img" :src="src" alt="贴图" style="--wails-draggable: drag" @wheel.prevent="onWheel" />

    <!-- 工具条：只在鼠标移上来时出现，平时让图片独占窗口。 -->
    <div v-if="src" class="pin__bar" :class="{ 'is-on': hover }">
      <button class="pin__btn" title="缩小" @click="applyZoom(zoom - 0.1)">−</button>
      <button class="pin__btn pin__btn--wide" title="恢复原始大小" @click="applyZoom(1)">
        {{ zoomText }}
      </button>
      <button class="pin__btn" title="放大" @click="applyZoom(zoom + 0.1)">+</button>
      <span class="pin__sep" />
      <button class="pin__btn" title="另存为 (Ctrl+S)" @click="saveAs">
        <Icon name="download" :size="14" />
      </button>
      <button class="pin__btn" title="关闭 (Esc)" @click="close">
        <Icon name="x" :size="14" />
      </button>
    </div>

    <div v-if="toast" class="pin__toast">{{ toast }}</div>
    <div v-if="err" class="pin__err">{{ err }}</div>
  </div>
</template>

<style scoped>
.pin {
  position: relative;
  width: 100vw;
  height: 100vh;
  overflow: hidden;
  /* 贴图只是一张图：不选中文字、不出现光标。 */
  user-select: none;
  background: transparent;
}

.pin__img {
  display: block;
  width: 100%;
  height: 100%;
  object-fit: fill;
  cursor: move;
}

/* 工具条：贴在右上角，半透明底，不抢图片的注意力。 */
.pin__bar {
  position: absolute;
  top: 6px;
  right: 6px;
  display: flex;
  align-items: center;
  gap: 2px;
  padding: 3px 4px;
  border-radius: 7px;
  background: rgba(24, 24, 28, 0.9);
  box-shadow: 0 4px 14px rgba(0, 0, 0, 0.4);
  opacity: 0;
  transition: opacity 0.12s ease;
}

.pin__bar.is-on {
  opacity: 1;
}

.pin__btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 22px;
  height: 22px;
  padding: 0 4px;
  border: none;
  border-radius: 4px;
  background: transparent;
  color: rgba(255, 255, 255, 0.88);
  font-size: 13px;
  line-height: 1;
  cursor: pointer;
}

.pin__btn:hover {
  background: rgba(255, 255, 255, 0.14);
}

.pin__btn--wide {
  font-family: var(--font-mono);
  font-size: 10px;
}

.pin__sep {
  width: 1px;
  height: 14px;
  margin: 0 2px;
  background: rgba(255, 255, 255, 0.16);
}

.pin__toast,
.pin__err {
  position: absolute;
  left: 50%;
  bottom: 8px;
  transform: translateX(-50%);
  max-width: 90%;
  padding: 4px 10px;
  border-radius: 6px;
  font-size: 12px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.pin__toast {
  background: rgba(0, 0, 0, 0.7);
  color: #fff;
}

.pin__err {
  background: rgba(120, 20, 20, 0.94);
  color: #fff;
}
</style>
