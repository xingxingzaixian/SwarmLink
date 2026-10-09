/**
 * 文件夹批次聚合的回归测试（Node 内置运行器，见 package.json 的 test 脚本）。
 *
 * 为什么这批用例值得单独存在：卡片上的「已完成 x/N」与「N-1/N 完成，1 个失败」
 * 是用户判断「这一批到底成没成」的唯一依据。判据写错不会报错、只会安静地
 * 把「还在传」显示成「已完成」，或者把用户自己取消的那条报成失败。
 */
import assert from 'node:assert/strict'
import { test } from 'node:test'

import type { TransferJob } from '../src/api/types.ts'
import { folderProgress } from '../src/utils/batch.ts'

/** 造一条任务：只写用例关心的字段，其余给合法的空值。 */
function job(jobId: string, batchId: string, status: string, extra: Partial<TransferJob> = {}): TransferJob {
  return {
    jobId,
    peerId: '1122334455667788',
    fileName: 'a.txt',
    fileSize: 0,
    direction: 'send',
    localPath: '',
    completed: 0,
    totalChunks: 0,
    percent: 0,
    status,
    updatedAt: 0,
    batchId,
    ...extra
  }
}

test('folderProgress 只统计本批次的任务', () => {
  const jobs = [
    job('1', 'b1', 'done', { fileSize: 100 }),
    job('2', 'b1', 'failed', { fileSize: 50 }),
    job('3', 'b2', 'done', { fileSize: 999 })
  ]
  const p = folderProgress(jobs, 'b1')
  assert.equal(p.total, 2)
  assert.equal(p.done, 1)
  assert.equal(p.failed, 1)
  assert.equal(p.allFinished, true)
})

test('有人还在传时不算结束', () => {
  const jobs = [job('1', 'b1', 'transferring')]
  assert.equal(folderProgress(jobs, 'b1').allFinished, false)
})

test('已取消算结束，但不能算失败', () => {
  // 用户自己按下的「取消全部」不该在卡片上被报成「1 个失败」。
  const jobs = [job('1', 'b1', 'done'), job('2', 'b1', 'cancelled')]
  const p = folderProgress(jobs, 'b1')
  assert.equal(p.cancelled, 1)
  assert.equal(p.hasFailure, false)
  assert.equal(p.allFinished, true)
})

test('单文件任务（无 batchId）不会被任何批次捞进来', () => {
  // 空 batchId 在后端是明确报错的取值（CancelBatch('') 会退化成「停掉全部单文件传输」）。
  // 前端同样不能把它当成「一个真实的批次」。
  const jobs = [job('1', '', 'done'), job('2', 'b1', 'done')]
  assert.equal(folderProgress(jobs, '').total, 0)
  assert.equal(folderProgress(jobs, 'b1').total, 1)
})

test('transferred 按字节口径折算，而不是把分块数当字节', () => {
  // 与传输入口同一个坑：completed 是【已确认的分块数】。
  const MB = 1024 * 1024
  const jobs = [
    job('1', 'b1', 'done', { fileSize: 15 * MB, percent: 100 }),
    job('2', 'b1', 'transferring', { fileSize: 10 * MB, percent: 50 })
  ]
  assert.equal(folderProgress(jobs, 'b1').transferred, 20 * MB)
})

test('看不到任何任务时不能声称「已完成」', () => {
  // 重启后 store 里可能是空的（ListRecent 窗口）；此时卡片要靠 ListBatch 补，
  // 在补齐之前把 0/0 说成「已完成」就是在骗用户。
  const p = folderProgress([], 'b1')
  assert.equal(p.total, 0)
  assert.equal(p.allFinished, false)
})
