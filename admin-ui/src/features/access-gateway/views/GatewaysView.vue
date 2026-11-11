<script setup lang="ts">
import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { ArrowRight, Plus, RefreshCw, Search, Network, ShieldAlert } from '@lucide/vue'
import { formatDate } from '../../../utils/format'
import GatewayState from '../components/GatewayState.vue'
import { adminStates, connectionStates, healthStates, stateLabel } from '../presentation'
import { gatewayAPI } from '../api'
import type { Gateway, Operation, Page } from '../types'
import GatewayEditor from '../components/GatewayEditor.vue'
import { useVisiblePoll } from '../composables/useVisiblePoll'
import { errorMessage } from '../../../services/api'
const data = shallowRef<Page<Gateway>>(); const error = ref(''); const loading = ref(false); const showEditor = ref(false)
const q = ref(''); const group = ref(''); const admin = ref(''); const connection = ref(''); const health = ref(''); const sort = ref('name'); const page = ref(1)
const { refresh } = useVisiblePoll(async signal => {
  loading.value = true
  try { const result = await gatewayAPI.list({ q: q.value, group: group.value, admin_state: admin.value, connection_state: connection.value, health_state: health.value, sort: sort.value, page: page.value, size: 20 }, signal); if (!signal.aborted) { data.value = result; error.value = '' } }
  catch (e) { if (!signal.aborted) error.value = errorMessage(e) } finally { if (!signal.aborted) loading.value = false }
})
const hasFilters = computed(() => !!(q.value || group.value || admin.value || connection.value || health.value))
const pageCount = computed(() => Math.max(1, Math.ceil((data.value?.total || 0) / 20)))
function resetFilters() { q.value = ''; group.value = ''; admin.value = ''; connection.value = ''; health.value = '' }
let searchTimer: ReturnType<typeof setTimeout> | undefined
watch([q, group, admin, connection, health, sort], () => { page.value = 1; clearTimeout(searchTimer); searchTimer = setTimeout(() => void refresh(), 250) })
watch(page, () => void refresh())
onBeforeUnmount(() => clearTimeout(searchTimer))
function saved(_result: Gateway | Operation) { void refresh() }
</script>
<template>
  <main class="page-content gateway-page">
    <header class="page-head">
      <div><h1 class="page-title">接入网关</h1><p class="page-desc">管理公网接入、设备绑定与媒体资源，查看网关连接和运行状态。</p></div>
      <div class="head-actions">
        <button class="btn" type="button" :disabled="loading" @click="refresh"><RefreshCw :class="{ 'animate-spin': loading }" aria-hidden="true" />{{ loading ? '正在刷新…' : '刷新' }}</button>
        <button class="btn btn-primary" type="button" @click="showEditor = true"><Plus aria-hidden="true" />添加网关</button>
      </div>
    </header>
    <div v-if="error" class="warning-box gateway-alert" role="alert"><ShieldAlert aria-hidden="true" /><span>{{ error }}{{ data ? '，下方保留上次成功读取的数据。' : '' }}</span><button class="btn btn-sm" :disabled="loading" @click="refresh">重试</button></div>
    <section class="card gateway-list" aria-label="接入网关列表" :aria-busy="loading">
      <div class="gateway-toolbar">
        <label class="field gateway-search"><Search aria-hidden="true" /><input v-model.trim="q" class="input" type="search" aria-label="搜索网关" placeholder="搜索网关名称或 ID" /></label>
        <input v-model.trim="group" class="input group-filter" aria-label="按分组筛选" placeholder="按分组筛选" />
        <select v-model="admin" class="select" aria-label="管理状态"><option value="">全部管理状态</option><option v-for="(label, value) in adminStates" :key="value" :value="value">{{ label }}</option></select>
        <select v-model="connection" class="select" aria-label="连接状态"><option value="">全部连接状态</option><option v-for="(label, value) in connectionStates" :key="value" :value="value">{{ label }}</option></select>
        <select v-model="health" class="select" aria-label="健康状态"><option value="">全部健康状态</option><option v-for="(label, value) in healthStates" :key="value" :value="value">{{ label }}</option></select>
        <button v-if="hasFilters" class="btn btn-ghost" @click="resetFilters">重置筛选</button>
      </div>
      <div class="gateway-list-meta"><span>{{ data ? `共 ${data.total} 个网关` : '正在读取网关' }}</span><label>排序<select v-model="sort" class="select" aria-label="排序方式"><option value="name">名称</option><option value="device_total">设备数</option><option value="device_online">在线设备数</option><option value="last_seen_at">最近上报</option><option value="created_at">创建时间</option></select></label></div>
      <div v-if="!data && loading" class="empty-state gateway-empty" role="status">正在读取接入网关…</div>
      <div v-else-if="!data || !data.items.length" class="empty-state gateway-empty">
        <Network aria-hidden="true" />
        <strong>{{ error && !data ? '网关列表暂不可用' : hasFilters ? '没有匹配的网关' : '尚未添加接入网关' }}</strong>
        <p>{{ error && !data ? '请重试，或检查服务连接。' : hasFilters ? '试试其他名称、分组或状态，或重置筛选条件。' : '添加网关后，导入证书并完成连接测试，即可启用公网接入。' }}</p>
        <button v-if="hasFilters" class="btn" @click="resetFilters">重置筛选</button>
        <button v-else-if="!error" class="btn" @click="showEditor = true"><Plus aria-hidden="true" />添加网关</button>
      </div>
      <div v-else class="gateway-table-wrap">
        <table class="gateway-table">
          <thead><tr><th scope="col">网关 / 分组</th><th scope="col">运行状态</th><th scope="col">设备与资源</th><th scope="col">配置版本</th><th scope="col">证书</th><th scope="col">最近上报</th><th scope="col">操作</th></tr></thead>
          <tbody><tr v-for="g in data.items" :key="g.id">
            <td data-label="网关 / 分组"><div class="gateway-cell"><RouterLink class="gateway-name" :to="{ name: 'access-gateway-detail', params: { id: g.id } }">{{ g.name || g.id }}</RouterLink><small class="mono">{{ g.id }}</small><small>{{ g.group || '未分组' }}</small></div></td>
            <td data-label="运行状态"><div class="gateway-cell"><div class="gateway-states"><GatewayState :value="g.admin_state" /><GatewayState :value="g.connection_state" /><GatewayState :value="g.health_state" /></div><small v-if="g.stale" class="gateway-warning">状态已过期，等待网关上报</small><small v-if="g.last_error" class="gateway-error">{{ g.last_error.message }}</small></div></td>
            <td data-label="设备与资源"><div class="gateway-cell"><span>设备 {{ g.counts?.device_online ?? '—' }} 在线 / {{ g.counts?.device_total ?? '—' }} 总数</span><small>通道 {{ g.counts?.channel_total ?? '—' }} · 活动源 {{ g.counts?.active_sources ?? '—' }} · 待清理 {{ g.counts?.cleanup_pending ?? '—' }}</small><small v-if="g.counts_error" class="gateway-warning">{{ g.counts_error.message }}</small></div></td>
            <td data-label="配置版本"><div class="gateway-cell"><span class="mono">{{ g.applied_revision }} / {{ g.config_revision }}</span><small :class="{ 'gateway-warning': g.config_revision !== g.applied_revision }">{{ g.config_revision !== g.applied_revision ? '有配置待应用' : '已同步' }} · 生效 / 期望</small></div></td>
            <td data-label="证书"><div class="gateway-cell"><template v-if="g.certificate"><span>{{ formatDate(g.certificate.not_after).split(' ')[0] }} 到期</span><small>{{ stateLabel(g.certificate.status) }}</small></template><span v-else class="gateway-muted">未导入证书</span></div></td>
            <td data-label="最近上报"><div class="gateway-cell">{{ formatDate(g.last_seen_at || undefined, '尚未上报') }}</div></td>
            <td data-label="操作"><div class="gateway-cell"><RouterLink class="btn btn-sm" :to="{ name: 'access-gateway-detail', params: { id: g.id } }" :aria-label="`查看 ${g.name || g.id} 的详情`">查看详情<ArrowRight aria-hidden="true" /></RouterLink></div></td>
          </tr></tbody>
        </table>
      </div>
      <footer v-if="data" class="gateway-pagination"><span>更新于 {{ formatDate(data.sampled_at) }}<span v-if="data.stale" class="gateway-warning"> · 数据已过期</span></span><div class="pagination-actions"><button class="btn btn-sm" :disabled="loading || page === 1" @click="page--">上一页</button><span>第 {{ page }} / {{ pageCount }} 页</span><button class="btn btn-sm" :disabled="loading || page >= pageCount" @click="page++">下一页</button></div></footer>
    </section>
    <p class="gateway-note">网关通过 WSS 接入平台。管理状态、连接状态与健康状态分别反映启停配置、控制连接和实际运行情况。</p>
    <GatewayEditor v-if="showEditor" @close="showEditor = false" @saved="saved" />
  </main>
</template>

<style scoped>
.gateway-list { overflow: hidden; }
.gateway-alert { margin-bottom: 14px; }
.gateway-toolbar { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; padding: 14px; border-bottom: 1px solid var(--line); }
.gateway-search { flex: 1 1 220px; max-width: 340px; }.gateway-search .input { width: 100%; padding-left: 32px; }.group-filter { width: 150px; }
.gateway-list-meta { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 10px 14px; color: var(--muted); font-size: 12px; }.gateway-list-meta label { display: flex; align-items: center; gap: 8px; }
.gateway-table-wrap { overflow: auto; }.gateway-table { width: 100%; border-collapse: collapse; text-align: left; font-size: 13px; font-variant-numeric: tabular-nums; }
.gateway-table th { padding: 10px 14px; background: var(--panel-2); color: var(--muted); font-weight: 500; white-space: nowrap; }.gateway-table td { padding: 10px 14px; border-top: 1px solid var(--line); vertical-align: middle; }.gateway-table tr:hover td { background: var(--panel-2); }
.gateway-name { color: var(--blue); font-weight: 500; overflow-wrap: anywhere; }.gateway-table small { display: block; margin-top: 4px; color: var(--muted); font-size: 12px; overflow-wrap: anywhere; }.gateway-table td:first-child { min-width: 150px; }.gateway-states { display: flex; flex-wrap: wrap; gap: 5px; min-width: 165px; }.gateway-table td:nth-child(3) { min-width: 190px; }
.gateway-muted { color: var(--muted); }.gateway-warning, .gateway-table small.gateway-warning { color: var(--amber); }.gateway-table small.gateway-error { color: var(--red); max-width: 320px; }
.gateway-pagination { display: flex; justify-content: space-between; align-items: center; flex-wrap: wrap; gap: 12px; padding: 12px 14px; border-top: 1px solid var(--line); color: var(--muted); font-size: 12px; font-variant-numeric: tabular-nums; }
.gateway-empty { display: flex; flex-direction: column; align-items: center; gap: 12px; padding: 44px 20px; font-size: 13px; }.gateway-empty > svg { width: 28px; height: 28px; color: var(--muted); }.gateway-empty strong { color: var(--ink-strong); font-size: 14px; font-weight: 500; }.gateway-empty p { margin: 0; max-width: 440px; line-height: 1.7; }.gateway-note { margin: 12px 0 0; color: var(--muted); font-size: 12px; line-height: 1.7; }
@media (max-width: 900px) { .gateway-table, .gateway-table tbody, .gateway-table tr { display: block; }.gateway-table thead { display: none; }.gateway-table tr { padding: 12px 14px; border-top: 1px solid var(--line); }.gateway-table td { display: grid; grid-template-columns: 90px minmax(0, 1fr); gap: 5px 12px; padding: 6px 0; border: 0; overflow-wrap: anywhere; }.gateway-table td::before { content: attr(data-label); grid-column: 1; color: var(--muted); }.gateway-cell { min-width: 0; }.gateway-table small { margin: 0; }.gateway-table .gateway-error { max-width: none; } }
@media (max-width: 640px) { .gateway-search { max-width: none; flex-basis: 100%; }.gateway-toolbar > .input, .gateway-toolbar > .select { flex: 1 1 calc(50% - 8px); min-width: 0; width: 100%; }.gateway-pagination { align-items: stretch; flex-direction: column; }.pagination-actions { justify-content: space-between; }.gateway-page .btn, .gateway-page .input, .gateway-page .select { min-height: 44px; } }
</style>
