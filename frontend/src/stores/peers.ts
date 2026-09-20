import { defineStore } from 'pinia'
import { ref } from 'vue'

import { getApi, type Diagnostics, type Peer, type Self } from '../api'

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
  const error = ref('')

  async function refresh(): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      const api = await getApi()
      const [s, list, d] = await Promise.all([api.self(), api.peerList(), api.diagnostics()])
      self.value = s
      peers.value = list
      diag.value = d
    } catch (e: any) {
      error.value = String(e?.message ?? e)
    } finally {
      loading.value = false
    }
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

  return { self, peers, diag, loading, error, refresh, applyUpdate, byId, nameOf }
})
