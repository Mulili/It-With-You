<script setup>
import petImg from './assets/pet.png'
import { ref, computed, onMounted, onUnmounted, nextTick, watch } from 'vue'
import Bubble from './components/Bubble.vue'
import {
  Ask, Cancel, Say, HideWindow, Quit, History, SessionMessages, Memories, DeleteMemory, SetMenuOpen,
  GetPersonaSnapshot, SetActivePersona, GetPersonaRules, GetPersonaChanges, GetPersonaMeta,
  CreatePersona, RenamePersona, DeletePersona, SaveSeedText,
  SaveRule, DeleteRule, SetRuleEnabled,
  RuleCandidates, AcceptRuleCandidate, RejectRuleCandidate,
  GetSettings, SetThinkingDisabled, ExportPersonaToFile, ImportPersonaFromFile,
} from '../wailsjs/go/main/App'
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
// 历史是**两级**的：先列会话，点进去再看那一段的消息（见 internal/ui/history.go 的说明）。
// openSession 为 null 表示正停在会话列表那一级。
const historySessions = ref([])
const openSession = ref(null)
const sessionMsgs = ref([])
// 「它记得什么」列表
const memories = ref([])
// 正在等二次确认的那条记忆 ID（空串 = 没有）。
// 删除不可恢复，而窗口很小、误触代价高，所以要点两下。
const memoryPendingDelete = ref('')

// 菜单内的分区：历史 / 记忆 / 人格 / 设置
const menuTab = ref('history')
const personaList = ref([])
const activePersonaId = ref('')
// 后端有没有连上持久化存储（PG）。false 时自建人格不会保存，界面要如实说明
const storageReady = ref(true)

// 全局设置（⑨）。思考开关是应用级的，不属于任何人格，所以不塞进人格快照里
const settings = ref({ thinkingDisabled: false })
// 导出哪个人格；空串 = 用当前生效的那个
const exportId = ref('')

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
    // 历史是只读快照，打开菜单时拉一次即可，不做轮询、不做增量推送
    historySessions.value = (await History()) ?? []
  } catch (e) {
    historySessions.value = []
    console.error('读取会话列表失败', e)
  }
}

// 点开一条会话：只拉这一条的消息。为什么要分两级而不是一次全带上，见 internal/ui/history.go
async function openSessionMessages(s) {
  openSession.value = s
  sessionMsgs.value = []
  try {
    sessionMsgs.value = (await SessionMessages(s.id)) ?? []
  } catch (e) {
    console.error('读取会话消息失败', e)
  }
}

// 返回会话列表。顺手清掉已展开的消息：下次点进去该重新拉——
// 期间后台可能刚把这一段结算完（标题都是新生成的），留着旧快照会让人以为标题没生效
function closeSession() {
  openSession.value = null
  sessionMsgs.value = []
}

async function loadMemories() {
  try {
    memories.value = (await Memories()) ?? []
  } catch (e) {
    memories.value = []
    console.error('读取记忆失败', e)
  }
}

async function removeMemory(m) {
  try {
    await DeleteMemory(m.id)
  } catch (e) {
    formError.value = errText(e)
  }
  memoryPendingDelete.value = ''
  await loadMemories()
}

// fact / preference / event / promise → 中文。
// 认不出来的按原值显示，而不是留空：将来后端加了新类型，界面上也该看得见
const MEMORY_KINDS = { fact: '事实', preference: '偏好', event: '经历', promise: '约定' }
function kindLabel(kind) {
  return MEMORY_KINDS[kind] ?? kind
}

// ---------- 设置（⑨）----------

async function loadSettings() {
  try {
    const s = await GetSettings()
    settings.value = { thinkingDisabled: s?.thinkingDisabled ?? false }
  } catch (e) {
    console.error('读取设置失败', e)
  }
}

// 思考开关。文案里要如实说出代价：关掉换来的是"快"，代价是复杂推理会变弱，
// 不能只说"更快了"——用户第二天遇到模型答错时会不知道是自己关的。
async function toggleThinking() {
  const next = !settings.value.thinkingDisabled
  try {
    await SetThinkingDisabled(next)
    settings.value.thinkingDisabled = next
    showToast(next ? '已关闭思考：回复更快，复杂推理会变弱' : '已开启思考：回复更稳，首字更慢')
  } catch (e) {
    showToast('保存失败：' + errText(e))
  }
}

// 导出选中的那个人格（没选就是当前人格）。
// 内置人格也允许导出——那正是"产出自己的人格"的通道：在应用里调好再导出来。
async function exportPersona() {
  const id = exportId.value || activePersonaId.value
  if (!id) return
  try {
    const path = await ExportPersonaToFile(id)
    if (!path) return // 用户取消了保存对话框，不是错误
    showToast('已导出到 ' + path)
  } catch (e) {
    showToast('导出失败：' + errText(e))
  }
}

// 导入：同名不覆盖，后端会存成一份副本，所以这里只要刷新列表并把新名字报出来
async function importPersona() {
  try {
    const p = await ImportPersonaFromFile()
    if (!p || !p.id) return // 用户取消了打开对话框
    await loadPersona()
    showToast(`已导入为「${p.name}」`)
  } catch (e) {
    showToast('导入失败：' + errText(e))
  }
}

// ---------- 数据库未就绪时阻断（⑪）----------
//
// 数据库是**硬性要求**：人格不持久，等于"随对话成长"这件事不存在，重启就回到出厂状态——
// 那是个假陪伴。所以这里不是"提示一下、继续用"，而是明确挡住，同时把"该怎么修"直接摆在眼前。
//
// 形态上选"启动但阻断"而不是"启动即退出"：桌面应用闪退是最难排查的故障（本项目的
// README 里就记着这条教训）。一个看得见的说明页，用户能自己走完，也知道自己卡在哪一步。

// dbReady 为 false 时整个界面进入阻断态
const dbReady = computed(() => storageReady.value !== false)

// 能命令代劳的两步。装 PostgreSQL 本身没法代劳，只在文案里指路（README 有完整步骤）。
const setupCmd = [
  'psql -U postgres -c "CREATE DATABASE companion;"',
  'psql -U postgres -d companion -c "CREATE EXTENSION IF NOT EXISTS vector;"',
].join('\n')

async function copySetupCmd() {
  try {
    await navigator.clipboard.writeText(setupCmd)
    showToast('命令已复制')
  } catch (e) {
    // WebView2 里 clipboard API 可能因非 secure context 不可用。不静默失败：
    // 告诉用户可以手动选中——那个 textarea 本来就是可选的。
    showToast('复制失败，请手动选中命令再复制')
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

// ---------- 人格编辑器（⑧ 最小片）----------
//
// 两层视图：列表（点一行 = 切换人格）→ 详情（主体文本 / 重命名 / 删除 / 复制 + 规则增删改停用 + 变更记录）。
//
// 表单与"删除确认"都做在浮层内，刻意不用 window.prompt / window.confirm：
// WebView2 对 prompt 的支持不可靠，而且系统对话框会盖住这个 340px 的小窗，观感很突兀。
// 槽位清单与各字段的字数上限都来自后端（GetPersonaMeta），不在前端硬编码一份会走样的表。

const slots = ref([])          // 规范槽位清单，由后端 SlotSpecs() 提供
// 各字段的字数上限。先给一份保守默认值，取到真值前按钮也能用（后端仍会兜底校验）
const limits = ref({ seedTextRunes: 1200, ruleValueRunes: 200, injectBudgetRunes: 1500 })
const detailId = ref('')       // 空 = 列表视图；否则为正在查看的人格 ID
const detailRules = ref([])    // 详情页里那个人格的规则
const detailChanges = ref([])  // 详情页里那个人格的最近变更（时间倒序）
// 详情页里那个人格的「她学到的」候选（隐式演化的待办，采纳/丢弃后才消失）
const candidates = ref([])
const seedForm = ref(null)     // null = 未在编辑；否则为 { text }
const ruleForm = ref(null)     // null = 未在编辑；否则为表单内容（id 为空即新增）
const nameForm = ref(null)     // { mode: 'create' | 'rename', name, copyFrom }
const confirmDelete = ref(false)
const formError = ref('')

const detailPersona = computed(
  () => personaList.value.find((p) => p.id === detailId.value) ?? null,
)

// 内置人格只读：能给出的入口只有「复制为我的」。
// 不让写操作的按钮显示出来，比"点了才报错"友好——后端的拒绝信息是给排查用的，不该当交互反馈。
const canEdit = computed(() => !!detailPersona.value && !detailPersona.value.isBuiltin)

function slotLabel(key) {
  return slots.value.find((s) => s.key === key)?.label ?? key
}

// 规则来源：让人一眼看出"这条是它自己记下的，还是我手动写的"
function sourceLabel(src) {
  return { manual: '手动', explicit: '你的要求', inferred: '自动' }[src] ?? src
}

function tierLabel(tier) {
  return { core: '核心', recent: '近期', archived: '归档' }[tier] ?? tier
}

// 变更记录里的 field：主体字段是固定的英文名，规则字段直接是槽位 key
function fieldLabel(field) {
  return { seedText: '主体文本', name: '名称' }[field] ?? slotLabel(field)
}

function actionLabel(action) {
  return { create: '新增', update: '修改', delete: '删除', enable: '启用', disable: '停用' }[action] ?? action
}

async function loadMeta() {
  if (slots.value.length) return
  try {
    const m = await GetPersonaMeta()
    slots.value = m?.slots ?? []
    limits.value = {
      seedTextRunes: m?.seedTextRunes ?? 1200,
      ruleValueRunes: m?.ruleValueRunes ?? 200,
      injectBudgetRunes: m?.injectBudgetRunes ?? 1500,
    }
  } catch (e) {
    console.error('读取元信息失败', e)
  }
}

async function loadRules(id) {
  try {
    detailRules.value = (await GetPersonaRules(id)) ?? []
  } catch (e) {
    detailRules.value = []
    console.error('读取规则失败', e)
  }
}

async function loadChanges(id) {
  try {
    detailChanges.value = (await GetPersonaChanges(id)) ?? []
  } catch (e) {
    detailChanges.value = []
    console.error('读取变更记录失败', e)
  }
}

// 「她学到的」候选：从对话里自动抽出来的行为倾向，采纳之后才成为真规则。
// 它挂在人格这一侧（不进「记忆」分区）：候选改的是行为方式，与记忆的风险等级不是一回事。
async function loadCandidates(id) {
  if (!id) {
    candidates.value = []
    return
  }
  try {
    candidates.value = (await RuleCandidates(id)) ?? []
  } catch (e) {
    candidates.value = []
    console.error('读取规则候选失败', e)
  }
}

// 采纳 = 后端写真规则 + 删掉候选。规则列表会由 persona:changed 事件刷回来，
// 这里只需要把候选重新拉一遍。
async function acceptCandidate(c) {
  try {
    await AcceptRuleCandidate(c)
  } catch (e) {
    formError.value = errText(e)
  }
  await loadCandidates(detailId.value)
}

async function rejectCandidate(c) {
  try {
    await RejectRuleCandidate(c.id)
  } catch (e) {
    formError.value = errText(e)
  }
  await loadCandidates(detailId.value)
}

function resetForms() {
  seedForm.value = null
  ruleForm.value = null
  nameForm.value = null
  confirmDelete.value = false
  formError.value = ''
}

async function openDetail(p) {
  resetForms()
  detailId.value = p.id
  await Promise.all([loadRules(p.id), loadChanges(p.id), loadCandidates(p.id)])
}

function closeDetail() {
  resetForms()
  detailId.value = ''
  candidates.value = []
}

function startCreate() {
  resetForms()
  // 复制来源取"当前查看的人格"，列表里没有查看对象时退回当前生效人格
  nameForm.value = { mode: 'create', name: '', copyFrom: true }
}

function startRename() {
  resetForms()
  nameForm.value = { mode: 'rename', name: detailPersona.value?.name ?? '' }
}

async function saveNameForm() {
  const f = nameForm.value
  if (!f) return
  const name = f.name.trim()
  if (!name) {
    formError.value = '人格名不能为空'
    return
  }
  try {
    if (f.mode === 'create') {
      const source = f.copyFrom ? (detailId.value || activePersonaId.value) : ''
      const id = await CreatePersona(name, source)
      nameForm.value = null
      await loadPersona()
      // 建完直接进它的详情页，方便接着加规则
      const created = personaList.value.find((p) => p.id === id)
      if (created) await openDetail(created)
    } else {
      await RenamePersona(detailId.value, name)
      nameForm.value = null
      await loadPersona()
    }
  } catch (e) {
    formError.value = errText(e)
  }
}

// 「复制为我的」：内置人格只读，想改就得先有一份自己的
async function copyPersona() {
  const name = (detailPersona.value?.name ?? '人格') + '（我的）'
  try {
    const id = await CreatePersona(name, detailId.value)
    await loadPersona()
    const created = personaList.value.find((p) => p.id === id)
    if (created) await openDetail(created)
  } catch (e) {
    formError.value = errText(e)
  }
}

async function removePersona() {
  try {
    await DeletePersona(detailId.value)
    closeDetail()
    // 删除会连带清掉该人格的历史，所以两张都要重拉
    await Promise.all([loadPersona(), loadHistory()])
  } catch (e) {
    formError.value = errText(e)
  }
}

// 主体文本 = 用户最初写下的那段提示词，注入时放在最前面，也是"这个人格是谁"的定义。
// 它不在规则表里，而在 personas.seed_text 这一列上，所以走单独的 SaveSeedText。
function startEditSeed() {
  resetForms()
  seedForm.value = { text: detailPersona.value?.seedText ?? '' }
}

async function saveSeedForm() {
  const f = seedForm.value
  if (!f) return
  const text = f.text.trim()
  if (!text) {
    formError.value = '主体文本不能为空'
    return
  }
  try {
    await SaveSeedText(detailId.value, text)
    seedForm.value = null
    formError.value = ''
    // 主体文本是人格列表里那一行的摘要，改完列表要跟着变
    await loadPersona()
  } catch (e) {
    formError.value = errText(e)
  }
}

function startAddRule() {
  resetForms()
  ruleForm.value = {
    id: '',
    personaId: detailId.value,
    slot: slots.value[0]?.key ?? '',
    value: '',
    tier: 'core',
    // 手动新增默认 0（同层里排最后）。内置人格用 10/8/6/5 分档，想插到前面就自己调大
    priority: 0,
  }
}

function startEditRule(r) {
  resetForms()
  ruleForm.value = {
    id: r.id,
    personaId: r.personaId,
    slot: r.slot,
    value: r.value,
    tier: r.tier,
    priority: r.priority ?? 0,
  }
}

async function saveRuleForm() {
  const f = ruleForm.value
  if (!f) return
  if (!f.value.trim()) {
    formError.value = '取值不能为空'
    return
  }
  try {
    // id 为空即新增；单值槽位再写入是"覆盖"，后端负责沿用原规则 ID
    await SaveRule({
      id: f.id,
      personaId: f.personaId,
      slot: f.slot,
      value: f.value.trim(),
      tier: f.tier,
      priority: Number(f.priority) || 0,
    })
    ruleForm.value = null
    formError.value = ''
    await loadRules(detailId.value)
  } catch (e) {
    formError.value = errText(e)
  }
}

async function removeRule(r) {
  try {
    await DeleteRule(r.id)
    await loadRules(detailId.value)
  } catch (e) {
    showToast('删除规则失败：' + errText(e))
  }
}

async function toggleRule(r) {
  try {
    await SetRuleEnabled(r.id, !r.enabled)
    await loadRules(detailId.value)
  } catch (e) {
    showToast('操作失败：' + errText(e))
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
  if (!menuOpen.value) return
  // 当前人格可能被后端换掉了（例如对内置人格提要求时自动复制一份"我的"），
  // 菜单开着就刷新列表，否则用户会看到一份过期的清单
  loadPersona()
  // 删除人格、切换人格都会影响"当前人格的历史"，历史页可能正开着
  loadHistory()
  // 记忆同样按人格隔离：删人格会带走它的私有记忆，切人格会换成另一份可见范围
  loadMemories()
  // 详情页开着就同步它那份规则与变更记录：改这两样的入口不止编辑器一处
  // （"记住我的要求"也会写），变更记录更是每写必增
  if (detailId.value && detailId.value === p.personaId) {
    loadRules(detailId.value)
    loadChanges(detailId.value)
  }
}

async function openMenu() {
  menuOpen.value = true
  SetMenuOpen(true)
  // 每次打开都回到"会话列表"那一级：上次展开的那条可能已经被收尾结算过了（标题、摘要都变了）
  closeSession()
  memoryPendingDelete.value = ''
  await Promise.all([loadHistory(), loadMemories(), loadPersona(), loadMeta(), loadSettings()])
}

function closeMenu() {
  menuOpen.value = false
  SetMenuOpen(false)
}

function toggleMenu() {
  menuOpen.value ? closeMenu() : openMenu()
}

// 换分区时把手上的临时状态收掉：展开的那段会话、等二次确认的删除。
// 不收的话会"跨分区带过去"——从记忆切回历史，人还停在上次展开的那一段里，
// 会以为列表没了；反过来也一样。
watch(menuTab, () => {
  closeSession()
  memoryPendingDelete.value = ''
})

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
  // dbReady 这一道是防御：阻断层已经盖住了输入区，但回车键仍可能走到这里
  if (!text || busy.value || !dbReady.value) return

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

onMounted(async () => {
  EventsOn(EVENT_SAY, onSayEvent)
  EventsOn(EVENT_CHUNK, onChunk)
  EventsOn(EVENT_DONE, onDone)
  EventsOn(EVENT_ERROR, onError)
  EventsOn(EVENT_PERSONA_CHANGED, onPersonaChanged)

  // 先把存储状态问出来，再决定说什么。
  // 顺序不能反：数据库没就绪时该立刻进阻断态，而不是等用户点开菜单才知道
  // （那时他已经开始打字了，白写一段话）。
  await loadPersona()
  if (storageReady.value === false) {
    say('数据库未连接')
    return
  }
  say('数据库已连接desu')
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
      <button
        class="dragbar__icon"
        :disabled="!dbReady"
        title="历史记录"
        @click="toggleMenu"
      >
        ☰
      </button>
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
            :class="{ 'menu__tab--on': menuTab === 'memory' }"
            @click="menuTab = 'memory'"
          >
            记忆
          </button>
          <button
            class="menu__tab"
            :class="{ 'menu__tab--on': menuTab === 'persona' }"
            @click="menuTab = 'persona'"
          >
            人格
          </button>
          <button
            class="menu__tab"
            :class="{ 'menu__tab--on': menuTab === 'settings' }"
            @click="menuTab = 'settings'"
          >
            设置
          </button>
        </nav>

        <!-- 历史是两级的：会话列表 → 点进去看那一段的消息。 -->
        <template v-if="menuTab === 'history'">
          <!-- 第二级：某一条会话的消息 -->
          <template v-if="openSession">
            <div class="bar">
              <button class="btn btn--ghost" @click="closeSession">← 返回</button>
              <span class="bar__name">{{ openSession.title || '未命名对话' }}</span>
            </div>
            <p v-if="!sessionMsgs.length" class="menu__empty">这一段还没有消息</p>
            <ul v-else class="menu__list">
              <li
                v-for="item in sessionMsgs"
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

          <!-- 第一级：会话列表。标题由结算在会话收尾时生成，
               所以正聊着的这一段（以及还没轮到结算的）标题是空的，给个兜底文案 -->
          <template v-else>
            <p v-if="!historySessions.length" class="menu__empty">还没有对话记录</p>
            <ul v-else class="menu__list">
              <li
                v-for="s in historySessions"
                :key="s.id"
                class="menu__item menu__item--pick"
                title="点一下看这一段聊了什么"
                @click="openSessionMessages(s)"
              >
                <div class="menu__meta">
                  <span>{{ fmtTime(s.startedAt) }}</span>
                  <span v-if="!s.endedAt" class="menu__tag menu__tag--on">进行中</span>
                </div>
                <p class="menu__text">{{ s.title || '未命名对话' }}</p>
              </li>
            </ul>
          </template>
        </template>

        <!-- 「它记得什么」：自动抽出来的记忆必须看得见、删得掉，
             否则抽错一条就只能忍着，或者把整个库清掉 -->
        <template v-else-if="menuTab === 'memory'">
          <p v-if="!storageReady" class="menu__warn">
            数据库未连接：记忆功能不可用（对话与人格不受影响）
          </p>
          <p v-else-if="!memories.length" class="menu__empty">
            它还没记住什么。聊完一段、等后台整理过之后，这里就会出现。
          </p>
          <ul v-else class="menu__list">
            <li v-for="m in memories" :key="m.id" class="menu__item">
              <div class="menu__meta">
                <span class="menu__tag">{{ kindLabel(m.kind) }}</span>
                <span class="menu__tag">{{ m.private ? '你们之间' : '关于你' }}</span>
                <span>{{ fmtTime(m.createdAt) }}</span>
              </div>
              <p class="menu__text">{{ m.content }}</p>

              <!-- 二次确认：删除不可恢复，而窗口这么小、误触代价高 -->
              <div v-if="memoryPendingDelete === m.id" class="form form--danger">
                <p class="form__warn">删掉之后她就不会再记得这件事了，不可恢复。</p>
                <div class="form__btns">
                  <button class="btn btn--ghost" @click="memoryPendingDelete = ''">取消</button>
                  <button class="btn btn--danger" @click="removeMemory(m)">确认删掉</button>
                </div>
              </div>
              <div v-else class="mem__acts">
                <button class="iconbtn" title="让她忘掉这一条" @click="memoryPendingDelete = m.id">
                  删
                </button>
              </div>
            </li>
          </ul>
        </template>

        <template v-else-if="menuTab === 'persona'">
          <p v-if="!storageReady" class="menu__warn">
            数据库未连接：自建人格不会保存，当前只能用内置人格
          </p>

          <!-- 名称表单：新建与重命名共用，两种视图里都能出现 -->
          <div v-if="nameForm" class="form">
            <p class="form__title">{{ nameForm.mode === 'create' ? '新建人格' : '重命名人格' }}</p>
            <label class="form__row">
              <span class="form__label">人格名</span>
              <input v-model="nameForm.name" class="form__input" type="text" @keydown.enter="saveNameForm" />
            </label>
            <label v-if="nameForm.mode === 'create'" class="form__check">
              <input v-model="nameForm.copyFrom" type="checkbox" />
              <span>复制现有规则（内置人格不能直接改，通常要复制一份）</span>
            </label>
            <p v-if="formError" class="form__err">{{ formError }}</p>
            <div class="form__btns">
              <button class="btn btn--ghost" @click="nameForm = null">取消</button>
              <button class="btn" @click="saveNameForm">保存</button>
            </div>
          </div>

          <!-- 详情：改名 / 删除 / 复制 + 规则增删改停用 -->
          <div v-if="detailId" class="pane">
            <div class="bar">
              <button class="iconbtn" title="返回列表" @click="closeDetail">←</button>
              <span class="bar__name">{{ detailPersona?.name ?? detailId }}</span>
              <span v-if="detailPersona?.isBuiltin" class="menu__tag">内置</span>
              <span v-if="detailId === activePersonaId" class="menu__tag menu__tag--on">当前</span>
              <button
                v-if="detailPersona && detailId !== activePersonaId"
                class="btn btn--ghost bar__switch"
                @click="switchPersona(detailPersona)"
              >
                切到它
              </button>
            </div>

            <p v-if="!canEdit" class="pane__note">
              内置人格只读：名字与规则都改不了。想改就先「复制为我的」，会得到一份完全一样的副本。
            </p>

            <div class="ops">
              <button v-if="canEdit" class="btn btn--ghost" @click="startRename">重命名</button>
              <button v-else class="btn btn--ghost" @click="copyPersona">复制为我的</button>
              <button v-if="canEdit" class="btn btn--ghost" @click="confirmDelete = true">删除</button>
            </div>

            <div v-if="confirmDelete" class="form form--danger">
              <p class="form__warn">
                删除「{{ detailPersona?.name }}」会一并删掉它的规则与「该人格的对话历史」，不可恢复。
              </p>
              <div class="form__btns">
                <button class="btn btn--ghost" @click="confirmDelete = false">取消</button>
                <button class="btn btn--danger" @click="removePersona">确认删除</button>
              </div>
            </div>

            <!-- 主体文本：人格的「我是谁」，注入时排在最前，也是人格列表里的那行摘要 -->
            <div class="rules__head">
              <span>主体文本</span>
              <button
                v-if="canEdit && !seedForm"
                class="btn btn--ghost"
                @click="startEditSeed"
              >
                编辑
              </button>
            </div>
            <div v-if="seedForm" class="form">
              <textarea
                v-model="seedForm.text"
                class="form__area"
                rows="5"
                :maxlength="limits.seedTextRunes"
                placeholder="写清楚它是谁、怎么说话、怎么称呼你"
              ></textarea>
              <p class="form__count">{{ seedForm.text.length }} / {{ limits.seedTextRunes }}</p>
              <p v-if="formError" class="form__err">{{ formError }}</p>
              <div class="form__btns">
                <button class="btn btn--ghost" @click="seedForm = null">取消</button>
                <button class="btn" @click="saveSeedForm">保存</button>
              </div>
            </div>
            <p v-else class="seed__text">
              {{ detailPersona?.seedText || '（还没有写主体文本）' }}
            </p>

            <!-- 「她学到的」：隐式演化攒下来的候选。只有非空时才出现——
                 没有待办的时候摆一个空标题，只会让这一页看着更挤 -->
            <template v-if="candidates.length">
              <div class="rules__head">
                <span>她学到的（{{ candidates.length }}）</span>
              </div>
              <p class="pane__note">
                这些是她从你们的对话里自己总结出来的说话方式。采纳之后才会生效——规则会一直影响她怎么说话，
                所以不自动写进去。
              </p>
              <ul class="rules">
                <li v-for="c in candidates" :key="c.id" class="rule">
                  <div class="rule__main">
                    <div class="rule__meta">
                      <span class="rule__slot">{{ slotLabel(c.slot) }}</span>
                      <span class="menu__tag menu__tag--on">待采纳</span>
                    </div>
                    <p class="rule__value">{{ c.value }}</p>
                    <!-- 原话是她"从哪句听出来的"：用户判断该不该采纳，看的就是这个 -->
                    <p v-if="c.evidence" class="cand__quote">「{{ c.evidence }}」</p>
                  </div>
                  <div class="rule__acts">
                    <button class="iconbtn" title="采纳：写进她的规则" @click="acceptCandidate(c)">收</button>
                    <button class="iconbtn" title="丢弃：不写规则，只清掉这条" @click="rejectCandidate(c)">
                      弃
                    </button>
                  </div>
                </li>
              </ul>
            </template>

            <div class="rules__head">
              <span>规则（{{ detailRules.length }}）</span>
              <button v-if="canEdit" class="btn btn--ghost" @click="startAddRule">+ 添加</button>
            </div>
            <p v-if="!detailRules.length" class="pane__note">
              还没有规则。{{ canEdit ? '点「+ 添加」写一条，例如「称呼用户 = 老板」。' : '' }}
            </p>
            <p v-else class="pane__note">
              注入有 {{ limits.injectBudgetRunes }} 字预算：超预算先截「近期」层，主体与核心层不截。
            </p>
            <ul v-if="detailRules.length" class="rules">
              <li
                v-for="r in detailRules"
                :key="r.id"
                class="rule"
                :class="{ 'rule--off': !r.enabled }"
              >
                <div
                  class="rule__main"
                  :title="canEdit ? '点一下编辑这条规则' : '内置人格的规则只读'"
                  @click="canEdit && startEditRule(r)"
                >
                  <div class="rule__meta">
                    <span class="rule__slot">{{ slotLabel(r.slot) }}</span>
                    <span class="menu__tag">{{ sourceLabel(r.source) }}</span>
                    <span class="menu__tag">{{ tierLabel(r.tier) }}</span>
                    <!-- 0 是默认值，不显示，免得每行都挂一个没有信息量的标 -->
                    <span
                      v-if="r.priority"
                      class="menu__tag"
                      title="优先级：同一层内越大越先注入"
                    >
                      P{{ r.priority }}
                    </span>
                    <span v-if="!r.enabled" class="menu__tag">已停用</span>
                  </div>
                  <p class="rule__value">{{ r.value }}</p>
                </div>
                <div v-if="canEdit" class="rule__acts">
                  <button
                    class="iconbtn"
                    :title="r.enabled ? '停用（仍保留，可再启用）' : '启用'"
                    @click="toggleRule(r)"
                  >
                    {{ r.enabled ? '停' : '启' }}
                  </button>
                  <button class="iconbtn" title="删除这条规则" @click="removeRule(r)">删</button>
                </div>
              </li>
            </ul>

            <div v-if="ruleForm" class="form">
              <p class="form__title">{{ ruleForm.id ? '编辑规则' : '新增规则' }}</p>
              <label class="form__row">
                <span class="form__label">槽位</span>
                <select v-model="ruleForm.slot" class="form__input">
                  <option v-for="s in slots" :key="s.key" :value="s.key">
                    {{ s.label }}{{ s.multi ? '（可多条）' : '' }}
                  </option>
                </select>
              </label>
              <label class="form__row">
                <span class="form__label">取值</span>
                <input
                  v-model="ruleForm.value"
                  class="form__input"
                  type="text"
                  :maxlength="limits.ruleValueRunes"
                  @keydown.enter="saveRuleForm"
                />
              </label>
              <label class="form__row">
                <span class="form__label">层级</span>
                <select v-model="ruleForm.tier" class="form__input">
                  <option value="core">核心（永不被预算截断）</option>
                  <option value="recent">近期（参与预算，可能被截）</option>
                </select>
              </label>
              <label class="form__row">
                <span class="form__label">优先级</span>
                <input v-model.number="ruleForm.priority" class="form__input" type="number" />
              </label>
              <p class="form__hint">
                层级决定「会不会被预算截断」；优先级只管同一层内谁先注入（越大越先，内置人格用 10/8/6/5 分档）。
              </p>
              <p v-if="formError" class="form__err">{{ formError }}</p>
              <div class="form__btns">
                <button class="btn btn--ghost" @click="ruleForm = null">取消</button>
                <button class="btn" @click="saveRuleForm">保存</button>
              </div>
            </div>

            <!-- 变更记录：回溯"它什么时候被我改成这样的"，只读。
                 与规则同放在详情里而不是另开一个 tab：看规则时最想知道的往往就是它怎么变成现在这样 -->
            <div class="rules__head">
              <span>变更记录（{{ detailChanges.length }}）</span>
            </div>
            <p v-if="!detailChanges.length" class="pane__note">
              还没有变更记录。改主体文本、名字、规则都会在这里留一条。
            </p>
            <ul v-else class="changes">
              <li v-for="c in detailChanges" :key="c.id" class="change">
                <div class="rule__meta">
                  <span class="rule__slot">{{ fieldLabel(c.field) }}</span>
                  <span class="menu__tag">{{ actionLabel(c.action) }}</span>
                  <span class="menu__tag">{{ sourceLabel(c.source) }}</span>
                  <span class="change__time">{{ fmtTime(c.createdAt) }}</span>
                </div>
                <!-- 启用/停用这类动作没有取值新旧，只有标签，所以整行可省 -->
                <p v-if="c.oldValue || c.newValue" class="change__val">
                  <span v-if="c.oldValue" class="change__old">{{ c.oldValue }}</span>
                  <span v-if="c.oldValue && c.newValue" class="change__arrow">→</span>
                  <span v-if="c.newValue">{{ c.newValue }}</span>
                </p>
                <p v-if="c.evidence" class="change__ev">「{{ c.evidence }}」</p>
              </li>
            </ul>
          </div>

          <!-- 列表：点一行切换人格，点「改」进详情 -->
          <div v-else class="pane">
            <p v-if="!personaList.length" class="menu__empty">没有可用的人格</p>
            <ul v-else class="menu__list menu__list--flat">
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
                  <button
                    class="iconbtn meta__edit"
                    title="编辑这个人格的名字与规则"
                    @click.stop="openDetail(p)"
                  >
                    改
                  </button>
                </div>
                <p class="menu__text">{{ p.seedText }}</p>
              </li>
            </ul>
            <div class="ops">
              <button class="btn btn--ghost" @click="startCreate">+ 新建人格</button>
            </div>
            <p class="menu__hint">
              点一行切换人格；点「改」编辑它的名字与规则。每个人格有各自独立的对话历史
            </p>
          </div>
        </template>

        <!-- 设置：应用级的东西放这里，它们不属于任何一个人格 -->
        <template v-else>
          <div class="pane">
            <p v-if="!storageReady" class="menu__warn">
              数据库未连接：设置改完只在本次运行内有效
            </p>

            <div class="rules__head"><span>思考模式</span></div>
            <p class="pane__note">
              默认开启。关掉它：模型不必先算完思维链，首字更快，思考的 token 也不再计费；
              而且 temperature 这类「说话跳脱度」的参数会重新生效——思考模式下它们是被忽略的。
            </p>
            <div class="ops">
              <button class="btn btn--ghost" @click="toggleThinking">
                {{ settings.thinkingDisabled ? '已关闭 · 点它开启' : '已开启 · 点它关闭' }}
              </button>
            </div>
            <p class="pane__note">
              只影响对话本身；「记住我的要求」那条链路仍然会用思考，它靠的是准确性。
            </p>

            <div class="rules__head"><span>人格导入导出</span></div>
            <p class="pane__note">
              导出的文件可以直接分享给别人；导入遇到同名人格会新建一份副本，不会覆盖现有的。
            </p>
            <div class="setting">
              <label class="form__row">
                <span class="form__label">导谁</span>
                <select v-model="exportId" class="form__input">
                  <option value="">（当前人格）</option>
                  <option v-for="p in personaList" :key="p.id" :value="p.id">{{ p.name }}</option>
                </select>
              </label>
            </div>
            <div class="ops">
              <button class="btn btn--ghost" @click="exportPersona">导出为文件</button>
              <button class="btn btn--ghost" @click="importPersona">从文件导入</button>
            </div>
          </div>
        </template>
      </section>
    </Transition>

    <!-- 数据库未就绪：挡住 stage 与输入区，只留出拖拽区（否则窗口都没法拖走） -->
    <section v-if="!dbReady" class="blocker">
      <p class="blocker__title">需要数据库才能开始</p>
      <p class="blocker__desc">
        人格与记忆要存在 PostgreSQL 里。
      </p>
      <p class="blocker__steps">
        ① 装 PostgreSQL 18 + pgvector 扩展<br />
        ② 建库并启用扩展（下面两条命令）<br />
        ③ 在 .env 里填 COMPANION_PG_DSN，然后重启
      </p>
      <textarea class="blocker__cmd" rows="3" readonly :value="setupCmd"></textarea>
      <button class="btn btn--ghost blocker__copy" @click="copySetupCmd">复制命令</button>
      <p class="blocker__hint">完整步骤（含 pgvector 安装）见项目 README 的「运行依赖」一节</p>
    </section>

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
        :disabled="!dbReady"
        :placeholder="busy ? '正在回复…' : '说点什么，回车发送'"
        @keydown.enter="onEnter"
      />
      <button v-if="!busy" class="composer__btn" :disabled="!dbReady" @click="send">发送</button>
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

/* 菜单内的分区切换：历史 / 记忆 / 人格 / 设置。四个用 flex:1 均分，
   340px 宽下每个约 76px，放两个中文字够用 */
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

/* 记忆条目自己的动作行（"删"）。整行右对齐放在内容下方，
   与规则列表那种"内容左、动作右"的并排不同：记忆的正文是一句话，
   在 340px 宽度里再挤一个按钮会把它压成窄条 */
.mem__acts {
  display: flex;
  justify-content: flex-end;
  margin-top: 4px;
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

/* ---------- 数据库未就绪的阻断层 ---------- */

/* 与 .menu 同一套定位思路（absolute），但 z-index 更高——它要盖住菜单与 toast。
   top 留 44px 是为了让拖拽区露在外面，否则窗口都没法被拖走。 */
.blocker {
  position: absolute;
  left: 10px;
  right: 10px;
  top: 44px;
  bottom: 10px;
  z-index: 30;
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 14px 12px;
  box-sizing: border-box;
  overflow-y: auto;
  border-radius: 12px;
  background: rgba(255, 255, 255, 0.98);
  box-shadow: 0 12px 30px rgba(30, 25, 60, 0.22);
}

.blocker__title {
  margin: 0;
  font-size: 13px;
  font-weight: 600;
  color: #33333d;
}

.blocker__desc {
  margin: 0;
  font-size: 11px;
  line-height: 1.6;
  color: #5a5a6b;
}

.blocker__steps {
  margin: 0;
  padding: 8px;
  border-radius: 8px;
  background: rgba(0, 0, 0, 0.04);
  font-size: 11px;
  line-height: 1.8;
  color: #5a5a6b;
}

/* 命令要能看清、也能手动选中（复制按钮失败时的兜底）：等宽字体 + 不折行 */
.blocker__cmd {
  width: 100%;
  box-sizing: border-box;
  padding: 8px;
  border: 0;
  border-radius: 8px;
  background: rgba(0, 0, 0, 0.05);
  color: #2b2b33;
  font-family: Consolas, "Courier New", monospace;
  font-size: 10px;
  line-height: 1.6;
  white-space: pre;
  overflow-x: auto;
  resize: none;
  outline: none;
}

/* .btn 自带 flex:1，在 flex column 里会被拉高，这里取消 */
.blocker__copy {
  flex: none;
}

.blocker__hint {
  margin: 0;
  font-size: 10px;
  line-height: 1.5;
  color: #9a9aa8;
}

.dragbar__icon:disabled {
  opacity: 0.4;
  cursor: default;
}

.dragbar__icon:disabled:hover {
  background: transparent;
  color: #8a8a99;
}

.composer__input:disabled,
.composer__btn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
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

/* ---------- 人格编辑器（⑧）---------- */

/* 详情 / 列表两种视图的滚动容器：内容变长时整体滚动，内部不再套第二层滚动 */
.pane {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding-bottom: 4px;
}

/* 放进 .pane 里的列表不要再自己滚，否则与外层形成嵌套滚动 */
.menu__list--flat {
  flex: none;
  overflow: visible;
}

.bar {
  display: flex;
  gap: 6px;
  align-items: center;
  padding: 6px 8px;
}

.bar__name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 12px;
  font-weight: 600;
  color: #33333d;
}

.bar__switch {
  flex: none;
  padding: 3px 8px;
  font-size: 11px;
}

.ops {
  display: flex;
  gap: 6px;
  padding: 0 8px 6px;
}

.ops .btn {
  flex: none;
  padding: 5px 10px;
  font-size: 11px;
}

.pane__note {
  margin: 0;
  padding: 6px 10px;
  font-size: 11px;
  line-height: 1.5;
  color: #9a9aa8;
}

/* 候选的依据（原话）：比取值更小更淡——它是"她为什么这么想"，不是内容本身 */
.cand__quote {
  margin: 2px 0 0;
  font-size: 11px;
  line-height: 1.5;
  color: #7b7b8b;
  word-break: break-word;
}

.rules__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 6px 10px 4px;
  border-top: 1px solid rgba(0, 0, 0, 0.06);
  font-size: 11px;
  color: #6b6b7b;
}

.rules__head .btn {
  flex: none;
  padding: 3px 8px;
  font-size: 11px;
}

.rules {
  margin: 0;
  padding: 4px 8px 8px;
  list-style: none;
}

.rule {
  display: flex;
  gap: 4px;
  padding: 5px 6px;
  border-radius: 8px;
  background: rgba(0, 0, 0, 0.03);
}

.rule + .rule {
  margin-top: 5px;
}

/* 停用的规则仍然显示，只是变淡——"停用"和"删除"要能一眼分清 */
.rule--off {
  opacity: 0.55;
}

.rule__main {
  flex: 1;
  min-width: 0;
  cursor: pointer;
}

.rule__meta {
  display: flex;
  gap: 4px;
  align-items: center;
  font-size: 10px;
  color: #9a9aa8;
}

.rule__slot {
  font-weight: 600;
  color: #5a5a6b;
}

.rule__value {
  margin: 2px 0 0;
  font-size: 12px;
  line-height: 1.5;
  color: #2b2b33;
  word-break: break-word;
}

.rule__acts {
  display: flex;
  gap: 3px;
  align-items: center;
}

/* 主体文本：只读展示。它是长文本且可能带换行，保留原样换行比压成一行好读 */
.seed__text {
  margin: 0;
  padding: 2px 10px 8px;
  font-size: 12px;
  line-height: 1.6;
  color: #2b2b33;
  white-space: pre-wrap;
  word-break: break-word;
}

/* 变更记录：只读回溯列表，样式比规则更轻——它不承载操作，只回答"变过什么" */
.changes {
  margin: 0;
  padding: 4px 8px 8px;
  list-style: none;
}

.change {
  padding: 5px 6px;
  border-radius: 8px;
  background: rgba(0, 0, 0, 0.02);
}

.change + .change {
  margin-top: 5px;
}

.change__time {
  margin-left: auto;
}

.change__val {
  margin: 2px 0 0;
  font-size: 12px;
  line-height: 1.5;
  color: #2b2b33;
  word-break: break-word;
}

/* 旧值划掉：一眼看出"改成了什么"，而不是只看到两个值并排 */
.change__old {
  color: #9a9aa8;
  text-decoration: line-through;
}

.change__arrow {
  margin: 0 4px;
  color: #9a9aa8;
}

/* 原始触发话（evidence）：比取值更次要，弱化处理 */
.change__ev {
  margin: 2px 0 0;
  font-size: 10px;
  line-height: 1.4;
  color: #9a9aa8;
  word-break: break-word;
}

/* 行内小按钮：人格列表行与规则行共用，尺寸明显小于 .btn */
.iconbtn {
  --wails-draggable: no-drag;

  flex: none;
  min-width: 22px;
  height: 22px;
  padding: 0 5px;
  border: 0;
  border-radius: 6px;
  background: rgba(0, 0, 0, 0.05);
  color: #6b6b7b;
  font-size: 11px;
  line-height: 1;
  cursor: pointer;
}

.iconbtn:hover {
  background: rgba(124, 108, 245, 0.16);
  color: #5b4bd6;
}

/* 人格列表行尾的「改」：靠右，不与名字挤在一起 */
.meta__edit {
  margin-left: auto;
}

/* 内联表单：新建 / 重命名 / 规则编辑 / 删除确认共用。
   刻意不用系统对话框——它会盖住这个 340px 的小窗，且 WebView2 对 prompt 支持不可靠。 */
.form {
  margin: 6px 8px 8px;
  padding: 8px;
  border-radius: 10px;
  background: #fff;
  box-shadow: 0 4px 14px rgba(30, 25, 60, 0.12);
}

.form--danger {
  box-shadow: 0 4px 14px rgba(200, 60, 60, 0.16);
}

.form__title {
  margin: 0 0 6px;
  font-size: 12px;
  font-weight: 600;
  color: #33333d;
}

.form__row {
  display: flex;
  gap: 6px;
  align-items: center;
  margin-bottom: 6px;
}

.form__label {
  flex: none;
  width: 40px;
  font-size: 11px;
  color: #6b6b7b;
}

.form__input {
  flex: 1;
  min-width: 0;
  padding: 5px 7px;
  border: 0;
  border-radius: 6px;
  background: rgba(0, 0, 0, 0.05);
  color: #2b2b33;
  font-size: 12px;
  outline: none;
}

.form__input:focus {
  box-shadow: inset 0 0 0 1px rgba(124, 108, 245, 0.55);
}

/* 主体文本是多行输入，不能用 .form__input 的单行样式；只允许纵向拉伸，避免被拉宽撑破浮层 */
.form__area {
  display: block;
  width: 100%;
  box-sizing: border-box;
  padding: 6px 8px;
  border: 0;
  border-radius: 6px;
  background: rgba(0, 0, 0, 0.05);
  color: #2b2b33;
  font-family: inherit;
  font-size: 12px;
  line-height: 1.6;
  resize: vertical;
  outline: none;
}

.form__area:focus {
  box-shadow: inset 0 0 0 1px rgba(124, 108, 245, 0.55);
}

/* 字数提示：给上限，但不阻止输入（超了后端也会拒）——只让人心里有数 */
.form__count {
  margin: 4px 0 6px;
  text-align: right;
  font-size: 10px;
  color: #9a9aa8;
}

.form__check {
  display: flex;
  gap: 6px;
  align-items: flex-start;
  margin-bottom: 6px;
  font-size: 10px;
  line-height: 1.4;
  color: #6b6b7b;
}

.form__check input {
  margin-top: 2px;
}

/* 直接放在 .pane 里的表单行要自己补左右内边距：.form 那张卡片自带，这里没有卡片 */
.setting {
  padding: 0 10px 6px;
}

/* 表单里的说明：比错误文字浅、比正文小，用来解释字段之间的关系，不打断填写 */
.form__hint {
  margin: 0 0 6px;
  font-size: 10px;
  line-height: 1.45;
  color: #9a9aa8;
}

.form__err {
  margin: 0 0 6px;
  font-size: 11px;
  line-height: 1.4;
  color: #c0392b;
  word-break: break-word;
}

.form__warn {
  margin: 0 0 6px;
  font-size: 11px;
  line-height: 1.5;
  color: #b2652b;
}

.form__btns {
  display: flex;
  gap: 6px;
}

.form__btns .btn {
  padding: 5px 0;
  font-size: 11px;
}

.btn--danger {
  background: linear-gradient(135deg, #e8604c, #f0897a);
}
</style>
