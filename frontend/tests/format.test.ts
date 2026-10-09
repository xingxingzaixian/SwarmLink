/**
 * 展示层格式化的回归测试（Node 内置运行器，见 package.json 的 test 脚本）。
 */
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { fileSize, transferStatus, transferredBytes } from '../src/utils/format.ts'

const MB = 1024 * 1024

test('已传字节数按字节口径折算，而不是拿分块数当字节', () => {
  // 15MB 的文件、512KB 分块 = 30 块。传完之后必须显示 15MB，
  // 曾经这里显示的是「30 B」（把 30 个分块当成了 30 字节）。
  assert.equal(transferredBytes(15 * MB, 100, true), 15 * MB)
  assert.equal(fileSize(transferredBytes(15 * MB, 100, true)), '15 MB')
})

test('完成态一律按全量算，抹掉百分比取整的尾差', () => {
  // 30 块里完成 29 块 → 96.67%，折算后是 14.5MB；但状态已 done 就该是满量
  assert.equal(transferredBytes(15 * MB, 96.6, true), 15 * MB)
})

test('进行中按百分比折算', () => {
  assert.equal(transferredBytes(1000, 42, false), 420)
  assert.equal(transferredBytes(15 * MB, 0, false), 0)
})

test('百分比缺失或越界不会产生负数/超额', () => {
  assert.equal(transferredBytes(1000, Number.NaN, false), 0)
  assert.equal(transferredBytes(1000, -20, false), 0)
  assert.equal(transferredBytes(1000, 999, false), 1000)
})

test('未知文件大小时返回 0（DTO 尚未补齐时列表不该显示乱数）', () => {
  assert.equal(transferredBytes(0, 100, false), 0)
  assert.equal(transferredBytes(0, 100, true), 0)
})

test('批次排队与「等待对方上线」是两回事，文案不能混', () => {
  // 两者的出口完全不同：一个等对端上线，一个等前一个文件传完。
  // 混成一句会让用户去检查网络，而实际上它只是在排队。
  assert.equal(transferStatus('idle', 'batch_queued').text, '排队中')
  assert.equal(transferStatus('idle', 'batch_queued').final, false)
})

test('等待对方上线要显示出来，而且不能算终态', () => {
  // 等待态与「还没轮到」共用 idle/queued，判据只能是 waitReason。
  // 标成终态会让它从「进行中」列表里掉出去，用户以为这条任务已经结束了。
  assert.equal(transferStatus('idle', 'peer_offline').text, '等待对方上线')
  assert.equal(transferStatus('queued', 'peer_offline').text, '等待对方上线')
  assert.equal(transferStatus('idle', 'peer_offline').final, false)

  // 没有 waitReason 时语义不变
  assert.equal(transferStatus('idle').text, '协商中')
  assert.equal(transferStatus('queued').text, '排队中')
  // 其它终态不受影响
  assert.equal(transferStatus('done').final, true)
  assert.equal(transferStatus('failed').final, true)
})
