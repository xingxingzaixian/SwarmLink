/**
 * 联系人列表「移除节点」的纯逻辑回归测试（Node 内置运行器，见 package.json 的 test 脚本）。
 *
 * 为什么要单独测这个一行函数：本仓前端没有 jsdom / 组件测试能力，
 * 把「移除后列表长什么样」的规则留在 store 里就等于没测。
 */
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { withoutPeer } from '../src/utils/peers.ts'

test('withoutPeer 只移除目标节点，其余保持原顺序', () => {
  const list = [{ nodeId: 'a' }, { nodeId: 'b' }, { nodeId: 'c' }]
  assert.deepEqual(
    withoutPeer(list, 'b').map((p) => p.nodeId),
    ['a', 'c']
  )
})

test('withoutPeer 对不存在的节点是原样返回', () => {
  const list = [{ nodeId: 'a' }]
  assert.deepEqual(withoutPeer(list, 'zz'), [{ nodeId: 'a' }])
})

test('withoutPeer 不误删前缀相同的其它节点', () => {
  // node_id 是 16 位 hex，前 8 位相同很常见；用 startsWith 之类的实现会误删一片
  const list = [{ nodeId: 'aabbccddeeff0011' }, { nodeId: 'aabbccddeeff0022' }]
  assert.deepEqual(
    withoutPeer(list, 'aabbccddeeff0011').map((p) => p.nodeId),
    ['aabbccddeeff0022']
  )
})

test('withoutPeer 返回新数组，不改动入参', () => {
  const list = [{ nodeId: 'a' }]
  const out = withoutPeer(list, 'a')
  assert.equal(list.length, 1)
  assert.equal(out.length, 0)
})
