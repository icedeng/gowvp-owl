<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Camera, ChevronLeft, ChevronRight, Clapperboard, LayoutGrid, ListFilter, LoaderCircle, Play, RefreshCcw, Rows3, Search, Settings2, ShieldAlert } from '@lucide/vue'
import { useRoute, useRouter } from 'vue-router'
import RemoteChannelSelect from '../components/RemoteChannelSelect.vue'
import ModalDialog from '../components/ModalDialog.vue'
import EventPolicyEditor from '../components/EventPolicyEditor.vue'
import EventDetailDialog from '../features/events/EventDetailDialog.vue'
import EventPreviewCard from '../features/events/EventPreviewCard.vue'
import { alarmSummary, channelLabel, deviceLabel, eventTitle, evidenceLabel, isAlarm, sourceLabel, statusLabels } from '../features/events/format'
import { api, errorMessage } from '../services/api'
import type { ApiEvent, EventStatus } from '../types/api'
import { formatDate } from '../utils/format'
import { useEventNoticesStore } from '../stores/event-notices'
const route = useRoute(), router = useRouter()
const eventNotices = useEventNoticesStore()
type Presentation = 'cards' | 'table'
const presentation = ref<Presentation>(readPresentation())
const view = ref('events'), query = ref(''), source = ref('all'), period = ref('all'), status = ref('pending')
const device = ref('all'), channel = ref('all'), evidence = ref(''), policyDevice = ref('all')
const page = ref(1), size = ref(20), jumpPage = ref(1), total = ref(0)
const events = ref<ApiEvent[]>([]), loading = ref(false), loadError = ref('')
const selected = ref<number[]>([]), selectedId = ref(0), autoRefresh = ref(true), imageErrors = ref(new Set<number>())
const refreshing = ref(false), lastRefreshedAt = ref(''), pageVisible = ref(document.visibilityState !== 'hidden')
const bulkItems = ref<ApiEvent[]>([]), bulkStatus = ref<EventStatus>('processing'), bulkNote = ref(''), bulkSaving = ref(false), bulkError = ref('')
let loadSequence = 0, timer: number | undefined
const pageCount = computed(() => Math.max(1, Math.ceil(total.value / size.value)))
const pageNumbers = computed(() => Array.from({ length: Math.min(5, pageCount.value) }, (_, i) => Math.min(Math.max(1, page.value - 2), Math.max(1, pageCount.value - 4)) + i))
const chosen = computed(() => events.value.filter(item => selected.value.includes(item.id)))
const isClosed = (item: ApiEvent) => item.status === 'resolved' || item.status === 'ignored'
const canBatch = computed(() => chosen.value.length > 0 && (chosen.value.every(isClosed) || chosen.value.every(item => !isClosed(item))))
const allSelected = computed(() => events.value.length > 0 && events.value.every(item => selected.value.includes(item.id)))
const hasFilters = computed(() => Boolean(query.value || source.value !== 'all' || period.value !== 'all' || device.value !== 'all' || channel.value !== 'all' || evidence.value))
const refreshPaused = computed(() => Boolean(selected.value.length || selectedId.value || bulkItems.value.length))
const canAutoRefresh = computed(() => autoRefresh.value && view.value === 'events' && pageVisible.value && !refreshPaused.value)
function readPresentation(): Presentation {
 try { return window.localStorage.getItem('owl.events.presentation') === 'table' ? 'table' : 'cards' }
 catch { return 'cards' }
}
function changePresentation(value: Presentation) {
 presentation.value = value
 try { window.localStorage.setItem('owl.events.presentation', value) } catch { /* 存储不可用时仍允许切换视图。 */ }
}
function selectEvent(id: number, checked: boolean) {
 selected.value = checked ? [...new Set([...selected.value, id])] : selected.value.filter(value => value !== id)
}
function ranges() {
 if (period.value === 'all') return {}
 const end = Date.now(), today = new Date(); today.setHours(0, 0, 0, 0)
 return { start_ms: period.value === 'today' ? today.getTime() : end - (period.value === '7d' ? 7 : 30) * 86400000, end_ms: end }
}
async function load(silent = false) {
 if (silent && (!canAutoRefresh.value || loading.value || refreshing.value)) return
 void eventNotices.refresh()
 const sequence = ++loadSequence
 refreshing.value = true
 if (!silent) loading.value = true
 loadError.value = ''
 try {
  const { data } = await api.events({ page: page.value, size: size.value, key: query.value.trim() || undefined, status: status.value || undefined, source: source.value, did: device.value === 'all' ? undefined : device.value, cid: channel.value === 'all' ? undefined : channel.value, evidence: evidence.value || undefined, ...ranges() })
  if (sequence !== loadSequence || (silent && !canAutoRefresh.value)) return
  events.value = data.items || []; total.value = data.total || 0; selected.value = []; imageErrors.value = new Set()
  lastRefreshedAt.value = new Date().toLocaleTimeString('zh-CN', { hour12: false })
  if (page.value > pageCount.value) { navigate(pageCount.value); return }
 } catch (cause) { if (sequence === loadSequence) loadError.value = errorMessage(cause, '事件加载失败，请重试') }
 finally { if (sequence === loadSequence) { loading.value = false; refreshing.value = false } }
}
function navigate(nextPage = 1) {
 const params: Record<string, string> = { status: status.value || 'all', page: String(nextPage), size: String(size.value) }
 if (query.value.trim()) params.key = query.value.trim()
 if (source.value !== 'all') params.source = source.value
 if (period.value !== 'all') params.period = period.value
 if (device.value !== 'all') params.did = device.value
 if (channel.value !== 'all') params.cid = channel.value
 if (evidence.value) params.evidence = evidence.value
 if (view.value === 'policies') params.view = 'policies'
 if (view.value === 'policies' && policyDevice.value !== 'all') params.policy_device = policyDevice.value
 if (JSON.stringify(route.query) === JSON.stringify(params)) { page.value = nextPage; void load() }
 else void router.replace({ query: params })
}
function applyFilters() { navigate(1) }
function changePage(value: number) { if (loading.value || value < 1 || value > pageCount.value) return; navigate(value) }
function resetFilters() { query.value = ''; source.value = 'all'; period.value = 'all'; device.value = 'all'; channel.value = 'all'; evidence.value = ''; navigate(1) }
function changeView(value: string) { view.value = value; navigate(page.value) }
function showPolicy(did: string) { selectedId.value = 0; policyDevice.value = did; view.value = 'policies'; navigate(page.value) }
function selectAll(checked: boolean) { selected.value = checked ? events.value.map(item => item.id) : [] }
function openBatch() {
 if (!canBatch.value) return
 bulkItems.value = chosen.value.map(item => ({ ...item })); bulkStatus.value = bulkItems.value.every(isClosed) ? 'pending' : 'processing'; bulkNote.value = ''; bulkError.value = ''
}
async function submitBatch() {
 if (bulkSaving.value || !bulkItems.value.length) return
 bulkSaving.value = true; bulkError.value = ''
 try { await api.processEvents(bulkItems.value.map(item => ({ id: item.id, revision: item.revision })), bulkStatus.value, bulkNote.value); bulkItems.value = []; await load() }
 catch (cause) { bulkError.value = errorMessage(cause, '未能确认批量处理结果，请刷新事件后核对') }
 finally { bulkSaving.value = false }
}
function toggleRefresh() {
 autoRefresh.value = !autoRefresh.value
}
function refreshVisiblePage() {
 const wasVisible = pageVisible.value
 pageVisible.value = document.visibilityState !== 'hidden'
 // 从后台返回由 watch 补刷；已可见窗口重新获得焦点时也核对一次。
 if (wasVisible && pageVisible.value) void load(true)
}
function restore() {
 const q = route.query
 view.value = q.view === 'policies' ? 'policies' : 'events'
 query.value = String(q.key || ''); source.value = ['alarm', 'ai', 'gateway'].includes(String(q.source)) ? String(q.source) : 'all'
 period.value = ['today', '7d', '30d'].includes(String(q.period)) ? String(q.period) : 'all'
 status.value = q.status === 'all' ? '' : ['pending', 'processing', 'resolved', 'ignored'].includes(String(q.status)) ? String(q.status) : 'pending'
 device.value = String(q.did || 'all'); channel.value = String(q.cid || 'all')
 evidence.value = ['image', 'video', 'failed'].includes(String(q.evidence)) ? String(q.evidence) : ''
 page.value = Math.max(1, Math.min(1000000, Math.trunc(Number(q.page)) || 1)); size.value = [20, 50, 100].includes(Number(q.size)) ? Number(q.size) : 20; jumpPage.value = page.value
 policyDevice.value = String(q.policy_device || q.did || 'all')
 if (q.event && Number(q.event) > 0) selectedId.value = Number(q.event)
 if (view.value === 'events') void load()
}
watch(() => route.query, restore, { immediate: true })
watch(canAutoRefresh, active => { if (active) void load(true) })
onMounted(() => {
 timer = window.setInterval(() => { void load(true) }, 30000)
 document.addEventListener('visibilitychange', refreshVisiblePage)
 window.addEventListener('focus', refreshVisiblePage)
})
onBeforeUnmount(() => {
 ++loadSequence; window.clearInterval(timer)
 document.removeEventListener('visibilitychange', refreshVisiblePage)
 window.removeEventListener('focus', refreshVisiblePage)
})
</script>
<template>
 <main class="page-content event-workspace">
  <header class="page-head"><div><h1 class="page-title">事件中心</h1><p class="page-desc">按设备核实报警、查看现场证据并记录处置结果。</p></div><div v-if="view === 'events'" class="head-actions"><button class="btn" :aria-pressed="autoRefresh" @click="toggleRefresh">{{ autoRefresh ? '自动刷新：开（30 秒）' : '自动刷新：关' }}</button><button class="btn" :disabled="loading" @click="load()"><RefreshCcw :class="{ 'animate-spin': refreshing }" />刷新事件</button></div></header>
  <nav class="resource-detail-nav event-main-nav" aria-label="事件中心分区"><button :class="{ active: view === 'events' }" :aria-current="view === 'events' ? 'page' : undefined" @click="changeView('events')">事件处理</button><button :class="{ active: view === 'policies' }" :aria-current="view === 'policies' ? 'page' : undefined" @click="changeView('policies')"><Settings2 />联动策略</button></nav>
  <section v-if="view === 'policies'" class="card card-pad event-policy-panel"><div class="policy-selector"><label class="form-label">配置设备</label><RemoteChannelSelect v-model="policyDevice" resource="device" type="GB28181" all-label="请选择国标设备" aria-label="选择配置联动策略的国标设备" @change="navigate(page)"/><RouterLink v-if="policyDevice !== 'all'" class="btn" :to="`/devices/${encodeURIComponent(policyDevice)}?tab=events`">打开设备详情</RouterLink></div><EventPolicyEditor :device-id="policyDevice"/></section>
  <template v-else>
   <section class="card event-list-panel">
    <nav class="event-status-nav" aria-label="按处理状态筛选"><button v-for="entry in [{ key: 'pending', label: '待处理' }, { key: 'processing', label: '处理中' }, { key: 'resolved', label: '已处理' }, { key: 'ignored', label: '已忽略' }, { key: '', label: '全部事件' }]" :key="entry.key" :class="{ active: status === entry.key }" :aria-pressed="status === entry.key" @click="status = entry.key; applyFilters()">{{ entry.label }}</button></nav>
    <form class="event-filters" @submit.prevent="applyFilters"><label class="field event-search"><Search /><input v-model="query" class="input" maxlength="128" placeholder="搜索设备、通道名称或编码" aria-label="搜索全部事件的设备、通道、标签或模型"></label><RemoteChannelSelect v-model="device" resource="device" all-label="全部设备" aria-label="按设备筛选事件" @change="channel = 'all'; applyFilters()"/><RemoteChannelSelect v-model="channel" all-label="全部通道" aria-label="按通道筛选事件" @change="applyFilters"/><select v-model="source" class="select" aria-label="事件来源" @change="applyFilters"><option value="all">全部来源</option><option value="alarm">设备报警</option><option value="ai">AI 检测</option><option value="gateway">网关故障</option></select><select v-model="period" class="select" aria-label="事件时间范围" @change="applyFilters"><option value="all">全部时间</option><option value="today">今天</option><option value="7d">近 7 天</option><option value="30d">近 30 天</option></select><select v-model="evidence" class="select" aria-label="取证条件" @change="applyFilters"><option value="">全部取证情况</option><option value="image">有抓拍图片</option><option value="video">有视频片段</option><option value="failed">取证失败</option></select><button class="btn btn-primary" :disabled="loading"><Search />查询</button><button v-if="hasFilters" type="button" class="btn" :disabled="loading" @click="resetFilters">重置</button></form>
    <div v-if="loadError" class="warning-box event-list-error" role="alert"><ShieldAlert /><span>{{ loadError }}</span><button class="btn btn-sm" @click="load()">重试</button></div>
    <div class="event-list-summary">
     <div class="event-selection-summary">
      <label v-if="presentation === 'cards'" class="event-select-all"><input type="checkbox" :checked="allSelected" :indeterminate="selected.length > 0 && !allSelected" :disabled="loading || !events.length" @change="selectAll(($event.target as HTMLInputElement).checked)">选择本页</label>
      <span aria-live="polite">{{ status ? statusLabels[status as EventStatus] : '全部事件' }} · 共 {{ total }} 条记录</span>
      <span v-if="selected.length" class="event-selection-actions">已选 {{ selected.length }} 条 <button class="btn btn-sm" :disabled="!canBatch" @click="openBatch">批量处理</button><small v-if="!canBatch">请分别选择未结束或已结束事件</small></span>
      <small v-if="autoRefresh">{{ refreshPaused ? '勾选或处理期间暂停，结束后立即刷新' : '每 30 秒自动刷新' }}</small><small v-if="lastRefreshedAt">最近更新 {{ lastRefreshedAt }}</small>
     </div>
     <div class="event-presentation-switch" role="group" aria-label="事件展示方式">
      <button type="button" class="btn btn-sm" :class="{ active: presentation === 'cards' }" :aria-pressed="presentation === 'cards'" @click="changePresentation('cards')"><LayoutGrid aria-hidden="true" />卡片</button>
      <button type="button" class="btn btn-sm" :class="{ active: presentation === 'table' }" :aria-pressed="presentation === 'table'" @click="changePresentation('table')"><Rows3 aria-hidden="true" />表格</button>
     </div>
    </div>
    <div v-if="presentation === 'table'" class="table-wrap" :aria-busy="loading"><table class="data-table event-table"><thead><tr><th class="check-cell"><input type="checkbox" :checked="allSelected" :indeterminate="selected.length > 0 && !allSelected" :disabled="loading || !events.length" aria-label="选择本页全部事件" @change="selectAll(($event.target as HTMLInputElement).checked)"></th><th>发生时间</th><th>事件</th><th>设备 / 通道</th><th>现场证据</th><th>处理状态</th><th class="event-action-cell">操作</th></tr></thead><tbody v-if="!loading && !loadError"><tr v-for="item in events" :key="item.id" :class="{ 'event-row-selected': selected.includes(item.id) }"><td class="check-cell"><input v-model="selected" type="checkbox" :value="item.id" :aria-label="`选择 ${deviceLabel(item)} 的 ${eventTitle(item)}事件`"></td><td class="event-table-time">{{ formatDate(item.started_at || item.created_at) }}<small>事件 #{{ item.id }}<template v-if="(item.occurrence_count || 1) > 1"> · 连续 {{ item.occurrence_count }} 次</template></small></td><td><button class="event-title-link" @click="selectedId = item.id">{{ eventTitle(item) }}</button><small>{{ sourceLabel(item) }}<template v-if="alarmSummary(item)"> · {{ alarmSummary(item) }}</template></small></td><td class="event-device"><strong>{{ deviceLabel(item) }}</strong><span>{{ channelLabel(item) }}</span><small>{{ item.device_code || item.did || item.channel_code || item.cid }}</small></td><td><span v-if="item.historical" class="section-note">历史补报、现场取证不可用</span><span v-else-if="item.model === 'AccessGateway'" class="section-note">网关运行事件</span><div v-else class="event-evidence-status"><button v-if="item.image_path && !imageErrors.has(item.id)" class="event-thumb" :aria-label="`查看 ${deviceLabel(item)} 的报警抓拍`" @click="selectedId = item.id"><img :src="api.eventThumbnail(item.image_path)" loading="lazy" alt="" @error="imageErrors.add(item.id)"></button><div><span :class="{ 'evidence-failed': item.evidence?.snapshot_state === 'failed' }"><Camera />{{ item.image_path ? (imageErrors.has(item.id) ? '图片不可用' : '有抓拍') : evidenceLabel(item.evidence?.snapshot_state) }}</span><span :class="{ 'evidence-failed': item.evidence?.video_state === 'failed' }"><Clapperboard />{{ item.video_path ? '有视频片段' : evidenceLabel(item.evidence?.video_state) }}</span></div></div></td><td><span class="status" :class="item.status === 'resolved' ? 'online' : item.status === 'ignored' ? '' : 'pending'">{{ statusLabels[item.status] }}</span></td><td class="event-action-cell"><div class="event-row-actions"><button class="btn btn-sm" @click="selectedId = item.id">查看 / 处理</button><RouterLink v-if="item.cid" class="btn btn-sm" :to="`/live?channel=${encodeURIComponent(item.cid)}`"><Play aria-hidden="true" />实时画面</RouterLink></div></td></tr></tbody></table>
    </div>
    <div v-else-if="!loading && !loadError && events.length" class="event-preview-grid" aria-label="事件卡片">
     <EventPreviewCard v-for="item in events" :key="item.id" :item="item" :selected="selected.includes(item.id)" :image-failed="imageErrors.has(item.id)" @open="selectedId = $event" @select="selectEvent" @image-error="imageErrors.add($event)" />
    </div>
     <div v-if="loading" class="empty-state" role="status"><LoaderCircle class="mx-auto h-6 w-6 animate-spin" />正在加载事件…</div><div v-else-if="!loadError && !events.length" class="empty-state empty-action"><ListFilter /><strong>{{ status === 'pending' ? '当前没有待处理事件' : '没有符合条件的事件' }}</strong><span>可切换处理状态、调整设备或时间范围查看历史记录。</span><button v-if="hasFilters" class="btn" @click="resetFilters">清除筛选</button><button v-else-if="status" class="btn" @click="status = ''; applyFilters()">查看全部事件</button></div>
    <nav class="pagination event-pagination" aria-label="事件分页"><label>每页 <select v-model.number="size" class="select" :disabled="loading" aria-label="每页事件条数" @change="navigate(1)"><option :value="20">20 条</option><option :value="50">50 条</option><option :value="100">100 条</option></select></label><div class="event-page-controls"><button class="page-btn" :disabled="loading || page <= 1" aria-label="上一页" @click="changePage(page - 1)"><ChevronLeft /></button><button v-for="value in pageNumbers" :key="value" class="page-btn" :class="{ active: value === page }" :aria-current="value === page ? 'page' : undefined" :disabled="loading" @click="changePage(value)">{{ value }}</button><button class="page-btn" :disabled="loading || page >= pageCount" aria-label="下一页" @click="changePage(page + 1)"><ChevronRight /></button><span>第 {{ page }} / {{ pageCount }} 页</span><form class="event-jump" @submit.prevent="changePage(jumpPage)"><label class="sr-only" for="event-jump-page">跳转页码</label><input id="event-jump-page" v-model.number="jumpPage" class="input" type="number" min="1" :max="pageCount" required :disabled="loading"><button class="btn btn-sm" :disabled="loading">跳转</button></form></div></nav>
   </section>
  </template>
  <EventDetailDialog :event-id="selectedId" @close="selectedId = 0" @processed="load()" @policy="showPolicy"/>
  <ModalDialog :open="bulkItems.length > 0" title="批量处理事件" description="对所选事件应用相同处理结果和说明，并逐条保留操作记录。" @close="!bulkSaving && (bulkItems = [])"><form class="bulk-form" @submit.prevent="submitBatch"><p>已选 {{ bulkItems.length }} 条事件。若其中有事件已被他人更新，本次批量操作将全部取消。</p><label class="form-group"><span class="form-label">处理结果</span><select v-model="bulkStatus" class="select" :disabled="bulkSaving"><option v-if="bulkItems.every(isClosed)" value="pending">重新打开 · 待处理</option><template v-else><option value="processing">开始处理</option><option value="resolved">处理完成</option><option value="ignored">忽略事件</option></template></select></label><label class="form-group"><span class="form-label">处理说明 {{ bulkStatus === 'processing' ? '（选填）' : '（必填）' }}</span><textarea v-model="bulkNote" class="input" rows="4" maxlength="1000" :required="bulkStatus !== 'processing'" :disabled="bulkSaving" placeholder="填写核实情况、处置结果或忽略原因"/></label><div v-if="bulkError" class="warning-box" role="alert">{{ bulkError }}</div><div class="button-row"><button type="button" class="btn" :disabled="bulkSaving" @click="bulkItems = []">取消</button><button class="btn btn-primary" :disabled="bulkSaving"><LoaderCircle v-if="bulkSaving" class="animate-spin"/>{{ bulkSaving ? '正在提交…' : '保存处理结果' }}</button></div></form></ModalDialog>
 </main>
</template>
<style scoped>
.event-main-nav { margin-bottom: 16px; }
.event-main-nav button { flex: none; }
.event-status-nav { display: flex; flex-wrap: wrap; gap: 4px; padding: 12px 14px 0; border-bottom: 1px solid var(--line); }
.event-status-nav button { min-height: 36px; padding: 0 14px 10px; border: 0; border-bottom: 2px solid transparent; background: transparent; color: var(--muted); cursor: pointer; }
.event-status-nav button:hover { color: var(--blue); }
.event-status-nav button.active { color: var(--blue); border-bottom-color: var(--blue); font-weight: 700; }
.event-filters { display: flex; gap: 10px; flex-wrap: wrap; align-items: center; padding: 14px; }
.event-search { flex: 0 1 280px; width: 280px; }
.event-search .input { width: 100%; }
.event-filters > .remote-select { flex: 0 1 180px; min-width: 160px; }
.event-filters > .select { width: auto; max-width: 180px; }
.event-list-summary { display: flex; flex-wrap: wrap; justify-content: space-between; align-items: center; gap: 12px; padding: 0 14px 12px; font-size: 13px; }
.event-selection-summary, .event-selection-actions, .event-select-all { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; }
.event-select-all { cursor: pointer; }
.event-presentation-switch { display: inline-flex; flex-shrink: 0; }
.event-presentation-switch .btn { border-radius: 0; }
.event-presentation-switch .btn:first-child { border-radius: var(--radius-sm) 0 0 var(--radius-sm); }
.event-presentation-switch .btn:last-child { margin-left: -1px; border-radius: 0 var(--radius-sm) var(--radius-sm) 0; }
.event-presentation-switch .btn.active { z-index: 1; border-color: var(--blue); background: var(--blue-soft); color: var(--blue); }
.event-preview-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 260px), 1fr)); gap: 12px; padding: 0 14px 14px; }
.event-list-summary small { color: var(--muted); }
.event-list-error { margin: 0 14px 14px; }
.event-table { min-width: 1040px; }
.event-table td { vertical-align: middle; }
.event-table .check-cell { width: 38px; text-align: center; padding-right: 0; }
.event-table small { display: block; color: var(--muted); font-size: 12px; margin-top: 5px; max-width: 250px; overflow-wrap: anywhere; }
.event-table-time { white-space: nowrap; font-variant-numeric: tabular-nums; }
.event-title-link { padding: 0; color: var(--blue); background: transparent; border: 0; text-align: left; font-weight: 600; cursor: pointer; }
.event-title-link:hover { text-decoration: underline; text-underline-offset: 3px; }
.event-device { min-width: 190px; max-width: 260px; overflow-wrap: anywhere; }
.event-device > strong, .event-device > span { display: block; }
.event-device > span { color: var(--muted); font-size: 12px; margin-top: 4px; }
.event-action-cell { white-space: nowrap; text-align: right; }
.event-row-actions { display: flex; justify-content: flex-end; align-items: center; gap: 8px; }
.event-row-selected { background: var(--blue-soft); }
.event-evidence-status { display: flex; align-items: center; gap: 10px; }
.event-evidence-status > div { display: grid; gap: 6px; }
.event-evidence-status span { display: flex; align-items: center; gap: 6px; white-space: nowrap; font-size: 12px; color: var(--muted); }
.event-evidence-status svg { width: 14px; height: 14px; }
.event-evidence-status .evidence-failed { color: var(--red); }
.event-thumb { width: 52px; height: 40px; flex-shrink: 0; padding: 0; background: var(--panel-2); border: 1px solid var(--line); border-radius: var(--radius-sm); overflow: hidden; cursor: pointer; }
.event-thumb img { width: 100%; height: 100%; object-fit: cover; }
.event-pagination, .event-page-controls, .event-pagination > label, .event-jump { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }
.event-pagination { justify-content: space-between; padding: 14px; }
.event-pagination .select { width: auto; }
.event-jump { margin-left: 8px; }
.event-jump .input { width: 68px; }
.page-btn.active { color: var(--blue); background: var(--blue-soft); border-color: var(--blue); }
.policy-selector { display: flex; align-items: center; gap: 14px; flex-wrap: wrap; padding-bottom: 20px; margin-bottom: 20px; border-bottom: 1px solid var(--line); }
.bulk-form { padding: 20px; display: grid; gap: 16px; }
.bulk-form p { margin: 0; color: var(--muted); font-size: 13px; }
.bulk-form textarea { height: auto; min-height: 100px; padding: 10px; resize: vertical; }
@media(max-width: 640px) { .event-filters { align-items: stretch; } .event-filters > *, .event-filters > .select { flex: 1 1 100%; max-width: none; width: 100%; } .event-search { min-width: 0; } .event-filters > .remote-select { min-width: 0; flex-basis: 100%; } .event-presentation-switch .btn, .event-select-all { min-height: 44px; } .event-status-nav button { min-height: 44px; padding-inline: 10px; } .event-pagination { align-items: flex-start; gap: 14px; } .event-list-summary .section-note { display: block; } .event-main-nav button { min-height: 44px; } }
</style>
