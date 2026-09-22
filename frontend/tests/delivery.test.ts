/**
 * 送达状态账本的回归测试。
 *
 * 为什么这份文件放在 frontend/tests 而不是 src：它用 Node 内置的测试运行器
 * （`npm run test`）跑，靠 Node 的类型擦除直接加载 .ts —— 不引入 vitest 之类的
 * 额外依赖。代价是 import 必须写全扩展名，所以 tsconfig 只 include src/**，
 * 不让 vue-tsc 去检查这里。
 *
 * 覆盖的缺陷：真实时序下 chat:delivered 可能早于「发送 RPC 返回」到达，
 * 那时消息还没进列表，事件被丢掉，气泡就永远停在「发送中」。
 */
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { createDeliveryLedger } from '../src/stores/delivery.ts'

const outPending = { msgId: 'm1', direction: 'out', state: 'pending' }

test('送达事件早于消息进入列表时，消息仍应显示已送达', () => {
  const ledger = createDeliveryLedger()

  // 后端在 SendMessage 返回之前就发出了 delivered 事件
  ledger.ack('m1')

  // 之后才把消息（返回值里的 pending 快照）写进列表
  const msg = ledger.reconcile(outPending)

  assert.equal(msg.state, 'delivered')
})

test('没有确认过的消息不受影响（保持 pending）', () => {
  const ledger = createDeliveryLedger()
  assert.equal(ledger.reconcile(outPending).state, 'pending')
})

test('已送达的消息保持不变，不产生多余的对象替换', () => {
  const ledger = createDeliveryLedger()
  const done = { msgId: 'm1', direction: 'out', state: 'delivered' }
  assert.equal(ledger.reconcile(done), done)
})

test('入站消息不受影响：它们本来就由对端确认', () => {
  const ledger = createDeliveryLedger()
  ledger.ack('m2')
  const inbound = { msgId: 'm2', direction: 'in', state: 'delivered' }
  assert.equal(ledger.reconcile(inbound), inbound)
})

test('空 msgId 不应写进账本（事件载荷可能不完整）', () => {
  const ledger = createDeliveryLedger(4)
  ledger.ack('')
  assert.equal(ledger.reconcile({ msgId: '', direction: 'out', state: 'pending' }).state, 'pending')
})

test('账本有上限，长期运行不会无限增长', () => {
  const ledger = createDeliveryLedger(2)
  ledger.ack('a')
  ledger.ack('b')
  ledger.ack('c') // 挤掉 'a'

  assert.equal(ledger.reconcile({ msgId: 'c', direction: 'out', state: 'pending' }).state, 'delivered')
  assert.equal(ledger.reconcile({ msgId: 'a', direction: 'out', state: 'pending' }).state, 'pending')
})

test('重复确认是幂等的', () => {
  const ledger = createDeliveryLedger(2)
  ledger.ack('a')
  ledger.ack('a')
  ledger.ack('b')
  ledger.ack('c')
  // 若重复确认被当成新条目，'a' 会被保留、'b' 被挤掉；幂等时挤掉的才是 'a'
  assert.equal(ledger.reconcile({ msgId: 'a', direction: 'out', state: 'pending' }).state, 'pending')
  assert.equal(ledger.reconcile({ msgId: 'b', direction: 'out', state: 'pending' }).state, 'delivered')
})
