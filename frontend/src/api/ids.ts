/**
 * ID 形态约定。
 *
 * 前端拿到的都是一串 hex / uuid，没有类型信息，只能靠【形态】区分。
 * 这套形态必须与 Go 侧严格对应（internal/domain/identity、group、message），
 * 否则同一段会话会在两端分裂成两条：
 *
 *   node_id      16 位 hex（8 字节公钥指纹）      ← identity.NodeID.String()
 *   group_id     32 位 hex（16 字节）             ← group.NewID() = SHA-256(...)[:16]
 *   msg_id/job_id  36 字符 UUIDv7                 ← message.NewID()
 *   单聊 conv    "<16hex>:<16hex>"（33 字符）      ← message.DirectConvID
 *   群聊 conv    = group_id（32 字符）             ← message.GroupConvID
 *
 * 因此「是群还是人」只能看长度：16 是人、32 是群。
 * 不要改用「有没有冒号」之外的其它特征来猜 —— 一旦 node_id 长度变了，
 * 这里必须同步改，否则所有单聊都会被误判成群聊。
 */

/** 是否为群 ID（32 位 hex）。 */
export function isGroupId(id: string): boolean {
  return id.length === 32 && !id.includes(':')
}

/**
 * 计算单聊会话 ID。
 *
 * 必须【对称】：两个 NodeID 排序后以 ':' 连接（Go 侧 message.DirectConvID）。
 * hex 字符串的字典序与字节序一致，因此这里的排序结果与 Go 的 NodeID.Less 完全相同。
 */
export function directConvId(selfId: string, peerId: string): string {
  return [selfId, peerId].sort().join(':')
}
