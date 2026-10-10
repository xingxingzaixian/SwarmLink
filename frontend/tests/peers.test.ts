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

test('withoutPeer 只比严格相等，不做短 ID / 前缀匹配', () => {
  // (a) 定长 16 位 hex 下前 8 位相同很常见：按 shortId（前 8 位）比较会一次误删两个
  const sameShort = [{ nodeId: 'aabbccddeeff0011' }, { nodeId: 'aabbccddeeff0022' }]
  assert.deepEqual(
    withoutPeer(sameShort, 'aabbccddeeff0011').map((p) => p.nodeId),
    ['aabbccddeeff0022']
  )

  // (b) 严格前缀：把「目标以该项开头」也算命中，会连带删掉更短的 id。
  //     16 位 hex 的真实数据里不会出现这种项（等长时 startsWith 等价于 ===），
  //     这条钉的是「按严格相等比较」这个属性 —— 改成 nodeId.startsWith(p.nodeId)
  //     或 nodeId.includes(p.nodeId) 都会红。
  const withPrefix = [{ nodeId: 'aabb' }, { nodeId: 'aabbccddeeff0011' }]
  assert.deepEqual(
    withoutPeer(withPrefix, 'aabbccddeeff0011').map((p) => p.nodeId),
    ['aabb']
  )
})

test('withoutPeer 返回新数组，不改动入参', () => {
  const list = [{ nodeId: 'a' }]
  const out = withoutPeer(list, 'a')
  assert.equal(list.length, 1)
  assert.equal(out.length, 0)
})
