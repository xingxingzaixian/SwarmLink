import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { getApi, type Conversation, type FolderSummary, type Group, type Message } from '../api'
import { directConvId, isGroupId } from '../api/ids'
import { createDeliveryLedger } from './delivery'
import { usePeersStore } from './peers'

/** 会话与消息状态。单聊与群聊共用同一套结构（conv_id 统一）。 */
export const useChatStore = defineStore('chat', () => {
  const conversations = ref<Conversation[]>([])
  const messagesByConv = ref<Record<string, Message[]>>({})
  const unread = ref<Record<string, number>>({})
  /**
   * 会话列表的「最后一条消息」预览。
   *
   * 单独一张表而不是复用 messagesByConv：预览只需要 1 条，
   * 而 messagesByConv 是聊天区的完整历史 —— 混用会让打开会话的瞬间
   * 先渲染出 1 条再做全量替换，出现可见的闪烁。
   */
  const previews = ref<Record<string, Message>>({})
  const activeConvId = ref('')
  const loading = ref(false)
  const error = ref('')

  /**
   * 跳转定位的目标消息（由搜索弹层触发，见 openAt）。
   *
   * 为什么只存一个「意图」、而不是在 openAt 里直接 scrollIntoView：
   *   - 滚动需要 DOM，store 拿不到 ChatView 的滚动容器；
   *   - 更要紧的是【时机】—— 跳转窗口要到下一次渲染才进 DOM，
   *     在 openAt 里同步 querySelector 必然查不到（拿到 null 而静默不滚）。
   * 所以由 ChatView 在渲染后消费，消费完（含 2s 高亮）调 clearAnchor 撤掉。
   */
  const anchorMsgId = ref('')

  /**
   * 会话内的提示条：「不是错误，但不能静默」的那类事。
   *
   * 与 error 分开的理由是生命周期：error 会被下一次 loadHistory 开头清空，
   * 而这条提示恰恰是在 loadHistory【之后】才成立的（「跳转失败 → 已回退最新」），
   * 塞进同一个字段必然互相踩。
   */
  const notice = ref('')

  /**
   * 每个会话的「清空点」（毫秒；缺省/0 = 未清空）。
   *
   * 这是【视图规则】，不参与任何后端查询：被清空的消息仍在
   * `messagesByConv` 里，所以撤销清空时无需重新请求，界面立刻恢复。
   * 由此也带来一条纪律 —— 清空点绝不能拿去当 History 的参数，
   * 否则左栏预览（复用同一个 History）会跟着一起空掉。
   */
  const clearedAt = ref<Record<string, number>>({})

  /** 送达状态账本：让「事件先到」与「消息先到」两种顺序都收敛到已完成。 */
  const delivery = createDeliveryLedger()

  const activeMessages = computed(() =>
    (messagesByConv.value[activeConvId.value] ?? []).filter(
      (m) => m.sentAt > (clearedAt.value[activeConvId.value] ?? 0)
    )
  )
  const activeConversation = computed(
    () => conversations.value.find((c) => c.convId === activeConvId.value) ?? null
  )

  /** 当前会话的对端节点 ID；群聊为空。右栏据此取资料与传输任务。 */
  const activePeerId = computed(() => activeConversation.value?.peerId ?? '')

  const activeIsGroup = computed(() => activeConversation.value?.kind === 'group')

  async function loadConversations(): Promise<void> {
    try {
      const api = await getApi()
      const [direct, groups] = await Promise.all([api.conversations(), api.groupList()])
      const merged: Conversation[] = [...direct]
      for (const g of groups as Group[]) {
        if (!merged.some((c) => c.convId === g.groupId)) {
          merged.push({
            convId: g.groupId,
            kind: 'group',
            title: g.name,
            isOnline: true,
            lastTime: g.createdAt
          })
        }
      }
      merged.sort((a, b) => b.lastTime - a.lastTime)
      conversations.value = merged
    } catch (e: any) {
      error.value = String(e?.message ?? e)
      return
    }
    await primePreviews()
  }

  /**
   * 补齐会话列表的消息预览。
   *
   * 每个会话单独取 1 条：局域网内节点数上限 20，一次并发的开销可以忽略，
   * 而它换来的是左栏「像聊天软件」而不是「像通讯录」。
   * 失败不报错 —— 预览是锦上添花，不能因为它挡住会话列表。
   */
  async function primePreviews(): Promise<void> {
    const api = await getApi()
    await Promise.all(
      conversations.value.map(async (c) => {
        if (previews.value[c.convId]) return
        try {
          const one =
            c.kind === 'group'
              ? await api.groupHistory(c.convId, 1, 0)
              : await api.history(c.peerId ?? '', 1, 0)
          if (one.length) previews.value = { ...previews.value, [c.convId]: one[0] }
        } catch {
          /* 预览失败可忽略 */
        }
      })
    )
  }

  /** 某会话的最后一条消息（左栏预览）。 */
  function previewOf(convId: string): Message | undefined {
    return previews.value[convId]
  }

  async function open(conv: Conversation): Promise<void> {
    activeConvId.value = conv.convId
    delete unread.value[conv.convId]
    notice.value = ''
    anchorMsgId.value = ''
    // 两件事都要等：只等历史会让「已清空的会话」先闪一下全部消息再消失
    await Promise.all([loadHistory(conv), loadClearedAt(conv.convId)])
  }

  /**
   * 跳转窗口的取数范围：向上多取、向下少取。
   *
   * 不对称是有意的 —— 锚点会被滚到视口中央，上方那 40 条是「前情提要」，
   * 下方 20 条是「后来怎样了」。取成对称的话，用户想看前情就得先往上翻。
   */
  const JUMP_BEFORE = 40
  const JUMP_AFTER = 20

  /**
   * 打开会话并定位到某条消息（搜索结果点击、P3）。
   *
   * 与 open 的区别只有一处：历史不是「最新 100 条」，而是 JumpContext 给的
   * 【以该条为中心的一页】—— 消息可能在几千条之前，靠 Latest 翻不到。
   *
   * 这条路径有两个必须守住的边界：
   *   1. 空窗口 = 后端约定的「正常用户路径」（不存在 / 不属于该会话 /
   *      已被清空点隐藏），此时回退到最新一页并【明确提示】，不静默失败；
   *   2. 失败也要让会话可用 —— 否则用户点完搜索结果面对的是一个空聊天区，
   *      而他连「我在哪个会话里」都看不出来。
   */
  async function openAt(conv: Conversation, msgId: string): Promise<void> {
    if (!conv || !msgId) return
    activeConvId.value = conv.convId
    delete unread.value[conv.convId]
    error.value = ''
    notice.value = ''
    anchorMsgId.value = ''
    loading.value = true
    try {
      const api = await getApi()
      const win = await api.jumpContext(conv.convId, msgId, JUMP_BEFORE, JUMP_AFTER)
      // 清空点要一起取：窗口已被后端按清空点过滤，但 activeMessages 还会按
      // 本地这份再过滤一次（撤销清空后要能立刻看到更多）。两边不同步时，
      // 表现是「跳过去只看到一个洞」—— 而这条消息其实就在窗口里。
      await loadClearedAt(conv.convId)

      if (!win.length) {
        await loadHistory(conv)
        notice.value = '这条消息已不在了（可能已被清空），已回到最新消息'
        return
      }
      // 后端返回「新→旧」，界面按时间正序渲染 —— 与 loadHistory 同一条方向约定
      messagesByConv.value[conv.convId] = [...win].reverse()
      anchorMsgId.value = msgId
    } catch (e: any) {
      // 失败也把会话加载出来（见上「边界 2」），再报错
      await loadHistory(conv)
      error.value = String(e?.message ?? e)
    } finally {
      loading.value = false
    }
  }

  /** 锚点已被 ChatView 消费（滚完 + 高亮结束）后撤掉。 */
  function clearAnchor(): void {
    anchorMsgId.value = ''
  }

  async function loadHistory(conv: Conversation): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      const api = await getApi()
      const list =
        conv.kind === 'group'
          ? await api.groupHistory(conv.convId, 100, 0)
          : await api.history(conv.peerId ?? '', 100, 0)
      // 后端返回「新→旧」，界面按时间正序渲染
      messagesByConv.value[conv.convId] = [...list].reverse()
    } catch (e: any) {
      error.value = String(e?.message ?? e)
    } finally {
      loading.value = false
    }
  }

  /**
   * 拉取某会话的清空点。
   *
   * 失败按「未清空」（0）处理：宁可多显示消息，也不要让用户的聊天记录
   * 因为一次读错误而看起来丢了。
   */
  async function loadClearedAt(convId: string): Promise<void> {
    if (!convId) return
    try {
      const api = await getApi()
      const at = await api.clearedAt(convId)
      clearedAt.value = { ...clearedAt.value, [convId]: at }
    } catch {
      /* 读不到就当作未清空 */
    }
  }

  /** 清空一个会话的聊天区：消息不删除，只是不再显示。 */
  async function clearConversation(conv: Conversation): Promise<void> {
    const api = await getApi()
    await api.clearConversation(conv.convId)
    // 以后端返回的值为准，而不是本地 Date.now()：两者差一点就会让
    // 「刚发的最后一条」处在边界上，出现「清空后还剩一条」的怪象。
    await loadClearedAt(conv.convId)
  }

  /** 撤销清空，让被隐藏的消息重新显示。 */
  async function restoreConversation(conv: Conversation): Promise<void> {
    const api = await getApi()
    await api.restoreConversation(conv.convId)
    await loadClearedAt(conv.convId)
  }

  /** 某会话的清空点（毫秒；0 = 未清空）。 */
  function clearedAtOf(convId: string): number {
    return clearedAt.value[convId] ?? 0
  }

  /** 某会话是否处于「已清空」状态。 */
  function isCleared(convId: string): boolean {
    return clearedAtOf(convId) > 0
  }

  function upsert(convId: string, m: Message): void {
    // 进列表前先补齐送达状态：chat:delivered 可能比这条消息先到
    // （如 SendMessage 的返回值还在 IPC 路上，对端的 ACK 就已经回来了），
    // 那时事件找不到消息、只能被丢掉，全靠这里把状态补回来。
    const msg = delivery.reconcile(m)
    const list = messagesByConv.value[convId] ?? []
    const i = list.findIndex((x) => x.msgId === msg.msgId)
    if (i >= 0) {
      list[i] = { ...list[i], ...msg }
    } else {
      list.push(msg)
      list.sort((a, b) => a.sentAt - b.sentAt)
    }
    messagesByConv.value = { ...messagesByConv.value, [convId]: list }
  }

  /** 把会话顶到列表最前（有新消息时用）。 */
  function bump(convId: string, at: number): void {
    const i = conversations.value.findIndex((c) => c.convId === convId)
    if (i < 0) return
    const conv = { ...conversations.value[i], lastTime: at }
    conversations.value = [conv, ...conversations.value.filter((_, k) => k !== i)]
  }

  /** 入站消息（由 chat:new-message 事件驱动）。 */
  function receive(m: Message): void {
    upsert(m.convId, m)
    previews.value = { ...previews.value, [m.convId]: m }

    if (m.convId !== activeConvId.value) {
      unread.value[m.convId] = (unread.value[m.convId] ?? 0) + 1
    }

    // 未知会话（对方先开口）时补一条，避免消息「无处可去」
    if (!conversations.value.some((c) => c.convId === m.convId)) {
      const isGroup = isGroupId(m.convId)
      const title = isGroup ? m.convId.slice(0, 8) : usePeersStore().nameOf(m.senderId)
      conversations.value.unshift({
        convId: m.convId,
        kind: isGroup ? 'group' : 'direct',
        title,
        peerId: isGroup ? undefined : m.senderId,
        isOnline: true,
        lastTime: m.sentAt
      })
      return
    }
    bump(m.convId, m.sentAt)
  }

  async function send(conv: Conversation, text: string): Promise<void> {
    if (!text.trim()) return
    const api = await getApi()
    const m =
      conv.kind === 'group'
        ? await api.groupSendMessage(conv.convId, text)
        : await api.sendMessage(conv.peerId ?? '', text)
    upsert(conv.convId, m)
    previews.value = { ...previews.value, [conv.convId]: m }
    bump(conv.convId, m.sentAt)
  }

  /**
   * 发送图片消息。
   *
   * 与文本发送共用 upsert/bump：图片同样会等到 chat:delivered 事件回来，
   * 走同一条回填路径 —— 若各写一套，两条链路的送达状态迟早会分叉。
   */
  async function sendImage(conv: Conversation, path: string): Promise<void> {
    const api = await getApi()
    const m =
      conv.kind === 'group'
        ? await api.groupSendImage(conv.convId, path)
        : await api.sendImage(conv.peerId ?? '', path)
    upsert(conv.convId, m)
    previews.value = { ...previews.value, [conv.convId]: m }
    bump(conv.convId, m.sentAt)
  }

  /** 发送剪贴板图片（只有字节，没有本地路径）。 */
  async function sendImageBytes(conv: Conversation, name: string, dataB64: string): Promise<void> {
    const api = await getApi()
    const m =
      conv.kind === 'group'
        ? await api.groupSendImageBytes(conv.convId, name, dataB64)
        : await api.sendImageBytes(conv.peerId ?? '', name, dataB64)
    upsert(conv.convId, m)
    previews.value = { ...previews.value, [conv.convId]: m }
    bump(conv.convId, m.sentAt)
  }

  /**
   * 发送截图草稿。
   *
   * 走独立的「已压缩图片」通道：抓屏时已经按 1920px / 480KB 压过一次，
   * 若再走通用通道（sendImageBytes）会被压回 1280px / 300KB —— 4K 屏的
   * 截图会糊掉一大截，而这正是截屏最不该损失的东西。
   */
  async function sendScreenshot(
    conv: Conversation,
    name: string,
    mime: string,
    width: number,
    height: number,
    dataB64: string
  ): Promise<void> {
    const api = await getApi()
    const m = await api.sendScreenshotDraft(
      conv.kind,
      conv.kind === 'group' ? conv.convId : (conv.peerId ?? ''),
      name,
      mime,
      width,
      height,
      dataB64
    )
    upsert(conv.convId, m)
    previews.value = { ...previews.value, [conv.convId]: m }
    bump(conv.convId, m.sentAt)
  }

  /**
   * 发送文件消息。
   *
   * 与文本发送共用 upsert/bump：文件消息同样会等到 chat:delivered 事件回来，
   * 走同一条回填路径。
   */
  async function sendFile(conv: Conversation, jobId: string, fileName: string): Promise<void> {
    const api = await getApi()
    const m =
      conv.kind === 'group'
        ? await api.groupSendFile(conv.convId, jobId, fileName)
        : await api.sendFileMessage(conv.peerId ?? '', jobId, fileName)
    upsert(conv.convId, m)
    previews.value = { ...previews.value, [conv.convId]: m }
    bump(conv.convId, m.sentAt)
  }

  /**
   * 发送文件夹消息：一条聚合卡片，只让两端聊天框知道「有一个文件夹在路上」。
   *
   * 单聊与群聊都支持：群聊的卡片同样只是「一张说明」，文件字节由调用方
   * 对每个在线成员各自发起点对点传输（见 useFileSend 的群分支）。
   *
   * 与文本/文件发送共用 upsert/bump，因此同样会等到 chat:delivered 回来。
   */
  async function sendFolder(conv: Conversation, summary: FolderSummary): Promise<void> {
    const api = await getApi()
    const m =
      conv.kind === 'group'
        ? await api.groupSendFolder(
            conv.convId,
            summary.batchId,
            summary.folderName,
            summary.fileCount,
            summary.totalSize,
            summary.skipped
          )
        : await api.sendFolderMessage(
            conv.peerId ?? '',
            summary.batchId,
            summary.folderName,
            summary.fileCount,
            summary.totalSize,
            summary.skipped
          )
    upsert(conv.convId, m)
    previews.value = { ...previews.value, [conv.convId]: m }
    bump(conv.convId, m.sentAt)
  }

  /** 送达确认（由 chat:delivered 事件驱动）。 */
  function markDelivered(msgId: string): void {
    // 先记账：这条消息可能还没进列表（事件早于发送返回值），
    // 那就等 upsert 时由 reconcile 补上，否则气泡会一直停在「发送中」。
    delivery.ack(msgId)

    for (const [convId, list] of Object.entries(messagesByConv.value)) {
      const i = list.findIndex((m) => m.msgId === msgId)
      if (i >= 0) {
        // 群消息的 delivered 语义是「全员送达」（后端只在全员确认后才发这条事件），
        // 因此这里顺带把未送达计数清零，避免「已送达」与「N 人未送达」自相矛盾。
        list[i] = { ...list[i], state: 'delivered', pendingCount: 0 }
        messagesByConv.value = { ...messagesByConv.value, [convId]: [...list] }
        return
      }
    }
  }

  /**
   * 打开与某节点（或某群）的会话。
   *
   * 单聊 conv_id 由 directConvId 对称算出（见 api/ids.ts）——
   * 若两端算出的结果不一致，同一段会话会在两端分裂成两条。
   */
  async function openPeer(peerId: string, title: string): Promise<void> {
    const isGroup = isGroupId(peerId)
    const selfId = usePeersStore().self?.nodeId ?? ''

    // 单聊必须先知道自己的 node_id 才能算出 conv_id。
    // 这里宁可明确报错，也不要静默拼出 ":xxx" 这种畸形 ID。
    if (!isGroup && !selfId) {
      error.value = '本机身份尚未就绪，请稍后重试'
      return
    }

    const convId = isGroup ? peerId : directConvId(selfId, peerId)
    const existing = conversations.value.find((c) => c.convId === convId)

    if (existing) {
      // 已有会话：只更新标题（对方可能改了显示名）
      bump(convId, Date.now())
      const i = conversations.value.findIndex((c) => c.convId === convId)
      conversations.value[i] = { ...conversations.value[i], title, isOnline: true }
      await open(conversations.value[i])
      return
    }

    // 首次聊天：会话表里还没有它，必须补进去。
    // 否则 activeConversation 是 null，聊天区会一直显示「未选择会话」。
    const conv: Conversation = {
      convId,
      kind: isGroup ? 'group' : 'direct',
      title,
      peerId: isGroup ? undefined : peerId,
      isOnline: true,
      lastTime: Date.now()
    }
    conversations.value = [conv, ...conversations.value]
    await open(conv)
  }

  /** 未读数（左栏角标）。 */
  function unreadOf(convId: string): number {
    return unread.value[convId] ?? 0
  }

  return {
    conversations,
    messagesByConv,
    previews,
    unread,
    activeConvId,
    loading,
    error,
    notice,
    anchorMsgId,
    activeMessages,
    activeConversation,
    activePeerId,
    activeIsGroup,
    loadConversations,
    open,
    openAt,
    clearAnchor,
    openPeer,
    loadHistory,
    clearedAt,
    loadClearedAt,
    clearConversation,
    restoreConversation,
    clearedAtOf,
    isCleared,
    previewOf,
    unreadOf,
    receive,
    send,
    sendImage,
    sendImageBytes,
    sendScreenshot,
    sendFile,
    sendFolder,
    markDelivered
  }
})
