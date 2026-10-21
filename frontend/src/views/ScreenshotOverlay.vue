<script setup lang="ts">
/**
 * 截屏覆盖层：一个独立的截屏工具。
 *
 * 这个组件跑在【覆盖层窗口】里 —— 那是后端开的一个铺满整块显示器的
 * 透明置顶窗口（每块屏一个）。它不依赖聊天界面，能力上自成一个工具：
 * 框选 → 调选区 → 标注 → 取色 → 完成。
 *
 * 完成后的去向仍然是「交回主窗口落进输入框草稿」（screenshot:ready 事件），
 * 这个外壳没有变；变的是选区与标注这两块的能力。
 *
 * 为什么选区与标注都在前端做（而不是在 Go 里）：
 *   - 同一套代码在 Windows / macOS / Linux 上通用，不用写三遍；
 *   - Canvas 2D 画矩形、箭头、文字、马赛克都是十几行的事，
 *     而在 Go 里做同样的事要动图像库。
 *
 * 坐标系（本组件里最重要的一件事）：
 *   - 画布的位图尺寸是【物理像素】（= 抓屏拿到的原始分辨率）；
 *   - 画布的 CSS 尺寸是【DIP 像素】（= 窗口尺寸）；
 *   - 两者之比就是 scaleFactor。
 *   所有内部坐标一律用物理像素存，鼠标坐标按 scaleFactor 换算回物理像素。
 *   混用两套坐标是这类界面最常见的「差几个像素」的来源。
 */
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { Clipboard } from '@wailsio/runtime'

import Icon from '../components/Icon.vue'
import { getApi } from '../api'
import { saveFile } from '../api/dialog'

// ---------------------------------------------------------------------------
// 类型
// ---------------------------------------------------------------------------

type Tool =
  | 'select'
  | 'rect'
  | 'ellipse'
  | 'line'
  | 'arrow'
  | 'pen'
  | 'highlight'
  | 'text'
  | 'mosaic'
  | 'number'
  | 'picker'

type Handle = 'nw' | 'n' | 'ne' | 'e' | 'se' | 's' | 'sw' | 'w'

interface Pt {
  x: number
  y: number
}

interface Rect {
  x: number
  y: number
  w: number
  h: number
}

interface Stroke {
  color: string
  width: number
}

type Annotation =
  | ({ kind: 'rect' } & Rect & Stroke)
  | ({ kind: 'ellipse' } & Rect & Stroke)
  | ({ kind: 'mosaic' } & Rect)
  | ({ kind: 'arrow' } & { x1: number; y1: number; x2: number; y2: number } & Stroke)
  | ({ kind: 'line' } & { x1: number; y1: number; x2: number; y2: number } & Stroke)
  | ({ kind: 'pen' } & { points: Pt[] } & Stroke)
  | ({ kind: 'highlight' } & { points: Pt[] } & Stroke)
  | ({ kind: 'text' } & { x: number; y: number; text: string; size: number; color: string })
  | ({ kind: 'number' } & { x: number; y: number; n: number; color: string })

/** 指针拖拽的状态机：新建选区 / 整体移动 / 控制点缩放。 */
type Drag =
  | { kind: 'select'; start: Pt }
  | { kind: 'move'; start: Pt; origin: Rect }
  | { kind: 'resize'; handle: Handle; start: Pt; origin: Rect }

// ---------------------------------------------------------------------------
// 状态
// ---------------------------------------------------------------------------

/** 本块显示器在整机里的序号（后端按序号抓屏与开窗）。 */
const displayIndex = Number(new URLSearchParams(window.location.search).get('display') ?? '0')

const bgRef = ref<HTMLCanvasElement | null>(null)
const annoRef = ref<HTMLCanvasElement | null>(null)
const magRef = ref<HTMLCanvasElement | null>(null)

/** 底图（抓屏得到的静态画面）。 */
const baseImg = ref<HTMLImageElement | null>(null)
/**
 * 干净底图（不含变暗遮罩）。
 *
 * 放大镜取色、马赛克取样都必须从这张图上取：bgRef 上叠着遮罩，
 * 在选区外取色会取到「被压暗的颜色」—— 那不是屏幕上的颜色。
 */
const clean = ref<HTMLCanvasElement | null>(null)
/** DIP → 物理像素的倍率。 */
const scale = ref(1)
/** 画布的物理像素尺寸。 */
const physW = ref(0)
const physH = ref(0)

/** 当前选区（物理像素）。null = 还没选。 */
const selection = ref<Rect | null>(null)
/** 正在进行的拖拽（null = 空闲）。 */
const drag = ref<Drag | null>(null)

/** 工具的真实状态（对外只暴露下面那个带 setter 的 computed）。 */
const toolState = ref<Tool>('select')

/**
 * 当前工具。默认为「选择」：先框选、再调整、再标注。
 *
 * 用带 setter 的 computed 而不是裸 ref：切换工具的入口有三个
 * （工具栏按钮、快捷键、脚本内部），把「标注工具需要一块画布」
 * 这条规则放进 setter，就不会漏掉任何一个入口。
 */
const tool = computed<Tool>({
  get: () => toolState.value,
  set: (t) => {
    toolState.value = t
    // 「点一下就该有东西」的两个工具（文字、序号）在没有选区时先把整屏选中：
    // 否则这一下点击会被当成「重新框选」，表现为「点了没反应」。
    // 形状类工具不做这一步 —— 它们的第一下本来就是框选，抢掉会打断习惯。
    if ((t === 'text' || t === 'number') && !selection.value) selectAll()
  }
})
/** 画笔颜色与粗细。 */
const color = ref('#ff3b30')
const strokeWidth = ref(3)

/** 已完成的标注（撤销栈）。 */
const annotations = ref<Annotation[]>([])
/** 被撤销掉的标注（重做栈）。撤销之后又画了新东西就清空。 */
const redoStack = ref<Annotation[]>([])
/** 正在画、还没松手的那个标注。 */
const draft = ref<Annotation | null>(null)

/** 文字输入框的位置与内容。 */
const textBox = ref<{ x: number; y: number } | null>(null)
const textValue = ref('')
const textInputRef = ref<HTMLInputElement | null>(null)

/** 放大镜的位置（DIP）与取到的色值。 */
const mag = ref<{ x: number; y: number } | null>(null)
const magHex = ref('')
const magCoord = ref('')
/** 指针是否落在选区内（决定光标形状）。 */
const hoverIn = ref(false)

/** 窗口尺寸（工具栏贴着选区摆放时要靠它判断上下有没有空间）。 */
const viewW = ref(window.innerWidth)
const viewH = ref(window.innerHeight)

/** 出错时的提示（覆盖在画面中间）。 */
const fatal = ref('')
/** 短暂的轻提示（「已保存」这类，不打断操作）。 */
const toast = ref('')
let toastTimer = 0

function notify(msg: string): void {
  toast.value = msg
  window.clearTimeout(toastTimer)
  toastTimer = window.setTimeout(() => {
    toast.value = ''
  }, 1800)
}

/** 是否已经把键盘焦点抢到本窗口（多屏时需要）。 */
let focused = false

/** 可选的标注颜色。 */
const COLORS = ['#ff3b30', '#ffcc00', '#34c759', '#0a84ff', '#ffffff', '#111111']
/** 可选线宽（物理像素）。 */
const WIDTHS = [2, 3, 5, 8]

// ---------------------------------------------------------------------------
// 模板用的派生值
// ---------------------------------------------------------------------------

/** 选区边框的样式（DOM 用 DIP，画布用物理像素 —— 这里做一次换算）。 */
const selBoxStyle = computed(() => {
  const s = selection.value
  if (!s) return {}
  const k = scale.value
  return {
    left: `${s.x / k}px`,
    top: `${s.y / k}px`,
    width: `${s.w / k}px`,
    height: `${s.h / k}px`
  }
})

/** 选区尺寸标签（给用户看截到多大，单位是物理像素 = 输出图的真实像素）。 */
const selSizeText = computed(() => {
  const s = selection.value
  if (!s) return ''
  return `${Math.round(s.w)} × ${Math.round(s.h)}`
})

/**
 * 文字输入框的位置。
 *
 * 贴边点击时往回收一点：输入框如果落在窗口外，用户看到的只是「点了没反应」。
 */
const textBoxStyle = computed(() => {
  const t = textBox.value
  if (!t) return {}
  const k = scale.value
  const x = Math.min(t.x / k, Math.max(0, viewW.value - 220))
  const y = Math.min(t.y / k, Math.max(0, viewH.value - 44))
  return { left: `${x}px`, top: `${y}px` }
})

/** 八个控制点的位置（DIP）。 */
const handles = computed(() => {
  const s = selection.value
  if (!s) return []
  const k = scale.value
  const x = s.x / k
  const y = s.y / k
  const w = s.w / k
  const h = s.h / k
  const list: Array<[Handle, number, number]> = [
    ['nw', x, y],
    ['n', x + w / 2, y],
    ['ne', x + w, y],
    ['e', x + w, y + h / 2],
    ['se', x + w, y + h],
    ['s', x + w / 2, y + h],
    ['sw', x, y + h],
    ['w', x, y + h / 2]
  ]
  return list.map(([k2, cx, cy]) => ({
    key: k2,
    style: { left: `${cx}px`, top: `${cy}px` }
  }))
})

/**
 * 工具栏位置：贴着选区下沿；下面放不下就翻到上沿。
 *
 * 固定贴在屏幕底部是最省事的，但选区常在屏幕下半部分 ——
 * 那时工具栏会盖住正要标注的内容。
 */
const barStyle = computed<Record<string, string | undefined>>(() => {
  const s = selection.value
  if (!s) return {}
  const k = scale.value
  const BAR = 48
  const GAP = 14
  const cx = (s.x + s.w / 2) / k
  const below = (s.y + s.h) / k + GAP
  const above = s.y / k - GAP - BAR
  const top = below + BAR <= viewH.value - 8 ? below : Math.max(8, above)
  const half = 300
  const left = Math.min(Math.max(cx, half), Math.max(half, viewW.value - half))
  return { left: `${left}px`, top: `${top}px`, bottom: 'auto' }
})

/**
 * 放大镜的位置样式。
 *
 * 用 v-show + 这段样式，而不是 v-if：放大镜一出现就要立刻往上画像素，
 * v-if 插入的 DOM 在本次事件里还没挂载，第一次移动会画出空白。
 */
const magStyle = computed<Record<string, string | undefined>>(() =>
  mag.value ? { left: `${mag.value.x}px`, top: `${mag.value.y}px` } : { display: 'none' }
)

/** 画布光标：选择工具且指针在选区内 → 移动；其余 → 十字。 */
const canvasCursor = computed(() => {
  if (drag.value?.kind === 'move') return 'move'
  if (drag.value) return 'crosshair'
  if (tool.value === 'select' && hoverIn.value) return 'move'
  return 'crosshair'
})

// ---------------------------------------------------------------------------
// 画布准备
// ---------------------------------------------------------------------------

function ctx2d(c: HTMLCanvasElement | null): CanvasRenderingContext2D | null {
  return c ? c.getContext('2d') : null
}

function normalizeRect(r: Rect): Rect {
  return {
    x: r.w < 0 ? r.x + r.w : r.x,
    y: r.h < 0 ? r.y + r.h : r.y,
    w: Math.abs(r.w),
    h: Math.abs(r.h)
  }
}

/**
 * 画底图并铺好遮罩。
 *
 * 遮罩的做法：先铺一层半透明黑，再用四个矩形把选区【留亮】。
 *
 * 为什么不直接铺黑再挖洞：挖洞（destination-out）会把底图一起挖穿，
 * 选区里露出的是窗口的透明背景 —— 也就是【实时桌面】，
 * 而不是抓屏那一刻的画面。用户正在拖动选区时，底下的内容可能已经变了
 * （比如鼠标悬停导致的高亮），那就不是他要截的东西了。
 */
function paintBase(): void {
  const canvas = bgRef.value
  const ctx = ctx2d(canvas)
  const img = baseImg.value
  if (!canvas || !ctx || !img) return

  ctx.clearRect(0, 0, physW.value, physH.value)
  ctx.drawImage(img, 0, 0, physW.value, physH.value)

  const sel = selection.value
  if (!sel) {
    // 还没选区：整屏变暗。
    ctx.fillStyle = 'rgba(0, 0, 0, 0.45)'
    ctx.fillRect(0, 0, physW.value, physH.value)
    return
  }

  // 选区外变暗（四个矩形围出选区）。
  ctx.fillStyle = 'rgba(0, 0, 0, 0.45)'
  const { x, y, w, h } = sel
  ctx.fillRect(0, 0, physW.value, y)
  ctx.fillRect(0, y + h, physW.value, physH.value - (y + h))
  ctx.fillRect(0, y, x, h)
  ctx.fillRect(x + w, y, physW.value - (x + w), h)
}

// ---------------------------------------------------------------------------
// 标注绘制
// ---------------------------------------------------------------------------

/** 画一个箭头（含箭头头部）。 */
function drawArrow(
  ctx: CanvasRenderingContext2D,
  x1: number,
  y1: number,
  x2: number,
  y2: number,
  color: string,
  width: number
): void {
  const angle = Math.atan2(y2 - y1, x2 - x1)
  const head = Math.max(12, width * 4)
  ctx.strokeStyle = color
  ctx.fillStyle = color
  ctx.lineWidth = width
  ctx.lineCap = 'round'
  ctx.beginPath()
  ctx.moveTo(x1, y1)
  ctx.lineTo(x2, y2)
  ctx.stroke()

  ctx.beginPath()
  ctx.moveTo(x2, y2)
  ctx.lineTo(x2 - head * Math.cos(angle - Math.PI / 7), y2 - head * Math.sin(angle - Math.PI / 7))
  ctx.lineTo(x2 - head * Math.cos(angle + Math.PI / 7), y2 - head * Math.sin(angle + Math.PI / 7))
  ctx.closePath()
  ctx.fill()
}

/** 折线（画笔 / 荧光笔共用一个描边过程）。 */
function strokePolyline(ctx: CanvasRenderingContext2D, points: Pt[]): void {
  if (points.length < 2) return
  ctx.beginPath()
  ctx.moveTo(points[0].x, points[0].y)
  for (const p of points) ctx.lineTo(p.x, p.y)
  ctx.stroke()
}

/**
 * 画马赛克。
 *
 * 做法是「把那一小块缩小再放大」：缩小丢掉细节，放大时关掉插值
 * 就成了马赛克。像素块的大小随选区大小走（固定 8 倍缩小在很小的
 * 选区上会糊成一片，在很大的选区上又不够花）。
 *
 * 取样源是 clean 而不是 bgRef：bgRef 上有遮罩层，若马赛克矩形有一半
 * 落在选区外，取样就会把「压暗后的像素」也一起放进来。
 */
function drawMosaic(ctx: CanvasRenderingContext2D, a: Extract<Annotation, { kind: 'mosaic' }>): void {
  const src = clean.value ?? bgRef.value
  if (!src) return
  const r = normalizeRect(a)
  const w = Math.max(1, r.w)
  const h = Math.max(1, r.h)
  const block = Math.max(4, Math.round(Math.max(w, h) / 24))
  const cols = Math.max(1, Math.ceil(w / block))
  const rows = Math.max(1, Math.ceil(h / block))

  const tmp = document.createElement('canvas')
  tmp.width = cols
  tmp.height = rows
  const tctx = tmp.getContext('2d')
  if (!tctx) return
  tctx.drawImage(src, r.x, r.y, w, h, 0, 0, cols, rows)

  ctx.save()
  ctx.imageSmoothingEnabled = false
  ctx.drawImage(tmp, 0, 0, cols, rows, r.x, r.y, w, h)
  ctx.restore()
}

/** 颜色是否偏亮（亮底上的数字要用深色，否则白字白底看不见）。 */
function isLight(hex: string): boolean {
  const m = /^#([0-9a-f]{6})$/i.exec(hex)
  if (!m) return false
  const n = parseInt(m[1], 16)
  const r = (n >> 16) & 255
  const g = (n >> 8) & 255
  const b = n & 255
  return (r * 299 + g * 587 + b * 114) / 1000 > 160
}

/** 画序号气泡（① 这类标记，用圆圈 + 数字，不依赖字体里的带圈字符）。 */
function drawBadge(
  ctx: CanvasRenderingContext2D,
  x: number,
  y: number,
  n: number,
  bg: string
): void {
  const r = 15
  ctx.beginPath()
  ctx.arc(x, y, r, 0, Math.PI * 2)
  ctx.fillStyle = bg
  ctx.fill()
  ctx.fillStyle = isLight(bg) ? '#111' : '#fff'
  ctx.font = '700 18px "Plus Jakarta Sans", system-ui, sans-serif'
  ctx.textAlign = 'center'
  ctx.textBaseline = 'middle'
  ctx.fillText(String(n), x, y + 1)
  // 复位：textAlign 是全局状态，不复位会让后面的文字标注偏移。
  ctx.textAlign = 'start'
  ctx.textBaseline = 'alphabetic'
}

/** 画一个标注。 */
function drawAnnotation(ctx: CanvasRenderingContext2D, a: Annotation): void {
  switch (a.kind) {
    case 'rect': {
      const r = normalizeRect(a)
      ctx.strokeStyle = a.color
      ctx.lineWidth = a.width
      ctx.lineJoin = 'round'
      ctx.strokeRect(r.x, r.y, r.w, r.h)
      break
    }
    case 'ellipse': {
      const r = normalizeRect(a)
      if (r.w < 1 || r.h < 1) return
      ctx.strokeStyle = a.color
      ctx.lineWidth = a.width
      ctx.beginPath()
      ctx.ellipse(r.x + r.w / 2, r.y + r.h / 2, r.w / 2, r.h / 2, 0, 0, Math.PI * 2)
      ctx.stroke()
      break
    }
    case 'arrow':
      drawArrow(ctx, a.x1, a.y1, a.x2, a.y2, a.color, a.width)
      break
    case 'line':
      ctx.strokeStyle = a.color
      ctx.lineWidth = a.width
      ctx.lineCap = 'round'
      ctx.beginPath()
      ctx.moveTo(a.x1, a.y1)
      ctx.lineTo(a.x2, a.y2)
      ctx.stroke()
      break
    case 'pen':
      ctx.strokeStyle = a.color
      ctx.lineWidth = a.width
      ctx.lineCap = 'round'
      ctx.lineJoin = 'round'
      strokePolyline(ctx, a.points)
      break
    case 'highlight':
      // 荧光笔：粗、半透明、圆头。半透明是重点 —— 它要「标出来」而不是
      // 「盖住」，下方的文字必须还能读。
      ctx.save()
      ctx.globalAlpha = 0.32
      ctx.strokeStyle = a.color
      ctx.lineWidth = Math.max(8, a.width * 5)
      ctx.lineCap = 'round'
      ctx.lineJoin = 'round'
      strokePolyline(ctx, a.points)
      ctx.restore()
      break
    case 'text': {
      ctx.fillStyle = a.color
      ctx.font = `600 ${a.size}px "Plus Jakarta Sans", system-ui, sans-serif`
      ctx.textBaseline = 'top'
      ctx.fillText(a.text, a.x, a.y)
      break
    }
    case 'mosaic':
      drawMosaic(ctx, a)
      break
    case 'number':
      drawBadge(ctx, a.x, a.y, a.n, a.color)
      break
  }
}

/** 重画整个标注层。 */
function repaintAnnotations(): void {
  const ctx = ctx2d(annoRef.value)
  if (!ctx) return
  ctx.clearRect(0, 0, physW.value, physH.value)
  for (const a of annotations.value) drawAnnotation(ctx, a)
  if (draft.value) drawAnnotation(ctx, draft.value)
}

// ---------------------------------------------------------------------------
// 放大镜与取色
// ---------------------------------------------------------------------------

/** 取向上的颜色（clean 底图，不含遮罩）。 */
function hexAt(p: Pt): string {
  const c = clean.value
  if (!c) return ''
  const ctx = c.getContext('2d', { willReadFrequently: true })
  if (!ctx) return ''
  const x = Math.max(0, Math.min(c.width - 1, Math.floor(p.x)))
  const y = Math.max(0, Math.min(c.height - 1, Math.floor(p.y)))
  const d = ctx.getImageData(x, y, 1, 1).data
  return '#' + [d[0], d[1], d[2]].map((v) => v.toString(16).padStart(2, '0')).join('')
}

/** 把放大镜画出来：中心那一格就是取到的像素。 */
function drawMag(p: Pt): void {
  const c = magRef.value
  const src = clean.value
  if (!c || !src) return
  const ctx = c.getContext('2d')
  if (!ctx) return
  const SRC = 11
  const half = (SRC - 1) / 2
  ctx.imageSmoothingEnabled = false
  ctx.clearRect(0, 0, c.width, c.height)
  ctx.drawImage(src, p.x - half, p.y - half, SRC, SRC, 0, 0, c.width, c.height)
  const cell = c.width / SRC
  ctx.strokeStyle = 'rgba(255, 255, 255, 0.9)'
  ctx.lineWidth = 1
  ctx.strokeRect(half * cell, half * cell, cell, cell)
}

/** 隐藏放大镜（指针移出覆盖层时）。 */
function hideMag(): void {
  mag.value = null
  hoverIn.value = false
}

/** 指针一动就更新放大镜位置、坐标与色值。 */
function updateMagnifier(e: PointerEvent, p: Pt): void {
  const SIZE = 116
  const GAP = 18
  let left = e.clientX + GAP
  let top = e.clientY + GAP
  if (left + SIZE > viewW.value - 8) left = e.clientX - SIZE - GAP
  if (top + SIZE + 22 > viewH.value - 8) top = e.clientY - SIZE - GAP - 22
  mag.value = { x: left, y: top }
  magCoord.value = `${Math.round(p.x)}, ${Math.round(p.y)}`
  magHex.value = hexAt(p)
  drawMag(p)
}

/**
 * 把文本写进系统剪贴板。失败返回 false（调用方负责提示）。
 *
 * 先走 Wails 的 Clipboard：它直接调平台 API，不像浏览器的
 * navigator.clipboard 那样受「安全上下文 / 文档焦点」约束 ——
 * 覆盖层是个无边框置顶窗口，后者在部分环境下会被拒。
 */
async function copyText(text: string): Promise<boolean> {
  try {
    await Clipboard.SetText(text)
    return true
  } catch {
    // 降级预览（非 Wails 环境）会走到下面两条路。
  }
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    // WebView 未授予剪贴板权限时退回 execCommand：
    // 覆盖层窗口此刻有焦点，这条路通常能成。
  }
  try {
    const el = document.createElement('textarea')
    el.value = text
    el.style.position = 'fixed'
    el.style.left = '-9999px'
    el.style.opacity = '0'
    document.body.appendChild(el)
    el.select()
    const ok = document.execCommand('copy')
    el.remove()
    return ok
  } catch {
    return false
  }
}

/**
 * 吸管：取色并【复制到剪贴板】。
 *
 * 取色的用途几乎都是「把这个色值拿到别处去用」（写 CSS、填设计稿），
 * 所以主动作是复制；顺带把它设为当前标注色，方便接着画。
 * 工具不切走：连点几个色块是常见用法。
 */
async function pickColor(p: Pt): Promise<void> {
  const hex = hexAt(p)
  if (!hex) {
    notify('这一处取不到颜色')
    return
  }
  color.value = hex
  const ok = await copyText(hex)
  notify(ok ? `已复制颜色 ${hex}` : `色值 ${hex}（复制失败）`)
}

// ---------------------------------------------------------------------------
// 鼠标交互
// ---------------------------------------------------------------------------

/** 屏幕坐标 → 画布的物理像素坐标。 */
function toPhys(e: PointerEvent): Pt {
  const canvas = annoRef.value
  if (!canvas) return { x: 0, y: 0 }
  const rect = canvas.getBoundingClientRect()
  return {
    x: (e.clientX - rect.left) * scale.value,
    y: (e.clientY - rect.top) * scale.value
  }
}

/** 限制在画布范围内。 */
function clamp(p: Pt): Pt {
  return {
    x: Math.max(0, Math.min(physW.value, p.x)),
    y: Math.max(0, Math.min(physH.value, p.y))
  }
}

/** 点是否落在已有选区里。 */
function inSelection(p: Pt): boolean {
  const s = selection.value
  if (!s) return false
  return p.x >= s.x && p.x <= s.x + s.w && p.y >= s.y && p.y <= s.y + s.h
}

/**
 * 第一次动手时把键盘焦点抢回本块屏。
 *
 * 多屏时焦点可能在别的屏上，不抢回来的话 Esc / Enter 会被送到那边去
 * （表现为「按了没反应」）。
 */
function stealFocus(): void {
  if (focused) return
  focused = true
  void getApi()
    .then((api) => api.focusOverlay(displayIndex))
    .catch(() => {
      /* 抢焦点失败不影响鼠标操作 */
    })
}

function startSelection(p: Pt): void {
  selection.value = { x: p.x, y: p.y, w: 0, h: 0 }
  drag.value = { kind: 'select', start: p }
  paintBase()
}

function onDown(e: PointerEvent): void {
  // 正在输入文字时：点别处等于「确认这段文字」，这一下点击不再做别的事。
  if (textBox.value) {
    commitText()
    return
  }
  annoRef.value?.setPointerCapture(e.pointerId)
  stealFocus()
  const p = clamp(toPhys(e))

  if (tool.value === 'picker') {
    void pickColor(p)
    return
  }

  if (tool.value === 'select') {
    // 在选区内按下 = 整体平移；在外按下 = 重新框选。
    if (selection.value && inSelection(p)) {
      drag.value = { kind: 'move', start: p, origin: { ...selection.value } }
      return
    }
    startSelection(p)
    return
  }

  // 标注工具：落点在选区内才画，否则当作重新框选。
  if (selection.value && inSelection(p)) {
    beginAnnotation(p)
    return
  }
  startSelection(p)
}

/** 按拖拽状态机更新选区。 */
function applyDrag(p: Pt): void {
  const d = drag.value
  if (!d) return
  if (d.kind === 'select') {
    selection.value = normalizeRect({ x: d.start.x, y: d.start.y, w: p.x - d.start.x, h: p.y - d.start.y })
  } else if (d.kind === 'move') {
    const o = d.origin
    const x = Math.max(0, Math.min(physW.value - o.w, o.x + (p.x - d.start.x)))
    const y = Math.max(0, Math.min(physH.value - o.h, o.y + (p.y - d.start.y)))
    selection.value = { x, y, w: o.w, h: o.h }
  } else {
    selection.value = resizeRect(d.origin, d.handle, p.x - d.start.x, p.y - d.start.y)
  }
  paintBase()
}

/**
 * 控制点缩放。
 *
 * 四条边各自独立：往左拖「西」边时，x 变、w 反向变，右下角不动 ——
 * 这是用户对「拖动一个角」的预期。最小保留 4 个物理像素，
 * 否则可以把选区拖没，之后既看不见也没法再抓住它。
 */
function resizeRect(o: Rect, h: Handle, dx: number, dy: number): Rect {
  let x = o.x
  let y = o.y
  let w = o.w
  let hh = o.h
  if (h.includes('w')) {
    const nx = Math.max(0, Math.min(o.x + o.w - 4, o.x + dx))
    x = nx
    w = o.x + o.w - nx
  }
  if (h.includes('e')) w = Math.max(4, Math.min(physW.value - o.x, o.w + dx))
  if (h.includes('n')) {
    const ny = Math.max(0, Math.min(o.y + o.h - 4, o.y + dy))
    y = ny
    hh = o.y + o.h - ny
  }
  if (h.includes('s')) hh = Math.max(4, Math.min(physH.value - o.y, o.h + dy))
  return { x, y, w, h: hh }
}

function onMove(e: PointerEvent): void {
  const p = clamp(toPhys(e))
  updateMagnifier(e, p)
  if (drag.value) {
    applyDrag(p)
    return
  }
  hoverIn.value = tool.value === 'select' && inSelection(p)
  if (draft.value) {
    updateDraft(p)
    repaintAnnotations()
  }
}

function onUp(): void {
  const d = drag.value
  drag.value = null
  if (d && d.kind === 'select') {
    const s = selection.value
    // 极小的框按误触处理：直接丢掉。
    if (!s || s.w < 4 || s.h < 4) selection.value = null
    paintBase()
    return
  }
  if (draft.value) finishAnnotation()
}

function onHandleDown(e: PointerEvent, h: Handle): void {
  if (textBox.value) return
  e.preventDefault()
  e.stopPropagation()
  ;(e.currentTarget as HTMLElement).setPointerCapture(e.pointerId)
  const s = selection.value
  if (!s) return
  stealFocus()
  drag.value = { kind: 'resize', handle: h, start: clamp(toPhys(e)), origin: { ...s } }
}

function onHandleMove(e: PointerEvent): void {
  const p = clamp(toPhys(e))
  updateMagnifier(e, p)
  applyDrag(p)
}

function onHandleUp(): void {
  drag.value = null
}

/** 双击选区内部 = 完成（与微信/QQ 的习惯一致）。 */
function onDoubleClick(e: MouseEvent): void {
  if (textBox.value) return
  if (tool.value !== 'select') return
  const canvas = annoRef.value
  if (!canvas) return
  const rect = canvas.getBoundingClientRect()
  const p = clamp({
    x: (e.clientX - rect.left) * scale.value,
    y: (e.clientY - rect.top) * scale.value
  })
  if (inSelection(p)) void finish()
}

// ---------------------------------------------------------------------------
// 标注的开始 / 更新 / 收尾
// ---------------------------------------------------------------------------

/** 下一个序号（按现有序号标注计数，撤销后自动回退）。 */
function nextNumber(): number {
  return annotations.value.filter((a) => a.kind === 'number').length + 1
}

function beginAnnotation(p: Pt): void {
  const base: Stroke = { color: color.value, width: strokeWidth.value }
  switch (tool.value) {
    case 'rect':
      draft.value = { kind: 'rect', x: p.x, y: p.y, w: 0, h: 0, ...base }
      break
    case 'ellipse':
      draft.value = { kind: 'ellipse', x: p.x, y: p.y, w: 0, h: 0, ...base }
      break
    case 'mosaic':
      draft.value = { kind: 'mosaic', x: p.x, y: p.y, w: 0, h: 0 }
      break
    case 'arrow':
      draft.value = { kind: 'arrow', x1: p.x, y1: p.y, x2: p.x, y2: p.y, ...base }
      break
    case 'line':
      draft.value = { kind: 'line', x1: p.x, y1: p.y, x2: p.x, y2: p.y, ...base }
      break
    case 'pen':
      draft.value = { kind: 'pen', points: [p], ...base }
      break
    case 'highlight':
      draft.value = { kind: 'highlight', points: [p], ...base }
      break
    case 'text':
      openTextBox(p)
      break
    case 'number':
      // 序号没有「拖」的过程：按一下就该落一个气泡。
      annotations.value.push({
        kind: 'number',
        x: p.x,
        y: p.y,
        n: nextNumber(),
        color: color.value
      })
      redoStack.value = []
      repaintAnnotations()
      break
    case 'select':
    case 'picker':
      break
  }
}

function updateDraft(p: Pt): void {
  const d = draft.value
  if (!d) return
  if (d.kind === 'rect' || d.kind === 'mosaic' || d.kind === 'ellipse') {
    d.w = p.x - d.x
    d.h = p.y - d.y
  } else if (d.kind === 'arrow' || d.kind === 'line') {
    d.x2 = p.x
    d.y2 = p.y
  } else if (d.kind === 'pen' || d.kind === 'highlight') {
    d.points.push(p)
  }
}

function finishAnnotation(): void {
  const d = draft.value
  draft.value = null
  if (!d) return
  // 忽略误触（点一下就松手）。
  if (d.kind === 'rect' || d.kind === 'mosaic' || d.kind === 'ellipse') {
    if (Math.abs(d.w) < 4 || Math.abs(d.h) < 4) return
    // 反向拖出来的框统一成左上角 + 正的宽高：合成时按矩形裁切，
    // 留着负宽高会在下游多一次「这里会不会是反的」的猜。
    const r = normalizeRect(d)
    d.x = r.x
    d.y = r.y
    d.w = r.w
    d.h = r.h
  } else if (d.kind === 'arrow' || d.kind === 'line') {
    if (Math.hypot(d.x2 - d.x1, d.y2 - d.y1) < 6) return
  } else if (d.kind === 'pen' || d.kind === 'highlight') {
    if (d.points.length < 2) return
  }
  annotations.value.push(d)
  redoStack.value = []
  repaintAnnotations()
}

// ---------------------------------------------------------------------------
// 文字
// ---------------------------------------------------------------------------

/**
 * 在点击处打开文字输入框。
 *
 * 焦点要抢两次：nextTick 负责「元素刚挂载」，setTimeout 负责「窗口焦点
 * 刚被 RPC 抢回来」—— 多屏时 focusOverlay 是异步的，它晚于微任务落地，
 * 会把刚拿到焦点的输入框再抢走，表现就是「点了有框、但打字没反应」。
 */
function openTextBox(p: Pt): void {
  textBox.value = { x: p.x, y: p.y }
  textValue.value = ''
  // 放大镜是「指针附近的浮层」，它盖住输入框就谁也打不了字。
  mag.value = null
  nextTick(() => {
    textInputRef.value?.focus({ preventScroll: true })
    window.setTimeout(() => textInputRef.value?.focus({ preventScroll: true }), 60)
  })
}

function commitText(): void {
  const box = textBox.value
  const text = textValue.value.trim()
  textBox.value = null
  if (!box || !text) return
  annotations.value.push({
    kind: 'text',
    x: box.x,
    y: box.y,
    text,
    color: color.value,
    // 字号按 DIP 定，落到物理像素上乘以缩放比。
    size: Math.max(16, Math.round(20 * scale.value))
  })
  redoStack.value = []
  repaintAnnotations()
}

function cancelText(): void {
  textBox.value = null
  textValue.value = ''
}

/**
 * 输入框里的回车。
 *
 * 必须先看 isComposing：中文输入法选词时按回车是「确认候选词」，
 * 不是「确认这段文字」。不判这一下，打一半的拼音会被直接提交。
 */
function onTextEnter(e: KeyboardEvent): void {
  if (e.isComposing || e.keyCode === 229) return
  e.preventDefault()
  e.stopPropagation()
  commitText()
}

/** 输入框里的 Esc：输入法正在组词时，这一次 Esc 归输入法。 */
function onTextEsc(e: KeyboardEvent): void {
  if (e.isComposing || e.keyCode === 229) return
  e.preventDefault()
  e.stopPropagation()
  cancelText()
}

// ---------------------------------------------------------------------------
// 工具栏动作
// ---------------------------------------------------------------------------

function undo(): void {
  const last = annotations.value.pop()
  if (!last) return
  redoStack.value.push(last)
  repaintAnnotations()
}

function redo(): void {
  const a = redoStack.value.pop()
  if (!a) return
  annotations.value.push(a)
  repaintAnnotations()
}

function selectAll(): void {
  selection.value = { x: 0, y: 0, w: physW.value, h: physH.value }
  paintBase()
}

function clearSelection(): void {
  selection.value = null
  paintBase()
}

async function cancel(): Promise<void> {
  try {
    const api = await getApi()
    await api.cancelOverlay()
  } catch {
    // 后端已经把窗口关了；这里失败无所谓。
  }
}

/**
 * 导出并压缩到能发出去的大小。
 *
 * 为什么要压：接收侧对单条图片消息有 512KB 上限（协议面的约束），
 * 而 4K 屏上随手框一块就可能编出 1MB 以上的 JPEG。做法是逐档降质量，
 * 都不够再按比例缩小 —— 宁可稍微糊一点，也不要让用户点了「完成」之后
 * 收到一句「图片过大」。
 *
 * 底图与标注层各取选区那一块贴到新画布上 —— 选区外的部分（变暗的）
 * 自然被裁掉。
 */
function composeFitted(sel: Rect): { data: string; width: number; height: number } | null {
  const bg = bgRef.value
  const anno = annoRef.value
  if (!bg || !anno) return null

  const limit = 460 * 1024
  const qualities = [0.92, 0.85, 0.75, 0.65, 0.5]
  const scales = [1, 0.85, 0.7, 0.55, 0.4]

  for (const s of scales) {
    const w = Math.max(1, Math.round(sel.w * s))
    const h = Math.max(1, Math.round(sel.h * s))
    const out = document.createElement('canvas')
    out.width = w
    out.height = h
    const ctx = out.getContext('2d')
    if (!ctx) return null
    ctx.imageSmoothingQuality = 'high'
    ctx.drawImage(bg, sel.x, sel.y, sel.w, sel.h, 0, 0, w, h)
    ctx.drawImage(anno, sel.x, sel.y, sel.w, sel.h, 0, 0, w, h)

    for (const q of qualities) {
      const url = out.toDataURL('image/jpeg', q)
      const data = url.slice(url.indexOf(',') + 1)
      // base64 的长度换算回字节：每 4 个字符 3 个字节。
      const bytes = Math.floor((data.length * 3) / 4)
      if (bytes <= limit) return { data, width: w, height: h }
    }
  }
  return null
}

/**
 * 合成无损原图（PNG）。
 *
 * 与 composeFitted 的分工：那条路是「发出去」（要压到 460KB 内），
 * 这条路是「存下来 / 贴到桌面」—— 这两件事都不该有画质损失。
 */
function composeFullPng(sel: Rect): { data: string; width: number; height: number } | null {
  const bg = bgRef.value
  const anno = annoRef.value
  if (!bg || !anno) return null
  const w = Math.max(1, Math.round(sel.w))
  const h = Math.max(1, Math.round(sel.h))
  const out = document.createElement('canvas')
  out.width = w
  out.height = h
  const ctx = out.getContext('2d')
  if (!ctx) return null
  ctx.drawImage(bg, sel.x, sel.y, sel.w, sel.h, 0, 0, w, h)
  ctx.drawImage(anno, sel.x, sel.y, sel.w, sel.h, 0, 0, w, h)
  const url = out.toDataURL('image/png')
  return { data: url.slice(url.indexOf(',') + 1), width: w, height: h }
}

/** 保存对话框里的默认文件名。 */
function suggestedName(ext: string): string {
  const d = new Date()
  const p = (n: number) => String(n).padStart(2, '0')
  return `截屏-${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}.${ext}`
}

/** 另存为：存无损 PNG。存完不退出会话 —— 用户往往还要再发一份。 */
async function saveToFile(): Promise<void> {
  const sel = selection.value
  if (!sel) return
  const full = composeFullPng(sel)
  if (!full) {
    fatal.value = '合成图片失败'
    return
  }
  const dest = await saveFile(suggestedName('png'))
  if (!dest) return
  try {
    const api = await getApi()
    await api.saveOverlay(full.data, dest)
    notify('已保存')
  } catch (e: any) {
    fatal.value = String(e?.message ?? e) || '保存失败'
  }
}

/**
 * 贴图：把这块画面钉在桌面上，然后收起覆盖层。
 *
 * 顺序不能反：PinOverlay 要用当前会话的显示器信息算窗口位置，
 * 会话一关就再也算不出来了。
 */
async function pinToScreen(): Promise<void> {
  const sel = selection.value
  if (!sel) return
  const full = composeFullPng(sel)
  if (!full) {
    fatal.value = '合成图片失败'
    return
  }
  try {
    const api = await getApi()
    await api.pinOverlay(
      displayIndex,
      full.data,
      full.width,
      full.height,
      Math.round(sel.x),
      Math.round(sel.y)
    )
    await api.cancelOverlay()
  } catch (e: any) {
    fatal.value = String(e?.message ?? e) || '贴图失败'
  }
}

async function finish(): Promise<void> {
  const sel = selection.value
  if (!sel || sel.w < 1 || sel.h < 1) return
  const fitted = composeFitted(sel)
  if (!fitted) {
    fatal.value = '这张图太大了，压到能发出去的大小失败了'
    return
  }
  try {
    const api = await getApi()
    await api.commitOverlay(fitted.data, fitted.width, fitted.height)
  } catch (e: any) {
    fatal.value = String(e?.message ?? e) || '提交截图失败'
  }
}

// ---------------------------------------------------------------------------
// 快捷键
// ---------------------------------------------------------------------------

/** 方向键微调选区（Shift 加速 10 倍）。 */
function nudge(dx: number, dy: number): void {
  const s = selection.value
  if (!s) return
  const x = Math.max(0, Math.min(physW.value - s.w, s.x + dx))
  const y = Math.max(0, Math.min(physH.value - s.h, s.y + dy))
  selection.value = { x, y, w: s.w, h: s.h }
  paintBase()
}

/** 工具快捷键（与主流截屏工具的习惯对齐）。 */
const TOOL_KEYS: Record<string, Tool> = {
  v: 'select',
  r: 'rect',
  e: 'ellipse',
  l: 'line',
  a: 'arrow',
  p: 'pen',
  h: 'highlight',
  t: 'text',
  m: 'mosaic',
  n: 'number',
  i: 'picker'
}

function onKey(e: KeyboardEvent): void {
  if (textBox.value) {
    // 焦点万一没落在输入框上（窗口焦点竞争），这一下算白按，但不能再
    // 静默吞掉后面的按键 —— 先把焦点找回来。
    if (e.target !== textInputRef.value) textInputRef.value?.focus({ preventScroll: true })
    return
  }
  const mod = e.ctrlKey || e.metaKey

  if (mod && e.key.toLowerCase() === 'z') {
    e.preventDefault()
    if (e.shiftKey) redo()
    else undo()
    return
  }
  if (mod && e.key.toLowerCase() === 'y') {
    e.preventDefault()
    redo()
    return
  }
  if (mod && e.key.toLowerCase() === 'a') {
    e.preventDefault()
    selectAll()
    return
  }
  if (mod && e.key.toLowerCase() === 's') {
    e.preventDefault()
    void saveToFile()
    return
  }
  if (mod || e.altKey) return

  // F3 贴图：与常见截屏工具的习惯一致（按下就把图钉在桌面上）。
  if (e.key === 'F3') {
    e.preventDefault()
    void pinToScreen()
    return
  }

  if (e.key === 'Escape') {
    e.preventDefault()
    // 有选区 → 先退选区（还能重新框）；没有选区 → 退出整个截屏。
    if (selection.value) clearSelection()
    else void cancel()
    return
  }
  if (e.key === 'Enter') {
    e.preventDefault()
    void finish()
    return
  }
  const step = e.shiftKey ? 10 : 1
  if (e.key === 'ArrowLeft') {
    e.preventDefault()
    nudge(-step, 0)
    return
  }
  if (e.key === 'ArrowRight') {
    e.preventDefault()
    nudge(step, 0)
    return
  }
  if (e.key === 'ArrowUp') {
    e.preventDefault()
    nudge(0, -step)
    return
  }
  if (e.key === 'ArrowDown') {
    e.preventDefault()
    nudge(0, step)
    return
  }

  const t = TOOL_KEYS[e.key.toLowerCase()]
  if (t) {
    e.preventDefault()
    tool.value = t
  }
}

function onResize(): void {
  viewW.value = window.innerWidth
  viewH.value = window.innerHeight
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

/** 把底图拷一份到离屏画布，供取色与马赛克取样。 */
function buildClean(img: HTMLImageElement): void {
  const c = document.createElement('canvas')
  c.width = physW.value
  c.height = physH.value
  const ctx = c.getContext('2d', { willReadFrequently: true })
  if (!ctx) return
  ctx.drawImage(img, 0, 0, physW.value, physH.value)
  clean.value = c
}

onMounted(async () => {
  window.addEventListener('keydown', onKey)
  window.addEventListener('resize', onResize)
  try {
    const api = await getApi()
    const img = await api.getOverlayImage(displayIndex)
    scale.value = img.scaleFactor || 1
    physW.value = img.width
    physH.value = img.height

    const el = new Image()
    await new Promise<void>((resolve, reject) => {
      el.onload = () => resolve()
      el.onerror = () => reject(new Error('底图解码失败'))
      el.src = `data:${img.mime};base64,${img.dataBase64}`
    })
    baseImg.value = el

    for (const c of [bgRef.value, annoRef.value]) {
      if (!c) continue
      c.width = physW.value
      c.height = physH.value
    }
    buildClean(el)
    paintBase()
    repaintAnnotations()
  } catch (e: any) {
    fatal.value = String(e?.message ?? e) || '截屏失败'
  } finally {
    // 无论成功失败都要让窗口显示出来 —— 否则用户看到的是一片死寂，
    // 错误提示也无处可看。
    try {
      const api = await getApi()
      await api.overlayReady(displayIndex)
    } catch {
      // 后端没准备好就算了。
    }
  }
})

onBeforeUnmount(() => {
  window.clearTimeout(toastTimer)
  window.removeEventListener('keydown', onKey)
  window.removeEventListener('resize', onResize)
})
</script>

<template>
  <div class="overlay" @pointerleave="hideMag">
    <!-- 底图层：抓屏那一刻的画面 + 选区外的变暗 -->
    <canvas ref="bgRef" class="layer" />

    <!-- 交互层：标注画在这上面 -->
    <canvas
      ref="annoRef"
      class="layer layer--top"
      :style="{ cursor: canvasCursor }"
      @pointerdown="onDown"
      @pointermove="onMove"
      @pointerup="onUp"
      @pointercancel="onUp"
      @dblclick="onDoubleClick"
    />

    <!-- 选区边框与八个控制点（用 DOM 画：边框、尺寸标签、缩放手柄都是它更省事） -->
    <div v-if="selection" class="sel" :style="selBoxStyle">
      <div class="sel__tag">{{ selSizeText }}</div>
    </div>
    <template v-if="selection && tool === 'select'">
      <div
        v-for="h in handles"
        :key="h.key"
        class="handle"
        :class="`handle--${h.key}`"
        :style="h.style"
        @pointerdown="onHandleDown($event, h.key)"
        @pointermove="onHandleMove"
        @pointerup="onHandleUp"
        @pointercancel="onHandleUp"
      />
    </template>

    <!-- 文字输入 -->
    <input
      v-if="textBox"
      ref="textInputRef"
      v-model="textValue"
      class="text-input"
      :style="textBoxStyle"
      placeholder="输入文字后回车"
      autocomplete="off"
      spellcheck="false"
      @keydown.enter="onTextEnter"
      @keydown.esc="onTextEsc"
      @pointerdown.stop
      @click.stop
    />

    <!-- 放大镜 + 取色：指针旁显示 11×11 像素的放大图与色值 -->
    <div v-show="mag" class="mag" :style="magStyle">
      <canvas ref="magRef" class="mag__canvas" width="116" height="116" />
      <div class="mag__meta">
        <span class="mag__hex">
          <i class="mag__dot" :style="{ background: magHex || 'transparent' }" />
          {{ magHex }}
        </span>
        <span class="mag__coord">{{ magCoord }}</span>
      </div>
    </div>

    <!-- 工具栏 -->
    <div class="bar" :style="barStyle">
      <div class="bar__group">
        <button
          class="bar__btn"
          :class="{ 'is-on': tool === 'select' }"
          title="选择 / 调整选区 (V)"
          @click="tool = 'select'"
        >
          <Icon name="cursor" :size="16" />
        </button>
        <button
          class="bar__btn"
          :class="{ 'is-on': tool === 'rect' }"
          title="矩形 (R)"
          @click="tool = 'rect'"
        >
          <Icon name="square" :size="16" />
        </button>
        <button
          class="bar__btn"
          :class="{ 'is-on': tool === 'ellipse' }"
          title="椭圆 (E)"
          @click="tool = 'ellipse'"
        >
          <Icon name="ellipse" :size="16" />
        </button>
        <button
          class="bar__btn"
          :class="{ 'is-on': tool === 'arrow' }"
          title="箭头 (A)"
          @click="tool = 'arrow'"
        >
          <Icon name="arrowUpRight" :size="16" />
        </button>
        <button
          class="bar__btn"
          :class="{ 'is-on': tool === 'line' }"
          title="直线 (L)"
          @click="tool = 'line'"
        >
          <Icon name="line" :size="16" />
        </button>
        <button
          class="bar__btn"
          :class="{ 'is-on': tool === 'pen' }"
          title="画笔 (P)"
          @click="tool = 'pen'"
        >
          <Icon name="edit" :size="16" />
        </button>
        <button
          class="bar__btn"
          :class="{ 'is-on': tool === 'highlight' }"
          title="荧光笔 (H)"
          @click="tool = 'highlight'"
        >
          <Icon name="highlight" :size="16" />
        </button>
        <button
          class="bar__btn"
          :class="{ 'is-on': tool === 'text' }"
          title="文字 (T)"
          @click="tool = 'text'"
        >
          <Icon name="text" :size="16" />
        </button>
        <button
          class="bar__btn"
          :class="{ 'is-on': tool === 'number' }"
          title="序号标记 (N)"
          @click="tool = 'number'"
        >
          <Icon name="number" :size="16" />
        </button>
        <button
          class="bar__btn"
          :class="{ 'is-on': tool === 'mosaic' }"
          title="马赛克 (M)"
          @click="tool = 'mosaic'"
        >
          <Icon name="grid" :size="16" />
        </button>
        <button
          class="bar__btn"
          :class="{ 'is-on': tool === 'picker' }"
          title="取色 (I)"
          @click="tool = 'picker'"
        >
          <Icon name="eyedropper" :size="16" />
        </button>
      </div>

      <div class="bar__sep" />

      <div class="bar__group">
        <button
          v-for="c in COLORS"
          :key="c"
          class="bar__swatch"
          :class="{ 'is-on': color === c }"
          :style="{ background: c }"
          :title="`颜色 ${c}`"
          @click="color = c"
        />
      </div>

      <div class="bar__sep" />

      <div class="bar__group">
        <button
          v-for="w in WIDTHS"
          :key="w"
          class="bar__width"
          :class="{ 'is-on': strokeWidth === w }"
          :title="`线宽 ${w}px`"
          @click="strokeWidth = w"
        >
          <i :style="{ width: `${w + 2}px`, height: `${w + 2}px` }" />
        </button>
      </div>

      <div class="bar__sep" />

      <div class="bar__group">
        <button class="bar__btn" title="撤销 (Ctrl+Z)" :disabled="!annotations.length" @click="undo">
          <Icon name="rotateCcw" :size="16" />
        </button>
        <button class="bar__btn" title="重做 (Ctrl+Shift+Z)" :disabled="!redoStack.length" @click="redo">
          <Icon name="redo" :size="16" />
        </button>
        <button class="bar__btn" title="整块屏幕 (Ctrl+A)" @click="selectAll">
          <Icon name="maximize" :size="16" />
        </button>
      </div>

      <div class="bar__sep" />

      <!-- 输出：存成文件 / 钉在桌面上。两个都不进聊天，与「完成」并列。 -->
      <div class="bar__group">
        <button class="bar__btn" title="另存为 (Ctrl+S)" :disabled="!selection" @click="saveToFile">
          <Icon name="download" :size="16" />
        </button>
        <button class="bar__btn" title="贴图 (F3)" :disabled="!selection" @click="pinToScreen">
          <Icon name="pushpin" :size="16" />
        </button>
      </div>

      <div class="bar__sep" />

      <div class="bar__group">
        <button class="bar__btn" title="取消 (Esc)" @click="cancel">
          <Icon name="x" :size="16" />
        </button>
        <button class="bar__btn bar__btn--primary" title="完成 (Enter)" @click="finish">
          <Icon name="check" :size="16" />
        </button>
      </div>
    </div>

    <!-- 还没选区时的提示 -->
    <div v-if="!selection && !fatal" class="hint">
      按住左键拖动框选区域
      <span class="hint__keys">Enter 完成 · F3 贴图 · Ctrl+S 保存 · Esc 退出</span>
    </div>

    <div v-if="toast" class="toast">{{ toast }}</div>

    <div v-if="fatal" class="fatal">{{ fatal }}</div>
  </div>
</template>

<style scoped>
.overlay {
  position: relative;
  width: 100%;
  height: 100%;
  overflow: hidden;
  /* 覆盖层不需要选中文本；双击也不该选中。 */
  user-select: none;
  cursor: crosshair;
}

.layer {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  display: block;
}

.layer--top {
  z-index: 2;
}

/* 选区边框：只画一圈亮边，选区内部不遮挡（内部要保持清晰）。 */
.sel {
  position: absolute;
  z-index: 3;
  border: 1px solid #0af;
  pointer-events: none;
}

.sel__tag {
  position: absolute;
  left: 0;
  top: -22px;
  padding: 2px 6px;
  border-radius: 4px;
  background: #0af;
  color: #04252c;
  font-family: var(--font-mono);
  font-size: 11px;
  white-space: nowrap;
}

/* 八个控制点：3px 视觉尺寸太小抓不住，用 10px 的热区 + 中间的实心块。 */
.handle {
  position: absolute;
  z-index: 4;
  width: 10px;
  height: 10px;
  margin: -5px 0 0 -5px;
  border: 1px solid #0af;
  border-radius: 2px;
  background: #fff;
  touch-action: none;
}

.handle--nw,
.handle--se {
  cursor: nwse-resize;
}

.handle--ne,
.handle--sw {
  cursor: nesw-resize;
}

.handle--n,
.handle--s {
  cursor: ns-resize;
}

.handle--e,
.handle--w {
  cursor: ew-resize;
}

.text-input {
  position: absolute;
  /* 高于工具栏（6）：贴边输入时不能被工具栏压住。 */
  z-index: 7;
  min-width: 160px;
  padding: 4px 8px;
  border: 1px solid #0af;
  border-radius: 4px;
  background: rgba(0, 0, 0, 0.85);
  color: #fff;
  font-family: var(--font-body);
  /* 与落到画布上的字号一致（20 DIP），输入时看到的就是最终效果。 */
  font-size: 20px;
  outline: none;
  /* 覆盖层的 user-select: none 不能传染给输入框，否则改不了已输入的字符。 */
  user-select: text;
  cursor: text;
}

/* 放大镜：贴在指针右下方，压在所有层之上；不接收鼠标事件。 */
.mag {
  position: absolute;
  z-index: 8;
  pointer-events: none;
  border-radius: 6px;
  overflow: hidden;
  background: rgba(18, 18, 22, 0.92);
  box-shadow: 0 6px 20px rgba(0, 0, 0, 0.5);
}

.mag__canvas {
  display: block;
  width: 116px;
  height: 116px;
}

.mag__meta {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 3px 6px;
  font-family: var(--font-mono);
  font-size: 10px;
  color: rgba(255, 255, 255, 0.85);
}

.mag__hex {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}

.mag__dot {
  display: inline-block;
  width: 9px;
  height: 9px;
  border: 1px solid rgba(255, 255, 255, 0.5);
  border-radius: 2px;
}

.mag__coord {
  opacity: 0.65;
}

/* 工具栏：没有选区时贴在底部居中，有选区时贴着选区（见 barStyle）。 */
.bar {
  position: absolute;
  z-index: 6;
  left: 50%;
  bottom: 28px;
  transform: translateX(-50%);
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 6px 10px;
  border-radius: 10px;
  background: rgba(24, 24, 28, 0.94);
  box-shadow: 0 8px 28px rgba(0, 0, 0, 0.45);
  white-space: nowrap;
}

.bar__group {
  display: flex;
  align-items: center;
  gap: 4px;
}

.bar__sep {
  width: 1px;
  height: 20px;
  background: rgba(255, 255, 255, 0.16);
}

.bar__btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  border: none;
  border-radius: 6px;
  background: transparent;
  color: rgba(255, 255, 255, 0.86);
  cursor: pointer;
}

.bar__btn:hover:not(:disabled) {
  background: rgba(255, 255, 255, 0.12);
}

.bar__btn:disabled {
  opacity: 0.32;
  cursor: default;
}

.bar__btn.is-on {
  background: #0af;
  color: #04252c;
}

.bar__btn--primary {
  background: #0af;
  color: #04252c;
}

.bar__btn--primary:hover {
  background: #3ff;
}

.bar__swatch {
  width: 18px;
  height: 18px;
  border: 2px solid transparent;
  border-radius: 50%;
  cursor: pointer;
  padding: 0;
}

.bar__swatch.is-on {
  border-color: #fff;
}

/* 线宽：按钮里放一个实心圆点，点的大小就是线宽（不用读数字）。 */
.bar__width {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  border: none;
  border-radius: 5px;
  background: transparent;
  cursor: pointer;
}

.bar__width:hover {
  background: rgba(255, 255, 255, 0.12);
}

.bar__width.is-on {
  background: rgba(0, 170, 255, 0.22);
}

.bar__width i {
  display: block;
  border-radius: 50%;
  background: rgba(255, 255, 255, 0.9);
}

.hint {
  position: absolute;
  z-index: 4;
  left: 50%;
  top: 50%;
  transform: translate(-50%, -50%);
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  padding: 10px 18px;
  border-radius: 8px;
  background: rgba(0, 0, 0, 0.6);
  color: #fff;
  font-size: 14px;
  pointer-events: none;
}

.hint__keys {
  font-family: var(--font-mono);
  font-size: 11px;
  opacity: 0.7;
}

/* 轻提示：贴在工具栏上方一点，不遮选区。 */
.toast {
  position: absolute;
  z-index: 9;
  left: 50%;
  bottom: 88px;
  transform: translateX(-50%);
  padding: 6px 14px;
  border-radius: 7px;
  background: rgba(20, 90, 40, 0.94);
  color: #fff;
  font-size: 13px;
  pointer-events: none;
}

.fatal {
  position: absolute;
  z-index: 9;
  left: 50%;
  top: 50%;
  transform: translate(-50%, -50%);
  max-width: 60%;
  padding: 12px 18px;
  border-radius: 8px;
  background: rgba(120, 20, 20, 0.94);
  color: #fff;
  font-size: 14px;
  text-align: center;
}
</style>
