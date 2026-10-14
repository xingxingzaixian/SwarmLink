<script setup lang="ts">
/**
 * 传输任务列表（右栏「传输」页与设置里的「文件传输」共用）。
 *
 * 采用紧凑表格式布局：每个任务一行，进度以百分比文字展示，
 * 速率 / ETA / 已传字节放在同一行的次要信息里，不单独占行。
 */
import { computed } from "vue";

import Icon from "./Icon.vue";
import { getApi } from "../api";
import type { TransferJob } from "../api/types";
import { isActive, isWaiting } from "../stores/transfer";
import { usePeersStore } from "../stores/peers";
import { useUiStore } from "../stores/ui";
import {
  etaText,
  fileSize,
  isSending,
  speedText,
  transferStatus,
  transferredBytes,
} from "../utils/format";

const props = withDefaults(
  defineProps<{
    jobs: TransferJob[];
    /** 是否显示「对方」名字（全局列表需要，单会话面板不需要）。 */
    showPeer?: boolean;
  }>(),
  { showPeer: false },
);

const peers = usePeersStore();
const ui = useUiStore();

/** 在系统文件管理器里定位已传输的文件（Windows/macOS 会选中文件本身）。 */
async function reveal(path: string): Promise<void> {
  try {
    await (await getApi()).revealFile(path);
  } catch (e: any) {
    // 最常见的原因：文件已被手动删除或移动
    ui.notify(String(e?.message ?? e) || "无法定位该文件");
  }
}

/** 立刻重试一条等待中的任务（不等退避，也不等对方上线）。 */
async function retry(j: TransferJob): Promise<void> {
  try {
    await (await getApi()).retryTransfer(j.jobId);
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || "重试失败");
  }
}

/** 取消一条等待中的任务。正在传的任务后端会拒绝（没有中断点，假取消更糟）。 */
async function cancel(j: TransferJob): Promise<void> {
  try {
    await (await getApi()).cancelTransfer(j.jobId);
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || "取消失败");
  }
}

/** 进行中的排前面，其余按更新时间倒序 —— 用户关心的是「还在跑的」。 */
const ordered = computed(() =>
  [...props.jobs].sort((a, b) => {
    const aa = isActive(a.status) ? 0 : 1;
    const bb = isActive(b.status) ? 0 : 1;
    if (aa !== bb) return aa - bb;
    return b.updatedAt - a.updatedAt;
  }),
);

const running = computed(
  () => props.jobs.filter((j) => isActive(j.status)).length,
);

function percentOf(j: TransferJob): number {
  return Math.max(0, Math.min(100, j.percent || 0));
}

/**
 * 行上显示的名字。
 *
 * 文件夹批次里【必须】显示相对路径：同一张文件夹里的 a/dup.txt 与 b/dup.txt
 * 只显示文件名就变成两行一模一样的东西，用户没法判断是哪一行。
 * 目录条目补一个尾斜杠 —— 它与同名的空文件在列表里长得完全一样。
 */
function nameOf(j: TransferJob): string {
  if (j.relPath) return `${j.relPath}${j.isDir ? "/" : ""}`;
  return j.fileName || "";
}

/**
 * 已传字节数。
 *
 * 必须由字节口径算（transferredBytes），不能像以前那样把 DTO 的 completed
 * 直接喂给 fileSize()：那个字段是【已确认的分块数】，所以 15MB 的文件
 * 会显示成「30 B / 15 MB」。
 */
function doneBytes(j: TransferJob): number {
  return transferredBytes(j.fileSize, percentOf(j), j.status === "done");
}
</script>

<template>
  <div class="wrap">
    <div v-if="!ordered.length" class="empty">
      <div class="empty-title">还没有传输记录</div>
      <div>在与某人的聊天里点「回形针」或把文件拖进聊天区即可发起。</div>
    </div>

    <template v-else>
      <div v-if="running" class="sum">
        <Icon name="activity" :size="13" />
        <span>{{ running }} 个任务进行中</span>
      </div>

      <div class="rows">
        <div
          v-for="j in ordered"
          :key="j.jobId"
          class="row"
          :class="{ err: !!j.error }"
        >
          <div class="top">
            <span class="dir" :class="isSending(j.direction) ? 'up' : 'down'">
              <Icon
                :name="isSending(j.direction) ? 'upload' : 'download'"
                :size="13"
              />
            </span>

            <div class="name nowrap" :title="j.error || nameOf(j)">
              {{ nameOf(j) || "正在获取文件名…" }}
            </div>
          </div>

          <div class="bottom">
            <div class="info nowrap">
              <span v-if="showPeer">{{ peers.nameOf(j.peerId) }}</span>
              <span v-if="j.fileSize" class="mono"
                >{{ fileSize(doneBytes(j)) }} / {{ fileSize(j.fileSize) }}</span
              >
              <span
                v-if="isActive(j.status) && speedText(j.speed)"
                class="nowrap"
                >{{ speedText(j.speed)
                }}<template v-if="etaText(j.etaMs)">
                  · {{ etaText(j.etaMs) }}</template
                ></span
              >
            </div>

            <span class="spacer" />

            <span class="pct" :class="{ dim: !isActive(j.status) }">
              {{ isActive(j.status) ? percentOf(j).toFixed(0) + "%" : "" }}
            </span>

            <span class="tag" :class="transferStatus(j.status, j.waitReason).cls">
              {{ transferStatus(j.status, j.waitReason).text }}
            </span>

            <!-- 末尾固定两个槽位，保证各行右侧对齐（等待中占用两格） -->
            <button
              v-if="isWaiting(j)"
              class="reveal"
              title="立即重试（不等退避，也不等对方上线）"
              aria-label="立即重试"
              @click="retry(j)"
            >
              <Icon name="refresh" :size="13" />
            </button>
            <button
              v-else-if="j.status === 'done' && j.localPath"
              class="reveal"
              :title="`定位到：${j.localPath}`"
              aria-label="在文件管理器中显示该文件"
              @click="reveal(j.localPath)"
            >
              <Icon name="folder" :size="13" />
            </button>
            <span v-else class="reveal-ph" />

            <button
              v-if="isWaiting(j)"
              class="reveal"
              title="取消传输"
              aria-label="取消传输"
              @click="cancel(j)"
            >
              <Icon name="x" :size="13" />
            </button>
            <span v-else class="reveal-ph" />
          </div>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.wrap {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.sum {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--text-secondary);
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
}

.rows {
  display: flex;
  flex-direction: column;
}

/* 表格式行：上下两行 —— 第一行「图标 + 文件名」，第二行其余信息。
   细分隔线代替卡片阴影，整行仍然矮而密。 */
.row {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 7px 8px;
  border-radius: var(--r-sm);
  font-size: var(--fs-xs);
  color: var(--text-secondary);
}

.top {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

/* 左边留出图标宽度，让第二行与文件名左对齐。 */
.bottom {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  padding-left: 28px;
}

.spacer {
  flex: 1 1 auto;
}

.row + .row {
  border-top: 1px solid var(--divider);
}

.row.err {
  color: var(--danger);
}

/* 方向徽标：上传 = 中性（往外发是自己主动的事），
   下载 = 强调色（收到东西是「好」的结果）。 */
.dir {
  display: flex;
  align-items: center;
  justify-content: center;
  flex: 0 0 auto;
  width: 20px;
  height: 20px;
  border-radius: var(--r-xs);
}

.dir.up {
  background: var(--bg-elevated);
  color: var(--text-secondary);
}

.dir.down {
  background: var(--accent-soft);
  color: var(--accent);
}

.name {
  flex: 1 1 auto;
  min-width: 0;
  font-size: var(--fs-sm);
  font-weight: 500;
  color: var(--text-primary);
}

.info {
  flex: 0 1 auto;
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  color: var(--text-muted);
}

.pct {
  flex: 0 0 38px;
  text-align: right;
  font-variant-numeric: tabular-nums;
  font-weight: 500;
  color: var(--text-primary);
}

.pct.dim {
  color: var(--text-muted);
}

.reveal {
  flex: 0 0 auto;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 20px;
  height: 20px;
  padding: 0;
  border-radius: var(--r-xs);
  background: transparent;
  color: var(--text-secondary);
  transition:
    background var(--dur-1) var(--ease),
    color var(--dur-1) var(--ease);
}

.reveal:hover {
  background: var(--bg-hover);
  color: var(--accent);
}

/* 占位：与按钮等宽，保证各行末尾对齐。 */
.reveal-ph {
  flex: 0 0 20px;
}

.nowrap {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
</style>
