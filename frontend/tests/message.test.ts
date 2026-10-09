/**
 * 消息 content 解析的回归测试。
 *
 * 覆盖的缺陷：失败消息的 content 曾经是 Go 字段名（{"JobID":"",...}），
 * 而前端按 job_id 读 —— 一个字段都读不到，整条消息在聊天框里显示成一段
 * 裸 JSON。这些消息已经存在于用户的历史记录里，因此解析必须同时认
 * 三种写法：snake_case（现在）、camelCase（早期设计）、Go 字段名（历史数据）。
 */
import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  parseFileContent,
  parseFileFailedContent,
  parseFolderContent,
  previewText
} from '../src/utils/message.ts'

test('文件消息：snake_case（当前后端写法）', () => {
  const got = parseFileContent('{"job_id":"j1","file_name":"报告.pdf"}')
  assert.deepEqual(got, { jobId: 'j1', fileName: '报告.pdf' })
})

test('文件消息：camelCase（早期设计写法）', () => {
  const got = parseFileContent('{"jobId":"j2","fileName":"设计稿.png"}')
  assert.deepEqual(got, { jobId: 'j2', fileName: '设计稿.png' })
})

test('文件消息：内容坏掉时不能连带毁掉整段聊天记录', () => {
  assert.deepEqual(parseFileContent('不是 JSON'), { jobId: '', fileName: '文件消息' })
  assert.deepEqual(parseFileContent(''), { jobId: '', fileName: '文件消息' })
})

test('文件消息：JSON 合法但没有文件名时给占位，而不是空白气泡', () => {
  assert.equal(parseFileContent('{"job_id":"j3"}').fileName, '未知文件')
})

test('失败消息：snake_case', () => {
  const got = parseFileFailedContent(
    '{"job_id":"j4","file_name":"丢了.pdf","reason":"文件已被删除或移动"}'
  )
  assert.deepEqual(got, { jobId: 'j4', fileName: '丢了.pdf', reason: '文件已被删除或移动' })
})

test('失败消息：Go 字段名的历史数据也要能正常显示（而不是裸 JSON）', () => {
  const got = parseFileFailedContent(
    '{"JobID":"j5","FileName":"老文件.zip","Reason":"文件已被删除或移动"}'
  )
  assert.deepEqual(got, { jobId: 'j5', fileName: '老文件.zip', reason: '文件已被删除或移动' })
})

test('失败消息：字段全空的历史垃圾（JobID 为空的那种）仍要能显示成卡片', () => {
  const got = parseFileFailedContent('{"JobID":"","FileName":"","Reason":"文件已被删除或移动"}')
  assert.equal(got.fileName, '文件') // 兜底名字，不能是空字符串
  assert.equal(got.reason, '文件已被删除或移动') // 原因要保住：这是用户唯一的线索
})

test('失败消息：坏 JSON 不抛异常', () => {
  assert.deepEqual(parseFileFailedContent('{'), { jobId: '', fileName: '文件', reason: '' })
})

// --- 左栏预览：content 是 JSON 信封，绝不能原样透出去 ---
//
// 缺陷现场：文件消息发出后，左栏联系人行显示成
// 「{"job_id":"01a0ed92-0204-7…」——用户看到的是内部标识。
// 文件夹消息（msgType = folder）是同一种信封，没有分支时表现为同一个事故。

test('左栏预览：文本消息原样显示', () => {
  assert.equal(previewText({ msgType: 'text', content: '晚上一起吃饭？' }), '晚上一起吃饭？')
})

test('左栏预览：图片消息给「[图片]」而不是 base64 信封', () => {
  const content = JSON.stringify({ v: 1, mime: 'image/png', w: 8, h: 8, b64: 'iVBORw0KGgo=' })
  assert.equal(previewText({ msgType: 'image', content }), '[图片]')
})

test('左栏预览：文件消息给「[文件] 文件名」而不是裸 JSON', () => {
  const content = '{"job_id":"01a0ed92-0204-7b3c","file_name":"SwarmLink.exe"}'
  assert.equal(previewText({ msgType: 'file', content }), '[文件] SwarmLink.exe')
})

test('左栏预览：Go 字段名的历史文件消息也要给出文件名', () => {
  const content = '{"JobID":"j9","FileName":"老安装包.zip"}'
  assert.equal(previewText({ msgType: 'file', content }), '[文件] 老安装包.zip')
})

test('左栏预览：传输失败的文件消息要点出失败', () => {
  const content = '{"job_id":"j10","file_name":"丢了.pdf","reason":"文件已被删除或移动"}'
  assert.equal(previewText({ msgType: 'file_failed', content }), '[文件] 丢了.pdf（发送失败）')
})

test('左栏预览：坏内容的文件消息不能又把 JSON 漏出来', () => {
  assert.equal(previewText({ msgType: 'file', content: '不是 JSON' }), '[文件] 文件消息')
  assert.equal(previewText({ msgType: 'file_failed', content: '{' }), '[文件] 文件（发送失败）')
})

// --- 文件夹消息（msgType = folder）---

test('文件夹消息：snake_case（当前后端写法）', () => {
  const got = parseFolderContent(
    '{"batch_id":"b1","folder_name":"照片","file_count":32,"total_size":1048576,"skipped":2}'
  )
  assert.deepEqual(got, {
    batchId: 'b1',
    folderName: '照片',
    fileCount: 32,
    totalSize: 1048576,
    skipped: 2
  })
})

test('文件夹消息：字段缺失时给能渲染的默认值，而不是 undefined', () => {
  // 卡片上少一个数字只是难看；NaN/undefined 传进 fileSize() 会渲染成乱数。
  const got = parseFolderContent('{"batch_id":"b2"}')
  assert.equal(got.folderName, '文件夹')
  assert.equal(got.fileCount, 0)
  assert.equal(got.totalSize, 0)
  assert.equal(got.skipped, 0)
})

test('文件夹消息：坏 JSON 不抛异常', () => {
  assert.equal(parseFolderContent('不是 JSON').folderName, '文件夹')
  assert.equal(parseFolderContent('').batchId, '')
})

test('左栏预览：文件夹消息给「[文件夹] 名字」而不是裸 JSON', () => {
  const content =
    '{"batch_id":"b1","folder_name":"照片","file_count":3,"total_size":1024,"skipped":0}'
  assert.equal(previewText({ msgType: 'folder', content }), '[文件夹] 照片')
  // 坏内容也不能漏出 JSON
  assert.equal(previewText({ msgType: 'folder', content: '{' }), '[文件夹] 文件夹')
})
