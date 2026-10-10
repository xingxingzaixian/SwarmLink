import { directConvId } from './ids'
import { EV } from './types'
import { baseName } from '../utils/format'
import type {
  Conversation,
  Diagnostics,
  FolderSummary,
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
  /** 每会话的清空点（毫秒）；与后端一样，缺省 = 未清空。 */
  const cleared = new Map<string, number>()

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

  // 预置一条入站图片消息：一打开预览就能确认图片气泡的渲染路径是通的
  push(convAlice, {
    msgId: 'seed-image-1',
    convId: convAlice,
    senderId: ALICE,
    direction: 'in',
    content: JSON.stringify({
      v: 1,
      mime: 'image/jpeg',
      w: 640,
      h: 360,
      b64: mockImageB64('对方发来的截图').b64
    }),
    msgType: 'image',
    state: 'delivered',
    sentAt: now - 34 * 60_000
  })

  /**
   * 预置一个「已发出的文件夹批次」：一条卡片 + 四条任务。
   *
   * 为什么必须在预览里预置：浏览器里没有原生对话框，pickFolder 永远不可用，
   * 于是 sendFolder 这条路【点不出来】—— 不预置的话，文件夹卡片在预览模式下
   * 根本渲染不到，走查等于没走（与预置那条入站图片消息是同一个理由）。
   *
   * 四条任务刻意摆成四种形态（完成 / 失败 / 空目录 / 排队中），因为卡片上
   * 最容易写错的就是这几种结局的文案与配色。
   */
  const SEED_BATCH = 'mock-batch-seed'
  push(convAlice, {
    msgId: 'seed-folder-1',
    convId: convAlice,
    senderId: self.nodeId,
    direction: 'out',
    content: JSON.stringify({
      batch_id: SEED_BATCH,
      folder_name: '照片',
      file_count: 4,
      total_size: 15 * 1024 * 1024,
      skipped: 1
    }),
    msgType: 'folder',
    state: 'delivered',
    sentAt: now - 8 * 60_000
  })

  const jobs: TransferJob[] = [
    {
      jobId: 'mock-done-1',
      peerId: ALICE,
      fileName: 'firmware-v2.4.1.bin.zip',
      fileSize: 42_318_592,
      direction: 'recv',
      localPath: '~/Downloads/SwarmLink/firmware-v2.4.1.bin.zip',
      // 与真实后端一致：completed 是【分块数】，不是字节数
      completed: 81,
      totalChunks: 81,
      percent: 100,
      status: 'done',
      updatedAt: now - 600_000
    },
    {
      jobId: `${SEED_BATCH}-0`,
      peerId: ALICE,
      fileName: 'a.jpg',
      fileSize: 3 * 1024 * 1024,
      direction: 'send',
      localPath: '',
      completed: 6,
      totalChunks: 6,
      percent: 100,
      status: 'done',
      batchId: SEED_BATCH,
      relPath: '照片/2024/a.jpg',
      updatedAt: now - 7 * 60_000
    },
    {
      jobId: `${SEED_BATCH}-1`,
      peerId: ALICE,
      fileName: 'b.jpg',
      fileSize: 2 * 1024 * 1024,
      direction: 'send',
      localPath: '',
      completed: 0,
      totalChunks: 4,
      percent: 0,
      status: 'failed',
      error: '文件已被删除或移动',
      batchId: SEED_BATCH,
      relPath: '照片/2024/b.jpg',
      updatedAt: now - 6 * 60_000
    },
    {
      jobId: `${SEED_BATCH}-2`,
      peerId: ALICE,
      fileName: '空目录',
      fileSize: 0,
      direction: 'send',
      localPath: '',
      completed: 0,
      totalChunks: 0,
      percent: 100,
      status: 'done',
      batchId: SEED_BATCH,
      relPath: '照片/空目录',
      isDir: true,
      updatedAt: now - 6 * 60_000
    },
    {
      jobId: `${SEED_BATCH}-3`,
      peerId: ALICE,
      fileName: '报告.pdf',
      fileSize: 12 * 1024 * 1024,
      direction: 'send',
      localPath: '',
      completed: 0,
      totalChunks: 24,
      percent: 0,
      status: 'idle',
      waitReason: 'batch_queued',
      batchId: SEED_BATCH,
      relPath: '照片/报告.pdf',
      updatedAt: now - 5 * 60_000
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
    encryption: 'off',
    notifySound: true,
    notifyMinimizeToTray: true,
    autoStart: false
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

  /**
   * 现画一张小图当图片消息的载荷。
   *
   * 用 canvas 而不是内置一张 base64 常量：前者能带上文字，
   * 预览时一眼就能分辨「这是哪条消息的图」，常量图则十张一个样。
   */
  function mockImageB64(text: string): { b64: string; w: number; h: number } {
    const w = 320
    const h = 200
    if (typeof document === 'undefined') {
      // 无 DOM 的极端情况：退回 1x1 透明 PNG，保证结构仍然合法
      return {
        w: 1,
        h: 1,
        b64: 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=='
      }
    }
    const canvas = document.createElement('canvas')
    canvas.width = w
    canvas.height = h
    const g = canvas.getContext('2d')!
    const grad = g.createLinearGradient(0, 0, w, h)
    grad.addColorStop(0, '#7c3aed')
    grad.addColorStop(1, '#ec4899')
    g.fillStyle = grad
    g.fillRect(0, 0, w, h)
    g.fillStyle = '#fff'
    g.font = '16px sans-serif'
    g.fillText((text || '预览图片').slice(0, 18), 16, h / 2)
    return { w, h, b64: canvas.toDataURL('image/jpeg', 0.8).split(',')[1] ?? '' }
  }

  function mockImageMessage(
    convId: string,
    senderId: string,
    direction: 'in' | 'out',
    text: string
  ): Message {
    const img = mockImageB64(text)
    return {
      msgId: `mock-img-${Date.now()}-${Math.random().toString(16).slice(2, 6)}`,
      convId,
      senderId,
      direction,
      content: JSON.stringify({ v: 1, mime: 'image/jpeg', w: img.w, h: img.h, b64: img.b64 }),
      msgType: 'image',
      state: 'delivered',
      sentAt: Date.now()
    }
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
          job.completed = job.totalChunks
          job.localPath = `/remote/${name}`
          diagnostics.activeTransfers = 0
        }
        emit(EV.transferDone, { jobId, peerId, path: `/remote/${name}` })
      }
    }, 320)

    return jobId
  }

  const sleep = (ms: number) => new Promise<void>((resolve) => void window.setTimeout(resolve, ms))

  /**
   * 模拟一次文件夹发送：先整批「排队中」落进列表，再逐个文件跑完。
   *
   * 两条与真实后端对齐的行为，预览模式必须照做（否则走查看到的界面与真机不是一回事）：
   *   1. 同一对端【同时只传一个】文件 —— 所以是串行的，不是一起跑；
   *   2. 「取消全部」必须真的能停下后面还没轮到的文件。
   */
  function simulateFolder(
    peerId: string,
    folderName: string,
    entries: { name: string; size: number; isDir?: boolean }[]
  ): FolderSummary {
    const batchId = `mock-batch-${Date.now()}-${Math.random().toString(16).slice(2, 6)}`
    const startedAt = Date.now()

    const specs = entries.map((e, i) => ({
      jobId: `${batchId}-${i}`,
      relPath: `${folderName}/${e.name}`,
      size: e.size,
      isDir: !!e.isDir
    }))

    for (const s of specs) {
      jobs.unshift({
        jobId: s.jobId,
        peerId,
        fileName: baseName(s.relPath),
        fileSize: s.size,
        direction: 'send',
        localPath: '',
        // 真实后端在返回前就落库为 idle + batch_queued，界面因此立刻有「排队中」
        status: 'queued',
        waitReason: 'batch_queued',
        batchId,
        relPath: s.relPath,
        isDir: s.isDir,
        completed: 0,
        totalChunks: s.isDir ? 0 : Math.ceil(s.size / (512 * 1024)),
        percent: 0,
        updatedAt: startedAt
      })
    }

    void (async () => {
      for (const s of specs) {
        const job = jobs.find((j) => j.jobId === s.jobId)
        if (!job) continue
        // 被「取消全部」停掉的批次：后面的文件不再开始
        if (job.status === 'cancelled') return

        // 空目录条目没有内容可传，与后端一致：直接完成
        if (!s.isDir && s.size > 0) {
          job.status = 'transferring'
          job.waitReason = undefined
          for (let percent = 20; percent <= 100; percent += 20) {
            await sleep(200)
            if (job.status === 'cancelled') return
            job.percent = percent
            job.updatedAt = Date.now()
            emit(EV.transferProgress, {
              jobId: s.jobId,
              peerId,
              percent,
              speed: 4 * 1024 * 1024 + Math.random() * 3 * 1024 * 1024,
              status: percent >= 100 ? 'verifying' : 'transferring'
            })
          }
        }

        job.percent = 100
        job.status = 'done'
        job.waitReason = undefined
        job.completed = job.totalChunks
        job.localPath = `/remote/${s.relPath}`
        job.updatedAt = Date.now()
        emit(EV.transferDone, { jobId: s.jobId, peerId, path: job.localPath })
      }
    })()

    return {
      batchId,
      folderName,
      fileCount: specs.length,
      totalSize: specs.reduce((sum, s) => sum + s.size, 0),
      skipped: 0
    }
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
    async autoStartEnable() {
      settings.autoStart = true
    },
    async autoStartDisable() {
      settings.autoStart = false
    },
    async autoStartIsEnabled() {
      return settings.autoStart
    },
    async listInterfaces() {
      return diagnostics.interfaceSummary
    },
    async downloadDir() {
      return settings.downloadDir
    },
    // 浏览器里没有托盘也没有提示决策，收下即可 ——
    // 保留这个调用点是为了让「上报当前会话」这条链路在 mock 模式下
    // 也被真实执行到（例如 watch 写错了会在这里报错）。
    async setActiveConversation() {
      // 空实现：mock 里没有提示决策，收下即可
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
    async sendImage(peerId, path) {
      const convId = directConvId(self.nodeId, peerId)
      const name = path.split(/[\\/]/).pop() || ''
      const m = mockImageMessage(convId, self.nodeId, 'out', name || '发出的图片')
      m.state = 'pending'
      push(convId, m)
      window.setTimeout(() => emit(EV.chatDelivered, { msgId: m.msgId }), 300)
      return m
    },
    async sendImageBytes(peerId, name) {
      const convId = directConvId(self.nodeId, peerId)
      const m = mockImageMessage(convId, self.nodeId, 'out', name || '粘贴的图片')
      m.state = 'pending'
      push(convId, m)
      window.setTimeout(() => emit(EV.chatDelivered, { msgId: m.msgId }), 300)
      return m
    },
    async saveImage() {
      // 降级预览里没有原生保存对话框，调用方会退化成浏览器下载
    },
    async clearConversation(convId) {
      cleared.set(convId, Date.now())
    },
    async restoreConversation(convId) {
      cleared.delete(convId)
    },
    async clearedAt(convId) {
      return cleared.get(convId) ?? 0
    },
    async sendFileMessage(peerId, jobId, fileName) {
      const convId = directConvId(self.nodeId, peerId)
      const m: Message = {
        msgId: `mock-file-${Date.now()}`,
        convId,
        senderId: self.nodeId,
        direction: 'out',
        content: JSON.stringify({ job_id: jobId, file_name: fileName }),
        msgType: 'file',
        state: 'pending',
        sentAt: Date.now()
      }
      push(convId, m)
      window.setTimeout(() => emit(EV.chatDelivered, { msgId: m.msgId }), 300)
      return m
    },
    async sendFolderMessage(peerId, batchId, folderName, fileCount, totalSize, skipped) {
      const convId = directConvId(self.nodeId, peerId)
      const m: Message = {
        msgId: `mock-folder-${Date.now()}`,
        convId,
        senderId: self.nodeId,
        direction: 'out',
        // 字段名与 Go 侧 message.FolderMessageContent 的 tag 严格一致
        content: JSON.stringify({
          batch_id: batchId,
          folder_name: folderName,
          file_count: fileCount,
          total_size: totalSize,
          skipped
        }),
        msgType: 'folder',
        state: 'pending',
        sentAt: Date.now()
      }
      push(convId, m)
      window.setTimeout(() => emit(EV.chatDelivered, { msgId: m.msgId }), 300)
      return m
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
    async sendFolder(peerId, path): Promise<FolderSummary> {
      const name = baseName(path)
      if (!name) throw new Error('请选择文件夹')
      // 造一棵有层次的假目录树：多层子目录、一个空目录、外加两个同名文件 ——
      // 恰好覆盖卡片与传输列表上最容易出错的三处渲染（相对路径、空目录、同名）。
      return simulateFolder(peerId, name, [
        { name: '2024/01/a.jpg', size: 3 * 1024 * 1024 },
        { name: '2024/02/a.jpg', size: 3 * 1024 * 1024 },
        { name: '空目录', size: 0, isDir: true },
        { name: '报告.pdf', size: 12 * 1024 * 1024 },
        { name: 'readme.txt', size: 4 * 1024 }
      ])
    },
    async transfers() {
      return [...jobs]
    },
    async batchJobs(batchId) {
      return jobs.filter((j) => j.batchId === batchId)
    },
    async cancelBatch(batchId) {
      let n = 0
      for (const j of jobs) {
        if (j.batchId !== batchId) continue
        if (j.status === 'done' || j.status === 'failed' || j.status === 'cancelled') continue
        j.status = 'cancelled'
        j.waitReason = undefined
        n++
      }
      return n
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
    // 预览模式下没有真实传输：这两个动作只把状态改到位，
    // 保证「等待对方上线 → 取消 / 立即重试」这两条交互在浏览器里能走通。
    async cancelTransfer(jobId) {
      const j = jobs.find((x) => x.jobId === jobId)
      if (!j) throw new Error('任务不存在')
      j.status = 'cancelled'
      j.waitReason = undefined
    },
    async retryTransfer(jobId) {
      const j = jobs.find((x) => x.jobId === jobId)
      if (!j) throw new Error('任务不存在')
      j.waitReason = undefined
      j.status = 'active'
    },

    async self() {
      return { ...self }
    },
    async peerList() {
      return [...peers]
    },
    async removePeer(nodeId) {
      // 预览模式没有后端：直接从内存列表里摘掉，行为与真实实现一致
      // （真实实现只在本机打标记，这里就是同一个语义的本地版）
      const i = peers.findIndex((p) => p.nodeId === nodeId)
      if (i >= 0) peers.splice(i, 1)
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
    },
    async groupSendImage(groupId, path) {
      const name = path.split(/[\\/]/).pop() || ''
      const m = mockImageMessage(groupId, self.nodeId, 'out', name || '群里的图片')
      m.state = 'pending'
      push(groupId, m)
      window.setTimeout(() => emit(EV.chatDelivered, { msgId: m.msgId }), 300)
      return m
    },
    async groupSendImageBytes(groupId, name) {
      const m = mockImageMessage(groupId, self.nodeId, 'out', name || '群里的图片')
      m.state = 'pending'
      push(groupId, m)
      window.setTimeout(() => emit(EV.chatDelivered, { msgId: m.msgId }), 300)
      return m
    },
    async groupSendFile(groupId, jobId, fileName) {
      const m: Message = {
        msgId: `mock-g-file-${Date.now()}`,
        convId: groupId,
        senderId: self.nodeId,
        direction: 'out',
        content: JSON.stringify({ job_id: jobId, file_name: fileName }),
        msgType: 'file',
        state: 'pending',
        sentAt: Date.now()
      }
      push(groupId, m)
      window.setTimeout(() => emit(EV.chatDelivered, { msgId: m.msgId }), 300)
      return m
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
