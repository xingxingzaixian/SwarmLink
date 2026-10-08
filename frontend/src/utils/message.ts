/**
 * 消息 content 的解析。
 *
 * 为什么单独抽成模块：这些字符串是【别人写、我们读】的数据 —— 它们已经
 * 落在数据库里、躺在对端的磁盘上，字段名改不动了。解析必须容忍多种历史
 * 写法，而且这份容忍值得单独测试（见 tests/message.test.ts），
 * 不该埋在组件的 computed 里靠肉眼看。
 *
 * 历史包袱的来历：
 *   1. snake_case —— job_id/file_name，当前后端（transfer:file_failed 修复后）写法；
 *   2. camelCase  —— 早期设计文档的写法；
 *   3. Go 字段名   —— JobID/FileName/Reason。Go 结构体曾经没有 json tag，
 *      json.Marshal 直接输出字段名，这些消息已经存在用户的历史记录里，
 *      前端若不认，它们就会以一段裸 JSON 的形式永久留在聊天框。
 */

export interface FileContent {
  jobId: string
  fileName: string
}

export interface FileFailedContent extends FileContent {
  reason: string
}

/** 从若干候选键里取第一个非空字符串。 */
function pick(obj: Record<string, unknown>, ...keys: string[]): string {
  for (const key of keys) {
    const v = obj[key]
    if (typeof v === 'string' && v !== '') return v
  }
  return ''
}

function parseObject(content: string): Record<string, unknown> | null {
  try {
    const parsed: unknown = JSON.parse(content)
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>
    }
  } catch {
    /* 坏数据：走下面的兜底 */
  }
  return null
}

/** 解析文件消息的 content（{job_id, file_name}）。 */
export function parseFileContent(content: string): FileContent {
  const obj = parseObject(content)
  if (!obj) return { jobId: '', fileName: '文件消息' }
  return {
    jobId: pick(obj, 'job_id', 'jobId', 'JobID'),
    fileName: pick(obj, 'file_name', 'fileName', 'FileName') || '未知文件'
  }
}

/** 解析文件失败消息的 content（{job_id, file_name, reason}）。 */
export function parseFileFailedContent(content: string): FileFailedContent {
  const obj = parseObject(content)
  if (!obj) return { jobId: '', fileName: '文件', reason: '' }
  return {
    jobId: pick(obj, 'job_id', 'jobId', 'JobID'),
    fileName: pick(obj, 'file_name', 'fileName', 'FileName') || '文件',
    reason: pick(obj, 'reason', 'Reason')
  }
}

/**
 * 会话列表的「最后一条消息」预览文案。
 *
 * 为什么必须按类型分派：图片与文件（含失败）消息的 content 是 JSON 信封，
 * 不是给人读的文字。直接透出去，左栏就会显示成
 * 「{"job_id":"01a0ed92-0204-…」—— 用户看到的是内部标识，
 * 而不是「对方发来了一个文件」。
 *
 * 只依赖最小结构（msgType + content），本模块因此保持零依赖；
 * Go 侧 internal/app/notify_app.go 的 previewOf 是同一份规则的镜像
 * （托盘气泡/悬停提示也要走它，否则托盘会单独漏出裸 JSON）。
 */
export function previewText(m: { msgType: string; content: string }): string {
  switch (m.msgType) {
    case 'image':
      return '[图片]'
    case 'file':
      return `[文件] ${parseFileContent(m.content).fileName}`
    case 'file_failed':
      return `[文件] ${parseFileFailedContent(m.content).fileName}（发送失败）`
    default:
      return m.content
  }
}
