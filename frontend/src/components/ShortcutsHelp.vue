<script setup lang="ts">
/**
 * 快捷键一览（Ctrl+/）。
 *
 * 只读参考页，**内容全部来自命令表**（hotkeys/commands.ts）—— 手写一份
 * 清单当帮助页是最容易腐化的做法：加了命令没人记得改，用户就会照着
 * 一份过期文档按半天。
 *
 * 它是独占层：开着的时候 Ctrl+K 之类的命令不派发。否则用户按 Ctrl+/ 想
 * 「看看还有哪些键」，一按别的组合就把搜索弹层叠在上面，两个都想关。
 */
import { computed, onBeforeUnmount, watch } from 'vue'

import Icon from './Icon.vue'
import { COMMANDS, GROUP_LABELS, type HotkeyGroup } from '../hotkeys/commands'
import { pushHotkeyLayer } from '../hotkeys/dispatcher'
import { formatAccel } from '../hotkeys/keys'
import { useHotkeysStore } from '../stores/hotkeys'
import { useUiStore } from '../stores/ui'

const ui = useUiStore()
const keys = useHotkeysStore()

/** 分组的展示顺序：先「在后台也能按的」，再会话，再面板。 */
const ORDER: HotkeyGroup[] = ['global', 'session', 'panel', 'help']

const sections = computed(() =>
  ORDER.map((g) => ({
    key: g,
    label: GROUP_LABELS[g],
    rows: COMMANDS.filter((c) => c.group === g)
  })).filter((s) => s.rows.length > 0)
)

/** 独占层：Esc 关掉自己，其余命令键不派发。 */
let pop: (() => void) | null = null

watch(
  () => ui.helpVisible,
  (visible) => {
    if (visible && !pop) {
      // allow: 让 Ctrl+/ 自己也能关掉本页（独占层默认把命令键全挡住）。
      pop = pushHotkeyLayer({
        exclusive: true,
        allow: ['help'],
        onEsc: () => ui.toggleHelp(false)
      })
      return
    }
    if (!visible) {
      pop?.()
      pop = null
    }
  },
  { immediate: true }
)

onBeforeUnmount(() => {
  pop?.()
  pop = null
})
</script>

<template>
  <Teleport to="body">
    <div v-if="ui.helpVisible" class="overlay" @click.self="ui.toggleHelp(false)">
      <section class="card anim-pop" role="dialog" aria-modal="true" aria-labelledby="hk-title">
        <header class="head">
          <div class="titles">
            <h2 id="hk-title">快捷键</h2>
            <p class="faint">这里显示的是当前实际生效的键位</p>
          </div>
          <button class="icon-btn" title="关闭" aria-label="关闭" @click="ui.toggleHelp(false)">
            <Icon name="x" :size="17" />
          </button>
        </header>

        <div class="body scroll">
          <div v-for="s in sections" :key="s.key" class="group">
            <h3>{{ s.label }}</h3>
            <div v-for="c in s.rows" :key="c.id" class="row">
              <span class="label">{{ c.label }}</span>
              <!-- 全局键“没绑上”是常态（被别的程序占着），必须如实说出来，
                   否则用户会一直以为是自己按错了。 -->
              <span v-if="keys.errorOf(c.id)" class="bad">已被其他程序占用</span>
              <kbd v-if="keys.boundOf(c)">{{ formatAccel(keys.boundOf(c)) }}</kbd>
              <span v-else class="faint">未绑定</span>
            </div>
          </div>

          <div class="group">
            <h3>固定键位（不可修改）</h3>
            <div class="row">
              <span class="label">关闭最上层弹层 / 取消当前操作</span>
              <kbd>Esc</kbd>
            </div>
            <div class="row">
              <span class="label">发送消息（焦点在输入框）</span>
              <kbd>Enter</kbd>
            </div>
            <div class="row">
              <span class="label">换行（焦点在输入框）</span>
              <kbd>Shift + Enter</kbd>
            </div>
          </div>

          <p v-if="keys.mock" class="note">
            浏览器预览模式没有后端，全局快捷键不可用；窗口内快捷键仍可生效。
          </p>
        </div>

        <footer class="bar">
          <span class="faint">想改键位？设置 → 快捷键</span>
          <button class="btn btn-soft" @click="ui.toggleHelp(false)">关闭</button>
        </footer>
      </section>
    </div>
  </Teleport>
</template>

<style scoped>
.overlay {
  position: fixed;
  inset: 0;
  z-index: 110;
  display: flex;
  align-items: center;
  justify-content: center;
  background: color-mix(in oklch, var(--bg-sunken) 62%, transparent);
  animation: sl-fade var(--dur-2) var(--ease) both;
}

.card {
  display: flex;
  flex-direction: column;
  width: 520px;
  max-width: calc(100vw - 48px);
  max-height: calc(100vh - 96px);
  background: var(--bg-panel);
  border: 1px solid var(--divider-strong);
  border-radius: var(--r-lg);
  box-shadow: var(--shadow-lg);
  overflow: hidden;
}

.head {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 16px 16px 14px 20px;
  border-bottom: 1px solid var(--divider);
}

.titles {
  flex: 1 1 auto;
  min-width: 0;
}

h2 {
  margin: 0;
  font-family: var(--font-display);
  font-size: var(--fs-xl);
  font-weight: 600;
}

.titles p {
  margin: 2px 0 0;
  font-size: var(--fs-xs);
}

.body {
  flex: 1 1 auto;
  min-height: 0;
  overflow-y: auto;
  padding: 14px 20px 18px;
}

.group + .group {
  margin-top: 16px;
  padding-top: 14px;
  border-top: 1px dashed var(--divider);
}

h3 {
  margin: 0 0 8px;
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
  gap: 10px;
  min-height: 28px;
  font-size: var(--fs-md);
}

.label {
  flex: 1 1 auto;
  min-width: 0;
  color: var(--text-secondary);
}

.bad {
  flex: 0 0 auto;
  font-size: var(--fs-xs);
  color: var(--warn);
}

kbd {
  flex: 0 0 auto;
  padding: 2px 7px;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-primary);
  background: var(--bg-elevated);
  border: 1px solid var(--divider-strong);
  border-bottom-width: 2px;
  border-radius: var(--r-xs);
}

.note {
  margin: 16px 0 0;
  font-size: var(--fs-xs);
  color: var(--text-muted);
}

.bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 20px;
  border-top: 1px solid var(--divider);
}

.bar .faint {
  font-size: var(--fs-xs);
}
</style>
