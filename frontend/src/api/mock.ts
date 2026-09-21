import { directConvId } from './ids'
import { EV } from './types'
import type {
  Conversation,
  Diagnostics,
  Group,
  Message,
  Peer,
  Self,
  SelfCheck,
  Settings,
  SwarmApi,
  TransferJob
} from './types'

/**
 * 降级实现：在没有生成绑定（即没有 Go 后端）时让前端仍可运行。
 *
 * 用途：
 *  - `npm run dev` 单独调样式 / 走查交互
 *  - CI 里 `npm run build` 不依赖 Go 工具链
 *
 * 两条纪律：
 * 1. 它【不是】业务逻辑的复刻，只保证 UI 有真实形状的数据可渲染；
 * 2. 数据形态必须与后端【完全一致】—— 尤其是 ID 长度。node_id 是 16 位 hex（8 字节）、
 *    group_id 是 32 位 hex，前端靠长度区分人与群；假数据用了别的长度，
 *    预览能跑但真机必崩，等于没预览。
 *
 * 发送文件会真的产生进度事件（走本地事件通道），
 * 因此右栏的「文件发送进度」在浏览器里也能被完整走查。
 */
export function createMockApi(): SwarmApi {
  const self: Self = {
    nodeId: 'aabbccddeeff0011', // 16 位 hex = 8 字节指纹
    displayName: '本机（预览模式）',
    tcpPort: 2425,
    udpPort: 2425,
    subnet: '192.168.1.0/24'
  }

  const ALICE = '1122334455667788'
  const BOB = '99aabbccddeeff00'
  const CAROL = '0123456789abcdef'
  const GROUP = 'abcdef0123456789abcdef0123456789' // 32 位 hex

  const peers: Peer[] = [
    mkPeer(ALICE, 'alice', 'online', true, true),
    mkPeer(BOB, 'bob', 'online', true, false),
    mkPeer(CAROL, 'carol', 'discovered', false, false)
  ]

  /** 会话 ID → 正序消息。 */
  const threads = new Map<string, Message[]>()
  const convAlice = directConvId(self.nodeId, ALICE)
  const convBob = directConvId(self.nodeId, BOB)

  const seed: [string, string, 'in' | 'out', number][] = [
    [convAlice, '在吗？我把测试包的校验和发你', 'in', -42],
    [convAlice, '在的', 'out', -40],
    [convAlice, 'SHA256 对不上，第 3 个分块坏的', 'in', -38],
    [convAlice, '收到，我这边重传一下那块', 'out', -36],
    [convAlice, '好，断了也能续，不用从头来', 'in', -35],
    [convBob, '网段二的种子又超时了', 'in', -12],
    [convBob, '我看看种子退避计数', 'out', -11],
    [GROUP, '今晚 8 点联调，都别关机器', 'in', -20],
    [GROUP, '收到', 'out', -19],
    [GROUP, '收到 +1', 'in', -18]
  ]

  const now = Date.now()
  for (const [convId, content, direction, offsetMin] of seed) {
    const list = threads.get(convId) ?? []
    list.push({
      msgId: `seed-${list.length}-${convId.slice(0, 6)}`,
      convId,
      senderId: direction === 'out' ? self.nodeId : ALICE,
      direction,
      content,
      msgType: 'text',
      state: direction === 'out' ? 'delivered' : 'delivered',
      sentAt: now + offsetMin * 60_000
    })
    threads.set(convId, list)
  }

  const jobs: TransferJob[] = [
    {
      jobId: 'mock-done-1',
      peerId: ALICE,
      fileName: 'firmware-v2.4.1.bin.zip',
      fileSize: 42_318_592,
      direction: 'recv',
      localPath: '~/Downloads/SwarmLink/firmware-v2.4.1.bin.zip',
      completed: 42_318_592,
      totalChunks: 81,
      percent: 100,
      status: 'done',
      updatedAt: now - 600_000
    }
  ]

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
      failCount: s.includes('192.168.2') ? 3 : 0,
      nodeId: '',
      tcpPort: 2425,
      subnet: s.includes('192.168.2') ? '192.168.2.0/24' : '192.168.1.0/24'
    })),
    interfaceSummary: ['en0(192.168.1.0/24)']
  }

  /** 直连本地事件通道（不 import events/index.ts，否则会形成循环依赖）。 */
  function emit(name: string, data: unknown): void {
    window.dispatchEvent(new CustomEvent(name, { detail: data }))
  }

  function push(convId: string, m: Message): void {
    threads.set(convId, [...(threads.get(convId) ?? []), m])
  }

  /** 倒序返回（与后端 Contract 一致：新 → 旧）。 */
  function page(convId: string, limit: number): Message[] {
    const list = threads.get(convId) ?? []
    return [...list].reverse().slice(0, limit)
  }

  /** 模拟一次发送：起一个定时器把进度推给真实的事件处理链路。 */
  function simulateSend(peerId: string, path: string): string {
    const jobId = `mock-job-${Date.now()}`
    const name = path.split(/[\\/]/).pop() || 'unnamed.bin'
    const size = 6 * 1024 * 1024 + Math.floor(Math.random() * 30 * 1024 * 1024)

    jobs.unshift({
      jobId,
      peerId,
      fileName: name,
      fileSize: size,
      direction: 'send',
      localPath: '',
      completed: 0,
      totalChunks: Math.ceil(size / (512 * 1024)),
      percent: 0,
      status: 'transferring',
      updatedAt: Date.now()
    })

    let percent = 0
    const timer = window.setInterval(() => {
      percent = Math.min(100, percent + 6 + Math.random() * 10)
      const speed = 3.5 * 1024 * 1024 + Math.random() * 4 * 1024 * 1024
      emit(EV.transferProgress, {
        jobId,
        peerId,
        percent,
        speed,
        eta: Math.round(((100 - percent) / 100) * size / speed * 1000),
        status: percent >= 100 ? 'verifying' : 'transferring'
      })
      if (percent >= 100) {
        window.clearInterval(timer)
        const job = jobs.find((j) => j.jobId === jobId)
        if (job) {
          job.percent = 100
          job.status = 'done'
          job.completed = size
          job.localPath = `/remote/${name}`
          diagnostics.activeTransfers = 0
        }
        emit(EV.transferDone, { jobId, peerId, path: `/remote/${name}` })
      }
    }, 320)

    return jobId
  }

  const groups: Group[] = [
    {
      groupId: GROUP,
      name: '联调小组',
      ownerId: ALICE,
      epoch: 3,
      isOwner: false,
      members: [
        { nodeId: ALICE, displayName: 'alice', role: 'owner', state: 'active' },
        { nodeId: self.nodeId, displayName: self.displayName, role: 'member', state: 'active' },
        { nodeId: BOB, displayName: 'bob', role: 'member', state: 'active' },
        { nodeId: CAROL, displayName: 'carol', role: 'member', state: 'inactive' }
      ],
      createdAt: now - 86_400_000
    }
  ]

  return {
    async getSettings() {
      return { ...settings }
    },
    async saveSettings() {
      return ['tcp_port']
    },
    async listInterfaces() {
      return diagnostics.interfaceSummary
    },
    async downloadDir() {
      return settings.downloadDir
    },
    async runSelfCheck(): Promise<SelfCheck> {
      const n = settings.seeds.length
      const learned = n === 0 ? 0 : Math.min(n, 3)
      return {
        performed: n > 0,
        seedCount: n,
        learned,
        p1OK: n > 0,
        p1Detail:
          n === 0
            ? '未配置种子，跳过（单网段部署无需自检）'
            : `已通过种子探通并拉取到 ${learned} 条跨网段目录`,
        p2Overlap: false,
        p2Detail: n === 0 ? '' : '未观察到地址段重叠的迹象',
        seeds: [...diagnostics.seedDetails]
      }
    },

    async sendMessage(peerId, text) {
      const convId = directConvId(self.nodeId, peerId)
      const m: Message = {
        msgId: `mock-${Date.now()}`,
        convId,
        senderId: self.nodeId,
        direction: 'out',
        content: text,
        msgType: 'text',
        state: 'pending',
        sentAt: Date.now()
      }
      push(convId, m)
      // 300ms 后确认送达，让「发送中 → 已送达」这条状态链路可见
      window.setTimeout(() => emit(EV.chatDelivered, { msgId: m.msgId }), 300)
      return m
    },
    async history(peerId, limit) {
      return page(directConvId(self.nodeId, peerId), limit)
    },
    async conversations(): Promise<Conversation[]> {
      const peerOf = (convId: string): Peer | undefined =>
        peers.find((p) => directConvId(self.nodeId, p.nodeId) === convId)

      return [...threads.keys()].map((convId) => {
        const p = peerOf(convId)
        const last = page(convId, 1)[0]
        return {
          convId,
          kind: 'direct',
          title: p?.displayName ?? convId.slice(0, 8),
          peerId: p?.nodeId,
          state: p?.state,
          isOnline: !!p?.connected,
          lastTime: last?.sentAt ?? 0
        }
      })
    },

    async sendFile(peerId, path) {
      return simulateSend(peerId, path)
    },
    async transfers() {
      return [...jobs]
    },
    // 降级预览里没有本机文件系统上下文，定位与清理都做成「记账」而非真实动作，
    // 保证点击后界面流程仍然完整可走通。
    async revealFile(path) {
      // 预览模式下没有真实文件系统可定位，只保证调用链走通
      void path
    },
    async clearTransfers() {
      const before = jobs.length
      const kept = jobs.filter(
        (j) => j.status !== 'done' && j.status !== 'failed' && j.status !== 'cancelled'
      )
      // jobs 是 const，原地替换而不是重新赋值
      jobs.splice(0, jobs.length, ...kept)
      return before - jobs.length
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
      return groups.map((g) => ({ ...g }))
    },
    async groupCreate(name, memberIds): Promise<Group> {
      const g: Group = {
        groupId: `mock-group-${Date.now()}`,
        name,
        ownerId: self.nodeId,
        epoch: 1,
        isOwner: true,
        members: memberIds.map((id) => ({
          nodeId: id,
          displayName: peers.find((p) => p.nodeId === id)?.displayName ?? id.slice(0, 8),
          role: 'member',
          state: 'active'
        })),
        createdAt: Date.now()
      }
      groups.push(g)
      return g
    },
    async groupAddMember(groupId) {
      const g = groups.find((x) => x.groupId === groupId) ?? groups[0]
      return { ...g }
    },
    async groupSendMessage(groupId, text) {
      const m: Message = {
        msgId: `mock-g-${Date.now()}`,
        convId: groupId,
        senderId: self.nodeId,
        direction: 'out',
        content: text,
        msgType: 'text',
        state: 'pending',
        sentAt: Date.now()
      }
      push(groupId, m)
      window.setTimeout(() => emit(EV.chatDelivered, { msgId: m.msgId }), 300)
      return m
    },
    async groupHistory(groupId, limit) {
      return page(groupId, limit)
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
