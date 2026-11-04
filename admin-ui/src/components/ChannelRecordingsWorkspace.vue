<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue';
import { CalendarDays, ChevronLeft, ChevronRight, Cloud, Download, HardDrive, LoaderCircle, Play, RefreshCw, Save, Search, ShieldAlert } from '@lucide/vue';
import { api, errorMessage } from '../services/api';
import type { ApiChannel, ApiRecording, DeviceRecordQueryOutput } from '../types/api';
import { formatBytes, formatDate } from '../utils/format';
import { calendarCells, localDateInput, recordingDateRange } from '../utils/recordings';
import ModalDialog from './ModalDialog.vue';
import StreamPlayer from './StreamPlayer.vue';

const props = defineProps<{ channel: ApiChannel; isGb: boolean; recordMode: string; busy: boolean; historyActive: boolean }>();
const emit = defineEmits<{ 'update:recordMode': [value: string]; save: []; 'play-device': [range: { start: number; end: number }] }>();
const source = ref<'cloud' | 'device'>('cloud');
const cloudDate = ref('');
const cloudPage = ref(1);
const cloudItems = ref<ApiRecording[]>([]);
const cloudTotal = ref(0);
const cloudLoading = ref(false);
const cloudError = ref('');
const today = localDateInput(new Date());
const month = ref(today.slice(0, 7));
const selectedDay = ref(today);
const remoteResult = ref<DeviceRecordQueryOutput | null>(null);
const remoteLoading = ref(false);
const remoteError = ref('');
const remoteScope = ref('');
const remotePage = ref(1);
const pageSize = 15;
const remoteQueried = ref(false);
let cloudSequence = 0, remoteSequence = 0, playerSequence = 0;
const activeRecord = ref<ApiRecording | null>(null);
const mediaUrl = ref('');
const mediaError = ref('');
const mediaLoading = ref(false);
const cloudPlayer = ref<InstanceType<typeof StreamPlayer>>();
const cells = computed(() => calendarCells(month.value));
const daysWithRecord = computed(() => new Set((remoteResult.value?.list || []).filter(day => day.items?.length).map(day => day.date)));
const dayRanges = computed(() => (remoteResult.value?.list || []).find(day => day.date === selectedDay.value)?.items || []);
const visibleRanges = computed(() => dayRanges.value.slice((remotePage.value - 1) * pageSize, remotePage.value * pageSize));
const remoteUnavailable = computed(() => !props.isGb ? '当前协议不支持设备本地录像查询。平台云端录像仍可使用。' : !props.channel.is_online ? '设备离线，暂时无法读取本地目录或发起回放。' : '');
const savedMode = computed(() => props.channel.ext?.record_mode || 'none');
const modeChanged = computed(() => props.recordMode !== savedMode.value);
const sourceOptions = [{ id: 'cloud' as const, label: '平台云端录像', icon: Cloud }, { id: 'device' as const, label: '设备本地录像', icon: HardDrive }];

async function setSource(value: 'cloud' | 'device') {
  source.value = value;
  if (value === 'device' && !remoteQueried.value && !remoteUnavailable.value) await queryRemote('month');
}
function sourceKeydown(event: KeyboardEvent) {
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
  event.preventDefault();
  const target = event.key === 'Home' ? 'cloud' : event.key === 'End' ? 'device' : source.value === 'cloud' ? 'device' : 'cloud';
  void setSource(target);
  document.getElementById(`record-source-${target}`)?.focus();
}
async function loadCloud() {
  const sequence = ++cloudSequence;
  cloudLoading.value = true; cloudError.value = '';
  try {
    const range = cloudDate.value ? recordingDateRange(cloudDate.value) : undefined;
    const { data } = await api.recordings({ cid: props.channel.id, page: cloudPage.value, size: pageSize, ...(range || {}) });
    if (sequence !== cloudSequence) return;
    cloudItems.value = data?.items || []; cloudTotal.value = Number(data?.total || 0);
  } catch (cause) {
    if (sequence === cloudSequence) cloudError.value = errorMessage(cause, '云端录像读取失败，请重试。');
  } finally { if (sequence === cloudSequence) cloudLoading.value = false; }
}
function changeCloudDate() { cloudPage.value = 1; void loadCloud(); }
function cloudPaging(delta: number) { cloudPage.value += delta; void loadCloud(); }
async function queryRemote(scope: 'month' | 'day') {
  if (remoteUnavailable.value || remoteLoading.value) return;
  const value = scope === 'month' ? month.value : selectedDay.value;
  if (!(scope === 'month' ? /^\d{4}-\d{2}$/ : /^\d{4}-\d{2}-\d{2}$/).test(value) || value > (scope === 'month' ? today.slice(0, 7) : today)) { remoteError.value = '请选择有效日期。'; return; }
  const range = recordingDateRange(value, scope === 'month');
  const sequence = ++remoteSequence;
  remoteLoading.value = true; remoteError.value = ''; remoteQueried.value = true; remotePage.value = 1;
  try {
    const { data } = await api.queryDeviceRecords(props.channel.id, { start_at: Math.floor(range.start_ms / 1000), end_at: Math.min(Math.floor(range.end_ms / 1000), Math.floor(Date.now() / 1000)), timeout: 15, type: 'all' });
    if (sequence !== remoteSequence) return;
    remoteResult.value = data; remoteScope.value = scope === 'month' ? `${value} 整月` : value;
    const available = (data?.list || []).filter(day => day.items?.length).map(day => day.date).sort();
    if (scope === 'month' && available.length && !available.includes(selectedDay.value)) selectedDay.value = available[available.length - 1];
  } catch (cause) {
    if (sequence === remoteSequence) { remoteResult.value = null; remoteScope.value = ''; remoteError.value = errorMessage(cause, '设备录像查询失败，请确认设备在线后重试。'); }
  } finally { if (sequence === remoteSequence) remoteLoading.value = false; }
}
function changeMonth() {
  if (!/^\d{4}-\d{2}$/.test(month.value)) month.value = today.slice(0, 7);
  remoteSequence += 1; remoteResult.value = null; remoteScope.value = ''; remoteError.value = ''; remoteQueried.value = false;
  selectedDay.value = month.value === today.slice(0, 7) ? today : `${month.value}-01`; remotePage.value = 1;
}
function moveMonth(delta: number) {
  const date = new Date(`${month.value}-01T00:00:00`); date.setMonth(date.getMonth() + delta);
  month.value = localDateInput(date).slice(0, 7); changeMonth(); void queryRemote('month');
}
function selectDay(day: string) { selectedDay.value = day; remotePage.value = 1; if (!remoteScope.value.endsWith('整月')) void queryRemote('day'); }
function timeLabel(value: number) { return new Date(value * 1000).toLocaleTimeString('zh-CN', { hour12: false }); }
function durationLabel(start: number, end: number) { const seconds = Math.max(0, end - start); return seconds < 60 ? `${seconds} 秒` : seconds < 3600 ? `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒` : `${Math.floor(seconds / 3600)} 小时 ${Math.floor(seconds % 3600 / 60)} 分`; }
function closeCloudPlayer() { playerSequence += 1; void cloudPlayer.value?.destroy(); activeRecord.value = null; mediaUrl.value = ''; mediaError.value = ''; mediaLoading.value = false; }
async function playCloud(record: ApiRecording) {
  const sequence = ++playerSequence;
  void cloudPlayer.value?.destroy(); activeRecord.value = record; mediaUrl.value = ''; mediaError.value = ''; mediaLoading.value = true;
  try {
    if (!record.path) throw new Error('该录像没有可读取的文件路径，请刷新列表或选择其他录像。');
    await api.checkRecordingMedia(record.path);
    if (sequence === playerSequence) { mediaUrl.value = api.recordingMediaUrl(record.path); mediaLoading.value = false; }
  } catch (cause) { if (sequence === playerSequence) { mediaError.value = errorMessage(cause, '录像文件无法读取，可能已清理。请刷新列表或选择其他录像。'); mediaLoading.value = false; } }
}
onBeforeUnmount(() => { cloudSequence += 1; remoteSequence += 1; closeCloudPlayer(); });
void loadCloud();
</script>

<template>
  <div class="channel-recordings-workspace">
    <div class="record-source-switch" role="tablist" aria-label="录像存储位置">
      <button v-for="item in sourceOptions" :id="`record-source-${item.id}`" :key="item.id" type="button" role="tab" :aria-selected="source === item.id" :aria-controls="`record-source-panel-${item.id}`" :tabindex="source === item.id ? 0 : -1" :class="{ active: source === item.id }" @click="setSource(item.id)" @keydown="sourceKeydown"><component :is="item.icon" aria-hidden="true" />{{ item.label }}</button>
    </div>

    <div v-show="source === 'cloud'" id="record-source-panel-cloud" role="tabpanel" aria-labelledby="record-source-cloud" class="record-source-panel">
      <div class="record-source-heading"><div><h2>平台云端录像</h2><p>保存在平台服务器上的录像，可直接回看或下载。</p></div><RouterLink class="btn" :to="`/recordings?channel=${encodeURIComponent(channel.id)}`">进入录像中心</RouterLink></div>
      <div class="cloud-recording-policy">
        <div><strong>云端录制</strong><span class="status" :class="savedMode === 'always' ? 'online' : ''">{{ savedMode === 'always' ? '持续录制中' : savedMode === 'ai' ? 'AI 触发模式' : '未开启' }}</span><p>手动开启后持续录制，关闭页面不会停止；停止后保留已有录像。</p></div>
        <div class="cloud-policy-actions"><label class="sr-only" for="cloud-record-mode">云端录像开关</label><select id="cloud-record-mode" class="select" :value="recordMode" :disabled="busy" @change="emit('update:recordMode', ($event.target as HTMLSelectElement).value)"><option value="none">不录制</option><option value="always">持续录制</option><option v-if="savedMode === 'ai'" value="ai" disabled>AI 触发（暂不可配置）</option></select><button class="btn" :class="{ 'btn-primary': modeChanged }" type="button" :disabled="busy || !modeChanged" @click="emit('save')"><LoaderCircle v-if="busy" class="animate-spin" /><Save v-else />保存设置</button></div>
      </div>
      <form class="record-list-toolbar" @submit.prevent="changeCloudDate"><label class="record-date-field"><span>录像日期</span><input v-model="cloudDate" class="input plain" type="date" :max="today" @change="changeCloudDate" /></label><button v-if="cloudDate" class="btn" type="button" @click="cloudDate = ''; changeCloudDate()">全部日期</button><span class="record-list-summary">{{ cloudDate || '全部日期 · 最新在前' }}<span v-if="!cloudError"> · 共 {{ cloudTotal }} 段</span></span><button class="btn" type="button" :disabled="cloudLoading" @click="loadCloud"><RefreshCw :class="{ 'animate-spin': cloudLoading }" />刷新</button></form>
      <div v-if="cloudError" class="inline-resource-error" role="alert"><span>{{ cloudError }}</span><button class="btn btn-sm" @click="loadCloud">重试</button></div>
      <div v-else-if="cloudLoading" class="empty-state" role="status"><LoaderCircle class="animate-spin" />正在读取云端录像…</div>
      <div v-else-if="!cloudItems.length" class="empty-state"><Cloud /><strong>{{ cloudDate ? '当天没有云端录像' : '暂无云端录像' }}</strong><p>{{ cloudDate ? '可切换日期或查看全部日期。' : '开启云端录制后，完成的录像片段会显示在这里。设备存储中的录像请切换到「设备本地录像」。' }}</p></div>
      <div v-else class="table-wrap"><table class="data-table channel-records-table"><thead><tr><th>开始时间</th><th>结束时间</th><th>时长</th><th>大小</th><th class="text-right">操作</th></tr></thead><tbody><tr v-for="record in cloudItems" :key="record.id"><td data-label="开始时间">{{ formatDate(record.started_at) }}</td><td data-label="结束时间">{{ formatDate(record.ended_at) }}</td><td data-label="时长">{{ record.duration ? durationLabel(0, Math.round(record.duration)) : '—' }}</td><td data-label="大小">{{ formatBytes(record.size) }}</td><td data-label="操作"><div class="row-actions"><button class="btn btn-sm" :disabled="record.delete_flag || !record.path" @click="playCloud(record)"><Play />播放</button><a class="device-row-action" :href="api.recordingDownloadUrl(record.id)" download :aria-label="`下载 ${formatDate(record.started_at)} 的云端录像`" title="下载录像"><Download /></a></div></td></tr></tbody></table></div>
      <div v-if="cloudTotal > pageSize" class="record-list-pagination"><span>第 {{ cloudPage }} / {{ Math.ceil(cloudTotal / pageSize) }} 页</span><button class="btn btn-sm" :disabled="cloudPage <= 1 || cloudLoading" @click="cloudPaging(-1)">上一页</button><button class="btn btn-sm" :disabled="cloudPage * pageSize >= cloudTotal || cloudLoading" @click="cloudPaging(1)">下一页</button></div>
    </div>

    <div v-show="source === 'device'" id="record-source-panel-device" role="tabpanel" aria-labelledby="record-source-device" class="record-source-panel">
      <div class="record-source-heading"><div><h2>设备本地录像</h2><p>读取摄像机或录像机中的录像目录，选择时段后通知设备推流回放。</p></div></div>
      <div v-if="remoteUnavailable" class="warning-box" role="status"><ShieldAlert /><span>{{ remoteUnavailable }}</span></div>
      <div class="device-record-browser">
        <aside class="device-record-calendar">
          <div class="record-calendar-month"><button class="icon-btn" aria-label="上个月" :disabled="remoteLoading" @click="moveMonth(-1)"><ChevronLeft /></button><label class="sr-only" for="device-record-month">录像月份</label><input id="device-record-month" v-model="month" type="month" class="input plain" :max="today.slice(0, 7)" :disabled="remoteLoading" @change="changeMonth" /><button class="icon-btn" aria-label="下个月" :disabled="remoteLoading || month >= today.slice(0, 7)" @click="moveMonth(1)"><ChevronRight /></button></div>
          <button class="btn record-month-query" :disabled="Boolean(remoteUnavailable) || remoteLoading" @click="queryRemote('month')"><LoaderCircle v-if="remoteLoading" class="animate-spin" /><CalendarDays v-else />{{ remoteLoading ? '正在读取设备目录…' : '查询整月录像' }}</button>
          <div class="record-calendar-week" aria-hidden="true"><span v-for="day in ['一','二','三','四','五','六','日']" :key="day">{{ day }}</span></div>
          <div class="record-calendar-grid"><template v-for="(day, index) in cells" :key="day || index"><button v-if="day" :class="{ selected: day === selectedDay, 'has-record': daysWithRecord.has(day) }" :aria-pressed="day === selectedDay" :aria-label="`${day}${daysWithRecord.has(day) ? '，有录像' : '，未确认有录像'}`" :disabled="day > today || remoteLoading" @click="selectDay(day)">{{ Number(day.slice(-2)) }}<span v-if="daysWithRecord.has(day)" class="record-day-dot" /></button><span v-else /></template></div>
          <p class="record-calendar-help"><span class="record-day-dot" />有录像日期 · 查询后显示<br />未标记日期可单独查询确认。</p>
        </aside>
        <section class="device-record-results" aria-label="设备录像时段">
          <form class="record-list-toolbar" @submit.prevent="queryRemote('day')"><label class="record-date-field"><span>录像日期</span><input v-model="selectedDay" class="input plain" type="date" :max="today" :disabled="remoteLoading" @change="month = selectedDay.slice(0, 7); remotePage = 1" /></label><button class="btn btn-primary" :disabled="Boolean(remoteUnavailable) || remoteLoading || !selectedDay"><Search />查询当天</button></form>
          <div v-if="remoteError" class="inline-resource-error" role="alert"><span>{{ remoteError }}</span><button class="btn btn-sm" :disabled="Boolean(remoteUnavailable) || remoteLoading" @click="queryRemote('day')">重试</button></div>
          <div v-if="remoteLoading" class="empty-state" role="status"><LoaderCircle class="animate-spin" />正在等待设备返回录像目录…</div>
          <template v-else-if="remoteResult">
            <p class="record-query-summary" aria-live="polite">{{ selectedDay }} · {{ dayRanges.length }} 个录像时段<span>已查询：{{ remoteScope }}</span></p>
            <div v-if="remoteResult.incomplete" class="warning-box" role="status"><ShieldAlert /><span>设备仅返回部分目录（{{ remoteResult.incomplete.received_count ?? '—' }} / {{ remoteResult.incomplete.expected_count ?? '—' }}），当前标记和时段可能不完整，请查询当天或重试。</span></div>
            <div v-if="!dayRanges.length" class="empty-state"><HardDrive /><strong>{{ remoteScope === selectedDay || remoteScope === `${month} 整月` ? '设备未返回当天录像' : '尚未查询此日期' }}</strong><p>可选择有标记的日期，或点击「查询当天」再次向设备读取。</p></div>
            <div v-else class="table-wrap"><table class="data-table channel-records-table"><thead><tr><th>开始时间</th><th>结束时间</th><th>时长</th><th class="text-right">操作</th></tr></thead><tbody><tr v-for="range in visibleRanges" :key="`${range.start}-${range.end}`"><td data-label="开始时间">{{ timeLabel(range.start) }}</td><td data-label="结束时间">{{ timeLabel(range.end) }}</td><td data-label="时长">{{ durationLabel(range.start, range.end) }}</td><td data-label="操作"><div class="row-actions"><button class="btn btn-sm" :disabled="Boolean(remoteUnavailable) || busy || historyActive" @click="emit('play-device', range)"><Play />回放</button></div></td></tr></tbody></table></div>
            <div v-if="dayRanges.length > pageSize" class="record-list-pagination"><span>第 {{ remotePage }} / {{ Math.ceil(dayRanges.length / pageSize) }} 页</span><button class="btn btn-sm" :disabled="remotePage <= 1" @click="remotePage--">上一页</button><button class="btn btn-sm" :disabled="remotePage * pageSize >= dayRanges.length" @click="remotePage++">下一页</button></div>
          </template>
          <div v-else-if="!remoteError" class="empty-state"><CalendarDays /><strong>选择月份，发现有录像的日期</strong><p>查询整月目录后，月历会标记有录像的日期；也可直接按天查询。</p></div>
          <p v-if="historyActive" class="capability-reason">当前有设备历史会话正在运行，请先停止会话后再选择其他时段。</p>
        </section>
      </div>
      <details class="device-record-advanced"><summary>下载与高级回放<span>自定义时间范围、下载传输与会话状态</span></summary><div class="device-record-advanced-body"><slot name="advanced" /></div></details>
    </div>

    <ModalDialog :open="Boolean(activeRecord)" :title="`${channel.name || channel.id} · 云端录像回放`" :description="activeRecord ? formatDate(activeRecord.started_at) : ''" size="wide" scroll-body @close="closeCloudPlayer">
      <div class="media-player-stage"><StreamPlayer v-if="activeRecord" ref="cloudPlayer" :src="mediaUrl" :duration="activeRecord.duration || 0" :loading="mediaLoading" :error="mediaError" loading-text="正在读取云端录像…" @error="message => mediaError = message" @retry="playCloud(activeRecord)" /></div>
      <div class="media-player-foot"><p>支持暂停、拖动进度、倍速和全屏。</p><a v-if="activeRecord" class="btn btn-sm" :href="api.recordingDownloadUrl(activeRecord.id)" download><Download aria-hidden="true" />下载录像</a></div>
    </ModalDialog>
  </div>
</template>
