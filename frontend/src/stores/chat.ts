import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { getApi, type Conversation, type Group, type Message } from '../api'
import { directConvId, isGroupId } from '../api/ids'
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

  const activeMessages = computed(() => messagesByConv.value[activeConvId.value] ?? [])
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
    await loadHistory(conv)
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

  function upsert(convId: string, m: Message): void {
    const list = messagesByConv.value[convId] ?? []
    const i = list.findIndex((x) => x.msgId === m.msgId)
    if (i >= 0) {
      list[i] = { ...list[i], ...m }
    } else {
      list.push(m)
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

  /** 送达确认（由 chat:delivered 事件驱动）。 */
  function markDelivered(msgId: string): void {
    for (const [convId, list] of Object.entries(messagesByConv.value)) {
      const i = list.findIndex((m) => m.msgId === msgId)
      if (i >= 0) {
        list[i] = { ...list[i], state: 'delivered' }
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
    activeMessages,
    activeConversation,
    activePeerId,
    activeIsGroup,
    loadConversations,
    open,
    openPeer,
    loadHistory,
    previewOf,
    unreadOf,
    receive,
    send,
    markDelivered
  }
})
