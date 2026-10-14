/**
 * 共享文件弹框的纯逻辑：分组、过滤、路径换算。
 *
 * 抽出来而不是写在组件里，是因为这几条规则都有「不报错的错法」：
 * 分组错了会让某条共享凭空消失、层级算错了会把目录树读成平铺、
 * 路径拼错了会让「打开所在目录」定位到别处。纯函数才能把它们钉住。
 */
import type { Share, ShareItem } from '../api/types'

/** 左树的一个分组：我的共享，或某个联系人的共享。 */
export interface ShareGroup {
  /** 'mine' 或联系人 node_id（做 key 用）。 */
  key: string
  label: string
  /** 该分组的共享条数（组头显示）。 */
  count: number
  /** 「发布者此刻可达」；我的共享没有这个概念，为 null。 */
  online: boolean | null
  shares: Share[]
}

/**
 * 按「我的共享 / 各联系人」把共享分成两组。
 *
 * 三条刻意的规则：
 *  1. 只有【确实发布了共享】的联系人才成组 —— 否则左侧会列出一串空联系人；
 *  2. 联系人按名字排序，保证每次渲染的顺序一致（否则列表会自己跳）；
 *  3. 我的共享恒在第一位 —— 它是用户最常看的那一组。
 */
export function buildShareGroups(
  shares: Share[],
  peerNames: Record<string, string> = {}
): ShareGroup[] {
  const mine: Share[] = []
  const byPeer = new Map<string, Share[]>()
  for (const s of shares) {
    if (s.mine) {
      mine.push(s)
      continue
    }
    const list = byPeer.get(s.ownerId) ?? []
    list.push(s)
    byPeer.set(s.ownerId, list)
  }

  const groups: ShareGroup[] = [
    { key: 'mine', label: '我的共享', count: mine.length, online: null, shares: mine }
  ]

  const peers = [...byPeer.entries()].map(([ownerId, list]) => ({
    key: ownerId,
    // 名字取不到时退到短 id：摘要里带了 ownerName，但它可能是空的（旧版本）。
    label: peerNames[ownerId] || list[0]?.ownerName || shortId(ownerId),
    // 只要有一条在线就算这个人可达：同一节点的摘要同源，单条离线的差异
    // 不该把整个联系人显示成灰的。
    online: list.some((s) => s.online),
    shares: list
  }))
  peers.sort((a, b) => a.label.localeCompare(b.label, 'zh-Hans-CN'))

  for (const p of peers) {
    groups.push({ key: p.key, label: p.label, count: p.shares.length, online: p.online, shares: p.shares })
  }
  return groups
}

/**
 * 模糊搜索右表。
 *
 * 名称与相对路径都要命中：用户可能只记得「它在 docs 下」，
 * 而只比名称的话那种记忆方式就永远找不到东西。
 */
export function filterShareItems(items: ShareItem[], query: string): ShareItem[] {
  const q = query.trim().toLowerCase()
  if (!q) return items
  return items.filter(
    (it) => it.path.toLowerCase().includes(q) || it.name.toLowerCase().includes(q)
  )
}

/** 路径在目录树里的层级（0 = 共享根下的第一层），用于左侧缩进。 */
export function pathDepth(path: string): number {
  const parts = path.split('/').filter(Boolean)
  return Math.max(0, parts.length - 1)
}

/** 路径里除文件名之外的部分（表格里灰显，让目录结构一眼可读）。 */
export function pathDir(path: string): string {
  const i = path.lastIndexOf('/')
  return i < 0 ? '' : path.slice(0, i + 1)
}

/**
 * 把条目拼成可用的绝对路径（只对【本机发布的共享】有意义：只有我们知道根在哪）。
 *
 * 分隔符跟着根走：Windows 根用反斜杠、POSIX 根用斜杠。不做这层换算的话，
 * 「打开所在目录」会拿到 `D:\共享\周报模板/docs/a.txt` 这种混着两种分隔符的
 * 路径 —— 文件管理器多半能认，但复制出来给别人就未必了。
 */
export function absolutePath(rootPath: string, itemPath: string): string {
  const trimmed = rootPath.replace(/[\\/]+$/, '')
  if (!trimmed) return itemPath
  const sep = trimmed.includes('\\') ? '\\' : '/'
  return trimmed + sep + (sep === '\\' ? itemPath.replace(/\//g, '\\') : itemPath)
}

/** 节点的短标识（名字取不到时的兜底）。 */
export function shortId(nodeId: string): string {
  return nodeId.length > 8 ? nodeId.slice(0, 8) : nodeId
}
