/**
 * 共享文件弹框的纯逻辑：分组、清单建树、过滤、路径换算。
 *
 * 抽出来而不是写在组件里，是因为这几条规则都有「不报错的错法」：
 * 分组错了会让某条共享凭空消失、中间目录漏推导会让一批文件凭空消失、
 * 目录展开时少算了子项会让「整包下载」少下几个文件、
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

/** 清单树的一个节点。 */
export interface ShareTreeNode {
  /** 相对共享根的完整路径（斜杠分隔；目录不带尾斜杠）。 */
  path: string
  name: string
  /** 目录节点：既包括空目录条目，也包括由路径推导出的中间目录。 */
  isDir: boolean
  /** 层级（0 = 共享根下的第一层），用于缩进。 */
  depth: number
  /** 清单里【真实存在】的条目；中间目录没有（它就是子节点们的容器）。 */
  item?: ShareItem
  children: ShareTreeNode[]
  /** 子树里的真实条目数与总字节（目录行显示合计）。 */
  count: number
  size: number
}

/**
 * 把扁平清单拼成目录树。
 *
 * 【为什么要推导中间目录】：索引里只有文件与空目录 —— 非空目录不占条目
 * （它的内容就是它的表示）。所以「docs/2024/a.txt」这条必须自己长出
 * docs、docs/2024 两层容器，树才立得起来。这也是本函数存在的全部理由。
 *
 * 排序：目录在前、同类按名字（与文件管理器一致）。后端按整串路径排序，
 * 那会让「a.txt」排在「docs/」之前，读起来不像树。
 */
export function buildItemTree(items: ShareItem[]): ShareTreeNode[] {
  const roots: ShareTreeNode[] = []
  const dirs = new Map<string, ShareTreeNode>()

  /** 取得（必要时创建）中间目录节点，并把它挂到父节点上；返回最深层那个。 */
  function ensureDir(segs: string[]): ShareTreeNode | null {
    let parent: ShareTreeNode | null = null
    let path = ''
    for (let i = 0; i < segs.length; i++) {
      path = path ? `${path}/${segs[i]}` : segs[i]
      let node = dirs.get(path)
      if (!node) {
        node = { path, name: segs[i], isDir: true, depth: i, children: [], count: 0, size: 0 }
        dirs.set(path, node)
        ;(parent ? parent.children : roots).push(node)
      }
      parent = node
    }
    return parent
  }

  for (const it of items) {
    const segs = it.path.split('/').filter(Boolean)
    // 空路径是坏数据（CleanRelPath 会挡住），直接跳过而不是让它变成一个无名节点。
    if (!segs.length) continue
    const parent = ensureDir(segs.slice(0, -1))
    const leaf: ShareTreeNode = {
      path: it.path,
      // 优先用清单给的名字：它与路径末段可能不同（后端已清洗过）。
      name: it.name || segs[segs.length - 1],
      isDir: !!it.isDir,
      depth: segs.length - 1,
      item: it,
      children: [],
      count: 0,
      size: 0
    }
    ;(parent ? parent.children : roots).push(leaf)
  }

  // 先排好序再算合计：合计与顺序无关，但排序要在渲染前定下来。
  sortNodes(roots)
  for (const n of roots) tally(n)
  return roots
}

/** 目录在前、同类按名字。递归排整棵树。 */
function sortNodes(nodes: ShareTreeNode[]): void {
  nodes.sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
    return a.name.localeCompare(b.name, 'zh-Hans-CN')
  })
  for (const n of nodes) sortNodes(n.children)
}

/**
 * 自底向上累计子树里的条目数与总字节。
 *
 * 目录行显示「N 项 · 合计大小」，让用户不必展开就能判断该不该整包下载；
 * 合计必须来自真实条目而不是 children.length —— 后者在深层目录上会少算。
 */
function tally(n: ShareTreeNode): { count: number; size: number } {
  let count = n.item ? 1 : 0
  let size = n.item && !n.item.isDir ? n.item.size : 0
  for (const c of n.children) {
    const sub = tally(c)
    count += sub.count
    size += sub.size
  }
  n.count = count
  n.size = size
  return { count, size }
}

/** 树展开后要渲染的一行（表格里的一行 = 树里的一个可见节点）。 */
export interface ShareTreeRow {
  path: string
  name: string
  isDir: boolean
  depth: number
  /** 目录行的合计；文件行就是它自己。 */
  count: number
  size: number
  /** 真实条目；中间目录为 undefined（没有大小/时间可显示）。 */
  item?: ShareItem
  /**
   * 勾选这一行等于勾选这些条目：文件行是它自己，目录行是整个子树里
   * 全部真实条目。下载请求按它展开 —— 后端只认文件路径，不认识目录。
   */
  targets: string[]
  /** 是否有子节点：决定画不画折叠箭头。 */
  hasChildren: boolean
  expanded: boolean
}

/**
 * 把树按折叠状态拍平成渲染用的行序列。
 *
 * 折叠状态的默认值是本函数的一部分而不是调用方的事：**没记录过的节点，
 * 第一层（depth 0）默认展开、更深的默认收起**。这样打开一条共享就能看到
 * 顶层结构、又不至于把几十层目录一次性铺满。
 *
 * `forceExpand`（搜索态）改的是**默认值**而不是最终状态：否则命中的文件会被
 * 折叠的目录挡住、看起来像「没匹配上」，而用户手动收起某个目录的意图也该被
 * 尊重 —— 强制展开会让箭头点了没反应。
 */
export function flattenTree(
  nodes: ShareTreeNode[],
  expanded: Record<string, boolean | undefined>,
  forceExpand = false
): ShareTreeRow[] {
  const rows: ShareTreeRow[] = []

  function walk(list: ShareTreeNode[]): void {
    for (const n of list) {
      const hasChildren = n.children.length > 0
      // 显式记录优先；没记录过的节点用默认值（搜索时默认全开，否则只开第一层）。
      const open = expanded[n.path] ?? (forceExpand || n.depth === 0)
      const targets: string[] = []
      collectTargets(n, targets)
      targets.sort()
      rows.push({
        path: n.path,
        name: n.name,
        isDir: n.isDir,
        depth: n.depth,
        count: n.count,
        size: n.size,
        item: n.item,
        targets,
        hasChildren,
        expanded: open
      })
      if (open && hasChildren) walk(n.children)
    }
  }

  walk(nodes)
  return rows
}

/** 收齐子树里全部真实条目的路径（前序，调用方自行排序）。 */
function collectTargets(n: ShareTreeNode, out: string[]): void {
  if (n.item) out.push(n.item.path)
  for (const c of n.children) collectTargets(c, out)
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
