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
import { isActive } from "../stores/transfer";
import { usePeersStore } from "../stores/peers";
import { useUiStore } from "../stores/ui";
import {
  etaText,
  fileSize,
  isSending,
  speedText,
  transferStatus,
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

            <div class="name nowrap" :title="j.error || j.fileName">
              {{ j.fileName || "正在获取文件名…" }}
            </div>
          </div>

          <div class="bottom">
            <div class="info nowrap">
              <span v-if="showPeer">{{ peers.nameOf(j.peerId) }}</span>
              <span v-if="j.fileSize" class="mono"
                >{{ fileSize(j.completed || 0) }} /
                {{ fileSize(j.fileSize) }}</span
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

            <span class="tag" :class="transferStatus(j.status).cls">
              {{ transferStatus(j.status).text }}
            </span>

            <button
              v-if="j.status === 'done' && j.localPath"
              class="reveal"
              :title="`定位到：${j.localPath}`"
              aria-label="在文件管理器中显示该文件"
              @click="reveal(j.localPath)"
            >
              <Icon name="folder" :size="13" />
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
  padding: 10px 12px 16px;
}

.sum {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--t2);
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
  padding: 6px 8px;
  border-radius: var(--r-sm);
  font-size: var(--fs-xs);
  color: var(--t2);
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
  border-top: 1px solid var(--edge);
}

.row.err {
  color: var(--err-ink);
}

.dir {
  display: flex;
  align-items: center;
  justify-content: center;
  flex: 0 0 auto;
  width: 20px;
  height: 20px;
  border-radius: var(--r-sm);
}

.dir.up {
  background: var(--brand-soft);
  color: var(--brand-ink);
}

.dir.down {
  background: var(--ok-soft);
  color: var(--ok-ink);
}

.name {
  flex: 1 1 auto;
  min-width: 0;
  font-size: var(--fs-sm);
  font-weight: 500;
  color: var(--t1);
}

.info {
  flex: 0 1 auto;
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  color: var(--t3);
}

.pct {
  flex: 0 0 38px;
  text-align: right;
  font-variant-numeric: tabular-nums;
  font-weight: 500;
  color: var(--t1);
}

.pct.dim {
  color: var(--t3);
}

.tag {
  flex: 0 0 auto;
  padding: 1px 7px;
  border-radius: 999px;
  font-size: var(--fs-xs);
}

.reveal {
  flex: 0 0 auto;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 20px;
  height: 20px;
  padding: 0;
  border-radius: var(--r-sm);
  background: transparent;
  color: var(--t2);
  transition:
    background var(--dur-1) var(--ease),
    color var(--dur-1) var(--ease);
}

.reveal:hover {
  background: var(--hover);
  color: var(--t1);
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
