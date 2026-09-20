import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { getApi, type Conversation, type Group, type Message } from '../api'
import { usePeersStore } from './peers'

/** 会话与消息状态。单聊与群聊共用同一套结构（conv_id 统一）。 */
export const useChatStore = defineStore('chat', () => {
  const conversations = ref<Conversation[]>([])
  const messagesByConv = ref<Record<string, Message[]>>({})
  const unread = ref<Record<string, number>>({})
  const activeConvId = ref('')
  const loading = ref(false)
  const error = ref('')

  const activeMessages = computed(() => messagesByConv.value[activeConvId.value] ?? [])
  const activeConversation = computed(
    () => conversations.value.find((c) => c.convId === activeConvId.value) ?? null
  )

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
    }
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

  /** 入站消息（由 chat:new-message 事件驱动）。 */
  function receive(m: Message): void {
    upsert(m.convId, m)
    if (m.convId !== activeConvId.value) {
      unread.value[m.convId] = (unread.value[m.convId] ?? 0) + 1
    }
    // 未知会话（对方先开口）时补一条，避免消息「无处可去」
    if (!conversations.value.some((c) => c.convId === m.convId)) {
      const isGroup = m.convId.length === 32 && !m.convId.includes(':')
      conversations.value.unshift({
        convId: m.convId,
        kind: isGroup ? 'group' : 'direct',
        title: isGroup ? m.convId.slice(0, 8) : m.senderId.slice(0, 8),
        peerId: isGroup ? undefined : m.senderId,
        isOnline: true,
        lastTime: m.sentAt
      })
    }
  }

  async function send(conv: Conversation, text: string): Promise<void> {
    if (!text.trim()) return
    const api = await getApi()
    const m =
      conv.kind === 'group'
        ? await api.groupSendMessage(conv.convId, text)
        : await api.sendMessage(conv.peerId ?? '', text)
    upsert(conv.convId, m)
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
   * 单聊 conv_id 必须与后端【对称】算出：两个 NodeID 排序后以 ':' 连接
   * （Go 侧 message.DirectConvID）。hex 字符串的字典序与字节序一致，
   * 因此这里的排序结果与 Go 的 NodeID.Less 完全相同 —— 若不一致，
   * 同一段会话会在两端分裂成两条。
   */
  async function openPeer(peerId: string, title: string): Promise<void> {
    const selfId = usePeersStore().self?.nodeId ?? ''
    const isGroup = peerId.length === 32 && !peerId.includes(':')

    const convId = isGroup ? peerId : [selfId, peerId].sort().join(':')
    await open({
      convId,
      kind: isGroup ? 'group' : 'direct',
      title,
      peerId: isGroup ? undefined : peerId,
      isOnline: true,
      lastTime: Date.now()
    })
  }

  return {
    conversations,
    messagesByConv,
    unread,
    activeConvId,
    loading,
    error,
    activeMessages,
    activeConversation,
    loadConversations,
    open,
    openPeer,
    loadHistory,
    receive,
    send,
    markDelivered
  }
})
