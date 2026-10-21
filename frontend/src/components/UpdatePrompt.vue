<script setup lang="ts">
/**
 * 局域网更新确认框（对应 Windows 更新的「立即重启 / 选择时间」）。
 *
 * 这里【只】做三选一，不展示下载进度：进度在设置页的「更新」分类里看。
 * 弹框出现在安装包已经下载并校验完成之后 —— 用户点「立即更新」就是真的装，
 * 不需要再等几十 MB 的下载。
 *
 * 关闭（Esc / 右上角 X）等价于「稍后提醒」，绝不等于「放弃本次」：
 * 误触关闭导致更新被取消，会让「全网拉齐」在无人值守的机器上悄悄失效。
 * 真正的放弃动作只在设置页里（那里有明确的文字）。
 */
import { computed, onBeforeUnmount, ref, watch } from 'vue'

import Icon from './Icon.vue'
import { pushHotkeyLayer } from '../hotkeys/dispatcher'
import { useUpdateStore } from '../stores/update'

const update = useUpdateStore()

const showTimePicker = ref(false)
const error = ref('')
const working = ref(false)

/** 默认排期：明天 02:00（凌晨是这类内网应用公认的维护窗口）。 */
function defaultSchedule(): Date {
  const at = new Date()
  at.setDate(at.getDate() + 1)
  at.setHours(2, 0, 0, 0)
  return at
}

function toInputValue(d: Date): string {
  const pad = (v: number): string => String(v).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

const customAt = ref(toInputValue(defaultSchedule()))

const sizeLabel = computed(() => {
  const n = update.state?.size ?? 0
  if (!n) return ''
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
})

const sourceLabel = computed(() => {
  const st = update.state
  if (!st) return ''
  const name = st.sourceName || (st.sourcePeerId ?? '').slice(0, 8)
  return st.sourceAddr ? `${name}（${st.sourceAddr}）` : name
})

/** 兜底自动安装时刻：用户知道自己不点也会被装，这是「无人值守也能拉齐」的关键。 */
const autoLabel = computed(() => {
  const at = update.state?.autoInstallAt ?? 0
  if (!at) return ''
  const d = new Date(at)
  const pad = (v: number): string => String(v).padStart(2, '0')
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
})

/** 快捷时间：今晚 22:00（已经过了就顺延到明天，而不是排到过去立刻执行）。 */
function tonight(): Date {
  const at = new Date()
  at.setHours(22, 0, 0, 0)
  if (at.getTime() <= Date.now()) at.setDate(at.getDate() + 1)
  return at
}

function tomorrowNight(): Date {
  const at = new Date()
  at.setDate(at.getDate() + 1)
  at.setHours(2, 0, 0, 0)
  return at
}

async function run(fn: () => Promise<void>): Promise<void> {
  if (working.value) return
  working.value = true
  error.value = ''
  try {
    await fn()
  } catch (e: any) {
    error.value = String(e?.message ?? e) || '操作失败'
  } finally {
    working.value = false
  }
}

const applyNow = (): Promise<void> => run(() => update.applyNow())

const snooze = (): Promise<void> => run(() => update.snooze())

function scheduleAt(at: Date): Promise<void> {
  if (at.getTime() <= Date.now()) {
    error.value = '请选择将来的时间'
    return Promise.resolve()
  }
  return run(() => update.scheduleAt(at.getTime()))
}

function scheduleCustom(): Promise<void> {
  const ms = new Date(customAt.value).getTime()
  if (!Number.isFinite(ms)) {
    error.value = '时间格式不正确'
    return Promise.resolve()
  }
  return scheduleAt(new Date(ms))
}

/**
 * Esc = 稍后提醒。
 *
 * 独占层：它弹出来就是要求用户做个决定，底下的弹层不该被同一次 Esc 顺带关掉。
 * 只在【真的可见时】占住 Esc —— 迁移前这里无条件挂在捕获阶段并
 * stopImmediatePropagation，于是提示框没出现时 Esc 也会被它吞掉，
 * 所有弹层都关不掉，而代码上完全看不出因果关系。
 */
let popLayer: (() => void) | null = null

watch(
  () => update.promptVisible,
  (visible) => {
    if (visible && !popLayer) {
      popLayer = pushHotkeyLayer({ exclusive: true, onEsc: () => void snooze() })
      return
    }
    if (!visible) {
      popLayer?.()
      popLayer = null
    }
  },
  { immediate: true }
)

onBeforeUnmount(() => {
  popLayer?.()
  popLayer = null
})
</script>

<template>
  <Teleport to="body">
    <div v-if="update.promptVisible" class="overlay" @click.self="snooze">
      <section class="card anim-pop" role="dialog" aria-modal="true" aria-labelledby="up-title">
        <header class="head">
          <div class="badge"><Icon name="download" :size="16" /></div>
          <div class="titles">
            <h2 id="up-title">发现新版本 v{{ update.state?.targetVersion }}</h2>
            <p class="faint">
              来自 {{ sourceLabel }}<template v-if="sizeLabel"> · {{ sizeLabel }}</template>
            </p>
          </div>
          <button class="icon-btn" title="稍后提醒" aria-label="稍后提醒" @click="snooze">
            <Icon name="x" :size="17" />
          </button>
        </header>

        <div class="body">
          <p class="lead">
            更新包已下载完成。安装过程约需一分钟，应用会退出并自动重启 ——
            正在进行的聊天与已接收的文件都不会丢失。
          </p>
          <p v-if="autoLabel" class="hint">
            如果一直没有操作，将在 <b>{{ autoLabel }}</b> 自动安装。
          </p>

          <div v-if="showTimePicker" class="picker">
            <div class="quick">
              <button class="btn btn-soft" :disabled="working" @click="scheduleAt(tonight())">
                今晚 22:00
              </button>
              <button class="btn btn-soft" :disabled="working" @click="scheduleAt(tomorrowNight())">
                明天 02:00
              </button>
            </div>
            <div class="custom">
              <input v-model="customAt" type="datetime-local" />
              <button class="btn btn-soft" :disabled="working" @click="scheduleCustom">确定</button>
            </div>
          </div>

          <p v-if="error" class="err">{{ error }}</p>
        </div>

        <footer class="foot">
          <button class="btn btn-soft" :disabled="working" @click="snooze">稍后提醒</button>
          <button
            class="btn btn-soft"
            :disabled="working"
            @click="showTimePicker = !showTimePicker"
          >
            选择时间…
          </button>
          <button class="btn btn-primary" :disabled="working" @click="applyNow">
            立即更新并重启
          </button>
        </footer>
      </section>
    </div>
  </Teleport>
</template>

<style scoped>
.overlay {
  position: fixed;
  inset: 0;
  z-index: 120;
  display: flex;
  align-items: center;
  justify-content: center;
  background: color-mix(in oklch, var(--bg-sunken) 62%, transparent);
  animation: sl-fade var(--dur-2) var(--ease) both;
}

.card {
  width: 460px;
  max-width: calc(100vw - 48px);
  display: flex;
  flex-direction: column;
  background: var(--bg-panel);
  border: 1px solid var(--divider-strong);
  border-radius: var(--r-lg);
  box-shadow: var(--shadow-lg);
  overflow: hidden;
}

.head {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 16px 16px 12px 18px;
  border-bottom: 1px solid var(--divider);
}

.badge {
  display: grid;
  place-items: center;
  width: 30px;
  height: 30px;
  flex: 0 0 auto;
  border-radius: var(--r-md);
  background: var(--accent-soft);
  color: var(--accent);
}

.titles {
  flex: 1 1 auto;
  min-width: 0;
}

h2 {
  margin: 0;
  font-family: var(--font-display);
  font-size: var(--fs-lg);
  font-weight: 600;
  letter-spacing: -0.01em;
}

.titles p {
  margin: 3px 0 0;
  font-size: var(--fs-xs);
  overflow-wrap: anywhere;
}

.body {
  padding: 14px 18px 6px;
}

.lead {
  margin: 0;
  font-size: var(--fs-sm);
  line-height: 1.65;
  color: var(--text-secondary);
}

.hint {
  margin: 8px 0 0;
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--text-muted);
}

.hint b {
  color: var(--text-secondary);
}

.picker {
  display: grid;
  gap: 8px;
  margin-top: 12px;
  padding: 10px;
  border: 1px solid var(--divider);
  border-radius: var(--r-md);
  background: var(--bg-sunken);
}

.quick,
.custom {
  display: flex;
  gap: 8px;
  align-items: center;
}

.custom input {
  flex: 1 1 auto;
  min-width: 0;
}

.err {
  margin: 10px 0 0;
  font-size: var(--fs-xs);
  color: var(--danger);
}

.foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  padding: 12px 18px 14px;
  border-top: 1px solid var(--divider);
}
</style>
