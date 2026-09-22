/**
 * 图片相关的前端工具。
 *
 * 图片消息的 content 是 Go 侧写下的 JSON 信封（见 message.InlineImage）。
 * 解析失败【不能抛错】：历史消息里可能存在旧版本或损坏的数据，
 * 一条坏消息不该让整块聊天区崩掉。
 */
export interface ImageContent {
  v: number
  mime: string
  w: number
  h: number
  b64: string
}

const IMAGE_EXT = ['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp']

/**
 * 按扩展名粗判是否图片。
 *
 * 只用于交互分流（拖拽时决定走图片还是文件传输），
 * 真实格式判定以后端的魔数嗅探为准。
 */
export function isImagePath(path: string): boolean {
  const ext = path.split('.').pop()?.toLowerCase() ?? ''
  return IMAGE_EXT.includes(ext)
}

/** 允许拼进 data URL 的 MIME 白名单。 */
const IMAGE_MIME = ['image/jpeg', 'image/png', 'image/gif', 'image/webp', 'image/bmp']

export function parseImageContent(content: string): ImageContent | null {
  try {
    const obj = JSON.parse(content) as Partial<ImageContent>
    if (!obj || typeof obj.b64 !== 'string' || !obj.b64) return null
    // mime 会被直接拼进 data URL：不收口就等于让消息内容决定 src 的 scheme。
    // 不在白名单里的（含缺失、非字符串、被篡改）一律回落到 jpeg。
    const mime =
      typeof obj.mime === 'string' && IMAGE_MIME.includes(obj.mime) ? obj.mime : 'image/jpeg'
    return {
      v: obj.v ?? 1,
      mime,
      w: Number(obj.w) || 0,
      h: Number(obj.h) || 0,
      b64: obj.b64
    }
  } catch {
    return null
  }
}

/** 拼成 <img src> 可直接使用的 data URL。 */
export function imageSrc(img: ImageContent): string {
  return `data:${img.mime};base64,${img.b64}`
}

/**
 * 按 max 上限与原始宽高比算出展示尺寸。
 *
 * 缺省或异常的宽高退回方形：0 会让元素塌陷成一条线，
 * 比「比例不准」难看得多。
 */
export function imageBox(img: ImageContent, max: number): { width: number; height: number } {
  const w = img.w > 0 ? img.w : 1
  const h = img.h > 0 ? img.h : 1
  const ratio = w / h
  return ratio >= 1
    ? { width: max, height: Math.max(1, Math.round(max / ratio)) }
    : { width: Math.max(1, Math.round(max * ratio)), height: max }
}

/**
 * 另存为时的默认文件名（扩展名按 mime 推导）。
 *
 * 扩展名必须与真实内容一致：原样保留的 webp 若被存成 .jpg，
 * 系统会用「看扩展名」的程序去打开它，显示失败甚至报损坏。
 */
export function defaultImageName(mime: string): string {
  const EXT: Record<string, string> = {
    'image/png': 'png',
    'image/gif': 'gif',
    'image/webp': 'webp',
    'image/bmp': 'bmp'
  }
  return `swarmlink-image.${EXT[mime] ?? 'jpg'}`
}
