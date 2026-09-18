<script setup>
import petImg from './assets/pet.png'
import { ref, onMounted, onUnmounted, nextTick, watch } from 'vue'
import Bubble from './components/Bubble.vue'
import { Ask, Cancel, Say, HideWindow, Quit, History, SetMenuOpen, GetPersonaSnapshot, SetActivePersona } from '../wailsjs/go/main/App'
import { EventsOn, EventsOff } from '../wailsjs/runtime/runtime'

// 与 Go 侧 internal/ui/events.go 里的常量保持一致
const EVENT_SAY = 'bubble:say'
const EVENT_CHUNK = 'chat:chunk'
const EVENT_DONE = 'chat:done'
const EVENT_ERROR = 'chat:error'
const EVENT_PERSONA_CHANGED = 'persona:changed'

const bubbleText = ref('')
const draft = ref('')
const busy = ref(false)   // 是否正在流式回复
const streamId = ref('')  // 当前这一轮的 ID，用来过滤掉上一轮的残留片段
const inputEl = ref(null) // 输入框，用来在发送后把焦点还回去

const menuOpen = ref(false)
const historyItems = ref([])

// 菜单内的分区：历史 / 人格
const menuTab = ref('history')
const personaList = ref([])
const activePersonaId = ref('')
// 后端有没有连上持久化存储（PG）。false 时自建人格不会保存，界面要如实说明
const storageReady = ref(true)

async function loadPersona() {
  try {
    const snap = await GetPersonaSnapshot()
    personaList.value = snap?.personas ?? []
    activePersonaId.value = snap?.activeId ?? ''
    storageReady.value = snap?.storageReady ?? true
  } catch (e) {
    personaList.value = []
    activePersonaId.value = ''
    console.error('读取人格失败', e)
  }
}

async function loadHistory() {
  try {
    // 历史是只读快照，打开时拉一次即可，不做轮询、不做增量推送
    historyItems.value = (await History()) ?? []
  } catch (e) {
    historyItems.value = []
    console.error('读取历史失败', e)
  }
}

// 切换人格：立即生效（后端每轮现拼 system）。历史按人格隔离，所以切完要重新拉一份——
// 看到的是**那个人格自己的**对话，而不是上一个人格聊过的内容。
async function switchPersona(p) {
  if (!p || p.id === activePersonaId.value) return
  try {
    await SetActivePersona(p.id)
    activePersonaId.value = p.id // 先本地标记，菜单立刻有反馈
    // 切换会掐掉正在生成的那一轮（半截回复归旧人格）；气泡里残留的是它的内容，清掉避免串台
    bubbleText.value = ''
    streamId.value = ''
    busy.value = false
    await Promise.all([loadPersona(), loadHistory()])
  } catch (e) {
    console.error('切换人格失败', e)
    showToast('切换人格失败：' + (e?.message ?? e))
  }
}

// 人格回执（toast）：独立元素，不复用气泡——气泡里可能正在流式输出正文，
// 覆盖它会直接把回复吃掉。
const toast = ref('')
let toastTimer = null

function showToast(text) {
  toast.value = text
  clearTimeout(toastTimer)
  toastTimer = setTimeout(() => {
    toast.value = ''
  }, 6000)
}

// 后端记住了用户的长期要求（"以后叫我主人"）后推来的回执。
// summary 由后端组装好，前端只负责显示——文案只有一处真相。
function onPersonaChanged(p) {
  if (!p || !p.summary) return
  showToast(p.summary)
  // 当前人格可能被后端换掉了（例如对内置人格提要求时自动复制一份"我的"），
  // 菜单开着就刷新列表，否则用户会看到一份过期的清单
  if (menuOpen.value) loadPersona()
}

async function openMenu() {
  menuOpen.value = true
  SetMenuOpen(true)
  await Promise.all([loadHistory(), loadPersona()])
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

// 把焦点还给输入框：连续对话时不该每轮都要再点一下。
//
// 注意这里**没有**用 :disabled="busy" 去锁输入框——被禁用的元素会被浏览器直接摘掉焦点，
// 等回复结束再启用，焦点也不会自己回来（这就是"每轮都要点一下"的根因）。
// 回复期间禁止重复发送由 send() 里的 busy 判断负责，输入框保持可打字，正好可以边等边写下一句。
function focusInput() {
  nextTick(() => inputEl.value?.focus())
}

// 一轮结束时（正常说完、出错、被停止）都把光标送回输入框
watch(busy, (running) => {
  if (!running) focusInput()
})

async function send() {
  const text = draft.value.trim()
  if (!text || busy.value) return

  draft.value = ''
  bubbleText.value = ''
  busy.value = true
  focusInput()
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
  EventsOn(EVENT_PERSONA_CHANGED, onPersonaChanged)
  setTimeout(() => say('你好呀，我已经驻留在你的桌面上了～'), 500)
})

onUnmounted(() => {
  EventsOff(EVENT_SAY)
  EventsOff(EVENT_CHUNK)
  EventsOff(EVENT_DONE)
  EventsOff(EVENT_ERROR)
  EventsOff(EVENT_PERSONA_CHANGED)
  clearTimeout(toastTimer)
})
</script>

<template>
  <div class="companion">
    <header class="dragbar">
      <button class="dragbar__icon" title="历史记录" @click="toggleMenu">☰</button>
      <span class="dragbar__title">With-You</span>
      <button class="dragbar__btn" title="隐藏到托盘" @click="onHide()">×</button>
    </header>

    <main class="stage">
      <!-- 流式期间 duration=0，避免气泡在长回复中途自动收起 -->
      <Bubble :text="bubbleText" :duration="busy ? 0 : 8000" />
      <button class="pet" title="点我：有输入就发送，没输入就打个招呼" @click="onPetClick">
        <img class = "pet__face" :src = "petImg" alt = "" draggable = "false" />
      </button>
    </main>

    <!-- 菜单浮层：必须放在 .stage 之外，那里的 overflow: hidden 会把它裁掉 -->
    <Transition name="menu">
      <section v-if="menuOpen" class="menu">
        <nav class="menu__tabs">
          <button
            class="menu__tab"
            :class="{ 'menu__tab--on': menuTab === 'history' }"
            @click="menuTab = 'history'"
          >
            历史
          </button>
          <button
            class="menu__tab"
            :class="{ 'menu__tab--on': menuTab === 'persona' }"
            @click="menuTab = 'persona'"
          >
            人格
          </button>
        </nav>

        <template v-if="menuTab === 'history'">
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
        </template>

        <template v-else>
          <p v-if="!storageReady" class="menu__warn">
            数据库未连接：自建人格不会保存，当前只能用内置人格
          </p>
          <p v-if="!personaList.length" class="menu__empty">没有可用的人格</p>
          <ul v-else class="menu__list">
            <li
              v-for="p in personaList"
              :key="p.id"
              class="menu__item menu__item--pick"
              :class="{ 'menu__item--on': p.id === activePersonaId }"
              :title="p.id === activePersonaId ? '当前人格' : '点一下切换到这个人格'"
              @click="switchPersona(p)"
            >
              <div class="menu__meta">
                <span>{{ p.name }}</span>
                <span v-if="p.isBuiltin" class="menu__tag">内置</span>
                <span v-if="p.id === activePersonaId" class="menu__tag menu__tag--on">当前</span>
              </div>
              <p class="menu__text">{{ p.seedText }}</p>
            </li>
          </ul>
          <p class="menu__hint">切换后立即生效；每个人格有各自独立的对话历史</p>
        </template>
      </section>
    </Transition>

    <!-- 人格回执：独立短提示，避免覆盖正在流式输出的气泡 -->
    <Transition name="menu">
      <p v-if="toast" class="toast">{{ toast }}</p>
    </Transition>

    <footer class="composer">
      <input
        ref="inputEl"
        v-model="draft"
        class="composer__input"
        type="text"
        :placeholder="busy ? '正在回复…' : '说点什么，回车发送'"
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

/* 菜单内的分区切换：历史 / 人格 */
.menu__tabs {
  display: flex;
  gap: 4px;
  padding: 6px 8px 0;
}

.menu__tab {
  flex: 1;
  padding: 5px 0;
  border: 0;
  border-radius: 8px;
  background: rgba(0, 0, 0, 0.04);
  color: #6b6b7b;
  font-size: 12px;
  cursor: pointer;
}

.menu__tab--on {
  background: rgba(124, 108, 245, 0.14);
  color: #5b4bd6;
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

/* 人格列表：可点选 */
.menu__item--pick {
  cursor: pointer;
  background: rgba(0, 0, 0, 0.03);
}

.menu__item--pick:hover {
  background: rgba(0, 0, 0, 0.07);
}

.menu__item--on {
  background: rgba(124, 108, 245, 0.14);
}

/* 人格的种子文本可能很长，列表里只露两行，避免一行人格占满整屏 */
.menu__item--pick .menu__text {
  display: -webkit-box;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
  overflow: hidden;
}

.menu__tag--on {
  background: rgba(124, 108, 245, 0.2);
  color: #5b4bd6;
}

.menu__hint {
  margin: 0;
  padding: 6px 10px 8px;
  border-top: 1px solid rgba(0, 0, 0, 0.06);
  font-size: 10px;
  color: #9a9aa8;
}

/* 数据库未连接一类的提醒：要让用户看见，不能只写进日志 */
.menu__warn {
  margin: 0;
  padding: 6px 10px;
  background: rgba(255, 157, 92, 0.14);
  color: #b2652b;
  font-size: 11px;
  line-height: 1.5;
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

/* 人格回执：贴在输入框上方，几秒后自动消失 */
.toast {
  position: absolute;
  left: 10px;
  right: 10px;
  bottom: 52px;
  z-index: 20;
  margin: 0;
  padding: 6px 10px;
  border-radius: 8px;
  background: rgba(124, 108, 245, 0.95);
  color: #fff;
  font-size: 11px;
  line-height: 1.5;
  box-shadow: 0 6px 18px rgba(30, 25, 60, 0.25);
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
