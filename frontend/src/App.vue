<script setup>
import petImg from './assets/pet.png'
import { ref, onMounted, onUnmounted } from 'vue'
import Bubble from './components/Bubble.vue'
import { Ask, Cancel, Say, HideWindow, Quit, History, SetMenuOpen } from '../wailsjs/go/main/App'
import { EventsOn, EventsOff } from '../wailsjs/runtime/runtime'

// 与 Go 侧 internal/ui/events.go 里的常量保持一致
const EVENT_SAY = 'bubble:say'
const EVENT_CHUNK = 'chat:chunk'
const EVENT_DONE = 'chat:done'
const EVENT_ERROR = 'chat:error'

const bubbleText = ref('')
const draft = ref('')
const busy = ref(false)   // 是否正在流式回复
const streamId = ref('')  // 当前这一轮的 ID，用来过滤掉上一轮的残留片段

const menuOpen = ref(false)
const historyItems = ref([])

async function openMenu() {
  menuOpen.value = true
  SetMenuOpen(true)
  try {
    // 历史是只读快照，打开时拉一次即可，不做轮询、不做增量推送
    historyItems.value = (await History()) ?? []
  } catch (e) {
    historyItems.value = []
    console.error('读取历史失败', e)
  }
}

function closeMenu() {
  menuOpen.value = false
  SetMenuOpen(false)
}

function toggleMenu() {
  menuOpen.value ? closeMenu() : openMenu()
}

// 隐藏前先收起菜单：否则窗口会带着「加高后的尺寸 + 打开的面板」一起被隐藏，
// 下次从托盘唤出时尺寸与预期不符
function onHide() {
  if (menuOpen.value) closeMenu()
  HideWindow()
}

// 菜单里只显示 时:分，够用了；跨天的记录补上月-日，否则长会话里分不清先后
function fmtTime(at) {
  const d = new Date(at)
  const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  return d.toDateString() === new Date().toDateString()
    ? hm
    : `${d.getMonth() + 1}-${d.getDate()} ${hm}`
}

function say(text) {
  if (!text) return
  bubbleText.value = ''
  requestAnimationFrame(() => {
    bubbleText.value = text
  })
}

function onSayEvent(payload) {
  say(typeof payload === 'string' ? payload : payload?.text)
}

function errText(e) {
  return typeof e === 'string' ? e : (e?.message ?? '未知错误')
}

async function send() {
  const text = draft.value.trim()
  if (!text || busy.value) return

  draft.value = ''
  bubbleText.value = ''
  busy.value = true
  try {
    // Ask 立刻返回本轮 ID，正文随后通过事件流回来
    streamId.value = await Ask(text)
  } catch (e) {
    busy.value = false
    bubbleText.value = '出错了：' + errText(e)
  }
}

function stop() {
  Cancel()
  busy.value = false
}

// 中文输入法组词时按回车是「选词」而不是「发送」，这个事件必须放行给输入法，
// 否则拼音还没上屏就会把半截内容发出去。
function onEnter(e) {
  if (e.isComposing) return
  send()
}

// 桌宠按钮：输入框里有草稿就当作发送，空着就退回阶段1 的 Say()。
// 保留 Say() 这条通路是有意的——它不依赖模型，key 没配好时也能确认气泡链路活着。
function onPetClick() {
  if (busy.value) return
  if (draft.value.trim()) {
    send()
    return
  }
  Say('我是Lapwing,你过得还好吗')
}

// 只认当前这一轮的事件：Ask 会掐掉上一轮，但上一轮可能还有片段在路上
function onChunk(p) {
  if (!p || p.id !== streamId.value) return
  bubbleText.value += p.delta
}

function onDone(p) {
  if (!p || p.id !== streamId.value) return
  busy.value = false
}

function onError(p) {
  if (!p || p.id !== streamId.value) return
  busy.value = false
  bubbleText.value = p.message
}

onMounted(() => {
  EventsOn(EVENT_SAY, onSayEvent)
  EventsOn(EVENT_CHUNK, onChunk)
  EventsOn(EVENT_DONE, onDone)
  EventsOn(EVENT_ERROR, onError)
  setTimeout(() => say('你好呀，我已经驻留在你的桌面上了～'), 500)
})

onUnmounted(() => {
  EventsOff(EVENT_SAY)
  EventsOff(EVENT_CHUNK)
  EventsOff(EVENT_DONE)
  EventsOff(EVENT_ERROR)
})
</script>

<template>
  <div class="companion">
    <header class="dragbar">
      <button class="dragbar__icon" title="历史记录" @click="toggleMenu">☰</button>
      <span class="dragbar__title">AI 桌面伴侣</span>
      <button class="dragbar__btn" title="隐藏到托盘" @click="onHide()">×</button>
    </header>

    <main class="stage">
      <!-- 流式期间 duration=0，避免气泡在长回复中途自动收起 -->
      <Bubble :text="bubbleText" :duration="busy ? 0 : 8000" />
      <button class="pet" title="点我：有输入就发送，没输入就打个招呼" @click="onPetClick">
        <img class = "pet__face" :src = "petImg" alt = "" draggable = "false" />
      </button>
    </main>

    <!-- 历史浮层：必须放在 .stage 之外，那里的 overflow: hidden 会把它裁掉 -->
    <Transition name="menu">
      <section v-if="menuOpen" class="menu">
        <p v-if="!historyItems.length" class="menu__empty">还没有对话记录</p>
        <ul v-else class="menu__list">
          <!-- 列表只增不改、每条 id 唯一，用 id 做 key 是安全的 -->
          <li
            v-for="item in historyItems"
            :key="item.id"
            class="menu__item"
            :class="item.role === 'user' ? 'menu__item--user' : 'menu__item--bot'"
          >
            <div class="menu__meta">
              <span>{{ item.role === 'user' ? '我' : '伴侣' }}</span>
              <span>{{ fmtTime(item.at) }}</span>
              <span v-if="item.status === 'canceled'" class="menu__tag">已打断</span>
            </div>
            <p class="menu__text">{{ item.text }}</p>
          </li>
        </ul>
      </section>
    </Transition>

    <footer class="composer">
      <input
        v-model="draft"
        class="composer__input"
        type="text"
        :placeholder="busy ? '正在回复…' : '说点什么，回车发送'"
        :disabled="busy"
        @keydown.enter="onEnter"
      />
      <button v-if="!busy" class="composer__btn" @click="send">发送</button>
      <button v-else class="composer__btn composer__btn--stop" @click="stop">停止</button>
    </footer>
  </div>
</template>

<style scoped>
.stage {
  /* 关键：长回复不能把桌宠挤出可视区 */
  min-height: 0;
}

.composer {
  display: flex;
  gap: 6px;
  align-items: center;
}

.composer__input {
  --wails-draggable: no-drag;

  flex: 1;
  min-width: 0;
  padding: 8px 10px;
  border: 0;
  border-radius: 8px;
  background: rgba(255, 255, 255, 0.92);
  box-shadow: inset 0 0 0 1px rgba(0, 0, 0, 0.07);
  font-size: 12px;
  color: #2b2b33;
  outline: none;
}

.composer__input:focus {
  box-shadow: inset 0 0 0 1px rgba(124, 108, 245, 0.55);
}

.composer__input:disabled {
  background: rgba(255, 255, 255, 0.6);
  color: #9a9aa8;
}

.composer__btn {
  --wails-draggable: no-drag;

  padding: 8px 12px;
  border: 0;
  border-radius: 8px;
  background: linear-gradient(135deg, #7c6cf5, #a98bfa);
  color: #fff;
  font-size: 12px;
  cursor: pointer;
}

.composer__btn--stop {
  background: rgba(255, 255, 255, 0.9);
  color: #5a5a6b;
  box-shadow: inset 0 0 0 1px rgba(0, 0, 0, 0.08);
}
.companion {
  /* 整个窗口默认不可拖，只有 .dragbar 例外 */
  --wails-draggable: no-drag;

  /* 历史浮层的定位参照：浮层是 absolute，需要一个 position 不为 static 的祖先 */
  position: relative;

  display: flex;
  flex-direction: column;
  height: 100%;
  padding: 8px 10px 10px;
  box-sizing: border-box;
  gap: 8px;
}

.dragbar {
  --wails-draggable: drag;

  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 5px 8px 5px 12px;
  border-radius: 10px;
  background: rgba(255, 255, 255, 0.78);
  box-shadow: 0 2px 10px rgba(30, 25, 60, 0.1);
  cursor: grab;
}

.dragbar:active {
  cursor: grabbing;
}

.dragbar__title {
  font-size: 12px;
  color: #6b6b7b;
  letter-spacing: 0.5px;
}

.dragbar__btn {
  --wails-draggable: no-drag;

  width: 22px;
  height: 22px;
  border: 0;
  border-radius: 6px;
  background: transparent;
  color: #8a8a99;
  font-size: 16px;
  line-height: 1;
  cursor: pointer;
}

.dragbar__btn:hover {
  background: rgba(0, 0, 0, 0.06);
  color: #33333d;
}

.dragbar__icon {
  --wails-draggable: no-drag;

  width: 22px;
  height: 22px;
  border: 0;
  border-radius: 6px;
  background: transparent;
  color: #8a8a99;
  font-size: 14px;
  line-height: 1;
  cursor: pointer;
}

.dragbar__icon:hover {
  background: rgba(0, 0, 0, 0.06);
  color: #33333d;
}

.stage {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: flex-end;
  gap: 16px;
  padding-bottom: 4px;
  overflow: hidden;
}

/* 阶段6 会换成 Live2D / VRM，这里先用静态占位 */
.pet {
  --wails-draggable: no-drag;

  display: flex;
  align-items: center;
  justify-content: center;
  width: 112px;
  height: 112px;
  border: 0;
  border-radius: 50%;
  overflow: hidden;
  background: transparent;
  cursor: pointer;
  transition: transform 0.16s ease;
}

.pet__face {
  width: 100%;
  height: 100%;
  object-fit: cover; 
  pointer-events: none;  /* 点击必须落在 button 上，别让 img 吃掉 */
  clip-path: circle(50% at 50% 50%);
  aspect-ratio: 1 / 1;
}

.pet:hover {
  transform: translateY(-3px) scale(1.03);
}

.pet:active {
  transform: translateY(0) scale(0.98);
}

/* 历史浮层盖住拖拽区以下的全部区域：只读浏览，不需要与桌宠争空间。
   top 取 44px 是为了让拖拽区（含 ☰ 与 ×）露在外面，随时能收起菜单。 */
.menu {
  position: absolute;
  left: 10px;
  right: 10px;
  top: 44px;
  bottom: 10px;
  z-index: 10;
  display: flex;
  flex-direction: column;
  border-radius: 12px;
  background: rgba(255, 255, 255, 0.97);
  box-shadow: 0 12px 30px rgba(30, 25, 60, 0.22);
  overflow: hidden;
}

.menu__list {
  flex: 1;
  margin: 0;
  padding: 8px;
  list-style: none;
  overflow-y: auto;
}

.menu__item {
  padding: 6px 8px;
  border-radius: 8px;
}

.menu__item + .menu__item {
  margin-top: 6px;
}

.menu__item--user {
  background: rgba(124, 108, 245, 0.08);
}

.menu__meta {
  display: flex;
  gap: 6px;
  align-items: center;
  font-size: 10px;
  color: #9a9aa8;
}

.menu__tag {
  padding: 1px 6px;
  border-radius: 6px;
  background: rgba(255, 157, 92, 0.18);
  color: #b2652b;
}

.menu__text {
  margin: 3px 0 0;
  font-size: 12px;
  line-height: 1.55;
  color: #2b2b33;
  white-space: pre-wrap;
  word-break: break-word;
}

.menu__empty {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  margin: 0;
  font-size: 12px;
  color: #9a9aa8;
}

.menu-enter-active,
.menu-leave-active {
  transition: opacity 0.16s ease;
}

.menu-enter-from,
.menu-leave-to {
  opacity: 0;
}

.actions {
  display: flex;
  gap: 6px;
}

.btn {
  --wails-draggable: no-drag;

  flex: 1;
  padding: 7px 0;
  border: 0;
  border-radius: 8px;
  background: linear-gradient(135deg, #7c6cf5, #a98bfa);
  color: #fff;
  font-size: 12px;
  cursor: pointer;
  transition: filter 0.15s ease;
}

.btn:hover {
  filter: brightness(1.08);
}

.btn--ghost {
  background: rgba(255, 255, 255, 0.82);
  color: #5a5a6b;
  box-shadow: inset 0 0 0 1px rgba(0, 0, 0, 0.06);
}

.btn--ghost:hover {
  background: #fff;
}
</style>
