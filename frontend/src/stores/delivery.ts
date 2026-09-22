/**
 * 送达状态的账本。
 *
 * 为什么需要它：后端的 chat:delivered 事件与「发送 RPC 的返回」是两条独立的
 * 异步路径，谁先到是竞态。同机回环下对端几百微秒就回了 ACK，而返回值还要
 * 经过 DTO 序列化 + Wails IPC —— 事件经常【先到】。
 *
 * 而返回值里的 state 永远是发出时的 pending 快照。若只在收到事件时给列表里
 * 已有的消息改状态，那么「事件先到」的那一次就会：事件找不到消息（丢弃）→
 * 随后消息以 pending 写进列表 → 气泡永久停在「发送中」，
 * 而数据库里其实早已是 delivered。
 *
 * 账本把「已确认送达」记在一处，两种顺序都能收敛到正确状态：
 *   - 事件先到：先记账，消息稍后进列表时补上 delivered；
 *   - 消息先到：正常路径，事件到达时直接改状态。
 *
 * 这个模块刻意【零依赖】：它只做纯数据变换，因此可以用 Node 内置的测试运行器
 * 直接验证（见 frontend/tests/delivery.test.ts），不必为它引入前端测试框架。
 */

/** 与 api/types.ts 的 Message 结构对齐的最小集合（避免这里反向依赖 API 层）。 */
export interface DeliveryAware {
  msgId: string
  direction: string
  state: string
}

export interface DeliveryLedger {
  /** 记下一条送达确认。幂等；空 id 忽略。 */
  ack(msgId: string): void
  /** 消息进入列表前调用：补齐它可能已经错过的送达状态。 */
  reconcile<T extends DeliveryAware>(msg: T): T
}

/**
 * @param limit 账本上限。账本只用于「事件早于消息到达」这一个窗口，
 *              正常情况下几乎为空；给上限是为了让长时间运行的桌面应用
 *              不会因为累积几万条 id 而缓慢吃内存。
 */
export function createDeliveryLedger(limit = 512): DeliveryLedger {
  // Set 保持插入顺序，因此第一个元素就是最旧的条目
  const acked = new Set<string>()

  return {
    ack(msgId: string): void {
      if (!msgId || acked.has(msgId)) return
      acked.add(msgId)
      if (acked.size > limit) {
        const oldest = acked.values().next().value
        if (oldest !== undefined) acked.delete(oldest)
      }
    },

    reconcile<T extends DeliveryAware>(msg: T): T {
      // 自己的出站消息才需要「补状态」；入站消息由对端确认，本来就是 delivered
      if (msg.direction !== 'out' || msg.state === 'delivered') return msg
      if (!acked.has(msg.msgId)) return msg
      return { ...msg, state: 'delivered' }
    }
  }
}
