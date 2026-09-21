import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { getApi, type Diagnostics, type Peer, type Self } from '../api'

/**
 * 该节点当前是否「能发起通信」—— 决定它进「在线」还是「离线」分组。
 *
 * 三档可达性，ADR-009 要求 UI 能把它们分开表达：
 *   1. connected             —— 已有 TCP 会话：首条消息零跳数，立刻发得出；
 *   2. state == 'online'     —— 握手过（连接层置位）；
 *   3. state == 'discovered' —— 只有新鲜的 UDP announce：节点正在广播、
 *                               地址已知，但尚未握手，发首条消息要多一个
 *                               RTT 的拨号。
 *
 * 三档都发得出消息，因此都算「在线」；差别由列表行上的「未连接」标签表达。
 *
 * ⚠ 过去这里只认 `online`（= TCP 握手成功，见 dto.go 的 ToPeerDTO）。
 * 而刚被广播发现、还没聊过天的节点状态是 discovered，于是明明在列表里
 * 却被归进「离线」—— 用户看到的现象就是「能发现，但一直显示离线，
 * 发了消息才变在线」。
 */
export function reachable(p: Peer): boolean {
  if (p.connected || p.online) return true
  // 只有 announce 而没有可拨号地址，无从发起通信，仍不算可达。
  return p.state === 'discovered' && p.lastAddr !== ''
}

/**
 * 节点目录状态。
 *
 * UI 必须同时呈现「在线」（announce 维护）与「已连接」（TCP 会话维护）：
 * ADR-009 的代价就是这两者可以不一致 —— 节点在线但还没拨号，
 * 若只显示一个状态，用户会看到「在线却发不出消息」而困惑。
 */
export const usePeersStore = defineStore('peers', () => {
  const self = ref<Self | null>(null)
  const peers = ref<Peer[]>([])
  const diag = ref<Diagnostics | null>(null)
  const loading = ref(false)
  /** 是否已完成过至少一次刷新（无论成败）。 */
  const ready = ref(false)
  const error = ref('')

  async function refresh(): Promise<void> {
    // 只有【首次】加载才置 loading。
    //
    // 后台周期刷新（App.vue 每 3 秒一次）若也置它，所有依赖 `!loading`
    // 的内容（例如「还没有发现任何节点」空状态）都会被卸载再挂载一遍 ——
    // 表现出来正是「左栏定时闪一下」。
    // 「正在加载」只对第一次有意义：之后屏幕上已经有真实内容了。
    const first = !ready.value
    if (first) loading.value = true
    try {
      const api = await getApi()
      const [s, list, d] = await Promise.all([api.self(), api.peerList(), api.diagnostics()])
      self.value = s
      mergePeers(list)
      diag.value = d
      error.value = ''
    } catch (e: any) {
      error.value = String(e?.message ?? e)
    } finally {
      if (first) loading.value = false
      // 首次刷新【结束】即算就绪（失败也计入）：否则后端异常时空状态
      // 永远不出现，左栏会变成一片空白，比显示空状态更糟。
      ready.value = true
    }
  }

  /** 浅比较：一次拉取是否真的改变了某个节点的展示内容。 */
  function samePeer(a: Peer, b: Peer): boolean {
    const kb = Object.keys(b) as (keyof Peer)[]
    if (kb.length !== Object.keys(a).length) return false
    return kb.every((k) => a[k] === b[k])
  }

  /**
   * 合并拉取结果：内容没变的节点【保留原对象引用】。
   *
   * 直接 `peers.value = list` 会让每个 Peer 都变成新对象，于是每一行的
   * 全部绑定失效、整个列表重渲染 —— 每 3 秒来一次，肉眼就是一次闪烁。
   * 保留引用后，未变化的节点在 Vue 看来是「没变」，根本不会进入 patch。
   */
  function mergePeers(next: Peer[]): void {
    const prev = new Map(peers.value.map((p) => [p.nodeId, p]))
    peers.value = next.map((n) => {
      const old = prev.get(n.nodeId)
      return old && samePeer(old, n) ? old : n
    })
  }

  /** 应用一条增量更新（由 peer:updated 事件驱动）。 */
  function applyUpdate(patch: Partial<Peer> & { nodeId: string }): void {
    const i = peers.value.findIndex((p) => p.nodeId === patch.nodeId)
    if (i < 0) {
      peers.value.push({
        nodeId: patch.nodeId,
        shortId: patch.nodeId.slice(0, 8),
        displayName: patch.displayName || patch.nodeId.slice(0, 8),
        state: patch.state ?? 'discovered',
        lastAddr: patch.lastAddr ?? '',
        subnet: '',
        source: '',
        lastSeen: Date.now(),
        online: patch.state === 'online',
        connected: false
      })
      return
    }
    const prev = peers.value[i]
    peers.value[i] = {
      ...prev,
      ...patch,
      displayName: patch.displayName || prev.displayName,
      lastAddr: patch.lastAddr || prev.lastAddr,
      // 状态缺失时保留原值；给了就以新状态为准
      online: patch.state ? patch.state === 'online' : prev.online
    }
  }

  function byId(nodeId: string): Peer | undefined {
    return peers.value.find((p) => p.nodeId === nodeId)
  }

  function nameOf(nodeId: string): string {
    return byId(nodeId)?.displayName || nodeId.slice(0, 8)
  }

  /** 排序：已连接 → 在线 → （同类内）按名字。连接可用比「在线」更靠前，因为它能立刻发消息。 */
  function byPresence(a: Peer, b: Peer): number {
    if (a.connected !== b.connected) return a.connected ? -1 : 1
    return a.displayName.localeCompare(b.displayName, 'zh-Hans-CN')
  }

  /** 联系人列表的上半区：能发消息的人（口径见 reachable）。 */
  const onlinePeers = computed(() => peers.value.filter(reachable).sort(byPresence))

  /** 下半区：还能看见，但现在发不了。 */
  const offlinePeers = computed(() =>
    peers.value
      .filter((p) => !reachable(p))
      .sort((a, b) => a.displayName.localeCompare(b.displayName, 'zh-Hans-CN'))
  )

  /**
   * 在线人数（口径：reachable，含「已发现但未握手」）。
   *
   * 注意别和 diag.activeSessions 混用：后者是「已有 TCP 会话」的数量，
   * ADR-009 下两者本就可以不一致。
   */
  const onlineCount = computed(() => onlinePeers.value.length)

  return {
    self,
    peers,
    diag,
    loading,
    ready,
    error,
    onlinePeers,
    offlinePeers,
    onlineCount,
    refresh,
    applyUpdate,
    byId,
    nameOf
  }
})
