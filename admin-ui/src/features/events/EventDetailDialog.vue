<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Camera, ExternalLink, LoaderCircle, RefreshCcw, ShieldAlert } from '@lucide/vue'
import ModalDialog from '../../components/ModalDialog.vue'
import { api, errorMessage } from '../../services/api'
import type { ApiEvent, EventAction, EventStatus } from '../../types/api'
import { formatDate } from '../../utils/format'
import { alarmSummary, channelLabel, deviceLabel, eventTitle, evidenceLabel, isAlarm, statusLabels } from './format'
const props = defineProps<{ eventId: number }>()
const emit = defineEmits<{ close: []; processed: []; policy: [did: string] }>()
const item = ref<ApiEvent | null>(null), actions = ref<EventAction[]>([])
const loading = ref(false), saving = ref(false), error = ref(''), actionError = ref(''), success = ref('')
const nextStatus = ref<EventStatus>('processing'), note = ref('')
let sequence = 0
const closed = computed(() => item.value?.status === 'resolved' || item.value?.status === 'ignored')
const imageFailed = ref(false), videoFailed = ref(false)
async function load() {
 const current = ++sequence; const id = props.eventId
 item.value = null; actions.value = []; error.value = ''; actionError.value = ''; success.value = ''; imageFailed.value = false; videoFailed.value = false; note.value = ''
 if (!id) return
 loading.value = true
 try {
  const [detail, history] = await Promise.all([api.event(id), api.eventActions(id)])
  if (current !== sequence) return
  item.value = detail.data; actions.value = history.data.items; nextStatus.value = closed.value ? 'pending' : 'processing'
 } catch (cause) { if (current === sequence) error.value = errorMessage(cause, '事件详情加载失败') }
 finally { if (current === sequence) loading.value = false }
}
async function submit() {
 if (!item.value || saving.value) return
 const target = item.value, current = sequence
 saving.value = true; actionError.value = ''; success.value = ''
 try {
  await api.processEvents([{ id: target.id, revision: target.revision }], nextStatus.value, note.value)
  emit('processed')
  if (current === sequence) { await load(); success.value = '处理结果已保存' }
 } catch (cause) { if (current === sequence) actionError.value = errorMessage(cause, '处理失败，请刷新事件后重试') }
 finally { saving.value = false }
}
function close() { if (!saving.value) emit('close') }
watch(() => props.eventId, load, { immediate: true })
</script>
<template>
 <ModalDialog :open="Boolean(eventId)" title="事件详情与处理" size="wide" scroll-body @close="close">
  <div class="event-dialog-content">
   <p v-if="loading" class="empty-state" role="status"><LoaderCircle class="animate-spin" />正在加载事件详情…</p>
   <div v-else-if="error" class="warning-box" role="alert"><ShieldAlert /><span>{{ error }}</span><button class="btn btn-sm" @click="load">重试</button></div>
   <template v-else-if="item">
    <div class="event-detail-heading"><div><h3>{{ eventTitle(item) }}</h3><p>{{ formatDate(item.started_at) }} · #{{ item.id }}</p></div><span class="status" :class="item.status === 'resolved' ? 'online' : item.status === 'ignored' ? '' : 'pending'">{{ statusLabels[item.status] }}</span><button class="btn btn-sm" :disabled="saving" @click="load"><RefreshCcw />刷新详情</button></div>
    <dl class="event-facts"><div><dt>设备</dt><dd>{{ deviceLabel(item) }}<small>{{ item.device_code || item.did }}</small></dd></div><div><dt>通道</dt><dd>{{ channelLabel(item) }}<small>{{ item.channel_code || item.cid }}</small></dd></div><div><dt>报警信息</dt><dd>{{ alarmSummary(item) || item.model || '—' }}</dd></div></dl>
    <div class="event-resource-actions"><RouterLink v-if="item.did" class="btn btn-sm" :to="`/devices/${encodeURIComponent(item.did)}`"><ExternalLink />设备详情</RouterLink><RouterLink v-if="item.cid" class="btn btn-sm" :to="`/live?channel=${encodeURIComponent(item.cid)}`">实时画面</RouterLink><RouterLink v-if="item.cid" class="btn btn-sm" :to="`/recordings?event=${item.id}&channel=${encodeURIComponent(item.cid)}`">查询平台录像</RouterLink><button v-if="item.did && isAlarm(item)" class="btn btn-sm" :disabled="saving" @click="emit('policy', item.did)">配置该设备联动</button></div>
    <section class="evidence-section" aria-label="事件现场证据"><h3>现场证据</h3><p class="section-note">抓拍和视频在平台收到报警后获取；“查询平台录像”单独查询已有录像。</p>
     <div class="event-evidence-grid"><div><h4>事件抓拍 · {{ item.image_path ? '已保存' : evidenceLabel(item.evidence?.snapshot_state) }}</h4><a v-if="item.image_path && !imageFailed" :href="api.eventImage(item.image_path)" target="_blank" rel="noopener"><img :src="api.eventImage(item.image_path)" :alt="`${deviceLabel(item)} ${eventTitle(item)}现场抓拍`" @error="imageFailed = true"></a><p v-else class="evidence-empty"><Camera />{{ imageFailed ? '图片不可用或已过期' : item.evidence?.snapshot_error || '本事件没有抓拍图片；可为后续报警启用自动抓拍。' }}</p></div><div><h4>报警后视频 · {{ item.video_path ? '已保存' : evidenceLabel(item.evidence?.video_state) }}</h4><video v-if="item.video_path && !videoFailed" :src="api.eventVideo(item.id)" controls preload="none" aria-label="报警后视频片段" @error="videoFailed = true"/><p v-else class="evidence-empty">{{ videoFailed ? '视频不可用或浏览器不支持，请下载查看' : item.evidence?.video_error || '本事件没有视频片段；可为后续报警启用平台录制。' }}</p><a v-if="item.video_path" class="btn btn-sm" :href="api.eventVideo(item.id, true)" download>下载视频</a></div></div>
     <p v-if="item.evidence?.started_at" class="section-note">取证任务开始于 {{ formatDate(item.evidence.started_at) }}<template v-if="item.evidence.finished_at">，结束于 {{ formatDate(item.evidence.finished_at) }}</template>。</p>
    </section>
    <section class="event-process-section"><h3>{{ closed ? '重新打开事件' : '处理事件' }}</h3><form @submit.prevent="submit"><div class="process-fields"><label class="form-group"><span class="form-label">处理结果</span><select v-model="nextStatus" class="select" :disabled="saving"><option v-if="closed" value="pending">重新打开 · 待处理</option><template v-else><option value="processing">开始处理</option><option v-if="item.status === 'processing'" value="pending">退回待处理</option><option value="resolved">处理完成</option><option value="ignored">忽略事件</option></template></select></label><label class="form-group"><span class="form-label">处理说明 {{ nextStatus === 'processing' ? '（选填）' : '（必填）' }}</span><textarea v-model="note" class="input" rows="3" maxlength="1000" :required="nextStatus !== 'processing'" :disabled="saving" placeholder="记录核实情况、处置结果或忽略原因" /></label></div><div v-if="actionError" class="warning-box" role="alert">{{ actionError }}</div><div class="button-row"><button class="btn btn-primary" :disabled="saving"><LoaderCircle v-if="saving" class="animate-spin" />{{ saving ? '正在保存…' : '保存处理结果' }}</button><span v-if="success" class="success-message" role="status">{{ success }}</span></div></form></section>
    <section class="event-history"><h3>处理记录</h3><p v-if="!actions.length" class="section-note">尚未人工处理；报警接收与自动取证不会更改处理状态。</p><ol v-else><li v-for="action in actions" :key="action.id"><div><strong>{{ action.from_status === action.to_status ? '补充处理说明' : `${statusLabels[action.from_status]} → ${statusLabels[action.to_status]}` }}</strong><span>{{ action.actor }} · {{ formatDate(action.created_at) }}</span></div><p v-if="action.note">{{ action.note }}</p></li></ol><p v-if="actions.length === 100" class="section-note">展示最近 100 条处理记录。</p></section>
    <details class="raw-event"><summary>原始事件数据</summary><pre>{{ item.zones || '无附加数据' }}</pre></details>
   </template>
  </div>
 </ModalDialog>
</template>
<style scoped>
.event-dialog-content { padding: 20px; }
.event-detail-heading, .event-resource-actions, .event-detail-heading > div { display: flex; gap: 12px; align-items: center; flex-wrap: wrap; }
.event-detail-heading > div { display: block; flex: 1; }
h3 { font-size: 14px; font-weight: 700; margin: 0 0 12px; }
.event-detail-heading h3 { font-size: 18px; margin-bottom: 4px; }
p { margin: 6px 0; }
.event-detail-heading p, dt { color: var(--muted); font-size: 12px; }
.event-facts { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; padding: 18px 0; margin: 0; }
.event-facts > div:last-child { grid-column: 1 / -1; }
dd { margin: 4px 0 0; overflow-wrap: anywhere; }
dd small { display: block; color: var(--muted); font-size: 12px; margin-top: 4px; }
.evidence-section, .event-process-section, .event-history, .raw-event { padding-top: 20px; margin-top: 20px; border-top: 1px solid var(--line); }
.event-evidence-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 20px; }
h4 { font-size: 13px; margin: 12px 0; }
img, video { width: 100%; max-height: 300px; object-fit: contain; background: var(--panel-3); border-radius: var(--radius-sm); }
.evidence-empty { display: flex; align-items: center; gap: 8px; min-height: 100px; color: var(--muted); background: var(--panel-2); padding: 16px; font-size: 12px; }
.evidence-empty svg { width: 20px; flex-shrink: 0; }
.process-fields { display: grid; grid-template-columns: 180px 1fr; gap: 16px; margin-bottom: 14px; }
textarea.input { height: auto; padding: 10px; resize: vertical; min-height: 84px; }
.success-message { color: var(--green); }
ol { list-style: none; padding: 0; margin: 0; }
li { padding: 12px 0; border-bottom: 1px solid var(--line); font-size: 13px; }
li > div { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 8px; }
li span { color: var(--muted); font-size: 12px; }
li p { white-space: pre-wrap; overflow-wrap: anywhere; }
summary { cursor: pointer; color: var(--muted); font-size: 13px; }
pre { padding: 12px; background: var(--panel-2); white-space: pre-wrap; overflow-wrap: anywhere; font-size: 12px; max-height: 240px; overflow: auto; }
@media(max-width: 640px) { .event-dialog-content { padding: 14px; } .event-facts, .event-evidence-grid, .process-fields { grid-template-columns: 1fr; } }
</style>
