<script setup lang="ts">
/**
 * 快捷键编辑器（设置 → 快捷键）。
 *
 * 交互是「点一下键位 → 直接按新的组合键」，而不是下拉选键名：
 * 快捷键的价值就在于肌肉记忆，让用户从「Ctrl / Shift / K」三个下拉里
 * 拼一个出来，等于把最容易出错的环节交给用户。
 *
 * 三条约束在录制时【当场】判掉（后端只兜底全局热键那一侧）：
 *   - 全局键必须含 Ctrl / Alt（只有 Shift 等于裸键，会抢掉正常打字）；
 *   - 窗口内键必须含 Ctrl / Alt，除非主键是 Esc / F1-F12；
 *   - 同一张表里不能重复 —— 后端不知道窗口内命令有哪些，查不了这个。
 *
 * 保存是【即时】的：录完就注册、就落盘。失败（被别的程序占用）会立刻
 * 显示在该行上 —— 若等用户点「保存」才知道，他得把整套动作重做一遍。
 */
import { computed, onBeforeUnmount, ref } from 'vue'

import Icon from './Icon.vue'
import { COMMANDS, GROUP_LABELS, type HotkeyCommand, type HotkeyGroup } from '../hotkeys/commands'
import { eventAccel, formatAccel, hasPrimaryModifier, isBareAllowedKey } from '../hotkeys/keys'
import { useHotkeysStore } from '../stores/hotkeys'

const hotkeys = useHotkeysStore()

/** 分组顺序：先系统级（用户最关心“会不会抢键”），再会话、面板、帮助。 */
const ORDER: HotkeyGroup[] = ['global', 'session', 'panel', 'help']

const sections = computed(() =>
  ORDER.map((g) => ({
    key: g,
    label: GROUP_LABELS[g],
    rows: COMMANDS.filter((c) => c.group === g && !c.locked)
  })).filter((s) => s.rows.length > 0)
)

/** 正在录制的命令 id（空串 = 不在录制）。 */
const recording = ref('')
/** 录制期间的即时校验错误（命令 id → 文案），保存成功后清空。 */
const localError = ref<Record<string, string>>({})

let keySink: ((e: KeyboardEvent) => void) | null = null

/**
 * 录制时用【捕获阶段】截住按键。
 *
 * 必须在捕获阶段：否则按键会先被派发器拿去执行（按下 Ctrl+K 会把搜索弹层
 * 打开，而用户只是想把这个组合键绑上去）。capture 里 preventDefault 之后
 * 派发器看到 defaultPrevented 就会放手。
 */
function startRecord(cmd: HotkeyCommand): void {
  stopRecord()
  recording.value = cmd.id
  keySink = (e: KeyboardEvent) => {
    e.preventDefault()
    e.stopPropagation()

    if (e.key === 'Escape') {
      stopRecord()
      return
    }
    if (e.key === 'Backspace' || e.key === 'Delete') {
      void commit(cmd, '')
      return
    }
    // 只按了修饰键：继续等主键，别把它当成一个完整组合。
    if (['Control', 'Alt', 'Shift', 'Meta'].includes(e.key)) return

    const accel = eventAccel(e)
    if (!accel) return
    void commit(cmd, accel)
  }
  window.addEventListener('keydown', keySink, true)
}

function stopRecord(): void {
  recording.value = ''
  if (keySink) {
    window.removeEventListener('keydown', keySink, true)
    keySink = null
  }
}

onBeforeUnmount(stopRecord)

/** 录制态的校验：只针对“这次按下的组合”，不碰已有配置。 */
function validate(cmd: HotkeyCommand, accel: string): string {
  if (!accel) return ''
  const primary = hasPrimaryModifier(accel)
  if (cmd.scope === 'global' && !primary) {
    return '系统级快捷键必须包含 Ctrl 或 Alt'
  }
  if (cmd.scope === 'window' && !primary && !isBareAllowedKey(accel)) {
    return '必须包含 Ctrl / Alt，或改用 Esc / F1-F12'
  }
  const conflict = hotkeys.conflictOf(accel, cmd.id)
  if (conflict) return `与「${conflict.label}」冲突`
  return ''
}

async function commit(cmd: HotkeyCommand, accel: string): Promise<void> {
  const msg = validate(cmd, accel)
  if (msg) {
    // 停在录制态：用户多半是想重按一次，而不是退出后重新点。
    localError.value = { ...localError.value, [cmd.id]: msg }
    return
  }
  delete localError.value[cmd.id]
  const ok = await hotkeys.setAccel(cmd.id, accel)
  if (ok) {
    stopRecord()
    return
  }
  localError.value = { ...localError.value, [cmd.id]: hotkeys.error || '保存失败' }
}

function toggleRecord(cmd: HotkeyCommand): void {
  if (recording.value === cmd.id) {
    stopRecord()
    return
  }
  delete localError.value[cmd.id]
  startRecord(cmd)
}

/**
 * 行内提示：本地校验 > 后端注册结果 > 预览模式说明。
 *
 * 注册失败只显示一句人话，原始错误（来自操作系统的英文报错）挂在
 * title 上 —— 用户需要的是「换个键」，排障时才需要那串英文。
 */
function noteOf(cmd: HotkeyCommand): string {
  const local = localError.value[cmd.id]
  if (local) return local
  if (hotkeys.errorOf(cmd.id)) return '已被其他程序占用，请换一个键位'
  if (cmd.scope === 'global' && hotkeys.mock) return '浏览器预览下不可用'
  return ''
}

/** 原始错误文案（悬停可见）。 */
function rawError(cmd: HotkeyCommand): string {
  return hotkeys.errorOf(cmd.id)
}

/** 有错误时用警示色；预览模式那种说明用灰色。 */
function isBad(cmd: HotkeyCommand): boolean {
  return Boolean(localError.value[cmd.id] || hotkeys.errorOf(cmd.id))
}
</script>

<template>
  <div class="hk">
    <div v-for="s in sections" :key="s.key" class="group">
      <h4>{{ s.label }}</h4>
      <div v-for="cmd in s.rows" :key="cmd.id" class="row">
        <div class="main">
          <span class="label">{{ cmd.label }}</span>
          <span v-if="noteOf(cmd)" class="msg" :class="{ bad: isBad(cmd) }" :title="rawError(cmd) || undefined">
            {{ noteOf(cmd) }}
          </span>
        </div>

        <button
          class="key"
          :class="{ rec: recording === cmd.id, empty: !hotkeys.boundOf(cmd) }"
          :disabled="hotkeys.loading"
          @click="toggleRecord(cmd)"
        >
          {{ recording === cmd.id ? '按下组合键…' : formatAccel(hotkeys.boundOf(cmd)) }}
        </button>

        <button
          class="icon-btn"
          title="清除绑定"
          aria-label="清除绑定"
          :disabled="!hotkeys.boundOf(cmd) || hotkeys.loading"
          @click="commit(cmd, '')"
        >
          <Icon name="x" :size="13" />
        </button>
      </div>
    </div>

    <p v-if="hotkeys.error" class="err">{{ hotkeys.error }}</p>
  </div>
</template>

<style scoped>
.hk {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.note {
  margin: 0 0 6px;
  font-size: var(--fs-xs);
  line-height: 1.7;
  color: var(--text-muted);
}

.note b {
  color: var(--text-secondary);
  font-weight: 600;
}

.group {
  margin-top: 10px;
  padding-top: 10px;
  border-top: 1px dashed var(--divider);
}

.group:first-of-type {
  margin-top: 0;
  padding-top: 0;
  border-top: none;
}

h4 {
  margin: 0 0 6px;
  font-family: var(--font-display);
  font-size: var(--fs-xs);
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--text-muted);
}

.row {
  display: flex;
  align-items: center;
  gap: 8px;
  min-height: 34px;
}

.main {
  flex: 1 1 auto;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 1px;
}

.label {
  font-size: var(--fs-md);
  color: var(--text-secondary);
}

.msg {
  font-size: var(--fs-xs);
  color: var(--text-muted);
}

.msg.bad {
  color: var(--warn);
}

.key {
  flex: 0 0 auto;
  min-width: 96px;
  height: 28px;
  padding: 0 10px;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-primary);
  background: var(--bg-elevated);
  border: 1px solid var(--divider-strong);
  border-bottom-width: 2px;
  border-radius: var(--r-xs);
  transition: border-color var(--dur-1) var(--ease), color var(--dur-1) var(--ease);
}

.key:hover {
  border-color: var(--accent-ring);
}

.key.empty {
  color: var(--text-muted);
  border-bottom-width: 1px;
}

/* 录制中：边框亮起来 + 轻微呼吸，让用户知道“现在按什么都归我”。 */
.key.rec {
  color: var(--accent);
  border-color: var(--accent);
  animation: hk-pulse 1.1s var(--ease) infinite;
}

@keyframes hk-pulse {
  0%,
  100% {
    box-shadow: 0 0 0 0 var(--accent-soft);
  }
  50% {
    box-shadow: 0 0 0 3px var(--accent-soft);
  }
}

.err {
  margin: 10px 0 0;
  font-size: var(--fs-xs);
  color: var(--danger);
}
</style>
