/**
 * 「清空聊天区」与「查看历史」两个按钮的可用性。
 *
 * 为什么值得单独抽出来：这两个条件写错的后果都是"看起来像功能坏了"——
 * 在空会话上点清空（什么也没发生）、或在一个没清空过的会话上点查看历史
 * （同样什么也没发生），用户只会认为按钮是坏的。
 */

/** 有消息才谈得上清空。 */
export function canClear(messageCount: number): boolean {
  return messageCount > 0
}

/** 有清空点才谈得上「查看历史」；0 = 未清空。 */
export function canRestore(clearedAt: number): boolean {
  return clearedAt > 0
}
