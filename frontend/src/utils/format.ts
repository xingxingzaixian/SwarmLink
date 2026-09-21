/**
 * 展示层格式化。
 *
 * 集中在一处的原因：同一个时间戳在左栏（紧凑）、气泡（只到分钟）、
 * 资料卡（完整）里是三套写法，散落各处必然出现「同一时刻两种显示」。
 */

const WEEK = ['日', '一', '二', '三', '四', '五', '六']

function pad(n: number): string {
  return String(n).padStart(2, '0')
}

function startOfDay(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
}

/** 今天 / 昨天 / 更早，用于「按天分组」。 */
function dayDiff(ms: number): number {
  const today = startOfDay(new Date())
  return Math.round((today - startOfDay(new Date(ms))) / 86_400_000)
}

/** 气泡与消息的时间：HH:MM。 */
export function clockTime(ms: number): string {
  if (!ms) return ''
  const d = new Date(ms)
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/**
 * 列表行右侧的时间（QQ 的收敛规则）：
 * 今天给时分，昨天给「昨天」，今年给月日，跨年给年月日。
 */
export function listTime(ms: number): string {
  if (!ms) return ''
  const d = new Date(ms)
  const diff = dayDiff(ms)
  if (diff <= 0) return clockTime(ms)
  if (diff === 1) return '昨天'
  if (d.getFullYear() === new Date().getFullYear()) {
    return `${d.getMonth() + 1}月${d.getDate()}日`
  }
  return `${d.getFullYear()}/${pad(d.getMonth() + 1)}/${pad(d.getDate())}`
}

/** 聊天区的日期分隔标签。 */
export function dateLabel(ms: number): string {
  const d = new Date(ms)
  const diff = dayDiff(ms)
  if (diff <= 0) return '今天'
  if (diff === 1) return '昨天'
  const base = `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日`
  return `${base} 周${WEEK[d.getDay()]}`
}

/** 资料卡里的完整时间。 */
export function fullTime(ms: number): string {
  if (!ms) return '—'
  const d = new Date(ms)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${clockTime(ms)}`
}

/** 「xx 前」：用于最后活跃时间这类相对时间。 */
export function ago(ms: number): string {
  if (!ms) return '—'
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000))
  if (s < 60) return '刚刚'
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`
  if (s < 86_400) return `${Math.floor(s / 3600)} 小时前`
  return `${Math.floor(s / 86_400)} 天前`
}

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB']

/** 文件大小。B 不带小数，其余保留 1 位（10 以上取整）。 */
export function fileSize(bytes: number): string {
  if (!bytes || bytes < 0) return '—'
  let v = bytes
  let i = 0
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i === 0 ? 0 : v >= 10 ? 0 : 1)} ${UNITS[i]}`
}

/** 速率：字节/秒 → 「3.4 MB/s」。 */
export function speedText(bps?: number): string {
  if (!bps || bps <= 0) return ''
  return `${fileSize(bps)}/s`
}

/** 预计剩余时间。超过 1 小时就不显示了 —— 那个精度没有意义。 */
export function etaText(ms?: number): string {
  if (ms === undefined || ms <= 0 || !Number.isFinite(ms)) return ''
  const s = Math.round(ms / 1000)
  if (s < 60) return `剩余 ${s} 秒`
  const m = Math.floor(s / 60)
  if (m < 60) return `剩余 ${m} 分 ${s % 60} 秒`
  return ''
}

/** 从路径里取文件名（兼容 Windows 与 POSIX 分隔符）。 */
export function baseName(path: string): string {
  return path.split(/[\\/]/).filter(Boolean).pop() ?? path
}

export interface Tone {
  text: string
  /** 对应 .tag 的修饰类：ok / warn / err / brand / 空。 */
  cls: string
  /** 是否为「已经结束、不会再动」的状态。 */
  final: boolean
}

/**
 * 传输状态 → 展示文案与色调。
 *
 * 集中一处的理由：状态字符串来自 domain 层，UI 若各处自己 switch，
 * 新增一个状态时必然有地方漏改，表现为「某个任务显示原始英文状态」。
 */
export function transferStatus(status: string): Tone {
  switch (status) {
    case 'done':
      return { text: '已完成', cls: 'ok', final: true }
    case 'failed':
      return { text: '失败', cls: 'err', final: true }
    case 'cancelled':
      return { text: '已取消', cls: '', final: true }
    case 'verifying':
      return { text: '校验中', cls: 'warn', final: false }
    case 'paused':
      return { text: '已暂停', cls: 'warn', final: false }
    case 'queued':
      return { text: '排队中', cls: '', final: false }
    // 任务已建但还没开始发块（等并发额度 / 等对端位图）
    case 'idle':
    case 'meta_exchange':
      return { text: '协商中', cls: 'brand', final: false }
    default:
      return { text: '传输中', cls: 'brand', final: false }
  }
}

/** 是不是「发送」方向。domain 用 send / recv。 */
export function isSending(direction: string): boolean {
  return direction !== 'recv'
}
