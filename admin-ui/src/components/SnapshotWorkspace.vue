<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { Camera, Download, Image, LoaderCircle, Pencil, RefreshCw, Trash2 } from '@lucide/vue'
import { api, errorMessage } from '../services/api'
import type { CaptureRecord, CaptureSchedule } from '../types/api'
import { formatDate } from '../utils/format'
import { useUiStore } from '../stores/ui'
import ModalDialog from './ModalDialog.vue'

const props = defineProps<{ channelId?: string; disabled?: boolean }>()
const emit = defineEmits<{ captured: [record: CaptureRecord]; changed: [] }>()
const ui = useUiStore()
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
const planEnabled = ref(false)
const firstRun = ref('')
const size = 24
let sequence = 0
let pollTimer: ReturnType<typeof setTimeout> | undefined
let requestSequence = 0
let pollFailures = 0
let latestCoverId = ''
const pages = computed(() => Math.max(1, Math.ceil(total.value / size)))
const busy = computed(() => submitting.value || Boolean(pending.value))
const stateNames: Record<CaptureRecord['state'], string> = { queued: '等待执行', running: '抓拍中', succeeded: '已保存', failed: '失败', deleting: '删除中' }
const imageUrl = (row: CaptureRecord, download = false) => api.captureImage(row.id, download, row.updated_at)

async function load() {
  const current = sequence
  const request = ++requestSequence
  loading.value = true
  try {
    const { data } = await api.captures({ channel_id: props.channelId, page: page.value, size })
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
  pollTimer = setTimeout(() => void poll(current), 2000)
}

async function poll(current: number) {
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
    }
    if (current !== sequence) return
    await load()
    if (current !== sequence) return
    if (props.channelId && planEnabled.value) {
      const { data } = await api.captureSchedule(props.channelId)
      if (current !== sequence) return
      Object.assign(schedule.value, { next_run_at: data.next_run_at, last_run_at: data.last_run_at, last_error: data.last_error })
      planEnabled.value = data.enabled
    }
    pollFailures = 0
    if (pending.value || rows.value.some(row => row.state === 'queued' || row.state === 'running' || row.state === 'deleting') || planEnabled.value) schedulePoll()
  } catch (cause) {
    if (current !== sequence) return
    pollFailures += 1
    taskError.value = `${errorMessage(cause, '抓拍状态读取失败')}。任务仍在后台执行，可刷新记录查看。`
    // 短暂断网自动恢复，持续失败时让用户主动重试，避免无限请求。
    if (pollFailures < 5) schedulePoll()
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
  }
}

async function saveSchedule() {
  if (!props.channelId || !scheduleLoaded.value) return
  const current = sequence
  savingSchedule.value = true
  scheduleError.value = ''
  try {
    const next = firstRun.value ? new Date(firstRun.value).toISOString() : undefined
    const { data } = await api.saveCaptureSchedule(props.channelId, { ...schedule.value, next_run_at: next })
    if (current !== sequence) return
    schedule.value = data
    planEnabled.value = data.enabled
    firstRun.value = ''
    ui.toast(data.enabled ? '定时抓拍已启用' : '定时抓拍已停用')
    if (data.enabled) schedulePoll()
  } catch (cause) {
    if (current === sequence) scheduleError.value = errorMessage(cause, '定时计划保存失败')
  } finally {
    if (current === sequence) savingSchedule.value = false
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
function changePage(value: number) { page.value = value; void load() }
async function refresh() {
  pollFailures = 0
  await load()
  if (pending.value || planEnabled.value || rows.value.some(row => row.state === 'queued' || row.state === 'running' || row.state === 'deleting')) schedulePoll()
}

watch(selected, () => { previewError.value = false })

watch(() => props.channelId, () => {
  sequence += 1
  clearTimeout(pollTimer)
  rows.value = []; page.value = 1; total.value = 0
  selected.value = null; pending.value = null; submitting.value = false
  editing.value = null; removing.value = null; mutating.value = false
  error.value = ''; taskError.value = ''; scheduleError.value = ''; firstRun.value = ''
  brokenImages.clear(); pollFailures = 0; latestCoverId = ''
  schedule.value = { channel_id: props.channelId || '', enabled: false, interval_seconds: 300 }
  scheduleLoaded.value = false; planEnabled.value = false; savingSchedule.value = false
  void refresh(); void loadSchedule()
}, { immediate: true })
onBeforeUnmount(() => { sequence += 1; clearTimeout(pollTimer) })
</script>

<template>
  <div class="capture-workspace">
    <section v-if="channelId" class="capture-plan" aria-labelledby="capture-plan-title">
      <div class="capture-heading"><div><h3 id="capture-plan-title">定时抓拍</h3><p>按固定间隔自动保存图片；停用计划会保留已有图片。</p></div><RouterLink to="/snapshot-settings">全局抓拍设置</RouterLink></div>
      <p v-if="scheduleError" class="warning-box" role="alert">{{ scheduleError }} <button v-if="!scheduleLoaded" class="btn btn-sm" type="button" @click="loadSchedule">重试</button></p>
      <form class="capture-plan-form" @submit.prevent="saveSchedule">
        <label class="capture-check"><input v-model="schedule.enabled" type="checkbox" :disabled="!scheduleLoaded || savingSchedule" />启用定时抓拍</label>
        <label class="form-group"><span class="form-label">抓拍间隔（秒）</span><input v-model.number="schedule.interval_seconds" class="input plain" type="number" min="10" max="31536000" required :disabled="!scheduleLoaded || savingSchedule" /></label>
        <label class="form-group"><span class="form-label">首次执行时间（可选）</span><input v-model="firstRun" class="input plain" type="datetime-local" :disabled="!scheduleLoaded || savingSchedule" /><small>留空则在保存后等待一个间隔</small></label>
        <button class="btn" type="submit" :disabled="!scheduleLoaded || savingSchedule"><LoaderCircle v-if="savingSchedule" class="animate-spin" />保存计划</button>
      </form>
      <div v-if="scheduleLoaded" class="capture-schedule-result"><span>下次执行：{{ schedule.enabled ? formatDate(schedule.next_run_at) : '未启用' }}</span><span>上次执行：{{ formatDate(schedule.last_run_at) }}</span><span v-if="schedule.last_error" class="capture-error">{{ schedule.last_error }}</span></div>
    </section>

    <section aria-label="抓拍图片">
      <div class="capture-heading"><div><h3>抓拍图片 <span class="capture-count">{{ total }}</span></h3><p>长期保存，可预览、下载和重命名；删除会同时移除原文件。</p></div><div class="button-row"><button class="btn" type="button" :disabled="loading" @click="refresh"><RefreshCw :class="{ 'animate-spin': loading }" />刷新记录</button><button v-if="channelId" class="btn btn-primary" type="button" :disabled="disabled || busy" @click="capture"><LoaderCircle v-if="busy" class="animate-spin" /><Camera v-else />{{ pending ? stateNames[pending.state] : submitting ? '提交中' : '立即抓拍' }}</button></div></div>
      <p v-if="pending" class="capture-progress" role="status">{{ stateNames[pending.state] }}，成功后会自动打开图片。离开页面后任务仍会继续，可在记录中查看结果。</p>
      <p v-if="taskError" class="warning-box" role="alert">{{ taskError }}</p>
      <p v-if="error" class="warning-box" role="alert">{{ error }}</p>
      <div v-if="!rows.length && !loading && !error" class="capture-empty"><Image /><strong>暂无抓拍图片</strong><span>{{ channelId ? '点击立即抓拍，或启用上方的定时计划。' : '在通道详情或实时监控中发起抓拍，图片会汇总到这里。' }}</span></div>
      <p v-if="loading && !rows.length" role="status">正在读取抓拍记录…</p>
      <div class="capture-grid">
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
      <nav v-if="total > size" class="capture-pagination" aria-label="抓拍图片分页"><button class="btn" type="button" :disabled="page <= 1 || loading" @click="changePage(page - 1)">上一页</button><span>第 {{ page }} / {{ pages }} 页</span><button class="btn" type="button" :disabled="page >= pages || loading" @click="changePage(page + 1)">下一页</button></nav>
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
.capture-workspace { min-width: 0; }
.capture-plan { padding-bottom: 24px; margin-bottom: 24px; border-bottom: 1px solid var(--line); }
.capture-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; flex-wrap: wrap; margin-bottom: 16px; }
.capture-heading h3 { margin: 0; font-size: 16px; font-weight: 700; color: var(--ink-strong); }
.capture-heading p { margin: 6px 0 0; color: var(--muted); font-size: 13px; }
.capture-heading a { color: var(--blue); text-decoration: underline; text-underline-offset: 3px; }
.capture-count { margin-left: 8px; font-size: 13px; color: var(--muted); font-variant-numeric: tabular-nums; }
.capture-plan-form { display: flex; gap: 16px; align-items: flex-end; flex-wrap: wrap; }
.capture-plan-form .form-group { flex: 1 1 190px; max-width: 320px; margin: 0; }
.capture-check { display: inline-flex; align-items: center; gap: 8px; min-height: 34px; }
.capture-schedule-result { margin-top: 14px; display: flex; flex-wrap: wrap; gap: 8px 24px; color: var(--muted); font-size: 12px; }
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
.capture-pagination { display: flex; align-items: center; justify-content: flex-end; gap: 12px; margin-top: 20px; font-variant-numeric: tabular-nums; }
.capture-full-image { display: block; width: 100%; height: auto; max-height: 70dvh; object-fit: contain; background: var(--panel-2); }
.capture-modal-form { padding: 20px; overflow-wrap: anywhere; }
@media (max-width: 640px) { .capture-plan-form .form-group { max-width: none; flex-basis: 100%; } .capture-heading .button-row { width: 100%; flex-wrap: wrap; } }
</style>
