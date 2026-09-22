<script setup lang="ts">
/**
 * 图片查看器。
 *
 * 复用 Modal 的遮罩、Esc 与焦点处理，不自己实现一遍 ——
 * 这类细节重复实现的代价是「每次都要重新发现一遍漏掉的那一种关闭方式」。
 */
import { computed, ref } from 'vue'

import Icon from './Icon.vue'
import Modal from './Modal.vue'
import { getApi, isMockMode } from '../api'
import type { Message } from '../api/types'
import { saveFile } from '../api/dialog'
import { useUiStore } from '../stores/ui'
import { defaultImageName, imageSrc, parseImageContent } from '../utils/image'

const props = defineProps<{ message: Message }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const ui = useUiStore()

const img = computed(() => parseImageContent(props.message.content))

/** JSON 合法但数据无法解码时同样落到占位，与气泡里的处理保持一致。 */
const broken = ref(false)
const sizeText = computed(() => {
  const m = img.value
  if (!m?.w || !m.h) return ''
  const kb = Math.round((m.b64.length * 3) / 4 / 1024)
  return `${m.w} × ${m.h} · 约 ${kb} KB`
})

/** 另存为：桌面走原生保存对话框，浏览器预览退化成下载。 */
async function saveAs(): Promise<void> {
  const m = img.value
  if (!m) return
  const name = defaultImageName(m.mime)

  if (isMockMode()) {
    const a = document.createElement('a')
    a.href = imageSrc(m)
    a.download = name
    a.click()
    return
  }

  const dest = await saveFile(name)
  if (!dest) return
  try {
    await (await getApi()).saveImage(props.message.msgId, dest)
    ui.notify('已保存')
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || '保存失败')
  }
}
</script>

<template>
  <Modal title="图片" :subtitle="sizeText" :width="900" :height="700" @close="emit('close')">
    <div class="lb">
      <img v-if="img && !broken" :src="imageSrc(img)" alt="图片" @error="broken = true" />
      <div v-else class="faint broken">图片显示失败</div>

      <div class="bar">
        <button v-if="img && !broken" class="btn btn-soft" @click="saveAs">
          <Icon name="download" :size="14" />另存为
        </button>
        <button class="btn btn-soft" @click="emit('close')">关闭</button>
      </div>
    </div>
  </Modal>
</template>

<style scoped>
.lb {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
  padding: 0 20px 18px;
}

.lb img {
  max-width: 100%;
  max-height: 62vh;
  object-fit: contain;
  border-radius: var(--r-lg);
  box-shadow: 0 18px 44px -26px rgba(2, 0, 12, 0.95);
}

.broken {
  padding: 60px 0;
}

.bar {
  display: flex;
  justify-content: center;
  gap: 8px;
}
</style>
