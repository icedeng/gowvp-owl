<script setup lang="ts">
import { createRequestKey } from '../requestKey'
import { computed, reactive, ref, shallowRef, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { ArrowLeft, RefreshCw, Settings2, PlugZap } from '@lucide/vue'
import { formatBytes, formatDate } from '../../../utils/format'
import GatewayState from '../components/GatewayState.vue'
import ConfigSummary from '../components/ConfigSummary.vue'
import { stateLabel, metricLabels, probeLabels } from '../presentation'
import { gatewayAPI } from '../api'
import type { Device, Event, Gateway, MetricSeries, Migration, Operation, Page, Session } from '../types'
import GatewayAccessInfo from '../components/GatewayAccessInfo.vue'
import { errorMessage } from '../../../services/api'
import ModalDialog from '../../../components/ModalDialog.vue'
import TrendChart from '../../../components/TrendChart.vue'
import GatewayEditor from '../components/GatewayEditor.vue'
import OperationStatus from '../components/OperationStatus.vue'
import { useVisiblePoll } from '../composables/useVisiblePoll'
const route = useRoute(); const router = useRouter(); const id = computed(() => String(route.params.id))
const loading = ref(false)
const gateway = shallowRef<Gateway>(); const error = ref(''); const tabError = ref(''); const busy = ref(false); const editor = ref(false)
const tabs = [
  ['metrics', '运行监控'], ['overview', '国标接入'], ['devices', '设备与迁移'], ['platforms', '级联平台'],
  ['sessions', '媒体与任务'], ['config', '配置与能力'], ['deployment', '部署与运维'], ['events', '事件审计'],
]
const tab = computed({ get: () => tabs.some(t => t[0] === route.query.tab) ? String(route.query.tab) : 'metrics', set: value => { void router.replace({ query: { ...route.query, tab: value === 'metrics' ? undefined : value } }) } }); const page = ref(1); const hours = ref(1); const metric = ref('media_sessions')
const devices = shallowRef<Page<Device>>(); const sessions = shallowRef<Page<Session>>(); const events = shallowRef<Page<Event>>(); const platforms = shallowRef<Page<Record<string, unknown>>>(); const metrics = shallowRef<MetricSeries>()
const operationID = ref(''); const selected = shallowRef(new Map<string, Device>()); const targets = shallowRef<Gateway[]>([]); const target = ref(''); const migration = shallowRef<Migration>(); const migrationID = ref(''); const migrationPage = ref(1)
const filters = reactive({ q: '', state: '', online: '', purpose: '', subject_id: '', type: '', start: '', end: '' })
function queryFilters() {
 const keys = tab.value === 'devices' ? ['q','online'] : tab.value === 'platforms' ? ['q','state'] : tab.value === 'sessions' ? ['purpose','state','subject_id'] : tab.value === 'events' ? ['type','subject_id','start','end'] : []
 return Object.fromEntries(keys.flatMap(key => { const value = filters[key as keyof typeof filters].trim(); return value ? [[key,value]] : [] }))
}
function applyFilters() { page.value = 1; void refresh() }
const template = ref(''); const warnings = shallowRef<string[]>([]); const templateFormat = ref('toml')
const action = shallowRef<{ label: string; kind: string; session?: string }>(); const reason = ref(''); const force = ref(false)
let pendingKey = ''; let pendingBody = ''
function keyFor(body: unknown) { const value = JSON.stringify(body); if (value !== pendingBody) { pendingBody = value; pendingKey = createRequestKey() } return pendingKey }
const currentPage = computed(() => tab.value === 'devices' ? devices.value : tab.value === 'sessions' ? sessions.value : tab.value === 'events' ? events.value : platforms.value)
const chartValues = computed(() => metrics.value?.points.map(p => p.valid && p.values ? p.values[metric.value] ?? null : null) || [])
const chartLabels = computed(() => metrics.value?.points.map(p => new Date(p.sampled_at).toLocaleTimeString()) || [])
const diagnosis = shallowRef<Awaited<ReturnType<typeof gatewayAPI.probe>>>()
const capabilities: Record<string, string> = { sip_udp: 'SIP / UDP', sip_tcp: 'SIP / TCP', rtp_udp: 'RTP / UDP', rtp_tcp_passive: 'RTP / TCP 被动', rtp_tcp_active: 'RTP / TCP 主动', rtcp_udp: 'RTCP / UDP', rtcp_tcp_mux: 'RTCP / TCP 复用', playback: '录像回放', rtp_download: 'RTP 录像下载', raw_tcp_download: 'TCP 文件下载', voice_duplex: '双向对讲', broadcast: '语音广播', cascade: '平台级联', https_transfer: 'HTTPS 文件传输' }
function metricNumber(key: string) {
  const value = gateway.value?.metrics[key]
  return value == null || value === '' || !Number.isFinite(Number(value)) || Number(value) < 0 ? undefined : Number(value)
}
function numberText(key: string, unit = '', digits = 0) {
  const value = metricNumber(key)
  return value === undefined ? '—' : `${value.toLocaleString('zh-CN', { maximumFractionDigits: digits })}${unit}`
}
function bytesText(key: string) { return formatBytes(metricNumber(key)) }
function bitrateText(key: string) {
  const value = metricNumber(key)
  if (value === undefined) return '—'
  const index = value >= 1e9 ? 3 : value >= 1e6 ? 2 : value >= 1e3 ? 1 : 0
  return `${(value / 1000 ** index).toLocaleString('zh-CN', { maximumFractionDigits: 2 })} ${['bps', 'Kbps', 'Mbps', 'Gbps'][index]}`
}
const runtimeFacts = computed(() => [
  ['到 A 端带宽', bitrateText('to_a_bps')], ['到设备带宽', bitrateText('to_device_bps')],
  ['往返时延', numberText('rtt_ms', ' ms', 1)], ['媒体端口占用', `${numberText('port_used')} / ${numberText('port_total')}`],
  ['CPU 使用率', numberText('cpu_percent', '%', 2)], ['内存占用', bytesText('rss_bytes')],
  ['发送队列', bytesText('queue_bytes')], ['设备侧累计收 / 发', `${bytesText('device_rx_bytes')} / ${bytesText('device_tx_bytes')}`],
])
const resourceFields: Record<string, [string, string][]> = {
  devices: [['device_total', '设备总数'], ['device_online', '在线设备'], ['channel_total', '通道']],
  platforms: [['platform_bindings', '级联平台']],
  sessions: [['active_sources', '活动媒体源'], ['voice_sessions', '语音会话'], ['transfer_sessions', '文件任务'], ['cleanup_pending', '待清理资源']],
}
const resourceSummary = computed(() => (resourceFields[tab.value] || []).map(([key, label]) => ({ label, value: gateway.value?.counts?.[key] ?? '—' })))
const { refresh } = useVisiblePoll(async signal => {
  loading.value = true
  try { const result = await gatewayAPI.get(id.value, signal); if (signal.aborted) return; gateway.value = result; error.value = ''; if (!operationID.value && gateway.value.pending_operation_id) operationID.value = gateway.value.pending_operation_id }
  catch (e) { if (!signal.aborted) error.value = errorMessage(e) }
  if (signal.aborted) return
  try {
    if (tab.value === 'devices') devices.value = await gatewayAPI.devices(id.value, page.value, signal, queryFilters())
    if (tab.value === 'sessions') sessions.value = await gatewayAPI.sessions(id.value, page.value, signal, queryFilters())
    if (tab.value === 'platforms') platforms.value = await gatewayAPI.platforms(id.value, page.value, signal, queryFilters())
    if (tab.value === 'events') events.value = await gatewayAPI.events(id.value, page.value, signal, queryFilters())
    if (tab.value === 'metrics') metrics.value = await gatewayAPI.metrics(id.value, hours.value, signal)
    if (migrationID.value) migration.value = await gatewayAPI.migration(migrationID.value, migrationPage.value, signal)
    tabError.value = ''
  } catch (e) { if (!signal.aborted) tabError.value = errorMessage(e) }
  finally { if (!signal.aborted) loading.value = false }
})
watch([tab, hours], () => { page.value = 1; void refresh() }); watch([page, migrationPage], () => void refresh())
watch(id, () => { gateway.value = undefined; devices.value = undefined; sessions.value = undefined; events.value = undefined; platforms.value = undefined; metrics.value = undefined; diagnosis.value = undefined; template.value = ''; warnings.value = []; selected.value = new Map(); operationID.value = ''; migrationID.value = ''; void refresh() })
function saved(result: Gateway | Operation) { if ('operation_id' in result) operationID.value = result.operation_id; void refresh() }
function select(d: Device) { const next = new Map(selected.value); if (next.has(d.device_id)) next.delete(d.device_id); else next.set(d.device_id, d); selected.value = next }
async function run(fn: () => Promise<void>) { if (busy.value) return; busy.value = true; error.value = ''; try { await fn() } catch (e) { error.value = errorMessage(e) } finally { busy.value = false } }
async function confirmAction() { await run(async () => {
  const g = gateway.value; const a = action.value; if (!g || !a) return
  let result: Operation | undefined
  if (a.kind === 'delete') { await gatewayAPI.remove(g, keyFor({ kind: 'delete', revision: g.revision })); await router.push({ name: 'access-gateways' }) }
  else if (a.kind === 'stop') result = await gatewayAPI.action(g, `sessions/${a.session}/stop`, { reason: reason.value }, keyFor({ kind: 'stop', session: a.session, reason: reason.value }))
  else if (a.kind === 'reconnect') result = await gatewayAPI.action(g, 'reconnect', { reason: reason.value }, keyFor({ kind: a.kind, reason: reason.value }))
  else result = await gatewayAPI.action(g, 'state', { state: a.kind, force: force.value, drain_timeout_ms: 30000 }, keyFor({ kind: a.kind, force: force.value, revision: g.revision }))
  if (result) operationID.value = result.operation_id
  action.value = undefined; await refresh()
}) }
async function diagnose() { await run(async () => { diagnosis.value = await gatewayAPI.probe(id.value, createRequestKey()); tab.value = 'deployment' }) }
async function deployTemplate() { await run(async () => { const result = await gatewayAPI.template(id.value, templateFormat.value); template.value = result.content; warnings.value = result.warnings }) }
async function loadTargets() { await run(async () => {
 const items: Gateway[] = []
 for (let index = 1; ; index++) {
   const result = await gatewayAPI.list({ admin_state: 'enabled', health_state: 'ready', size: 100, page: index })
   items.push(...result.items.filter(g => g.id !== id.value))
   if (index * result.size >= result.total) break
 }
 targets.value = items
}) }
async function migrate() { await run(async () => {
  if (!selected.value.size) throw new Error('请先选择迁移设备')
  const list = [...selected.value.values()]; const op = await gatewayAPI.migrate(id.value, target.value || null, list, keyFor({ kind: 'migration', target: target.value, list }))
  const result = op.result as { migration_id?: string } | null
  if (result?.migration_id) migrationID.value = result.migration_id
  operationID.value = op.operation_id; selected.value = new Map(); await refresh()
}) }
async function cancelMigration() { await run(async () => { if (!migration.value) return; await gatewayAPI.cancelMigration(migration.value, '管理员取消迁移', keyFor({ kind: 'cancel-migration', id: migration.value.id, revision: migration.value.revision })); await refresh() }) }
function openAction(kind: string, label: string, session?: string) { error.value = ''; action.value = { kind, label, session }; reason.value = ''; force.value = false }
</script>
<template>
<main class="page-content gateway-detail"><header class="device-command-header">
  <div class="device-command-identity">
    <RouterLink class="page-back-btn" :to="{ name: 'access-gateways' }" aria-label="返回接入网关列表" title="返回接入网关"><ArrowLeft aria-hidden="true" /></RouterLink>
    <div><div class="device-command-title"><h1>{{ gateway?.name || id }}</h1><GatewayState v-if="gateway" :value="gateway.admin_state" /></div><div class="device-command-meta"><span class="mono">{{ id }}</span><span>{{ gateway?.group || '未分组' }}</span></div><p v-if="gateway?.description" class="gateway-description">{{ gateway.description }}</p></div>
  </div>
  <div class="head-actions"><button class="btn" :disabled="busy || loading" @click="refresh"><RefreshCw :class="{ 'animate-spin': loading }" aria-hidden="true" />{{ loading ? '正在刷新…' : '刷新' }}</button><button class="btn" :disabled="busy || !gateway" @click="diagnose"><PlugZap aria-hidden="true" />连接诊断</button><button class="btn btn-primary" :disabled="busy || !gateway" @click="editor = true"><Settings2 aria-hidden="true" />配置与证书</button></div>
  <div v-if="gateway" class="gateway-header-status"><div><span>控制连接</span><GatewayState :value="gateway.connection_state" /></div><div><span>健康状态</span><GatewayState :value="gateway.health_state" /></div><div><span>配置版本</span><strong class="mono">{{ gateway.applied_revision }} / {{ gateway.config_revision }}</strong><small :class="{ 'gateway-warning': gateway.config_revision !== gateway.applied_revision }">{{ gateway.config_revision !== gateway.applied_revision ? '待应用' : '已同步' }}</small></div><span class="state-updated">更新于 {{ formatDate(gateway.sampled_at) }}<small v-if="gateway.stale" class="gateway-warning">状态已过期，等待最新上报</small></span></div>
</header>
<p v-if="error" role="alert" class="warning-box">{{ error }}</p><OperationStatus v-if="operationID" :key="operationID" :gateway-id="id" :operation-id="operationID" @finished="refresh" />
<div v-if="!gateway" class="card empty-state">{{ error ? '网关信息暂不可用，请重试。' : '正在读取网关信息…' }}</div>
<template v-if="gateway">
  <p v-if="gateway.last_error" class="warning-box">{{ gateway.last_error.code }}：{{ gateway.last_error.message }}</p>
  <div class="gateway-workspace"><nav class="channel-detail-tabs gateway-tabs" aria-label="网关详情"><button v-for="t in tabs" :key="t[0]" :aria-current="tab === t[0] ? 'page' : undefined" :class="{ active: tab === t[0] }" @click="tab = t[0]!">{{ t[1] }}</button></nav>
  <p v-if="tabError" role="alert" class="warning-box">{{ tabError }}。{{ currentPage || metrics ? '下方保留上次成功读取的数据。' : '请刷新重试。' }}</p>
<template v-if="resourceSummary.length">
  <dl class="resource-summary" aria-label="网关资源统计"><div v-for="item in resourceSummary" :key="item.label"><dt>{{ item.label }}</dt><dd>{{ item.value }}</dd></div></dl>
  <p v-if="gateway.counts_error" class="warning-box resource-warning" role="alert">资源统计暂不可用：{{ gateway.counts_error.message }}</p>
</template>
<form v-if="['devices','platforms','sessions','events'].includes(tab)" class="actions gateway-filters" @submit.prevent="applyFilters">
<input v-if="['devices','platforms'].includes(tab)" v-model="filters.q" class="input" placeholder="编码 / 名称" aria-label="搜索对象" />
<select v-if="tab === 'devices'" v-model="filters.online" class="select" aria-label="设备状态"><option value="">全部状态</option><option value="true">在线</option><option value="false">离线</option></select>
<select v-if="tab === 'sessions'" v-model="filters.purpose" class="select" aria-label="会话用途"><option value="">全部用途</option><option v-for="purpose in ['live', 'playback', 'download', 'talk', 'broadcast']" :key="purpose" :value="purpose">{{ stateLabel(purpose) }}</option></select>
<input v-if="['sessions','platforms'].includes(tab)" v-model="filters.state" class="input" :placeholder="tab === 'platforms' ? '状态：online / offline' : '状态：receiving / closing'" aria-label="资源状态" />
<input v-if="['sessions','events'].includes(tab)" v-model="filters.subject_id" class="input" placeholder="对象内部 ID" aria-label="对象 ID" />
<template v-if="tab === 'events'"><input v-model="filters.type" class="input" placeholder="事件类型" aria-label="事件类型" /><input v-model="filters.start" class="input" placeholder="开始时间（RFC3339）" aria-label="开始时间" /><input v-model="filters.end" class="input" placeholder="结束时间（RFC3339）" aria-label="结束时间" /></template>
<button class="btn" type="submit">应用筛选</button></form>
<section v-if="tab === 'overview'" class="content access-content">
  <GatewayAccessInfo :key="id" :gateway="gateway" />
</section>
<section v-else-if="tab === 'config'" class="content">
  <header class="section-head"><h2 class="section-title">实际应用配置</h2><span class="section-note">已应用版本 {{ gateway.applied_revision }}</span></header>
  <ConfigSummary :config="gateway.applied_config" />
  <details class="gateway-disclosure" :open="gateway.config_revision !== gateway.applied_revision">
    <summary>期望配置 · 版本 {{ gateway.config_revision }}<span>{{ gateway.config_revision !== gateway.applied_revision ? '待应用' : '已同步' }}</span></summary>
    <div class="disclosure-content"><ConfigSummary :config="gateway.desired_config" /></div>
  </details>
  <details class="gateway-disclosure">
    <summary>协议能力<span>已协商 {{ gateway.capabilities.length }} 项</span></summary>
    <div class="disclosure-content"><ul v-if="gateway.capabilities.length" class="capability-list"><li v-for="capability in gateway.capabilities" :key="capability" :title="capability">{{ capabilities[capability] || capability }}</li></ul><p v-else class="section-note">尚未确认协议能力，请检查网关连接。</p></div>
  </details>
</section>
<section v-else-if="tab === 'devices'" class="content"><div class="actions"><span>已选 {{ selected.size }} 个设备</span><button class="btn" :disabled="busy" @click="loadTargets">加载可用目标</button><select v-model="target" class="select" aria-label="迁移目标"><option value="">迁移为直连</option><option v-for="g in targets" :key="g.id" :value="g.id">{{ g.name }}（{{ g.id }}）</option></select><button class="btn" :disabled="busy || !selected.size" @click="migrate">创建一小时迁移计划</button></div><p>迁移先排空原有业务，目标需重新认证注册。设备地址须由运维同步调整。</p><table><thead><tr><th>选择</th><th>设备</th><th>状态</th><th>绑定版本</th><th>通道 / 在线</th></tr></thead><tbody><tr v-for="d in devices?.items" :key="d.device_id"><td><input type="checkbox" :checked="selected.has(d.device_id)" :aria-label="`选择 ${d.name || d.device_code}`" @change="select(d)" /></td><td><RouterLink :to="{name:'device-detail',params:{id:d.device_id}}">{{ d.name || d.device_code }}</RouterLink><small>{{ d.device_code }}</small></td><td><GatewayState :value="d.online ? 'online' : 'offline'" /></td><td>{{ d.binding_revision }}</td><td>{{ d.channel_total }} / {{ d.channel_online }}</td></tr><tr v-if="devices && !devices.items.length"><td colspan="5" class="empty-state">暂无符合条件的记录，请调整筛选条件。</td></tr></tbody></table><div v-if="migration"><h3>迁移计划 {{ migration.id }} · {{ stateLabel(migration.state) }}</h3><p>{{ migration.counts }}</p><ul><li v-for="item in migration.items" :key="item.subject_id">{{ item.subject_id }} · {{ stateLabel(item.state) }} · {{ item.error?.message }}</li></ul><div class="actions"><button class="btn" :disabled="migrationPage === 1" @click="migrationPage--">上一批</button><button class="btn" :disabled="migration.items.length < 20" @click="migrationPage++">下一批</button><button class="btn" :disabled="busy || ['completed','expired','cancelled'].includes(migration.state)" @click="cancelMigration">取消剩余迁移</button></div></div><label>查看已有迁移计划<input v-model="migrationID" class="input" placeholder="计划 ID" @change="refresh" /></label></section>
<section v-else-if="tab === 'platforms'" class="content"><h2 class="section-title">平台绑定</h2><table><thead><tr><th>平台 ID</th><th>角色</th><th>入口</th><th>版本</th></tr></thead><tbody><tr v-for="p in platforms?.items" :key="String(p.id)"><td>{{ p.subject_id }}</td><td>{{ p.role }}</td><td>{{ stateLabel(p.ingress_type) }} · {{ stateLabel(p.state) }}</td><td>{{ p.binding_revision }}</td></tr><tr v-if="platforms && !platforms.items.length"><td colspan="4" class="empty-state">暂无符合条件的记录，请调整筛选条件。</td></tr></tbody></table></section>
<section v-else-if="tab === 'sessions'" class="content"><header class="section-head"><h2 class="section-title">媒体与任务会话</h2><span class="section-note">媒体回流：{{ gateway.counts?.active_sources ? '已有活动源，播放状态以媒体节点为准' : gateway.counts ? '暂无活动源，尚未验证播放' : '资源统计不可用' }}</span></header><table><thead><tr><th>对象 / 会话</th><th>用途 / 状态</th><th>传输 / 方向</th><th>端点</th><th>流量采样（设备侧）</th><th>清理状态</th><th>操作</th></tr></thead><tbody><tr v-for="s in sessions?.items" :key="s.id"><td>{{ s.subject_id }}<small>{{ s.id }}</small></td><td>{{ stateLabel(s.purpose) }} / {{ stateLabel(s.state) }}<small>{{ s.error?.message }}</small></td><td>{{ s.wire_mode }} → {{ s.local_mode }}<small>{{ s.direction }} · SSRC {{ s.ssrc ?? '—' }}</small></td><td><details><summary>查看</summary><pre>公网 {{ s.public_endpoint }}
本地 {{ s.local_endpoint }}
媒体节点 {{ s.media_server_id ?? '—' }}
流 {{ s.stream_id ?? '—' }}</pre><small>{{ new Date(s.created_at).toLocaleString() }}</small></details></td><td><template v-if="s.traffic">收 / 发 {{ s.traffic.rx_bytes }} / {{ s.traffic.tx_bytes }} B<small>{{ s.traffic.rx_bps ?? '—' }} / {{ s.traffic.tx_bps ?? '—' }} bps · 排队 {{ s.traffic.queue_bytes }} B</small><small>{{ new Date(s.traffic.sampled_at).toLocaleString() }}{{ Date.now() - Date.parse(s.traffic.sampled_at) > 45000 ? '（最后采样）' : '' }}</small></template><span v-else>暂无采样</span></td><td><details><summary>查看</summary><pre>{{ s.cleanup_steps }}</pre></details></td><td><button class="btn" :disabled="busy || s.state === 'closed'" @click="openAction('stop','停止会话',s.id)">停止</button></td></tr><tr v-if="sessions && !sessions.items.length"><td colspan="7" class="empty-state">暂无符合条件的记录，请调整筛选条件。</td></tr></tbody></table></section>
<section v-else-if="tab === 'metrics'" class="content">
  <header class="section-head"><h2 class="section-title">实时运行指标</h2><span class="section-note">{{ gateway.metrics_stale ? '指标已过期，保留最后采样' : '随网关状态定时刷新' }}</span></header>
  <dl class="runtime-facts"><div v-for="fact in runtimeFacts" :key="fact[0]"><dt>{{ fact[0] }}</dt><dd>{{ fact[1] }}</dd></div></dl>
  <section class="tab-section"><header class="section-head"><h2 class="section-title">指标趋势</h2></header>
  <div class="actions"><select v-model="hours" class="select" aria-label="时间范围"><option :value="1">最近一小时</option><option :value="24">最近一天</option><option :value="168">最近一周</option></select><select v-model="metric" class="select" aria-label="指标"><option v-for="m in ['media_sessions','transfer_sessions','sip_flows','port_used','cpu_percent','rss_bytes','to_a_bps','to_device_bps','device_online','active_sources','queue_bytes','rtt_ms']" :key="m" :value="m">{{ metricLabels[m] || m }}</option></select><span>{{ metrics?.stale ? '指标已过期' : '缺失样本显示为空' }}</span></div><TrendChart :values="chartValues" :labels="chartLabels" :aria-label="`网关 ${metricLabels[metric] || metric} 趋势`" />
  </section>
</section>
<section v-else-if="tab === 'events'" class="content"><table><thead><tr><th>时间</th><th>类型</th><th>操作人</th><th>对象 / 原因</th><th>跟踪 ID</th></tr></thead><tbody><tr v-for="e in events?.items" :key="e.event_id"><td>{{ new Date(e.occurred_at).toLocaleString() }}</td><td>{{ e.type }}</td><td>{{ e.actor }}</td><td>{{ e.subject_id }} {{ e.reason }}<details><summary>详情</summary><pre>{{ e.details }}</pre></details></td><td>{{ e.trace_id }}</td></tr><tr v-if="events && !events.items.length"><td colspan="5" class="empty-state">暂无符合条件的记录，请调整筛选条件。</td></tr></tbody></table></section>
<section v-else-if="tab === 'deployment'" class="content"><section v-if="diagnosis" class="diagnosis-section"><header class="section-head"><h2 class="section-title">连接诊断结果</h2><GatewayState :value="diagnosis.can_enable ? 'passed' : 'failed'" /></header><ul class="diagnosis-list"><li v-for="check in diagnosis.checks" :key="check.name"><strong>{{ probeLabels[check.name] || check.name }}</strong><GatewayState :value="check.status" /><span>{{ check.msg || '—' }}</span></li></ul></section><h2 class="section-title">当前凭据</h2><template v-if="gateway.certificate"><p>状态 {{ stateLabel(gateway.certificate.status) }} · 有效期 {{ new Date(gateway.certificate.not_before).toLocaleString() }} 至 {{ new Date(gateway.certificate.not_after).toLocaleString() }}</p><p class="fingerprint">指纹 {{ gateway.certificate.fingerprint }}</p><p class="fingerprint">CA {{ gateway.certificate.ca_fingerprint }}</p></template><p v-else>尚未导入凭据。</p><div class="actions"><select v-model="templateFormat" class="select" aria-label="部署模板"><option value="toml">TOML</option><option value="compose">Compose</option></select><button class="btn" :disabled="busy" @click="deployTemplate">生成部署模板</button></div><p v-for="w in warnings" :key="w">{{ w }}</p><pre v-if="template">{{ template }}</pre>
  <section class="tab-section gateway-operations"><div><h2 class="section-title">网关运维</h2><p class="section-note">排空后停止接入新业务，待已有业务结束。停用与重连可能中断当前业务。</p></div><div class="actions"><button class="btn" :disabled="busy || gateway.admin_state === 'enabled'" @click="openAction('enabled','启用网关')">启用</button><button class="btn" :disabled="busy || gateway.admin_state !== 'enabled'" @click="openAction('draining','排空网关')">排空</button><button class="btn" :disabled="busy || gateway.admin_state === 'disabled'" @click="openAction('disabled','停用网关')">停用</button><button class="btn" :disabled="busy" @click="openAction('reconnect','重连网关')">重连</button><button class="btn btn-danger" :disabled="busy" @click="openAction('delete','删除网关')">删除</button></div></section>
</section>
<p class="empty-state" v-if="['devices','platforms','sessions','events'].includes(tab) && !currentPage && !tabError">正在加载…</p><footer v-if="['devices','platforms','sessions','events'].includes(tab)" class="actions gateway-detail-pagination"><span>共 {{ currentPage?.total ?? '—' }} 条</span><button class="btn" :disabled="loading || page === 1" @click="page--">上一页</button><span>第 {{ page }} / {{ Math.max(1, Math.ceil((currentPage?.total || 0) / (currentPage?.size || 20))) }} 页</span><button class="btn" :disabled="loading || !currentPage || page * currentPage.size >= currentPage.total" @click="page++">下一页</button></footer>
</div>
<GatewayEditor v-if="editor" :gateway="gateway" @saved="saved" @close="editor = false" /></template>
<ModalDialog :open="!!action" :title="action?.label || ''" @close="!busy && (action = undefined)"><div class="gateway-confirm"><p v-if="error" class="warning-box" role="alert">{{ error }}</p><p v-if="action?.kind === 'delete'">删除前必须解除全部绑定并完成资源清理，网关 ID 会保留审计记录。</p><p v-else>此操作可能影响当前公网业务，请确认后执行。</p><label v-if="['reconnect','stop'].includes(action?.kind || '')">原因<textarea v-model="reason" class="textarea" rows="3" maxlength="500" /></label><label v-if="action?.kind === 'disabled'"><input v-model="force" type="checkbox" /> 强制停止全部活动业务</label></div><template #footer><button class="btn" :disabled="busy" @click="action = undefined">取消</button><button class="btn" :class="action?.kind === 'delete' ? 'btn-danger' : 'btn-primary'" :disabled="busy || (['reconnect','stop'].includes(action?.kind || '') && !reason.trim())" @click="confirmAction">{{ busy ? '处理中…' : '确认执行' }}</button></template></ModalDialog>
</main></template>
<style scoped>
.gateway-detail { min-width: 0; }.gateway-detail > .device-command-header { margin-bottom: 14px; }.gateway-detail .device-command-meta { font-size: 12px; }.gateway-detail .device-command-identity { gap: 10px; min-width: 0; }.gateway-detail .device-command-identity > div { min-width: 0; }.gateway-detail .device-command-title h1 { font-size: 18px; overflow-wrap: anywhere; }.gateway-description { margin: 6px 0 0; color: var(--muted); font-size: 12px; overflow-wrap: anywhere; }
.gateway-workspace { min-width: 0; overflow: hidden; background: var(--panel); border: 1px solid var(--line); border-radius: var(--radius); }.gateway-tabs { border: 0; border-bottom: 1px solid var(--line); border-radius: 0; }.gateway-filters { border-width: 0 0 1px; border-radius: 0; margin: 0; }.gateway-workspace .content { margin: 0; }.access-content { overflow-x: visible; }.tab-section { margin-top: 20px; padding-top: 18px; border-top: 1px solid var(--line); }.section-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 12px; }.content .section-head h2 { margin: 0; }.section-note { color: var(--muted); font-size: 12px; line-height: 1.7; }.content p.section-note { font-size: 12px; margin: 8px 0 0; }.runtime-facts { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); margin: 0; }.runtime-facts > div { display: flex; justify-content: space-between; gap: 12px; padding: 10px 12px 10px 0; border-bottom: 1px solid var(--line); font-size: 13px; }.runtime-facts dt { color: var(--muted); }.runtime-facts dd { margin: 0; text-align: right; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }.capability-list { display: flex; flex-wrap: wrap; gap: 8px 18px; margin: 0; padding: 0; list-style: none; font-size: 13px; }.capability-list li { line-height: 1.8; overflow-wrap: anywhere; }.gateway-operations { display: flex; justify-content: space-between; gap: 14px; align-items: center; }.gateway-operations h2 { margin-bottom: 4px; }.diagnosis-section { margin-bottom: 20px; padding-bottom: 16px; border-bottom: 1px solid var(--line); }.diagnosis-list { margin: 0; padding: 0; list-style: none; }.diagnosis-list li { display: grid; grid-template-columns: 130px 90px minmax(0, 1fr); gap: 10px; align-items: center; padding: 9px 0; border-bottom: 1px solid var(--line); font-size: 13px; }.diagnosis-list strong { font-weight: 500; }.diagnosis-list li > span:last-child { color: var(--muted); overflow-wrap: anywhere; }
.actions { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }.gateway-header-status { display: flex; flex: 1 0 100%; width: 100%; align-items: center; flex-wrap: wrap; gap: 8px 22px; padding-top: 10px; border-top: 1px solid var(--line); font-size: 12px; }.gateway-header-status > div { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }.gateway-header-status > div > span:first-child { color: var(--muted); }.gateway-header-status strong { font-weight: 500; }.gateway-header-status small { display: inline; }.state-updated { margin-left: auto; color: var(--muted); font-size: 12px; }.state-updated small { display: block; margin-top: 4px; }
.gateway-warning { color: var(--amber); }
.gateway-filters { padding: 14px; }.gateway-filters .input { width: 200px; }.content { padding: 14px; margin-bottom: 14px; overflow-x: auto; }.content h2 { margin: 0 0 14px; }.content p { margin: 12px 0; font-size: 13px; line-height: 1.7; }.content > p, .content > label { color: var(--muted); }.content > label { display: grid; gap: 8px; margin-top: 16px; font-size: 12px; }.content > label .input { width: min(420px, 100%); }
.resource-summary { display: flex; align-items: center; flex-wrap: wrap; gap: 10px 24px; margin: 0; padding: 12px 14px; border-bottom: 1px solid var(--line); font-size: 13px; }.resource-summary > div { display: flex; align-items: baseline; gap: 8px; }.resource-summary dt { color: var(--muted); font-size: 12px; }.resource-summary dd { margin: 0; font-weight: 600; font-variant-numeric: tabular-nums; }.resource-warning { margin: 12px 14px; }
.gateway-disclosure { margin-top: 16px; border-top: 1px solid var(--line); }.gateway-disclosure > summary { display: flex; align-items: center; gap: 10px; min-height: 44px; padding: 10px 0; color: var(--ink-strong); font-size: 13px; font-weight: 500; list-style: none; }.gateway-disclosure > summary::-webkit-details-marker { display: none; }.gateway-disclosure > summary > span { color: var(--muted); font-size: 12px; font-weight: 400; }.gateway-disclosure > summary::after { content: ''; width: 7px; height: 7px; margin-left: auto; margin-right: 3px; border-right: 1px solid currentColor; border-bottom: 1px solid currentColor; transform: rotate(45deg); }.gateway-disclosure[open] > summary::after { transform: rotate(225deg); }.disclosure-content { padding-top: 4px; }

table { width: 100%; border-collapse: collapse; min-width: 640px; font-size: 13px; font-variant-numeric: tabular-nums; }th, td { text-align: left; padding: 10px 12px; border-bottom: 1px solid var(--line); vertical-align: middle; }th { color: var(--muted); font-weight: 500; background: var(--panel-2); white-space: nowrap; }td { overflow-wrap: anywhere; }td a { color: var(--blue); }tr:hover td { background: var(--panel-2); }
small { display: block; color: var(--muted); font-size: 12px; }td small { margin-top: 4px; }pre { white-space: pre-wrap; overflow-wrap: anywhere; padding: 12px; font-size: 12px; line-height: 1.7; background: var(--panel-2); border-radius: var(--radius-sm); }.fingerprint { overflow-wrap: anywhere; font-family: monospace; }summary { cursor: pointer; color: var(--blue); font-size: 12px; }.gateway-detail-pagination { border: 0; border-top: 1px solid var(--line); border-radius: 0; justify-content: flex-end; padding: 12px 14px; background: var(--panel); color: var(--muted); font-size: 12px; }.gateway-detail-pagination > span:first-child { margin-right: auto; }
.gateway-confirm { font-size: 13px; line-height: 1.8; }.gateway-confirm > p { margin: 0 0 14px; }.gateway-confirm label { display: block; }.gateway-confirm .textarea { display: block; width: 100%; margin-top: 8px; }
@media (max-width: 1100px) { .gateway-operations { align-items: flex-start; flex-direction: column; } }
@media (max-width: 900px) { .runtime-facts { grid-template-columns: 1fr; } .state-updated { flex-basis: 100%; margin-left: 0; } }
@media (max-width: 640px) { .gateway-detail .page-back-btn { width: 44px; height: 44px; }.gateway-detail .device-command-header { align-items: stretch; }.section-head { align-items: flex-start; flex-wrap: wrap; }.diagnosis-list li { grid-template-columns: minmax(0, 1fr) auto; }.diagnosis-list li > span:last-child { grid-column: 1 / -1; }.runtime-facts > div { padding-right: 0; }.gateway-filters > .input, .gateway-filters > .select { width: 100%; }.gateway-detail .btn, .gateway-detail .input, .gateway-detail .select { min-height: 44px; }.gateway-detail-pagination > span:first-child { flex-basis: 100%; } }
</style>
