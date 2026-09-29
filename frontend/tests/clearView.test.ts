/**
 * 「清空 / 查看历史」按钮可用性的回归测试。
 *
 * 覆盖的缺陷形态：条件写反或漏判时，按钮在不可用的场景下仍可点，
 * 点了没反应 —— 用户会认为功能坏了，而不是"这里本来就没东西可清"。
 */
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { canClear, canRestore } from '../src/utils/clearView.ts'

test('清空：有消息才可点', () => {
  assert.equal(canClear(0), false)
  assert.equal(canClear(1), true)
  assert.equal(canClear(1000), true)
})

test('清空：负数（不该出现的计数）不应被当成可点', () => {
  assert.equal(canClear(-1), false)
})

test('查看历史：没有清空点时不可点', () => {
  assert.equal(canRestore(0), false)
})

test('查看历史：有任何正数清空点都可点', () => {
  assert.equal(canRestore(1), true)
  assert.equal(canRestore(1759000000000), true)
})
