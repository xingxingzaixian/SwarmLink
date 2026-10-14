/**
 * 文件夹批次的聚合计算。
 *
 * 为什么是纯函数模块（而不是写进 ChatBubble 的 computed）：
 * 卡片上的「已完成 x/N」「N-1/N 完成，1 个失败」是用户判断这批成没成的唯一依据，
 * 而它要同时服务于「store 里已有任务」与「重启后向 ListBatch 补拉」两条数据来源。
 * 判据只写一份，两条来源才不可能给出不同的结论（例如一边说完成、一边还在转）。
 */
import type { TransferJob } from '../api/types'
import { transferredBytes, transferStatus } from './format.ts'

/** 一个批次的聚合进度。 */
export interface FolderProgress {
  /** 批次内的任务总数（含空目录条目）。 */
  total: number
  done: number
  failed: number
  cancelled: number
  /** 已传字节数。按字节口径折算 —— 不能拿 DTO 的 completed（那是分块数）。 */
  transferred: number
  /**
   * 整批都已进入终态。
   *
   * 【没有任务时是 false，不是 true】：`total === 0` 的含义是「还不知道这批有什么」，
   * 而不是「全都传完了」—— 重启后 store 里是空的，此时把 0/0 说成已完成就是在骗用户。
   */
  allFinished: boolean
  /** 有失败。已取消【不算】失败 —— 用户自己按下的「取消全部」不该被报成出错。 */
  hasFailure: boolean
}

function emptyProgress(): FolderProgress {
  return {
    total: 0,
    done: 0,
    failed: 0,
    cancelled: 0,
    transferred: 0,
    allFinished: false,
    hasFailure: false
  }
}

/**
 * 聚合某个批次的进度。
 *
 * 空 batchId 一律返回空：后端三处都明确拒绝它（`CancelBatch("")` 会退化成
 * 「停掉本机全部单文件传输」），前端也不能把它当成一个真实批次。
 */
export function folderProgress(jobs: TransferJob[], batchId: string): FolderProgress {
  const out = emptyProgress()
  if (!batchId) return out

  // 先按【文件】分组：群聊发文件夹时同一个文件会存在多份任务（每个成员一份），
  // 但用户看到的「这个文件夹有几个文件」只该数一次 —— 一个文件算完成，
  // 是它发给了所有成员且都成功。单聊场景每个 rel_path 只有一条任务，
  // 聚合结果与逐条统计完全一致（rel_path 为空时用 jobId 兜底，保证不误合并）。
  const byFile = new Map<string, TransferJob[]>()
  for (const j of jobs) {
    if (j.batchId !== batchId) continue
    // 已传字节按【每条任务】累加：这是真实的字节吞吐，不去重。
    out.transferred += transferredBytes(j.fileSize, j.percent, j.status === 'done')
    const key = j.relPath || j.jobId
    const bucket = byFile.get(key)
    if (bucket) bucket.push(j)
    else byFile.set(key, [j])
  }

  for (const bucket of byFile.values()) {
    out.total++
    // 终态判据复用 format.ts 里那一份（transferStatus().final）：界面文案与
    // 「算不算结束」必须同源，否则会出现「标着已完成、却还留在进行中列表里」。
    const allFinal = bucket.every((j) => transferStatus(j.status).final)
    if (!allFinal) continue
    if (bucket.every((j) => j.status === 'done')) {
      out.done++
    } else if (bucket.some((j) => j.status === 'failed')) {
      out.failed++
    } else {
      out.cancelled++
    }
  }

  out.allFinished = out.total > 0 && out.done + out.failed + out.cancelled === out.total
  out.hasFailure = out.failed > 0
  return out
}
