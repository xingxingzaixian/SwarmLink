import type {
  Conversation,
  Diagnostics,
  Group,
  Message,
  Peer,
  Self,
  Settings,
  SwarmApi,
  TransferJob
} from './types'

/**
 * 降级实现：在没有生成绑定（即没有 Go 后端）时让前端仍可运行。
 *
 * 用途：
 *  - `npm run dev` 单独调样式
 *  - CI 里 `npm run build` 不依赖 Go 工具链
 *
 * 它【不是】模拟业务逻辑，只是让 UI 有数据可渲染；真实行为一律以后端为准。
 */
export function createMockApi(): SwarmApi {
  const now = Date.now()

  const self: Self = {
    nodeId: 'aabbccddeeff0011',
    displayName: '（未连接后端）',
    tcpPort: 2425,
    udpPort: 2425,
    subnet: '192.168.1.0/24'
  }

  const peers: Peer[] = [
    mkPeer('1122334455667788', 'alice', 'online', true, true),
    mkPeer('99aabbccddeeff00', 'bob', 'discovered', false, false)
  ]

  const messages: Message[] = []
  const jobs: TransferJob[] = []

  const settings: Settings = {
    displayName: self.displayName,
    downloadDir: '~/Downloads/SwarmLink',
    autoOpen: false,
    tcpPort: 0,
    udpPort: 0,
    interfaceMode: 'auto',
    allowInterfaces: [],
    denyInterfaces: ['utun*', 'vmnet*', 'vboxnet*', 'docker*'],
    seeds: ['192.168.1.10:2425', '192.168.2.10:2425'],
    seedRefreshSec: 300,
    seedsPerRefresh: 2,
    maxConcurrent: 3,
    chunkSize: 512 * 1024,
    windowSize: 8,
    maxActiveConns: 32,
    idleTimeoutSec: 300,
    requireAuth: true,
    encryption: 'off'
  }

  const diagnostics: Diagnostics = {
    peersTotal: peers.length,
    peersOnline: peers.filter((p) => p.online).length,
    activeSessions: 1,
    activeTransfers: 0,
    seedCount: settings.seeds.length,
    seedDetails: settings.seeds.map((s) => ({
      addr: s,
      failCount: 0,
      nodeId: '',
      tcpPort: 2425,
      subnet: ''
    })),
    interfaceSummary: ['en0(192.168.1.0/24)']
  }

  return {
    async getSettings() {
      return { ...settings }
    },
    async saveSettings() {
      return []
    },
    async listInterfaces() {
      return diagnostics.interfaceSummary
    },
    async downloadDir() {
      return settings.downloadDir
    },

    async sendMessage(peerId, text) {
      const m: Message = {
        msgId: `mock-${now}-${messages.length}`,
        convId: peerId,
        senderId: self.nodeId,
        direction: 'out',
        content: text,
        msgType: 'text',
        state: 'pending',
        sentAt: Date.now()
      }
      messages.push(m)
      return m
    },
    async history() {
      return [...messages].reverse()
    },
    async conversations(): Promise<Conversation[]> {
      return peers.map((p) => ({
        convId: p.nodeId,
        kind: 'direct',
        title: p.displayName,
        peerId: p.nodeId,
        state: p.state,
        isOnline: p.connected,
        lastTime: now
      }))
    },

    async sendFile(peerId) {
      const jobId = `mock-job-${Date.now()}`
      jobs.push({
        jobId,
        peerId,
        fileName: 'demo.bin',
        fileSize: 0,
        direction: 'send',
        localPath: '',
        completed: 0,
        totalChunks: 0,
        percent: 0,
        status: 'active',
        updatedAt: Date.now()
      })
      return jobId
    },
    async transfers() {
      return [...jobs]
    },

    async self() {
      return { ...self }
    },
    async peerList() {
      return [...peers]
    },
    async diagnostics() {
      return { ...diagnostics }
    },

    async groupList(): Promise<Group[]> {
      return []
    },
    async groupCreate(name, memberIds): Promise<Group> {
      return {
        groupId: `mock-group-${Date.now()}`,
        name,
        ownerId: self.nodeId,
        epoch: 1,
        isOwner: true,
        members: memberIds.map((id) => ({
          nodeId: id,
          displayName: id.slice(0, 8),
          role: 'member',
          state: 'active'
        })),
        createdAt: Date.now()
      }
    },
    async groupAddMember(groupId) {
      return {
        groupId,
        name: 'mock',
        ownerId: self.nodeId,
        epoch: 2,
        isOwner: true,
        members: [],
        createdAt: now
      }
    },
    async groupSendMessage(groupId, text) {
      return {
        msgId: `mock-g-${Date.now()}`,
        convId: groupId,
        senderId: self.nodeId,
        direction: 'out',
        content: text,
        msgType: 'text',
        state: 'pending',
        sentAt: Date.now()
      }
    },
    async groupHistory() {
      return []
    }
  }
}

function mkPeer(
  nodeId: string,
  name: string,
  state: string,
  online: boolean,
  connected: boolean
): Peer {
  return {
    nodeId,
    shortId: nodeId.slice(0, 8),
    displayName: name,
    state,
    lastAddr: `192.168.1.${Math.floor(Math.random() * 200) + 2}:2425`,
    subnet: '192.168.1.0/24',
    source: 'broadcast',
    lastSeen: Date.now(),
    online,
    connected
  }
}
