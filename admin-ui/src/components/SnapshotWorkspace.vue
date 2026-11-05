<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { Camera, ChevronLeft, ChevronRight, Download, Image, LoaderCircle, Pencil, RefreshCw, Save, Trash2 } from '@lucide/vue'
import { api, errorMessage } from '../services/api'
import type { CaptureRecord, CaptureSchedule } from '../types/api'
import { formatDate } from '../utils/format'
import { useUiStore } from '../stores/ui'
import { useSnapshotStore } from '../stores/snapshot'
import ModalDialog from './ModalDialog.vue'

const props = defineProps<{ channelId?: string; disabled?: boolean }>()
const emit = defineEmits<{ captured: [record: CaptureRecord]; changed: [] }>()
const ui = useUiStore()
const snapshotStore = useSnapshotStore()
const rows = ref<CaptureRecord[]>([])
const page = ref(1)
const total = ref(0)
const loading = ref(false)
const error = ref('')
const taskError = ref('')
const pending = ref<CaptureRecord | null>(null)
const submitting = ref(false)
const selected = ref<CaptureRecord | null>(null)
const previewError = ref(false)
const brokenImages = reactive(new Set<string>())
const editing = ref<CaptureRecord | null>(null)
const newName = ref('')
const removing = ref<CaptureRecord | null>(null)
const mutating = ref(false)
const mutationError = ref('')
const schedule = ref<CaptureSchedule>({ channel_id: '', enabled: false, interval_seconds: 300 })
const scheduleLoaded = ref(false)
const scheduleError = ref('')
const savingSchedule = ref(false)
const scheduleLoading = ref(false)
const scheduleOpen = ref(false)
const scheduleSaveError = ref('')
const scheduleDraft = reactive({ enabled: false, interval_seconds: 300 })
const planEnabled = ref(false)
const firstRun = ref('')
const size = ref(24)
const jumpPage = ref<number | string>('')
const pageSizes = [12, 24, 48, 96]
let sequence = 0
let pollTimer: ReturnType<typeof setTimeout> | undefined
let requestSequence = 0
let pollFailures = 0
let pollingSequence: number | undefined
let latestCoverId = ''
let scheduleRevision = 0
const hasScheduledTime = computed(() => Boolean(schedule.value.next_run_at && new Date(schedule.value.next_run_at).getTime() > 0))
const scheduleConfigured = computed(() => hasScheduledTime.value || Boolean(schedule.value.last_run_at))
const scheduleSummary = computed(() => {
  if (!scheduleLoaded.value) return scheduleError.value ? '定时计划读取失败，请重试。' : '正在读取定时计划…'
  if (!schedule.value.enabled) return '未开启定时抓拍，已有图片继续保留。'
  const next = hasScheduledTime.value ? `，下一次将于 ${formatDate(schedule.value.next_run_at)} 开始执行。` : '，正在等待调度。'
  return `已开启，每 ${schedule.value.interval_seconds} 秒抓拍一次${next}`
})
const pages = computed(() => Math.max(1, Math.ceil(total.value / size.value)))
const rangeStart = computed(() => rows.value.length ? (page.value - 1) * size.value + 1 : 0)
const rangeEnd = computed(() => rows.value.length ? Math.min(total.value, (page.value - 1) * size.value + rows.value.length) : 0)
const pageNumbers = computed<(number | 'ellipsis')[]>(() => {
  if (pages.value <= 7) return Array.from({ length: pages.value }, (_, index) => index + 1)
  const start = Math.max(2, Math.min(page.value - 1, pages.value - 3))
  const end = Math.min(pages.value - 1, start + 2)
  const result: (number | 'ellipsis')[] = [1]
  if (start > 2) result.push('ellipsis')
  for (let value = start; value <= end; value += 1) result.push(value)
  if (end < pages.value - 1) result.push('ellipsis')
  result.push(pages.value)
  return result
})
const busy = computed(() => submitting.value || Boolean(pending.value))
const stateNames: Record<CaptureRecord['state'], string> = { queued: '等待执行', running: '抓拍中', succeeded: '已保存', failed: '失败', deleting: '删除中' }
const imageUrl = (row: CaptureRecord, download = false) => api.captureImage(row.id, download, row.updated_at)

async function load() {
  const current = sequence
  const request = ++requestSequence
  loading.value = true
  try {
    const { data } = await api.captures({ channel_id: props.channelId, page: page.value, size: size.value })
    if (current !== sequence || request !== requestSequence) return
    rows.value = data.items || []
    total.value = Number(data.total || 0)
    if (props.channelId && page.value === 1) {
      const latest = rows.value.find(row => row.state === 'succeeded')
      if (latest && latest.id !== latestCoverId) { latestCoverId = latest.id; emit('captured', latest) }
    }
    error.value = ''
    if (page.value > pages.value) { page.value = pages.value; void load() }
  } catch (cause) {
    if (current === sequence && request === requestSequence) error.value = errorMessage(cause, '抓拍记录读取失败，请重试')
  } finally {
    if (current === sequence && request === requestSequence) loading.value = false
  }
}

function schedulePoll() {
  clearTimeout(pollTimer)
  const current = sequence
  // 正常轮询采用管理端配置；弱网失败时退避，不加重链路负担。
  const interval = snapshotStore.settings.poll_interval_ms
  const delay = pollFailures > 0
    ? Math.min(Math.max(30000, interval), Math.max(2000, interval) * 2 ** Math.min(pollFailures - 1, 4))
    : interval
  pollTimer = setTimeout(() => void poll(current), delay)
}

async function poll(current: number) {
  if (current !== sequence || pollingSequence === current) return
  pollingSequence = current
  try {
    if (pending.value) {
      const { data } = await api.capture(pending.value.id)
      if (current !== sequence) return
      pending.value = data
      taskError.value = ''
      if (data.state === 'succeeded') {
        pending.value = null
        selected.value = data
        emit('captured', data)
        ui.toast('抓拍图片已永久保存')
      } else if (data.state === 'failed' || data.state === 'deleting') {
        pending.value = null
        taskError.value = data.error || '抓拍未完成，请检查设备状态后重试'
      }
      if (pending.value) {
        // 执行期间只查询这一个任务，完成后再刷新图库与计划。
        pollFailures = 0
        schedulePoll()
        return
      }
    }
    if (current !== sequence) return
    await load()
    if (current !== sequence) return
    if (props.channelId && planEnabled.value) {
      const revision = scheduleRevision
      const { data } = await api.captureSchedule(props.channelId)
      if (current !== sequence) return
      if (revision === scheduleRevision && !savingSchedule.value) {
        schedule.value = data
        planEnabled.value = data.enabled
      }
    }
    pollFailures = 0
    if (pending.value || rows.value.some(row => row.state === 'queued' || row.state === 'running' || row.state === 'deleting') || planEnabled.value) schedulePoll()
  } catch (cause) {
    if (current !== sequence) return
    pollFailures += 1
    taskError.value = `${errorMessage(cause, '抓拍状态读取失败')}。任务仍在后台执行，可刷新记录查看。`
    // 已提交的任务继续低频查询，不能把浏览器断网当作抓拍失败。
    if (pending.value || pollFailures < 5) schedulePoll()
  } finally {
    if (pollingSequence === current) pollingSequence = undefined
  }
}

async function capture() {
  if (!props.channelId || props.disabled || busy.value) return
  const current = sequence
  submitting.value = true
  taskError.value = ''
  try {
    const { data } = await api.createCapture(props.channelId)
    if (current !== sequence) return
    pending.value = data
    pollFailures = 0
    page.value = 1
    await load()
    if (current === sequence) schedulePoll()
  } catch (cause) {
    if (current === sequence) taskError.value = errorMessage(cause, '无法提交抓拍任务')
  } finally {
    if (current === sequence) submitting.value = false
  }
}

defineExpose({ capture })

async function loadSchedule() {
  if (!props.channelId) return
  const current = sequence
  scheduleLoading.value = true
  try {
    const { data } = await api.captureSchedule(props.channelId)
    if (current !== sequence) return
    schedule.value = data
    planEnabled.value = data.enabled
    scheduleLoaded.value = true
    scheduleError.value = ''
    if (data.enabled) schedulePoll()
  } catch (cause) {
    if (current === sequence) scheduleError.value = errorMessage(cause, '定时计划读取失败，请重试')
  } finally { if (current === sequence) scheduleLoading.value = false }
}

function openSchedule() {
  if (!scheduleLoaded.value || savingSchedule.value) return
  Object.assign(scheduleDraft, { enabled: schedule.value.enabled, interval_seconds: schedule.value.interval_seconds })
  firstRun.value = ''
  scheduleSaveError.value = ''
  scheduleOpen.value = true
}
function closeSchedule() {
  if (!savingSchedule.value) scheduleOpen.value = false
}
async function saveSchedule() {
  if (!props.channelId || !scheduleLoaded.value || !scheduleOpen.value || savingSchedule.value) return
  const current = sequence
  scheduleRevision += 1
  savingSchedule.value = true
  scheduleSaveError.value = ''
  try {
    const next = firstRun.value ? new Date(firstRun.value).toISOString() : undefined
    const { data } = await api.saveCaptureSchedule(props.channelId, { channel_id: props.channelId, ...scheduleDraft, next_run_at: next })
    if (current !== sequence) return
    schedule.value = data
    planEnabled.value = data.enabled
    firstRun.value = ''
    scheduleOpen.value = false
    ui.toast(data.enabled ? '定时抓拍已启用' : '定时抓拍已停用')
  } catch (cause) {
    if (current === sequence) scheduleSaveError.value = errorMessage(cause, '定时计划保存失败')
  } finally {
    if (current === sequence) {
      scheduleRevision += 1
      savingSchedule.value = false
      if (planEnabled.value) schedulePoll()
    }
  }
}

function edit(row: CaptureRecord) { editing.value = row; newName.value = row.name; mutationError.value = '' }
function askRemove(row: CaptureRecord) { removing.value = row; mutationError.value = '' }
async function rename() {
  if (!editing.value || mutating.value) return
  const current = sequence
  mutating.value = true
  try {
    const { data } = await api.renameCapture(editing.value.id, newName.value.trim())
    if (current !== sequence) return
    if (selected.value?.id === data.id) selected.value = data
    editing.value = null
    ui.toast('图片名称已修改')
    await load()
  } catch (cause) {
    if (current === sequence) mutationError.value = errorMessage(cause, '重命名失败')
  } finally { if (current === sequence) mutating.value = false }
}
async function remove() {
  if (!removing.value || mutating.value) return
  const current = sequence
  const id = removing.value.id
  mutating.value = true
  try {
    await api.deleteCapture(id)
    if (current !== sequence) return
    if (selected.value?.id === id) selected.value = null
    removing.value = null
    emit('changed')
    ui.toast('抓拍记录与图片文件已删除')
    await load()
  } catch (cause) {
    if (current === sequence) mutationError.value = errorMessage(cause, '删除失败，可重试')
  } finally { if (current === sequence) mutating.value = false }
}
function changePage(value: number) {
  if (loading.value || !Number.isFinite(value)) return
  const next = Math.max(1, Math.min(pages.value, Math.trunc(value)))
  if (next === page.value) return
  page.value = next
  rows.value = []
  void load()
}
function changeSize() {
  page.value = 1
  jumpPage.value = ''
  rows.value = []
  void load()
}
function goToPage() {
  if (jumpPage.value === '') return
  changePage(Number(jumpPage.value))
  jumpPage.value = ''
}
async function refresh() {
  pollFailures = 0
  await load()
  if (pending.value || planEnabled.value || rows.value.some(row => row.state === 'queued' || row.state === 'running' || row.state === 'deleting')) schedulePoll()
}

watch(selected, () => { previewError.value = false })

watch(() => props.channelId, () => {
  sequence += 1
  clearTimeout(pollTimer)
  rows.value = []; page.value = 1; total.value = 0; jumpPage.value = ''
  selected.value = null; pending.value = null; submitting.value = false
  editing.value = null; removing.value = null; mutating.value = false
  error.value = ''; taskError.value = ''; scheduleError.value = ''; firstRun.value = ''
  scheduleOpen.value = false; scheduleSaveError.value = ''; scheduleLoading.value = false
  scheduleRevision += 1
  brokenImages.clear(); pollFailures = 0; latestCoverId = ''
  schedule.value = { channel_id: props.channelId || '', enabled: false, interval_seconds: 300 }
  scheduleLoaded.value = false; planEnabled.value = false; savingSchedule.value = false
  void snapshotStore.load().catch(() => { /* 读取失败沿用上次配置或默认2000ms，不影响后台抓拍。 */ })
  void refresh(); void loadSchedule()
}, { immediate: true })
watch(() => snapshotStore.settings.poll_interval_ms, () => {
  if (pollTimer) schedulePoll()
})
onBeforeUnmount(() => { sequence += 1; clearTimeout(pollTimer) })
</script>

<template>
  <div class="capture-workspace">
    <section v-if="channelId" class="capture-plan" :aria-labelledby="`capture-plan-title-${channelId}`">
      <div class="capture-plan-summary">
        <div><h3 :id="`capture-plan-title-${channelId}`">定时抓拍</h3><p>{{ scheduleSummary }}</p></div>
        <button v-if="scheduleError && !scheduleLoaded" class="btn" type="button" :disabled="scheduleLoading" @click="loadSchedule"><RefreshCw :class="{ 'animate-spin': scheduleLoading }" aria-hidden="true" />重试</button>
        <button v-else class="btn" type="button" :disabled="!scheduleLoaded || scheduleLoading || savingSchedule" aria-haspopup="dialog" @click="openSchedule"><Pencil aria-hidden="true" />{{ scheduleLoading ? '读取中' : scheduleConfigured ? '修改' : '配置' }}</button>
      </div>
      <p v-if="scheduleError" class="capture-error capture-plan-error" role="alert">{{ scheduleError }}</p>
      <p v-if="scheduleLoaded && schedule.last_error" class="capture-error capture-plan-error">最近执行异常：{{ schedule.last_error }}</p>
    </section>

    <ModalDialog :open="scheduleOpen" title="定时抓拍配置" description="按固定间隔自动保存当前通道图片；停用计划会保留已有图片。" scroll-body @close="closeSchedule">
      <form :id="`capture-schedule-${channelId}`" class="capture-plan-form" @submit.prevent="saveSchedule">
        <fieldset :disabled="savingSchedule">
          <legend class="sr-only">定时抓拍计划</legend>
          <label class="capture-plan-toggle"><span>启用定时抓拍</span><span class="switch"><input v-model="scheduleDraft.enabled" type="checkbox" role="switch" /><span class="slider" /></span></label>
          <div class="capture-plan-fields">
            <label class="form-group"><span class="form-label">抓拍间隔（秒）</span><input v-model.number="scheduleDraft.interval_seconds" class="input plain" type="number" min="10" max="31536000" step="1" required /></label>
            <label class="form-group"><span class="form-label">首次执行时间（可选）</span><input v-model="firstRun" class="input plain" type="datetime-local" :aria-describedby="`capture-first-run-help-${channelId}`" /><small :id="`capture-first-run-help-${channelId}`" class="capture-plan-help">留空则在本次保存后等待一个抓拍间隔。</small></label>
          </div>
        </fieldset>
        <dl v-if="scheduleLoaded" class="capture-schedule-result">
          <div><dt>当前下次执行</dt><dd>{{ schedule.enabled && hasScheduledTime ? formatDate(schedule.next_run_at) : '未安排' }}</dd></div>
          <div><dt>最近执行</dt><dd>{{ formatDate(schedule.last_run_at) }}</dd></div>
        </dl>
        <p v-if="scheduleSaveError" class="warning-box" role="alert">{{ scheduleSaveError }}</p>
        <RouterLink class="capture-settings-link" to="/snapshot-settings?tab=settings">全局抓拍设置</RouterLink>
      </form>
      <template #footer><button class="btn" type="button" :disabled="savingSchedule" @click="closeSchedule">取消</button><button class="btn btn-primary" type="submit" :form="`capture-schedule-${channelId}`" :disabled="savingSchedule"><LoaderCircle v-if="savingSchedule" class="animate-spin" aria-hidden="true" /><Save v-else aria-hidden="true" />{{ savingSchedule ? '保存中' : '保存计划' }}</button></template>
    </ModalDialog>

    <section aria-label="抓拍图片">
      <div class="capture-heading"><div><h3>抓拍图片 <span class="capture-count">{{ total }}</span></h3><p>长期保存，可预览、下载和重命名；删除会同时移除原文件。</p></div><div class="button-row"><button class="btn" type="button" :disabled="loading" @click="refresh"><RefreshCw :class="{ 'animate-spin': loading }" />刷新记录</button><button v-if="channelId" class="btn btn-primary" type="button" :disabled="disabled || busy" @click="capture"><LoaderCircle v-if="busy" class="animate-spin" /><Camera v-else />{{ pending ? stateNames[pending.state] : submitting ? '提交中' : '立即抓拍' }}</button></div></div>
      <p v-if="pending" class="capture-progress" role="status">{{ stateNames[pending.state] }}，成功后会自动打开图片。离开页面后任务仍会继续，可在记录中查看结果。</p>
      <p v-if="taskError" class="warning-box" role="alert">{{ taskError }}</p>
      <p v-if="error" class="warning-box" role="alert">{{ error }}</p>
      <div v-if="!rows.length && !loading && !error" class="capture-empty"><Image /><strong>暂无抓拍图片</strong><span>{{ channelId ? '点击立即抓拍，或启用上方的定时计划。' : '在通道详情或实时监控中发起抓拍，图片会汇总到这里。' }}</span></div>
      <p v-if="loading && !rows.length" role="status">正在读取抓拍记录…</p>
      <div class="capture-grid" :aria-busy="loading">
        <article v-for="row in rows" :key="row.id" class="capture-item">
          <button v-if="row.state === 'succeeded'" class="capture-thumbnail" type="button" :aria-label="`查看 ${row.name}`" @click="selected = row">
            <img v-if="!brokenImages.has(row.id)" :src="imageUrl(row)" :alt="row.name" loading="lazy" @error="brokenImages.add(row.id)" />
            <span v-else>图片加载失败，点击重试预览</span>
          </button>
          <div v-else class="capture-thumbnail capture-state" :class="{ 'capture-error': row.state === 'failed' }"><LoaderCircle v-if="row.state === 'queued' || row.state === 'running'" class="animate-spin" /><span>{{ stateNames[row.state] }}</span></div>
          <div class="capture-caption"><strong :title="row.name">{{ row.name || '等待生成图片' }}</strong><time>{{ formatDate(row.captured_at || row.created_at) }}</time><span v-if="!channelId" class="capture-channel">通道 {{ row.channel_id }}</span><span>{{ row.source === 'schedule' ? '定时抓拍' : '手动抓拍' }}<template v-if="row.method"> · {{ row.method === 'ffmpeg' ? '实时流取帧' : row.method === 'url' ? '指定流取帧' : '设备上传' }}</template></span><p v-if="row.error" class="capture-error">{{ row.error }}</p></div>
          <div class="capture-actions"><template v-if="row.state === 'succeeded'"><a class="btn btn-sm" :href="imageUrl(row, true)"><Download />下载</a><button class="btn btn-sm" type="button" @click="edit(row)"><Pencil />改名</button></template><button class="btn btn-sm" type="button" :disabled="row.state === 'queued' || row.state === 'running'" @click="askRemove(row)"><Trash2 />{{ row.state === 'deleting' ? '重试删除' : '删除' }}</button></div>
        </article>
      </div>
      <nav class="capture-pagination" aria-label="抓拍图片分页">
        <div class="capture-page-summary">
          <span v-if="loading && !rows.length" role="status">正在读取…</span>
          <span v-else>共 {{ total }} 张<template v-if="rows.length">，当前 {{ rangeStart }}–{{ rangeEnd }} 张</template></span>
          <label class="capture-page-size">每页<select v-model.number="size" class="select" aria-label="抓拍图片每页数量" :disabled="loading" @change="changeSize"><option v-for="value in pageSizes" :key="value" :value="value">{{ value }} 张</option></select></label>
        </div>
        <div class="capture-page-controls">
          <div class="capture-page-buttons">
            <button class="btn capture-page-arrow" type="button" :disabled="page <= 1 || loading" aria-label="上一页" title="上一页" @click="changePage(page - 1)"><ChevronLeft aria-hidden="true" /></button>
            <span class="capture-page-position">第 {{ page }} / {{ pages }} 页</span>
            <template v-for="(value, index) in pageNumbers" :key="`${value}-${index}`">
              <span v-if="value === 'ellipsis'" class="capture-page-gap" aria-hidden="true">…</span>
              <button v-else class="btn capture-page-number" :class="{ 'is-current': value === page }" type="button" :aria-label="`第 ${value} 页`" :aria-current="value === page ? 'page' : undefined" :disabled="loading" @click="changePage(value)">{{ value }}</button>
            </template>
            <button class="btn capture-page-arrow" type="button" :disabled="page >= pages || loading" aria-label="下一页" title="下一页" @click="changePage(page + 1)"><ChevronRight aria-hidden="true" /></button>
          </div>
          <form class="capture-page-jump" @submit.prevent="goToPage">
            <label>跳至<input v-model.number="jumpPage" class="input plain" type="number" inputmode="numeric" min="1" :max="pages" step="1" :placeholder="String(page)" required aria-label="跳转页码" :disabled="loading || pages <= 1" />页</label>
            <button class="btn" type="submit" :disabled="loading || pages <= 1 || jumpPage === ''">跳转</button>
          </form>
        </div>
      </nav>
    </section>

    <ModalDialog :open="Boolean(selected)" :title="selected?.name || '抓拍图片'" :description="selected ? formatDate(selected.captured_at) : ''" size="wide" scroll-body @close="selected = null">
      <p v-if="previewError" class="warning-box" role="alert">图片读取失败，请刷新记录确认文件是否存在。</p>
      <img v-if="selected && !previewError" class="capture-full-image" :src="imageUrl(selected)" :alt="selected.name" @error="previewError = true" />
      <template #footer><a v-if="selected" class="btn btn-primary" :href="imageUrl(selected, true)"><Download />下载原图</a></template>
    </ModalDialog>
    <ModalDialog :open="Boolean(editing)" title="修改图片名称" description="名称会同时更新到图片文件，保留 .jpg 扩展名。" @close="!mutating && (editing = null)">
      <form id="capture-rename" class="capture-modal-form" @submit.prevent="rename"><label class="form-group"><span class="form-label">图片名称</span><input v-model="newName" class="input plain w-full" maxlength="180" required :disabled="mutating" /></label><p v-if="mutationError" class="warning-box" role="alert">{{ mutationError }}</p></form>
      <template #footer><button class="btn" type="button" :disabled="mutating" @click="editing = null">取消</button><button class="btn btn-primary" form="capture-rename" type="submit" :disabled="mutating">{{ mutating ? '保存中' : '保存名称' }}</button></template>
    </ModalDialog>
    <ModalDialog :open="Boolean(removing)" title="删除这次抓拍？" description="会永久删除抓拍记录及对应的图片文件，无法恢复。" @close="!mutating && (removing = null)">
      <div class="capture-modal-form"><p>{{ removing?.name || formatDate(removing?.created_at) }}</p><p v-if="mutationError" class="warning-box" role="alert">{{ mutationError }}</p></div>
      <template #footer><button class="btn" type="button" :disabled="mutating" @click="removing = null">取消</button><button class="btn btn-danger" type="button" :disabled="mutating" @click="remove">{{ mutating ? '删除中' : '删除记录与文件' }}</button></template>
    </ModalDialog>
  </div>
</template>

<style scoped>
.capture-workspace { min-width: 0; container-type: inline-size; }
.capture-plan { padding-bottom: 16px; margin-bottom: 16px; border-bottom: 1px solid var(--line); }
.capture-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; flex-wrap: wrap; margin-bottom: 16px; }
.capture-heading h3 { margin: 0; font-size: 16px; font-weight: 700; color: var(--ink-strong); }
.capture-heading p { margin: 6px 0 0; color: var(--muted); font-size: 13px; }
.capture-heading a { color: var(--blue); text-decoration: underline; text-underline-offset: 3px; }
.capture-count { margin-left: 8px; font-size: 13px; color: var(--muted); font-variant-numeric: tabular-nums; }
.capture-plan-summary { display: flex; align-items: center; justify-content: space-between; gap: 16px; }
.capture-plan-summary > div { min-width: 0; }
.capture-plan-summary h3 { margin: 0; color: var(--ink-strong); font-size: 14px; font-weight: 600; }
.capture-plan-summary p { margin: 6px 0 0; color: var(--muted); font-size: 13px; line-height: 1.6; overflow-wrap: anywhere; }
.capture-plan-summary > .btn { flex: 0 0 auto; }
.capture-plan-form { display: grid; gap: 16px; padding: 2px; }
.capture-plan-form fieldset { display: grid; gap: 16px; min-width: 0; padding: 0; margin: 0; border: 0; }
.capture-plan-toggle { display: flex; align-items: center; justify-content: space-between; gap: 12px; min-height: var(--control-height); font-size: 13px; cursor: pointer; }
.capture-plan-toggle input:disabled + .slider { opacity: .6; cursor: not-allowed; }
.capture-plan-fields { display: grid; grid-template-columns: minmax(0, 1fr); gap: 16px; }
.capture-plan-fields .form-group { margin: 0; }
.capture-plan-help { margin: 0; color: var(--muted); font-size: 12px; line-height: 1.5; }
.capture-settings-link { justify-self: start; color: var(--blue); font-size: 13px; text-decoration: underline; text-underline-offset: 3px; }
.capture-plan-form .sr-only { position: absolute; width: 1px; height: 1px; padding: 0; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
.capture-schedule-result { display: flex; flex-wrap: wrap; gap: 8px 24px; min-width: 0; margin: 0; font-size: 12px; line-height: 1.5; }
.capture-schedule-result > div { display: flex; flex-wrap: wrap; gap: 4px 8px; }
.capture-schedule-result dt { color: var(--muted); }
.capture-schedule-result dd { margin: 0; color: var(--ink); font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }
.capture-plan-error { margin: 12px 0 0; font-size: 12px; overflow-wrap: anywhere; }
.capture-progress { padding: 12px; margin-bottom: 16px; color: #1555a6; background: var(--blue-soft); border-radius: var(--radius-sm); }
.capture-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 240px), 1fr)); gap: 20px; }
.capture-item { min-width: 0; border: 1px solid var(--line); border-radius: 8px; overflow: hidden; }
.capture-thumbnail { display: flex; align-items: center; justify-content: center; width: 100%; aspect-ratio: 16/9; background: var(--panel-2); color: var(--muted); border: 0; padding: 0; cursor: pointer; overflow: hidden; }
.capture-thumbnail img { width: 100%; height: 100%; object-fit: contain; }
.capture-thumbnail:hover { background: var(--blue-soft); }
.capture-thumbnail:focus-visible { outline: 2px solid var(--blue); outline-offset: -3px; }
.capture-state { gap: 8px; cursor: default; }
.capture-state svg { width: 20px; height: 20px; }
.capture-caption { display: grid; gap: 5px; padding: 12px; font-size: 12px; color: var(--muted); }
.capture-caption strong { color: var(--ink-strong); font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.capture-caption p { margin: 0; overflow-wrap: anywhere; }
.capture-channel { overflow-wrap: anywhere; }
.capture-actions { display: flex; flex-wrap: wrap; gap: 6px; padding: 0 12px 12px; }
.capture-error { color: #b42318; }
.capture-empty { min-height: 200px; display: flex; align-items: center; justify-content: center; flex-direction: column; gap: 10px; text-align: center; color: var(--muted); }
.capture-empty svg { width: 28px; height: 28px; }
.capture-pagination { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 12px 24px; margin-top: 20px; padding-top: 16px; border-top: 1px solid var(--line); color: var(--muted); font-size: 12px; font-variant-numeric: tabular-nums; }
.capture-page-summary, .capture-page-controls { display: flex; align-items: center; flex-wrap: wrap; gap: 12px 16px; min-width: 0; }
.capture-page-size, .capture-page-jump, .capture-page-jump label { display: inline-flex; align-items: center; gap: 8px; }
.capture-page-size .select { width: auto; padding-inline: 8px; }
.capture-page-buttons { display: flex; align-items: center; gap: 4px; }
.capture-page-buttons .btn { min-width: var(--control-height); padding-inline: 8px; }
.capture-page-number.is-current { color: var(--blue); background: var(--blue-soft); border-color: var(--blue); }
.capture-page-number.is-current:disabled { color: var(--blue); background: var(--blue-soft); border-color: var(--blue); }
.capture-page-gap { padding-inline: 4px; }
.capture-page-position { display: none; }
.capture-page-jump .input { width: 64px; padding-inline: 8px; }
.capture-full-image { display: block; width: 100%; height: auto; max-height: 70dvh; object-fit: contain; background: var(--panel-2); }
.capture-modal-form { padding: 20px; overflow-wrap: anywhere; }
@container (max-width: 560px) {
  .capture-pagination, .capture-page-controls { flex-direction: column; align-items: flex-start; }
  .capture-page-summary { width: 100%; justify-content: space-between; }
}
@container (max-width: 380px) {
  .capture-page-number, .capture-page-gap { display: none; }
  .capture-page-position { display: inline; padding-inline: 8px; }
}
@media (max-width: 640px), (pointer: coarse) {
  .capture-plan-toggle, .capture-page-buttons .btn { min-height: 44px; }
  .capture-page-buttons .btn { min-width: 44px; }
}
@media (max-width: 640px) { .capture-heading .button-row { width: 100%; flex-wrap: wrap; } }
</style>
