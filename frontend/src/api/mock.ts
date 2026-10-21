import { directConvId } from './ids'
import { EV } from './types'
import { baseName } from '../utils/format'
import { likeContains } from '../utils/search'
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
  Share,
  ShareDownload,
  Background,
  ShareItem,
  SwarmApi,
  TransferJob,
  UpdateState
} from './types'

/**
 * 「消息 + 写入序号」：搜索与跳转窗口的排序条目。
 *
 * 与 mem 实现里的 `seqMessage` 是同一个东西，存在的理由也一样 ——
 * 排序键不能只有 sent_at（同一毫秒里可以有多条），必须带上唯一的写入序。
 */
interface SeqMessage {
  seq: number
  m: Message
}

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

  /**
   * 写入序号：搜索游标与跳转窗口的排序键。
   *
   * 它对应后端 sqlite 的 rowid / mem 实现里的 seq —— 游标必须是「唯一且单调」
   * 的写入序，不能是 sent_at：同一毫秒里发几条时按时间跳会漏掉边界上的命中。
   * 不塞进 Message 字段里，是因为 mock 的数据形态必须与后端 DTO 【逐字一致】，
   * 多一个字段就是给「预览能跑、真机没有」留一个坑。
   */
  const seqByMsg = new Map<string, number>()
  let seqCounter = 0

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
    // 走 push 而不是直接改 threads：写入序号必须与消息同时落账，
    // 否则种子消息在搜索游标 / 跳转窗口里没有排序键（分页会错位）。
    push(convId, {
      msgId: `seed-${threads.get(convId)?.length ?? 0}-${convId.slice(0, 6)}`,
      convId,
      senderId: direction === 'out' ? self.nodeId : ALICE,
      direction,
      content,
      msgType: 'text',
      state: direction === 'out' ? 'delivered' : 'delivered',
      sentAt: now + offsetMin * 60_000
    })
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
    },
    // 下面两条是给【全局传输队列浮层】预置的。理由与上面那条文件夹批次一样：
    // 浏览器里没有原生文件对话框，pickFile 点不出来，于是浮层（只在有未结束
    // 任务时才渲染）在预览模式下永远看不到，走查等于没走。
    //
    // 一条「正在传」+ 一条「已暂停」，是为了把浮层上全部四种形态都摆出来：
    // 并发数（1/3）、总网速（只有正在传的贡献）、总进度（分母含排队中的那个
    // 12MB）、以及按钮在「暂停全部 ⇄ 全部恢复」之间的切换。
    //
    // 刻意【不挂模拟定时器】：浮层要验的是布局与文案，挂上定时器反而会在三五
    // 秒内把它自己跑成 done，把要检查的形态跑没（速度沿用真实事件里的量级）。
    {
      jobId: 'mock-live-1',
      peerId: ALICE,
      fileName: 'dataset-2024-q3.tar.zst',
      fileSize: 486 * 1024 * 1024,
      direction: 'send',
      localPath: '',
      completed: 158,
      totalChunks: 972,
      percent: 42,
      // 起点 12% + 当前 42%：正是「续传自」最该被看懂的形态。
      // 不预置的话，那颗标记在预览模式里永远渲染不到（浏览器里没有真实续传）。
      resumeFromPercent: 12,
      speed: 6.4 * 1024 * 1024,
      status: 'transferring',
      updatedAt: now - 2_000
    },
    {
      jobId: 'mock-paused-1',
      peerId: BOB,
      fileName: '会议录像.mp4',
      fileSize: 218 * 1024 * 1024,
      direction: 'send',
      localPath: '',
      completed: 66,
      totalChunks: 436,
      percent: 31,
      status: 'paused',
      updatedAt: now - 30_000
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
    maxFileSize: 10 * 1024 * 1024 * 1024,
    maxActiveConns: 32,
    idleTimeoutSec: 300,
    requireAuth: true,
    encryption: 'off',
    notifySound: true,
    notifyMinimizeToTray: true,
    autoStart: false,
    updateEnabled: true,
    updateMode: 'prompt',
    updateAutoInstallHours: 4,
    screenshotEnabled: true,
    screenshotLastMode: 'keep_window'
  }

  // 种子 192.168.2.x 在 mock 里恒为失败：状态灯必须能演示出
  // 「同网段通、跨网段断了」这个最容易被漏掉的状态。
  const seedDetails = settings.seeds.map((s) => {
    const failed = s.includes('192.168.2')
    return {
      addr: s,
      failCount: failed ? 3 : 0,
      nodeId: '',
      tcpPort: 2425,
      subnet: failed ? '192.168.2.0/24' : '192.168.1.0/24',
      lastSeen: failed ? 0 : Date.now() - 60_000,
      available: !failed
    }
  })
  const sameSubnetPeers = peers.filter((p) => p.online).length
  const seedAvailable = seedDetails.filter((s) => s.available).length

  const diagnostics: Diagnostics = {
    peersTotal: peers.length,
    peersOnline: peers.filter((p) => p.online).length,
    activeSessions: 1,
    activeTransfers: 0,
    seedCount: seedDetails.length,
    seedAvailable,
    seedDetails,
    interfaceSummary: ['en0(192.168.1.0/24)'],
    sameSubnetPeers,
    // 与 Go 侧 app.ClassifyDiscovery 同规则；预览模式下不引后端，只能照抄一遍。
    discoveryState:
      sameSubnetPeers === 0 && seedAvailable === 0
        ? 'limited'
        : sameSubnetPeers > 0 && (seedDetails.length === 0 || seedAvailable > 0)
          ? 'live'
          : 'partial'
  }

  /** 直连本地事件通道（不 import events/index.ts，否则会形成循环依赖）。 */
  function emit(name: string, data: unknown): void {
    window.dispatchEvent(new CustomEvent(name, { detail: data }))
  }

  /** 追加一条消息并分配写入序号（搜索/跳转都要用它，见 seqByMsg）。 */
  function push(convId: string, m: Message): void {
    seqByMsg.set(m.msgId, ++seqCounter)
    threads.set(convId, [...(threads.get(convId) ?? []), m])
  }

  /** 某会话的清空点（毫秒；缺省 = 未清空）。判据与后端 `sent_at > cleared_at` 同向。 */
  function clearedOf(convId: string): number {
    return cleared.get(convId) ?? 0
  }

  /** 某会话里「未被清空点隐藏」的消息（正序，与 threads 的存入顺序一致）。 */
  function visibleOf(convId: string): Message[] {
    const at = clearedOf(convId)
    return (threads.get(convId) ?? []).filter((m) => m.sentAt > at)
  }

  /**
   * 显示序比较：(sent_at, 写入序号) 升序，返回三值（Array.prototype.sort 要的形状）。
   *
   * 与后端 sqlite 的 `ORDER BY sent_at, id` 是同一条规则（那边的 id 就是 rowid），
   * 两个实现必须逐字一致 —— 否则「同一毫秒里的多条消息」在预览与真机上
   * 会落进不同的窗口位置。
   */
  function byDisplay(a: SeqMessage, b: SeqMessage): number {
    if (a.m.sentAt !== b.m.sentAt) return a.m.sentAt - b.m.sentAt
    return a.seq - b.seq
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

  /**
   * 预览模式下每个任务一条进度定时器（按 jobId 索引）。
   *
   * 为什么要按 jobId 存句柄：暂停必须真的停掉定时器 —— 只改状态码的话，
   * 进度还会自己往前爬，走查「暂停」看到的是假的。
   */
  const simTimers = new Map<string, number>()

  /** 推进一次模拟进度。用 jobId 反查任务，暂停/恢复之间不丢任何状态。 */
  function tickSimSend(jobId: string): void {
    const job = jobs.find((x) => x.jobId === jobId)
    if (!job || job.status === 'paused' || job.status === 'cancelled') {
      stopSimTimer(jobId)
      return
    }
    const percent = Math.min(100, (job.percent || 0) + 6 + Math.random() * 10)
    const speed = 3.5 * 1024 * 1024 + Math.random() * 4 * 1024 * 1024
    job.percent = percent
    job.updatedAt = Date.now()
    emit(EV.transferProgress, {
      jobId,
      peerId: job.peerId,
      percent,
      speed,
      eta: Math.round((((100 - percent) / 100) * job.fileSize) / speed * 1000),
      status: percent >= 100 ? 'verifying' : 'transferring'
    })
    if (percent >= 100) {
      stopSimTimer(jobId)
      job.status = 'done'
      job.completed = job.totalChunks
      job.localPath = `/remote/${job.fileName}`
      diagnostics.activeTransfers = 0
      emit(EV.transferDone, { jobId, peerId: job.peerId, path: job.localPath })
    }
  }

  function startSimTimer(jobId: string): void {
    if (simTimers.has(jobId)) return
    simTimers.set(jobId, window.setInterval(() => tickSimSend(jobId), 320))
  }

  function stopSimTimer(jobId: string): void {
    const t = simTimers.get(jobId)
    if (t !== undefined) {
      window.clearInterval(t)
      simTimers.delete(jobId)
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

    startSimTimer(jobId)
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
    entries: { name: string; size: number; isDir?: boolean }[],
    batchIdOverride?: string
  ): FolderSummary {
    // 群聊发文件夹时多个成员共享一个批次（见 sendFolderTo），
    // 因此 jobId 必须带上 peerId，否则成员之间会互相覆盖。
    const batchId =
      batchIdOverride ?? `mock-batch-${Date.now()}-${Math.random().toString(16).slice(2, 6)}`
    const startedAt = Date.now()

    const specs = entries.map((e, i) => ({
      jobId: `${batchId}-${peerId.slice(0, 6)}-${i}`,
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
            // 暂停期间挂住不动：真实后端里这个文件会被 FILE_PAUSE 按在
            // 传输中，恢复后从当前进度继续 —— 预览模式照做，
            // 否则走查看不到「暂停后进度真的停住」。
            while (job.status === 'paused') await sleep(150)
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

  // 造一个「有人已退出」的群，好让面板的三种成员状态都能被走查到。
  const groups: Group[] = [
    {
      groupId: GROUP,
      name: '联调小组',
      ownerId: ALICE,
      epoch: 3,
      mintSeq: 5,
      state: 'active',
      isOwner: false,
      members: [
        { nodeId: ALICE, displayName: 'alice', role: 'owner', state: 'active', active: true, admitSeq: 1, kickSeq: 0, leftSeq: 0 },
        { nodeId: self.nodeId, displayName: self.displayName, role: 'member', state: 'active', active: true, admitSeq: 3, kickSeq: 0, leftSeq: 0 },
        { nodeId: BOB, displayName: 'bob', role: 'member', state: 'active', active: true, admitSeq: 4, kickSeq: 0, leftSeq: 0 },
        // carol 主动退出过：leftSeq == admitSeq ⇒ active=false
        { nodeId: CAROL, displayName: 'carol', role: 'member', state: 'left', active: false, admitSeq: 5, kickSeq: 0, leftSeq: 5 }
      ],
      createdAt: now - 86_400_000
    }
  ]

  /**
   * 共享（FR-14）。
   *
   * 三种形态都预置，因为它们各自触发一条不同的界面分支：
   *   - 我发布的（有「刷新索引 / 取消共享」）；
   *   - 他人发布且在线（可浏览 + 可下载）；
   *   - 他人发布但离线（浏览会失败，必须看到「共享方不在线」）。
   *
   * 没有原生文件对话框的环境里，发布向导点不出来 —— 不预置就永远走查不到。
   */
  const MY_SHARE = 'mock-share-mine'
  const ALICE_SHARE = 'mock-share-alice'
  const BOB_SHARE = 'mock-share-bob'

  const MY_ITEMS: ShareItem[] = [
    { path: '2026/W01.docx', name: 'W01.docx', size: 48 * 1024, mtime: 1_696_828_800 },
    { path: '2026/W02.docx', name: 'W02.docx', size: 52 * 1024, mtime: 1_696_828_800 },
    { path: '模板/说明.txt', name: '说明.txt', size: 2048, mtime: 1_696_828_800 },
    { path: '模板/空目录', name: '空目录', size: 0, isDir: true }
  ]

  const ALICE_ITEMS: ShareItem[] = [
    { path: 'build/SwarmLink.exe', name: 'SwarmLink.exe', size: 24 * 1024 * 1024 },
    { path: 'build/README.md', name: 'README.md', size: 6 * 1024 },
    { path: 'build/checksums.txt', name: 'checksums.txt', size: 512 }
  ]

  const shares: Share[] = [
    {
      shareId: MY_SHARE,
      name: '周报模板',
      ownerId: self.nodeId,
      ownerName: self.displayName,
      fileCount: MY_ITEMS.length,
      totalSize: MY_ITEMS.reduce((sum, i) => sum + i.size, 0),
      visibility: { type: 'users', allowed: [ALICE] },
      visibilityText: '指定人(1)',
      mine: true,
      rootPath: 'D:\\共享\\周报模板',
      online: true,
      downloadable: false,
      createdAt: now - 3600_000,
      updatedAt: now - 3600_000
    },
    {
      shareId: ALICE_SHARE,
      name: '测试包',
      ownerId: ALICE,
      ownerName: 'alice',
      fileCount: ALICE_ITEMS.length,
      totalSize: ALICE_ITEMS.reduce((sum, i) => sum + i.size, 0),
      visibility: { type: 'all' },
      visibilityText: '全部',
      mine: false,
      online: true,
      downloadable: true,
      createdAt: 0,
      updatedAt: 0
    },
    {
      shareId: BOB_SHARE,
      name: '日志归档',
      ownerId: BOB,
      ownerName: 'bob',
      fileCount: 12,
      totalSize: 180 * 1024 * 1024,
      visibility: { type: 'users', allowed: [self.nodeId] },
      visibilityText: '指定人(1)',
      mine: false,
      online: false,
      downloadable: false,
      createdAt: 0,
      updatedAt: 0
    }
  ]

  /** 各条共享的文件清单（发布新共享时往里塞一份）。 */
  const shareItems = new Map<string, ShareItem[]>([
    [MY_SHARE, MY_ITEMS],
    [ALICE_SHARE, ALICE_ITEMS]
  ])

  /** 取某条共享的清单（bob 那条故意没有 —— 它离线，浏览就该失败）。 */
  function itemsOf(shareId: string): ShareItem[] {
    return (shareItems.get(shareId) ?? []).map((i) => ({ ...i }))
  }

  /** 可见性文案：与后端 share.Visibility.String() 的取值保持一致。 */
  function visText(visType: string, allowed: number): string {
    if (visType === 'all') return '全部'
    if (visType === 'group') return '仅群组'
    return `指定人(${allowed})`
  }

  /**
   * 模拟一次共享下载：为每个文件建一条【接收方向】的任务，再逐个跑完。
   *
   * 与真实后端对齐的两点必须照做，否则走查看到的是另一种界面：
   *   1. 任务落库时是 idle + batch_queued（立刻出现在传输列表里）；
   *   2. 同一对端串行，且空目录条目直接完成（没有内容可传）。
   */
  function simulateShareDownload(peerId: string, rootName: string, items: ShareItem[]): ShareDownload {
    const batchId = `mock-batch-${Date.now()}-${Math.random().toString(16).slice(2, 6)}`
    const specs = items.map((it, i) => ({
      jobId: `${batchId}-${i}`,
      relPath: `${rootName}/${it.path}`,
      size: it.size,
      isDir: !!it.isDir
    }))

    for (const s of specs) {
      jobs.unshift({
        jobId: s.jobId,
        peerId,
        fileName: baseName(s.relPath),
        fileSize: s.size,
        direction: 'recv',
        localPath: '',
        status: 'queued',
        waitReason: 'batch_queued',
        batchId,
        relPath: s.relPath,
        isDir: s.isDir,
        completed: 0,
        totalChunks: s.isDir ? 0 : Math.ceil(s.size / (512 * 1024)),
        percent: 0,
        updatedAt: Date.now()
      })
    }

    void (async () => {
      for (const s of specs) {
        const job = jobs.find((j) => j.jobId === s.jobId)
        if (!job) continue
        if (job.status === 'cancelled') return
        if (!s.isDir && s.size > 0) {
          job.status = 'transferring'
          job.waitReason = undefined
          for (let percent = 25; percent <= 100; percent += 25) {
            await sleep(220)
            if (job.status === 'cancelled') return
            job.percent = percent
            job.updatedAt = Date.now()
            emit(EV.transferProgress, {
              jobId: s.jobId,
              peerId,
              percent,
              speed: 5 * 1024 * 1024 + Math.random() * 4 * 1024 * 1024,
              status: percent >= 100 ? 'verifying' : 'transferring'
            })
          }
        }
        job.percent = 100
        job.status = 'done'
        job.waitReason = undefined
        job.completed = job.totalChunks
        job.localPath = `${settings.downloadDir}/${s.relPath}`
        job.updatedAt = Date.now()
        emit(EV.transferDone, { jobId: s.jobId, peerId, path: job.localPath })
      }
    })()

    return { batchId, accepted: specs.length, rejected: [] }
  }

  // -------------------------------------------------------------------------
  // 外观（预览模式）
  // -------------------------------------------------------------------------

  /**
   * 背景图在预览模式下只存在内存里 —— 浏览器没有「数据目录」可写。
   *
   * 与真机不同的是：预览模式刷新页面就丢，这正好也提示了真机上「背景必须
   * 落盘」的必要性（localStorage 只有 5MB，而用户选的图常常比这更大）。
   */
  const mockAppearance: Background = {
    dataUrl: '',
    fileName: '',
    dim: 45,
    blur: 6,
    maxBytes: 8 << 20
  }

  function cloneAppearance(): Background {
    return { ...mockAppearance }
  }

  // -------------------------------------------------------------------------
  // 局域网更新（预览模式）
  // -------------------------------------------------------------------------

  /**
   * 预览模式下的更新状态。
   *
   * 为什么不直接返回一个写死的 idle：这一页的核心交互是「下载完 → 弹框 →
   * 选时间 / 立即装」，只靠真机才能走查的话，前端改动得等一台 Windows
   * 机器才能验证。这里用定时器把整个状态机跑一遍（下载进度 → ready），
   * 让 `npm run dev` 就能看到弹框。
   */
  const emptyUpdate = (): UpdateState => ({
    phase: 'idle',
    currentVersion: '1.0.0',
    promptPending: false,
    platformSupported: true,
    canDistribute: true,
    dataDir: '~/AppData/Roaming/SwarmLink',
    logPath: '~/AppData/Roaming/SwarmLink/logs/swarmlink.log'
  })

  const mockUpdate: UpdateState = emptyUpdate()

  function snapshotUpdate(): UpdateState {
    return {
      ...mockUpdate,
      higherPeers: (mockUpdate.higherPeers ?? []).map((p) => ({ ...p }))
    }
  }

  function publishUpdate(): void {
    emit(EV.updateState, snapshotUpdate())
  }

  /** 走一遍「发现 1.1.0 → 下载 → 就绪」的模拟流程。 */
  async function runMockUpdateCheck(): Promise<void> {
    mockUpdate.phase = 'checking'
    mockUpdate.higherPeers = [
      { peerId: ALICE, displayName: 'alice', addr: '192.168.1.21:2425', version: '1.1.0' }
    ]
    mockUpdate.lastCheckAt = Date.now()
    publishUpdate()

    await sleep(600)
    mockUpdate.phase = 'downloading'
    mockUpdate.targetVersion = '1.1.0'
    mockUpdate.sourcePeerId = ALICE
    mockUpdate.sourceName = 'alice'
    mockUpdate.sourceAddr = '192.168.1.21:2425'
    mockUpdate.size = 38.2 * 1024 * 1024
    mockUpdate.downloaded = 0
    mockUpdate.percent = 0
    publishUpdate()

    for (let percent = 20; percent <= 100; percent += 20) {
      await sleep(260)
      mockUpdate.percent = percent
      mockUpdate.downloaded = Math.round((mockUpdate.size ?? 0) * (percent / 100))
      publishUpdate()
    }

    mockUpdate.phase = 'ready'
    mockUpdate.promptPending = true
    mockUpdate.autoInstallAt = Date.now() + 4 * 60 * 60 * 1000
    publishUpdate()
  }

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
    // 浏览器里没有托盘也没有提示决策，收下即可 ——
    // 保留这个调用点是为了让「上报当前会话」这条链路在 mock 模式下
    // 也被真实执行到（例如 watch 写错了会在这里报错）。
    async setActiveConversation() {
      // 空实现：mock 里没有提示决策，收下即可
    },
    // 浏览器里抓不到本机屏幕。返回一张 1×1 的占位图：
    // 让「截屏 → 草稿 → 发送」这条链路在 mock 模式下也能跑通，
    // 而不是在中途抛错。
    async captureScreenshot() {
      return { mime: 'image/jpeg', width: 1, height: 1, dataBase64: '' }
    },
    async rememberScreenshotMode() {
      // 空实现：mock 里没有配置落盘
    },
    async beginCapture() {
      // mock 里没有真的屏幕可抓，也不该真的弹窗（也就不需要最小化主窗口）。
      // 返回一块「假显示器」让调用方能走通流程。
      return {
        displays: [
          {
            index: 0,
            x: 0,
            y: 0,
            width: 1920,
            height: 1080,
            physicalWidth: 1920,
            physicalHeight: 1080,
            scaleFactor: 1
          }
        ]
      }
    },
    async getOverlayImage() {
      // 一张 1x1 的黑图：够让覆盖层跑通，不占地方。
      return {
        mime: 'image/jpeg',
        width: 1,
        height: 1,
        scaleFactor: 1,
        dataBase64: '/9j/4AAQSkZJRgABAQEAYABgAAD/2wBDAAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQH/wgALAAABAQEBAREA/8QAFAABAQAAAAAAAAAAAAAAAAAAAAf/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/9oADAMBAAIQAxAAAAH/AP/EABQQAQAAAAAAAAAAAAAAAAAAACD/2gAIAQEAAT8Af//Z'
      }
    },
    async overlayReady() {
      // 空实现：mock 里没有真窗口
    },
    async focusOverlay() {
      // 空实现：mock 里没有真窗口
    },
    async commitOverlay() {
      // 空实现：mock 里没有真窗口
    },
    async cancelOverlay() {
      // 空实现：mock 里没有真窗口
    },
    async saveOverlay() {
      // 空实现：mock 里没有真文件系统
    },
    async pinOverlay() {
      // 空实现：mock 里没有真窗口。返回 0 让调用方走通「贴图后收起覆盖层」。
      return 0
    },
    async getPinImage() {
      // 一张 1x1 的占位图：够让贴图窗口跑通，不占地方。
      return { mime: 'image/jpeg', width: 1, height: 1, dataBase64: '' }
    },
    async closePin() {
      // 空实现：mock 里没有真窗口
    },
    async sendScreenshotDraft() {
      // mock 里没有真实的图片消息可造：返回一条占位消息，
      // 让「草稿 → 发送 → 气泡出现」这条链路在 mock 模式下也能走通。
      return {
        msgId: `mock-shot-${Date.now()}`,
        convId: '',
        senderId: 'self',
        direction: 'out',
        content: '',
        msgType: 'image',
        state: 'sent',
        sentAt: Date.now()
      }
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

    async testSeed(addr) {
      // 预览模式的拟真规则（与真机上的三种典型故障一一对应）：
      //   缺端口 → 格式错误；192.168.9.x → UDP 不通；192.168.2.x → UDP 通但 TCP 被挡。
      const raw = String(addr).trim()
      const base = {
        addr: raw,
        ok: false,
        udpOk: false,
        tcpOk: false,
        rttMs: 0,
        nodeId: '',
        displayName: '',
        subnet: '',
        tcpPort: 0,
        error: ''
      }
      if (!/:\d+$/.test(raw)) {
        return {
          ...base,
          error: '地址格式不对，应写成 IP:UDP端口（例如 192.168.2.10:2425）'
        }
      }
      await sleep(300)
      if (raw.includes('192.168.9')) {
        return { ...base, error: 'UDP 探测无应答：i/o timeout' }
      }
      const ok = {
        ...base,
        udpOk: true,
        tcpOk: true,
        ok: true,
        rttMs: 3 + Math.floor(Math.random() * 20),
        nodeId: 'a1b2c3d4e5f60718',
        displayName: 'seed-' + raw.split(':')[0].split('.').pop(),
        subnet: raw.replace(/:\d+$/, '') + '.0/24',
        tcpPort: 2425
      }
      if (raw.includes('192.168.2')) {
        return {
          ...ok,
          ok: false,
          tcpOk: false,
          error: `UDP 通了，但 TCP ${raw.split(':')[0]}:2425 连不上：跨网段目录拉不到，检查防火墙与路由`
        }
      }
      return ok
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
    // ---------------------------------------------------------------- 历史搜索
    //
    // 与后端 SearchService.Search 的语义逐条对齐（ADR-013）：
    //   - 空白关键词返回空（不查库）；
    //   - 白名单为空 = 「可见会话集合」：mock 里没有「已移除的对端」这回事，
    //     因此等价于全部已知会话；
    //   - 游标指向不存在的消息时返回空，【绝不】退化成首页 ——
    //     退化会让前端以为「又翻出一批」而把第一页重复贴一遍；
    //   - 排序是写入序号倒序（等价 sqlite 的 ORDER BY id DESC），不是 sent_at。
    async searchHistory(text, convIds, limit, beforeMsgId) {
      const needle = text.trim()
      if (!needle) return []

      const pageSize = limit > 0 ? limit : 50
      const allow = new Set(convIds.length ? convIds : [...threads.keys()])

      let cursor = 0
      if (beforeMsgId) {
        const c = seqByMsg.get(beforeMsgId)
        if (c === undefined) return []
        cursor = c
      }

      const hits: SeqMessage[] = []
      for (const [convId, list] of threads) {
        if (!allow.has(convId)) continue
        const clearedAtMs = clearedOf(convId)
        for (const m of list) {
          const seq = seqByMsg.get(m.msgId) ?? 0
          if (cursor > 0 && seq >= cursor) continue
          if (m.sentAt <= clearedAtMs) continue
          if (!likeContains(m.content, needle)) continue
          hits.push({ seq, m })
        }
      }
      hits.sort((a, b) => b.seq - a.seq)
      return hits.slice(0, pageSize).map((h) => h.m)
    },
    // 与后端 SearchService.JumpContext 对齐：锚点不存在 / 不属于该会话 /
    // 已被清空时一律返回空数组（调用方据此回退到会话最新一页，不当作错误）。
    async jumpContext(convId, msgId, before, after) {
      if (!convId || !msgId) return []
      const b = Math.max(0, before)
      const a = Math.max(0, after)

      const list = visibleOf(convId)
      const anchor = list.find((m) => m.msgId === msgId)
      if (!anchor) return []

      const anchorEntry: SeqMessage = { seq: seqByMsg.get(msgId) ?? 0, m: anchor }
      const older: SeqMessage[] = []
      const newer: SeqMessage[] = []
      for (const m of list) {
        const e: SeqMessage = { seq: seqByMsg.get(m.msgId) ?? 0, m }
        // 锚点本身归入旧侧（后端 `(sent_at, id) <= 锚点` 那一支）。
        if (byDisplay(anchorEntry, e) < 0) newer.push(e)
        else older.push(e)
      }

      // 显示序：旧侧要「紧邻锚点之前的 b 条」（含锚点，故 b+1），故按显示序倒排后取头；
      // 新侧同理要「紧邻锚点之后的 a 条」，按显示序正排后取头。取成整段尾部就错了。
      older.sort(byDisplay)
      older.reverse()
      newer.sort(byDisplay)
      const olderCut = older.slice(0, b + 1)
      const newerCut = newer.slice(0, a)

      // 新侧翻成新→旧接在前面，再接旧侧 —— 与后端 Window 的返回方向一致。
      const out: Message[] = []
      for (let i = newerCut.length - 1; i >= 0; i--) out.push(newerCut[i].m)
      for (const e of olderCut) out.push(e.m)
      return out
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
    async sendFolderTo(peerIds, path): Promise<FolderSummary> {
      const name = baseName(path)
      if (!name) throw new Error('请选择文件夹')
      if (!peerIds.length) throw new Error('没有可发送的成员')
      // 全批共享一个 batch_id：卡片进度按「文件夹里有几个文件」算，而不是乘成员数。
      const batchId = `mock-batch-${Date.now()}-${Math.random().toString(16).slice(2, 6)}`
      const entries = [
        { name: '2024/01/a.jpg', size: 3 * 1024 * 1024 },
        { name: '2024/02/a.jpg', size: 3 * 1024 * 1024 },
        { name: '空目录', size: 0, isDir: true },
        { name: '报告.pdf', size: 12 * 1024 * 1024 },
        { name: 'readme.txt', size: 4 * 1024 }
      ]
      let last: FolderSummary | null = null
      for (const pid of peerIds) last = simulateFolder(pid, name, entries, batchId)
      // peerIds 非空，循环至少跑一次。
      return last as FolderSummary
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
    //
    // 判据与后端 CancelJob 对齐（只有本机发出的、且未结束的任务能取消；
    // 「正在传」也允许）：预览模式下按钮的可用性与点下去的反馈才和真机一致。
    //
    // 改完状态必须自己 emit 一次 transfer:state —— 真机是后端推这条事件，
    // 而这里改的是夹具数组里的【原始对象】，绕过 store 那份响应式副本，
    // 不推事件的话界面要等下一次别的渲染才更新（表现为「点了没反应」）。
    async cancelTransfer(jobId) {
      const j = jobs.find((x) => x.jobId === jobId)
      if (!j) throw new Error('任务不存在')
      if (j.direction === 'recv') throw new Error('只能取消本机发出的任务')
      if (j.status === 'done' || j.status === 'cancelled') {
        throw new Error('任务已结束，无需取消')
      }
      j.status = 'cancelled'
      j.waitReason = undefined
      stopSimTimer(jobId)
      emit(EV.transferState, { jobId, status: 'cancelled' })
    },
    async retryTransfer(jobId) {
      const j = jobs.find((x) => x.jobId === jobId)
      if (!j) throw new Error('任务不存在')
      j.waitReason = undefined
      // 后端重试后落的是 idle（等待原因清空、等调度重新捡起），不是造一个
      // 叫 active 的新状态 —— 夹具里的每个 status 都必须是后端真会写出来的值。
      j.status = 'idle'
      emit(EV.transferState, { jobId, status: 'idle' })
    },
    // 暂停/恢复的判据与后端 PauseJob/ResumeJob 对齐（只有正在传的能暂停、
    // 只有暂停中的能恢复）：预览模式下按钮的可用性与点下去的反馈才和真机一致。
    async pauseTransfer(jobId) {
      const j = jobs.find((x) => x.jobId === jobId)
      if (!j) throw new Error('任务不存在')
      if (j.status !== 'transferring' && j.status !== 'meta_exchange') {
        throw new Error('只能暂停正在传输的任务')
      }
      j.status = 'paused'
      j.updatedAt = Date.now()
      stopSimTimer(jobId)
      emit(EV.transferState, { jobId, status: 'paused' })
    },
    async resumeTransfer(jobId) {
      const j = jobs.find((x) => x.jobId === jobId)
      if (!j) throw new Error('任务不存在')
      if (j.status !== 'paused') throw new Error('任务未暂停')
      j.status = 'transferring'
      j.updatedAt = Date.now()
      startSimTimer(jobId)
      emit(EV.transferState, { jobId, status: 'transferring' })
    },

    async self() {
      return { ...self }
    },
    async peerList() {
      return [...peers]
    },
    async removePeer(nodeId) {
      // 预览模式没有后端，也没有「自动恢复」这回事：这里就是本地摘除。
      // 真实实现只在本机打一个「已移除」标记（可被有效通告或握手自动解除，历史也不删），
      // 因此这条路径只保证「界面上立即消失」这一点与真实实现一致。
      const i = peers.findIndex((p) => p.nodeId === nodeId)
      if (i >= 0) peers.splice(i, 1)
    },
    async shareList(): Promise<Share[]> {
      return shares.map((s) => ({ ...s }))
    },
    async sharePublish(rootPath, name, visType, allowed): Promise<Share> {
      if (!rootPath.trim()) throw new Error('请选择要共享的目录或文件')
      if (!name.trim()) throw new Error('共享名不能为空')
      if (visType === 'users' && !allowed.length) {
        throw new Error('share: 可见性为「指定人」时必须至少选择一个人')
      }
      const shareName = name.trim()
      // 预览模式读不到真实目录，因此造一份形状真实的小清单：
      // 关键是「文件数 / 总大小」由清单算出，界面上的卡片才会与真机一致。
      const items: ShareItem[] =
        visType === 'group'
          ? [{ path: '联调记录/readme.txt', name: 'readme.txt', size: 4096 }]
          : MY_ITEMS.map((i) => ({ ...i }))
      const created: Share = {
        shareId: `mock-share-${Date.now()}`,
        name: shareName,
        ownerId: self.nodeId,
        ownerName: self.displayName,
        fileCount: items.length,
        totalSize: items.reduce((sum, i) => sum + i.size, 0),
        visibility: { type: visType, allowed: visType === 'all' ? [] : [...allowed] },
        visibilityText: visText(visType, allowed.length),
        mine: true,
        rootPath,
        online: true,
        downloadable: false,
        createdAt: Date.now(),
        updatedAt: Date.now()
      }
      shares.unshift(created)
      shareItems.set(created.shareId, items)
      // 真实后端会发 share:updated（本机共享清单变化），走查时这条链路也要通。
      emit(EV.shareUpdated, { shareId: created.shareId, action: 'published' })
      return { ...created }
    },
    async shareUnpublish(shareId) {
      const i = shares.findIndex((s) => s.shareId === shareId)
      if (i >= 0) shares.splice(i, 1)
      shareItems.delete(shareId)
      emit(EV.shareUpdated, { shareId, action: 'unpublished' })
    },
    async shareSetVisibility(shareId, visType, allowed): Promise<Share> {
      const s = shares.find((x) => x.shareId === shareId)
      if (!s) throw new Error('share: 找不到这条共享')
      // 两条取值规则与后端一致：空名单会让共享变成「谁都不许看」，
      // 「仅群组」多于一个群则无从判定 —— 都必须在界面这一侧就被拒。
      if (visType === 'users' && !allowed.length) {
        throw new Error('share: 可见性为「指定人」时必须至少选择一个人')
      }
      if (visType === 'group' && allowed.length !== 1) {
        throw new Error('share: 可见性为「仅群组」时必须且只能指定一个群')
      }
      s.visibility = { type: visType, allowed: visType === 'all' ? [] : [...allowed] }
      s.visibilityText = visText(visType, allowed.length)
      s.updatedAt = Date.now()
      emit(EV.shareUpdated, { shareId, action: 'visibility' })
      return { ...s }
    },
    async shareRefresh(shareId): Promise<Share> {
      const s = shares.find((x) => x.shareId === shareId)
      if (!s) throw new Error('share: not found')
      // 「刷新索引」的真实效果是清单可能变多或变少；预览里只更新时间戳，
      // 因为凭空改文件数会让界面上的数字与清单自相矛盾。
      s.updatedAt = Date.now()
      emit(EV.shareUpdated, { shareId, action: 'refreshed' })
      return { ...s }
    },
    async shareBrowse(shareId): Promise<ShareItem[]> {
      const s = shares.find((x) => x.shareId === shareId)
      if (!s || !s.online) {
        // bob 离线：浏览必须失败，界面才能走查到「共享方不在线」这条提示。
        throw new Error(`share: 共享方不在线（${s?.ownerName ?? '未知用户'}）`)
      }
      return itemsOf(shareId)
    },
    async shareDownload(shareId, paths): Promise<ShareDownload> {
      const s = shares.find((x) => x.shareId === shareId)
      if (!s) throw new Error('share: 找不到这条共享')
      if (!s.downloadable) throw new Error(`share: 共享方不在线（${s.ownerName}）`)
      const picked = itemsOf(shareId).filter((i) => paths.includes(i.path))
      if (!picked.length) throw new Error('共享方没有受理任何文件（清单可能已过期，请刷新后重试）')
      return simulateShareDownload(s.ownerId, s.name, picked)
    },

    async getBackground() {
      return cloneAppearance()
    },
    async saveBackground(dataBase64, ext) {
      const mime = ext.replace('.', '').toLowerCase()
      mockAppearance.dataUrl = `data:image/${mime === 'jpg' ? 'jpeg' : mime};base64,${dataBase64}`
      mockAppearance.fileName = `background.${ext.replace('.', '')}`
      return cloneAppearance()
    },
    async clearBackground() {
      mockAppearance.dataUrl = ''
      mockAppearance.fileName = ''
      return cloneAppearance()
    },
    async setBackgroundLook(dim, blur) {
      mockAppearance.dim = Math.min(100, Math.max(0, Math.round(dim)))
      mockAppearance.blur = Math.min(40, Math.max(0, Math.round(blur)))
      return cloneAppearance()
    },

    async getUpdateState() {
      return snapshotUpdate()
    },
    async checkUpdateNow() {
      void runMockUpdateCheck()
      return snapshotUpdate()
    },
    async applyUpdateNow() {
      // 浏览器里没有安装器也没有可重启的进程：只把阶段推到 applying，
      // 保证「点了立即更新 → 浮层收起 → 显示正在安装」这条链路能走查。
      mockUpdate.phase = 'applying'
      publishUpdate()
      return snapshotUpdate()
    },
    async scheduleUpdate(atMillis) {
      if (!(atMillis > 0)) throw new Error('请选择安装时间')
      mockUpdate.phase = 'scheduled'
      mockUpdate.scheduledAt = atMillis
      mockUpdate.promptPending = false
      publishUpdate()
      return snapshotUpdate()
    },
    async snoozeUpdate() {
      mockUpdate.promptPending = false
      publishUpdate()
      return snapshotUpdate()
    },
    async cancelUpdate() {
      Object.assign(mockUpdate, emptyUpdate())
      publishUpdate()
      return snapshotUpdate()
    },

    async diagnostics() {
      // 两个计数现算：mock 里删过节点之后，创建时的快照会与列表对不上
      return {
        ...diagnostics,
        peersTotal: peers.length,
        peersOnline: peers.filter((p) => p.online).length
      }
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
        // 群主自己是 1 号票；每加一个成员递增 —— 与后端「群级票号」同构。
        mintSeq: 1,
        state: 'active',
        isOwner: true,
        members: [
          {
            nodeId: self.nodeId,
            displayName: self.displayName,
            role: 'owner',
            state: 'active',
            active: true,
            admitSeq: 1,
            kickSeq: 0,
            leftSeq: 0
          },
          ...memberIds.map((id, i) => ({
            nodeId: id,
            displayName: peers.find((p) => p.nodeId === id)?.displayName ?? id.slice(0, 8),
            role: 'member',
            state: 'active',
            active: true,
            admitSeq: i + 2,
            kickSeq: 0,
            leftSeq: 0
          }))
        ],
        createdAt: Date.now()
      }
      g.mintSeq = g.members.length
      groups.push(g)
      return g
    },
    async groupAddMember(groupId, memberId) {
      const g = groups.find((x) => x.groupId === groupId) ?? groups[0]
      // 发票：票号取自群级计数器，因此【必然】大于该成员此前的任何票号 ——
      // 被踢/已退出的人重新拉回必定生效（这正是群级票号的意义）。
      const seq = ++g.mintSeq
      const m = g.members.find((x) => x.nodeId === memberId)
      if (m) {
        m.admitSeq = seq
        m.active = true
        m.state = 'active'
      } else {
        g.members.push({
          nodeId: memberId,
          displayName: peers.find((p) => p.nodeId === memberId)?.displayName ?? memberId.slice(0, 8),
          role: 'member',
          state: 'active',
          active: true,
          admitSeq: seq,
          kickSeq: 0,
          leftSeq: 0
        })
      }
      g.epoch += 1
      return { ...g }
    },
    async groupRemoveMember(groupId, memberId) {
      const g = groups.find((x) => x.groupId === groupId) ?? groups[0]
      const m = g.members.find((x) => x.nodeId === memberId)
      if (m) {
        // 撕票：撕票号【等于】票号 ⇒ active=false。写成「减一」或「动 state」
        // 都不是这个模型，预览态也会跟着长歪。
        m.kickSeq = m.admitSeq
        m.active = false
        m.state = 'kicked'
      }
      g.epoch += 1
      return { ...g }
    },
    async groupLeave(groupId) {
      const g = groups.find((x) => x.groupId === groupId) ?? groups[0]
      const m = g.members.find((x) => x.nodeId === self.nodeId)
      if (m) {
        m.leftSeq = m.admitSeq
        m.active = false
        m.state = 'left'
      }
      // 刻意【不】动 epoch：退出不经群主，名册块没变 —— 与后端一致。
      return { ...g }
    },
    async groupRename(groupId, name) {
      const g = groups.find((x) => x.groupId === groupId) ?? groups[0]
      g.name = name
      g.epoch += 1
      return { ...g }
    },
    async groupDissolve(groupId) {
      const g = groups.find((x) => x.groupId === groupId) ?? groups[0]
      g.state = 'dissolved'
      g.epoch += 1
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
        // 预览态带一个未送达计数，用来走查「N 人未送达」；送达事件到达后清零。
        memberCount: 3,
        pendingCount: 1,
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
    },
    async groupSendFolder(groupId, batchId, folderName, fileCount, totalSize, skipped) {
      const m: Message = {
        msgId: `mock-g-folder-${Date.now()}`,
        convId: groupId,
        senderId: self.nodeId,
        direction: 'out',
        content: JSON.stringify({
          batch_id: batchId,
          folder_name: folderName,
          file_count: fileCount,
          total_size: totalSize,
          skipped
        }),
        msgType: 'folder',
        state: 'pending',
        memberCount: 3,
        pendingCount: 1,
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
    connected,
    // 演示数据：只有真有会话才谈得上延迟。0 = 没数据（界面显示「测量中」）。
    rttMs: connected ? 12 : 0
  }
}
