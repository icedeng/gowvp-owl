<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { Activity, ArrowLeft, Layers, Network, Radio, RefreshCw, Server, ShieldAlert, Video } from '@lucide/vue';
import { api, errorMessage } from '../services/api';
import type { MediaInspectionResource, MediaInspectionResult, MediaServer } from '../types/api';
import { formatDate } from '../utils/format';
import { inspectionRows, inspectionText, inspectionValue, summarizeStreams, trackSummary, type InspectionColumn, type InspectionRow } from '../utils/mediaInspection';
import MediaInspectionTable from '../components/MediaInspectionTable.vue';

const route = useRoute();
const router = useRouter();
const node = ref<MediaServer | null>(null);
const nodeLoading = ref(false);
const nodeError = ref('');
const results = reactive<Partial<Record<MediaInspectionResource, MediaInspectionResult>>>({});
const pending = reactive<Partial<Record<MediaInspectionResource, boolean>>>({});
const autoRefresh = ref(false);
const selection = ref<InspectionRow | null>(null);
const selectedResults = reactive<Partial<Record<MediaInspectionResource, MediaInspectionResult>>>({});
const selectionLoading = ref(false);
const selectionPanel = ref<HTMLElement | null>(null);
let selectionTrigger: HTMLElement | null = null;
let generation = 0;
let selectionGeneration = 0;
let refreshTimer: ReturnType<typeof setInterval> | undefined;
const tabs = [
  { id: 'overview', label: '概览', icon: Server }, { id: 'streams', label: '媒体流', icon: Video },
  { id: 'sessions', label: '网络会话', icon: Network }, { id: 'rtp', label: 'RTP 服务', icon: Radio },
  { id: 'proxies', label: '代理任务', icon: Layers }, { id: 'diagnostics', label: '运行诊断', icon: Activity },
] as const;
type Tab = typeof tabs[number]['id'];
const activeTab = computed<Tab>(() => tabs.some((tab) => tab.id === route.query.tab) ? route.query.tab as Tab : 'overview');
const groups: Record<Tab, MediaInspectionResource[]> = {
  overview: ['version', 'streams', 'sessions', 'rtp_servers'], streams: ['streams'], sessions: ['sessions'], rtp: ['rtp_servers'],
  proxies: ['pull_proxies', 'push_proxies', 'ffmpeg'], diagnostics: ['threads', 'work_threads', 'statistics', 'apis'],
};
const refreshing = computed(() => nodeLoading.value || groups[activeTab.value].some((key) => pending[key]));
const summary = computed(() => summarizeStreams(results.streams));
const version = computed(() => results.version?.status === 'ok' && results.version.data && !Array.isArray(results.version.data) ? results.version.data as InspectionRow : {});
const overviewSignals = computed(() => [
  { label: '媒体源', value: summary.value?.sources, note: summary.value ? `${summary.value.outputs} 个协议输出` : '按虚拟主机、应用和流 ID 去重', resource: 'streams' },
  { label: '观看连接', value: summary.value?.readers, note: '同一媒体源跨协议不重复累计', resource: 'streams' },
  { label: '网络会话', value: results.sessions?.status === 'ok' ? inspectionRows(results.sessions).length : null, note: '来自节点实时连接列表', resource: 'sessions' },
  { label: 'RTP 收流服务', value: results.rtp_servers?.status === 'ok' ? inspectionRows(results.rtp_servers).length : null, note: '当前打开的接收端口', resource: 'rtp_servers' },
]);
const overviewErrors = computed(() => groups.overview.filter((key) => results[key] && results[key]?.status !== 'ok').map((key) => ({ key, message: results[key]?.message })));
const protocolCounts = computed(() => {
  const counts = new Map<string, number>();
  for (const row of inspectionRows(results.streams)) counts.set(String(row.schema || '未知'), (counts.get(String(row.schema || '未知')) || 0) + 1);
  return [...counts.entries()].map(([name, count]) => ({ name, count }));
});
const streamColumns: InspectionColumn[] = [
  { key: 'app', label: '应用' }, { key: 'stream', label: '流 ID' }, { key: 'schema', label: '协议' },
  { key: 'bytesSpeed', label: '码率', kind: 'bitrate' }, { key: 'readerCount', label: '本协议观看数' },
  { key: 'totalReaderCount', label: '源总观看数' }, { key: 'tracks', label: '媒体轨道', kind: 'tracks' },
  { key: 'isRecordingMP4', label: 'MP4 录像', kind: 'boolean' }, { key: 'aliveSecond', label: '存活时长', kind: 'duration' },
  { key: 'originTypeStr', label: '来源类型' },
];
const sessionColumns: InspectionColumn[] = [
  { key: 'id', label: '连接 ID' }, { key: 'local_ip', label: '本机 IP' }, { key: 'local_port', label: '本机端口' },
  { key: 'peer_ip', label: '对端 IP' }, { key: 'peer_port', label: '对端端口' }, { key: 'type', label: '传输协议' }, { key: 'typeid', label: '会话类型' },
];
const playerColumns: InspectionColumn[] = [
  { key: 'identifier', label: '连接标识' }, { key: 'id', label: '会话 ID' },
  { key: 'local_ip', label: '本机 IP' }, { key: 'local_port', label: '本机端口' },
  { key: 'peer_ip', label: '对端 IP' }, { key: 'peer_port', label: '对端端口' },
];
const streamFacts: InspectionColumn[] = [
  { key: 'schema', label: '协议' }, { key: 'vhost', label: '虚拟主机' },
  { key: 'app', label: '应用' }, { key: 'stream', label: '流 ID' },
  { key: 'originTypeStr', label: '来源类型' }, { key: 'originUrl', label: '来源地址' },
  { key: 'bytesSpeed', label: '当前码率', kind: 'bitrate' }, { key: 'totalBytes', label: '累计流量', kind: 'bytes' },
  { key: 'readerCount', label: '本协议观看数' }, { key: 'totalReaderCount', label: '源总观看数' },
  { key: 'createStamp', label: '创建时间', kind: 'time' }, { key: 'aliveSecond', label: '存活时长', kind: 'duration' },
  { key: 'isRecordingMP4', label: 'MP4 录像', kind: 'boolean' }, { key: 'isRecordingHLS', label: 'HLS 录制', kind: 'boolean' },
  { key: 'originSock.peer_ip', label: '来源 IP' }, { key: 'originSock.peer_port', label: '来源端口' },
];
const rtpFacts: InspectionColumn[] = [
  { key: 'peer_ip', label: '设备发流 IP' }, { key: 'peer_port', label: '设备发流端口' },
  { key: 'local_ip', label: '本机接收 IP' }, { key: 'local_port', label: '本机接收端口' },
  { key: 'identifier', label: '连接标识' },
];
function resultObject(result?: MediaInspectionResult): InspectionRow {
  return result?.data !== null && typeof result?.data === 'object' && !Array.isArray(result.data) ? result.data as InspectionRow : {};
}
function resultTracks(result?: MediaInspectionResult): InspectionRow[] {
  const tracks = resultObject(result).tracks;
  return Array.isArray(tracks) ? tracks.filter((track) => track !== null && typeof track === 'object') : [];
}
const rtpColumns: InspectionColumn[] = [
  { key: 'stream_id', label: '流 ID' }, { key: 'app', label: '应用' }, { key: 'vhost', label: '虚拟主机' },
  { key: 'port', label: '监听端口' }, { key: 'ssrc', label: 'SSRC' }, { key: 'tcp_mode', label: '传输模式', kind: 'tcpMode' }, { key: 'only_track', label: '轨道过滤', kind: 'trackFilter' },
];
const proxyColumns: InspectionColumn[] = [
  { key: 'src.app', label: '应用' }, { key: 'src.stream', label: '流 ID' }, { key: 'url', label: '代理地址' },
  { key: 'status', label: '运行状态', kind: 'proxyStatus' }, { key: 'bytesSpeed', label: '速率', kind: 'rate' },
  { key: 'liveSecs', label: '运行时长', kind: 'duration' }, { key: 'key', label: '任务标识' },
];
const pullColumns: InspectionColumn[] = [...proxyColumns, { key: 'status_str', label: '状态说明' }, { key: 'rePullCount', label: '重新拉流次数' }, { key: 'totalReaderCount', label: '观看数' }];
const pushColumns: InspectionColumn[] = [...proxyColumns, { key: 'rePublishCount', label: '重新推流次数' }];
const ffmpegColumns: InspectionColumn[] = [{ key: 'src_url', label: '源地址' }, { key: 'dst_url', label: '目的地址' }, { key: 'ffmpeg_cmd_key', label: '命令模板' }, { key: 'key', label: '任务标识' }];
const threadColumns: InspectionColumn[] = [{ key: 'name', label: '线程名称' }, { key: 'load', label: '线程负载', kind: 'load' }, { key: 'delay', label: '调度延迟', kind: 'delay' }, { key: 'fd_count', label: '文件描述符数' }];
const proxyTables = [
  { resource: 'pull_proxies' as const, title: '拉流代理', description: '节点主动拉取的 RTSP / RTMP 等源，以及连接状态、速率与重试次数。', columns: pullColumns },
  { resource: 'push_proxies' as const, title: '推流代理', description: '节点向其他媒体服务器转推的运行任务。', columns: pushColumns },
  { resource: 'ffmpeg' as const, title: 'FFmpeg 任务', description: '通过节点 API 创建的 FFmpeg 拉流或转推任务；源地址的认证信息已隐藏。', columns: ffmpegColumns },
];
const diagnosisTables = [
  { resource: 'threads' as const, title: '网络线程', description: '事件循环线程负载与调度延迟；负载并非服务器 CPU 使用率。', columns: threadColumns },
  { resource: 'work_threads' as const, title: '后台工作线程', description: '后台任务线程的实时负载与调度延迟，字段随节点版本变化。', columns: threadColumns },
  { resource: 'statistics' as const, title: '对象与资源统计', description: 'ZLM 当前持有的会话、套接字、帧、缓冲区等对象计数；并非宿主机磁盘或系统资源指标。', columns: [{ key: 'name', label: '统计项' }, { key: 'value', label: '当前值' }] },
  { resource: 'apis' as const, title: '节点 API 清单', description: '节点实际提供的 API 路径，仅用于能力核对；此列表不会执行接口。', columns: [{ key: 'value', label: 'API 路径' }] },
];

function chooseTab(tab: Tab) {
  void router.replace({ query: { ...route.query, tab } });
}
function tabKey(event: KeyboardEvent, index: number) {
  let next = index;
  if (event.key === 'ArrowRight') next = (index + 1) % tabs.length;
  else if (event.key === 'ArrowLeft') next = (index + tabs.length - 1) % tabs.length;
  else if (event.key === 'Home') next = 0;
  else if (event.key === 'End') next = tabs.length - 1;
  else return;
  event.preventDefault();
  chooseTab(tabs[next]!.id);
  document.getElementById(`media-tab-${tabs[next]!.id}`)?.focus();
}
async function loadResource(resource: MediaInspectionResource, force = false) {
  if (pending[resource] || (!force && results[resource])) return;
  const current = generation;
  const id = String(route.params.id);
  pending[resource] = true;
  try {
    const response = await api.inspectMediaServer(id, resource);
    if (current === generation) results[resource] = response.data;
  } catch (cause) {
    if (current === generation) results[resource] = { resource, status: 'unavailable', message: errorMessage(cause, '节点查询失败，请重试'), data: null, fetched_at: new Date().toISOString() };
  } finally { if (current === generation) pending[resource] = false; }
}
async function loadNode() {
  const current = generation;
  nodeLoading.value = true;
  nodeError.value = '';
  try {
    const response = await api.mediaServer(String(route.params.id));
    if (current === generation) node.value = response.data;
  } catch (cause) {
    if (current === generation) nodeError.value = errorMessage(cause, '节点信息读取失败');
  } finally { if (current === generation) nodeLoading.value = false; }
}
async function refresh() {
  if (refreshing.value) return;
  await Promise.allSettled([loadNode(), ...groups[activeTab.value].map((key) => loadResource(key, true))]);
}
async function inspectRow(row: InspectionRow) {
  const opening = selection.value !== row;
  if (opening) selectionTrigger = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  selection.value = row;
  const current = ++selectionGeneration;
  const nodeGeneration = generation;
  for (const key of Object.keys(selectedResults) as MediaInspectionResource[]) delete selectedResults[key];
  selectionLoading.value = true;
  if (opening) {
    await nextTick();
    if (current !== selectionGeneration || nodeGeneration !== generation) return;
    selectionPanel.value?.focus({ preventScroll: true });
    selectionPanel.value?.scrollIntoView({ block: 'start', behavior: 'instant' });
  }
  const resources: MediaInspectionResource[] = activeTab.value === 'rtp' ? ['rtp_info'] : ['stream_info', 'stream_players'];
  const params = Object.fromEntries(['schema', 'vhost', 'app', 'stream', 'stream_id'].filter((key) => row[key] !== undefined).map((key) => [key, String(row[key])])) ;
  await Promise.allSettled(resources.map(async (resource) => {
    let result: MediaInspectionResult;
    try { result = (await api.inspectMediaServer(String(route.params.id), resource, params)).data; }
    catch (cause) { result = { resource, status: 'unavailable', message: errorMessage(cause, '实时详情读取失败'), data: null, fetched_at: new Date().toISOString() }; }
    if (current === selectionGeneration && nodeGeneration === generation) selectedResults[resource] = result;
  }));
  if (current === selectionGeneration && nodeGeneration === generation) selectionLoading.value = false;
}
function closeSelection(restoreFocus = false) {
  selectionGeneration++;
  selection.value = null;
  selectionLoading.value = false;
  if (restoreFocus && selectionTrigger?.isConnected) selectionTrigger.focus();
  selectionTrigger = null;
}
function initialize() {
  generation++;
  closeSelection();
  node.value = null;
  nodeLoading.value = false;
  for (const key of Object.keys(results) as MediaInspectionResource[]) delete results[key];
  for (const key of Object.keys(pending) as MediaInspectionResource[]) delete pending[key];
  void refresh();
}
watch(() => route.params.id, initialize);
watch(activeTab, () => { closeSelection(); for (const resource of groups[activeTab.value]) void loadResource(resource); });
onMounted(() => {
  initialize();
  refreshTimer = setInterval(() => { if (autoRefresh.value && !document.hidden && !refreshing.value) void refresh(); }, 15000);
});
onBeforeUnmount(() => { generation++; selectionGeneration++; clearInterval(refreshTimer); });
</script>

<template>
  <main class="page-content media-node-detail">
    <header class="page-head">
      <div class="node-page-title"><RouterLink class="page-back-btn" to="/media-servers" aria-label="返回媒体节点列表" title="返回媒体节点列表"><ArrowLeft aria-hidden="true" /></RouterLink><div><h1 class="page-title">{{ node?.id || route.params.id }}</h1><p class="page-desc">媒体节点运行详情</p></div></div>
      <div class="head-actions"><RouterLink class="btn" :to="{ path: '/media-servers', query: { edit: String(route.params.id) } }">管理节点</RouterLink><label class="node-auto"><input v-model="autoRefresh" type="checkbox" />每 15 秒刷新当前分区</label><button class="btn btn-primary" type="button" :disabled="refreshing" @click="refresh"><RefreshCw :class="{ 'animate-spin': refreshing }" aria-hidden="true" />{{ refreshing ? '正在刷新…' : '刷新当前分区' }}</button></div>
    </header>
    <div v-if="nodeError" class="warning-box mb-4" role="alert"><ShieldAlert aria-hidden="true" /><span>{{ nodeError }}</span><button class="btn btn-sm" @click="loadNode">重试</button></div>
    <div class="node-identity"><span class="status" :class="node ? node.status ? 'online' : 'offline' : ''">{{ node ? node.disabled ? '节点已停用' : node.status ? '节点在线' : '节点离线' : '正在读取' }}</span><span>{{ node?.type || '—' }}</span><span class="node-address">{{ node?.ip || '—' }}:{{ node?.ports?.http || '—' }}</span><span>最近心跳 {{ formatDate(node?.last_keepalive_at) }}</span><span class="node-readonly">只读巡检</span></div>
    <nav class="node-tabs" role="tablist" aria-label="媒体节点详情分区"><button v-for="(tab, index) in tabs" :id="`media-tab-${tab.id}`" :key="tab.id" role="tab" type="button" :aria-selected="activeTab === tab.id" :aria-controls="`media-panel-${tab.id}`" :tabindex="activeTab === tab.id ? 0 : -1" :class="{ active: activeTab === tab.id }" @click="chooseTab(tab.id)" @keydown="tabKey($event, index)"><component :is="tab.icon" aria-hidden="true" />{{ tab.label }}</button></nav>
    <section :id="`media-panel-${activeTab}`" class="node-panel" role="tabpanel" :aria-labelledby="`media-tab-${activeTab}`" tabindex="0">
      <template v-if="activeTab === 'overview'">
        <div class="node-signal-grid"><div v-for="signal in overviewSignals" :key="signal.label"><span>{{ signal.label }}</span><strong>{{ pending[signal.resource as MediaInspectionResource] && !results[signal.resource as MediaInspectionResource] ? '…' : signal.value ?? '—' }}</strong><small>{{ signal.note }}</small></div></div>
        <div v-if="overviewErrors.length" class="node-query-notes"><p v-for="error in overviewErrors" :key="error.key"><ShieldAlert aria-hidden="true" />{{ error.message }}</p></div>
        <div class="node-overview-grid">
          <section><h2>节点档案</h2><dl class="node-facts"><div><dt>节点标识</dt><dd>{{ node?.id || '—' }}</dd></div><div><dt>驱动</dt><dd>{{ node?.type || 'zlm' }}</dd></div><div><dt>API 地址</dt><dd>{{ node?.ip || '—' }}:{{ node?.ports?.http || '—' }}</dd></div><div><dt>Hook 地址</dt><dd>{{ node?.hook_ip || '—' }}</dd></div><div><dt>SDP 地址</dt><dd>{{ node?.sdp_ip || '—' }}</dd></div><div><dt>播放地址</dt><dd>{{ node?.stream_ip || node?.ip || '—' }}</dd></div><div><dt>RTP 端口范围</dt><dd>{{ node?.rtpport_range || '—' }}</dd></div><div><dt>版本提交</dt><dd>{{ version.commitHash || '—' }}</dd></div><div><dt>构建分支</dt><dd>{{ version.branchName || '—' }}</dd></div><div><dt>构建时间</dt><dd>{{ version.buildTime || '—' }}</dd></div></dl><details v-if="node?.ports" class="node-disclosure"><summary>查看全部服务端口</summary><dl class="node-facts"><div v-for="(port, name) in node.ports" :key="name"><dt>{{ name }}</dt><dd>{{ port || '未启用' }}</dd></div></dl></details></section>
          <section><h2>协议输出</h2><p class="node-section-note">同一媒体源可以产生多种协议输出，以下按输出条目计数。</p><div v-if="results.streams?.status === 'ok' && protocolCounts.length" class="node-protocol-list"><div v-for="item in protocolCounts" :key="item.name"><span>{{ item.name.toUpperCase() }}</span><strong>{{ item.count }} 路</strong></div></div><p v-else class="node-section-note">{{ results.streams?.status === 'ok' ? '当前没有在线媒体流。' : '媒体流数据尚未就绪。' }}</p><button class="btn" type="button" @click="chooseTab('streams')">查看媒体流</button><h2 class="node-secondary-title">录像状态</h2><p class="node-section-note">{{ summary ? `${summary.recording} 路媒体源正在进行 MP4 录像。` : '尚未取得节点录像状态。' }}录像片段与回放请前往录像中心。</p><RouterLink class="btn" to="/recordings">打开录像中心</RouterLink></section>
        </div>
      </template>
      <MediaInspectionTable v-else-if="activeTab === 'streams'" title="媒体流" description="完整展示节点上的协议输出；展开全部字段可核对来源地址、创建时间、累计流量及轨道信息。实时详情会额外查询单路流与观看连接。" :result="results.streams" :loading="pending.streams" :columns="streamColumns" filter-key="schema" filter-label="协议" inspectable @refresh="loadResource('streams', true)" @inspect="inspectRow" />
      <MediaInspectionTable v-else-if="activeTab === 'sessions'" title="网络会话" description="来自节点的实时连接列表，可搜索源 IP、端口、连接 ID 或会话类型。本版仅查看，不会断开连接。" :result="results.sessions" :loading="pending.sessions" :columns="sessionColumns" filter-key="typeid" filter-label="会话类型" @refresh="loadResource('sessions', true)" />
      <MediaInspectionTable v-else-if="activeTab === 'rtp'" title="RTP 收流服务" description="查看当前打开的接收端口、SSRC、传输模式及轨道过滤；实时详情查询设备发流地址与接收状态。这里不是端口段容量或 RTP 发送会话列表。" :result="results.rtp_servers" :loading="pending.rtp_servers" :columns="rtpColumns" inspectable @refresh="loadResource('rtp_servers', true)" @inspect="inspectRow" />
      <div v-else-if="activeTab === 'proxies'" class="node-section-stack"><MediaInspectionTable v-for="table in proxyTables" :key="table.resource" :title="table.title" :description="table.description" :result="results[table.resource]" :loading="pending[table.resource]" :columns="table.columns" @refresh="loadResource(table.resource, true)" /></div>
      <div v-else class="node-section-stack"><MediaInspectionTable v-for="table in diagnosisTables" :key="table.resource" :title="table.title" :description="table.description" :result="results[table.resource]" :loading="pending[table.resource]" :columns="table.columns" @refresh="loadResource(table.resource, true)" /></div>

      <section v-if="selection" ref="selectionPanel" class="node-selected" tabindex="-1" aria-label="实时查询详情" :aria-busy="selectionLoading">
        <header class="inspection-selection-head"><div><h2>{{ selection.app || 'rtp' }} / {{ selection.stream || selection.stream_id }} · 实时详情</h2><p class="node-section-note">该区域保留选中条目的独立查询结果，不随自动刷新更新。</p></div><div><button class="btn" type="button" :disabled="selectionLoading" @click="inspectRow(selection!)">刷新详情</button><button class="btn" type="button" @click="closeSelection(true)">收起</button></div></header>
        <p v-if="selectionLoading" role="status">正在读取实时详情…</p>
        <template v-else>
          <section v-for="(result, resource) in selectedResults" :key="resource" class="node-detail-result">
            <MediaInspectionTable v-if="resource === 'stream_players'" title="观看连接" description="选中协议输出上的观看连接；源总观看数可能包含其他协议输出。" :result="result" :columns="playerColumns" :loading="selectionLoading" @refresh="inspectRow(selection!)" />
            <template v-else>
              <h3>{{ resource === 'rtp_info' ? 'RTP 接收信息' : '流媒体信息' }}</h3>
              <p v-if="result?.status !== 'ok'" class="node-section-note" role="status">{{ result?.message }}</p>
              <template v-else-if="result">
                <p v-if="resource === 'rtp_info'" class="node-receive-state" :class="{ receiving: resultObject(result).exist === true }">{{ resultObject(result).exist === true ? '已检测到 RTP 接收流' : resultObject(result).exist === false ? '未检测到 RTP 接收流：请检查设备发流、目标地址和端口。' : '节点未返回 RTP 接收状态' }}</p>
                <dl class="node-facts"><div v-for="fact in resource === 'rtp_info' ? rtpFacts : streamFacts" :key="fact.key"><dt>{{ fact.label }}</dt><dd>{{ inspectionText(inspectionValue(resultObject(result), fact.key), fact.kind) }}</dd></div></dl>
                <ul v-if="resultTracks(result).length" class="node-track-list"><li v-for="(track, index) in resultTracks(result)" :key="index"><strong>{{ trackSummary([track]) }}</strong><span>丢包率 {{ track.loss === undefined || track.loss === null || Number(track.loss) < 0 ? '未知' : `${track.loss}%` }}</span><span v-if="track.gop_size !== undefined">GOP {{ track.gop_size }} 帧</span></li></ul>
                <details class="node-disclosure"><summary>查看全部字段</summary><pre>{{ JSON.stringify(result.data, null, 2) }}</pre></details>
                <p class="node-section-note">采集于 {{ formatDate(result.fetched_at) }}</p>
              </template>
            </template>
          </section>
        </template>
      </section>
    </section>
  </main>
</template>

<style scoped>
.node-page-title { display: flex; align-items: center; gap: 12px; }.node-page-title .page-desc { margin-top: 3px; }
.node-auto { display: inline-flex; align-items: center; gap: 7px; color: var(--muted); font-size: 12px; cursor: pointer; }.node-auto input { accent-color: var(--blue); }
.node-identity { display: flex; flex-wrap: wrap; align-items: center; gap: 10px 18px; margin: 0 0 16px; color: var(--muted); font-size: 12px; }.node-address { font-variant-numeric: tabular-nums; }.node-readonly { margin-left: auto; }
.node-tabs { display: flex; overflow: auto; background: var(--panel); border: 1px solid var(--line); border-radius: var(--radius) var(--radius) 0 0; }
.node-tabs button { display: inline-flex; align-items: center; justify-content: center; gap: 7px; min-height: 46px; flex: 0 0 auto; padding: 0 18px; border: 0; border-bottom: 2px solid transparent; background: transparent; font-size: 13px; color: var(--muted); cursor: pointer; }
.node-tabs button svg { width: 15px; height: 15px; }.node-tabs button:hover { color: var(--blue); background: var(--panel-2); }.node-tabs button.active { color: var(--blue); border-bottom-color: var(--blue); background: var(--blue-soft); }
.node-panel { min-width: 0; min-height: 340px; padding: 16px; border: 1px solid var(--line); border-top: 0; background: var(--panel); border-radius: 0 0 var(--radius) var(--radius); }
.node-signal-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); padding: 2px 0 18px; border-bottom: 1px solid var(--line); }
.node-signal-grid > div { display: grid; gap: 8px; padding: 0 18px; border-right: 1px solid var(--line); }.node-signal-grid > div:first-child { padding-left: 0; }.node-signal-grid > div:last-child { border-right: 0; }
.node-signal-grid span, .node-signal-grid small { color: var(--muted); font-size: 12px; }.node-signal-grid strong { font-size: 24px; line-height: 1.2; font-weight: 500; font-variant-numeric: tabular-nums; }
.node-overview-grid { display: grid; grid-template-columns: 3fr 2fr; gap: 32px; margin-top: 22px; }.node-overview-grid h2, .node-selected h2 { margin: 0 0 12px; font-size: 14px; font-weight: 600; }
.node-facts { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0 24px; margin: 0; }.node-facts > div { display: grid; grid-template-columns: 86px minmax(0, 1fr); gap: 8px; padding: 10px 0; border-bottom: 1px solid var(--line); font-size: 12px; line-height: 1.7; }.node-facts dt { color: var(--muted); }.node-facts dd { margin: 0; overflow-wrap: anywhere; }
.node-section-note { color: var(--muted); font-size: 12px; line-height: 1.7; margin: 5px 0 12px; }.node-protocol-list { margin: 12px 0; }.node-protocol-list > div { display: flex; justify-content: space-between; gap: 16px; padding: 10px 0; border-bottom: 1px solid var(--line); font-size: 13px; }.node-protocol-list strong { font-weight: 500; }
.node-overview-grid .node-secondary-title { margin-top: 28px; }.node-disclosure { margin-top: 16px; }.node-disclosure summary { color: var(--blue); font-size: 12px; cursor: pointer; }
.node-query-notes { padding: 10px 0 0; }.node-query-notes p { display: flex; align-items: center; gap: 7px; margin: 6px 0; font-size: 12px; color: var(--amber); }.node-query-notes svg { width: 14px; height: 14px; flex-shrink: 0; }
.node-section-stack { display: grid; gap: 28px; }.node-section-stack > :deep(section + section) { padding-top: 24px; border-top: 1px solid var(--line); }
.node-selected { margin-top: 24px; padding-top: 24px; border-top: 1px solid var(--line); }.inspection-selection-head { display: flex; justify-content: space-between; gap: 16px; }.inspection-selection-head > div:last-child { display: flex; align-items: flex-start; gap: 8px; }
.node-detail-result { margin-top: 18px; }.node-detail-result h3 { margin: 0 0 8px; font-size: 13px; font-weight: 500; }.node-detail-result pre { max-height: 360px; overflow: auto; padding: 14px; margin: 0; background: var(--panel-2); border: 1px solid var(--line); border-radius: var(--radius-sm); font: 12px/1.7 monospace; white-space: pre-wrap; overflow-wrap: anywhere; }
.node-selected { scroll-margin-top: 80px; }.node-detail-result .node-disclosure pre { margin-top: 10px; }.node-track-list { list-style: none; padding: 0; margin: 14px 0; }.node-track-list li { display: flex; flex-wrap: wrap; gap: 8px 18px; padding: 10px 0; border-bottom: 1px solid var(--line); font-size: 12px; }.node-track-list strong { font-weight: 500; }.node-track-list span { color: var(--muted); }.node-receive-state { color: var(--amber); font-size: 13px; line-height: 1.7; }.node-receive-state.receiving { color: var(--green); }
@media (max-width: 1100px) { .node-overview-grid { grid-template-columns: 1fr; gap: 24px; }.node-signal-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 20px; }.node-signal-grid > div { padding: 0; border: 0; } }
@media (max-width: 640px) { .node-panel { padding: 12px; }.node-readonly { margin-left: 0; }.node-facts { grid-template-columns: 1fr; }.node-tabs button { min-height: 44px; padding: 0 14px; }.head-actions { align-items: stretch; }.node-auto { min-height: 44px; }.head-actions .btn { min-height: 44px; }.inspection-selection-head { flex-direction: column; }.node-disclosure summary { min-height: 44px; padding-top: 10px; } }
</style>
