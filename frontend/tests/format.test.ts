/**
 * 展示层格式化的回归测试（Node 内置运行器，见 package.json 的 test 脚本）。
 */
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { fileSize, transferredBytes } from '../src/utils/format.ts'

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
