<script setup>
import { ref, watch, onUnmounted } from 'vue'

const props = defineProps({
  text: { type: String, default: '' },
  // 自动收起时间（毫秒），设为 0 表示不自动收起
  duration: { type: Number, default: 8000 },
})

const visible = ref(false)
let timer = null

function clearTimer() {
  if (timer !== null) {
    clearTimeout(timer)
    timer = null
  }
}

function hide() {
  clearTimer()
  visible.value = false
}

// 文本变化 = 说了一句新话：显示气泡并重新计时
watch(
  [() => props.text, () => props.duration],
  ([val, duration]) => {
    if (!val) {
      hide()
      return
    }
    clearTimer()
    visible.value = true
    if (duration > 0) {
      timer = setTimeout(() => {
        visible.value = false
      }, duration)
    }
  },
  { immediate: true },
)

onUnmounted(clearTimer)
</script>

<template>
  <Transition name="bubble">
    <!-- 点一下气泡即可收起；气泡本身不是拖拽区 -->
    <div v-if="visible" class="bubble" title="点一下收起" @click="hide">
      <p class="bubble__text">{{ text }}</p>
      <span class="bubble__tail" aria-hidden="true"></span>
    </div>
  </Transition>
</template>

<style scoped>
.bubble {
  --wails-draggable: no-drag;

  position: relative;
  max-width: 260px;
  padding: 10px 14px;
  border-radius: 14px;
  background: rgba(255, 255, 255, 0.96);
  box-shadow: 0 6px 20px rgba(30, 25, 60, 0.18);
  cursor: pointer;
}

.bubble__text {
  margin: 0;
  font-size: 13px;
  line-height: 1.6;
  color: #2b2b33;
  white-space: pre-wrap;
  word-break: break-word;
  /* 长回复要在气泡内部滚动，否则会把桌宠挤出可视区、气泡顶部被 .stage 裁掉。
     滚动必须放在内层：直接给 .bubble 加 overflow 会把下方那个小尖角一起裁掉。 */
  max-height: 180px;
  overflow-y: auto;
}

/* 指向桌宠的小尖角 */
.bubble__tail {
  position: absolute;
  left: 50%;
  bottom: -6px;
  width: 14px;
  height: 14px;
  margin-left: -7px;
  border-radius: 2px;
  background: rgba(255, 255, 255, 0.96);
  transform: rotate(45deg);
}

.bubble-enter-active,
.bubble-leave-active {
  transition: opacity 0.18s ease, transform 0.18s ease;
}

.bubble-enter-from,
.bubble-leave-to {
  opacity: 0;
  transform: translateY(6px) scale(0.96);
}
</style>
