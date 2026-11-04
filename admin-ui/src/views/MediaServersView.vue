<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { ArrowRight, RefreshCw, Search, Server, ShieldAlert } from '@lucide/vue';
import { api, collectPages, errorMessage } from '../services/api';
import type { MediaServer } from '../types/api';
import { formatDate } from '../utils/format';

const rows = ref<MediaServer[]>([]);
const loading = ref(false);
const loadError = ref('');
const query = ref('');
const state = ref('');
const updatedAt = ref('');
const filtered = computed(() => rows.value.filter((row) => {
  if (state.value && (state.value === 'online') !== Boolean(row.status)) return false;
  return !query.value.trim() || [row.id, row.type, row.ip, row.sdp_ip].join(' ').toLowerCase().includes(query.value.trim().toLowerCase());
}));
const onlineCount = computed(() => rows.value.filter((row) => row.status).length);
async function load() {
  if (loading.value) return;
  loading.value = true;
  loadError.value = '';
  try {
    // 逐页读取已有节点，不把单页上限当作节点总数。
    const response = await collectPages(api.mediaServers);
    rows.value = response.items;
    updatedAt.value = new Date().toISOString();
  } catch (cause) { loadError.value = errorMessage(cause, '媒体节点加载失败，请重试'); }
  finally { loading.value = false; }
}
onMounted(load);
</script>

<template>
  <main class="page-content media-server-page">
    <header class="page-head"><div><h1 class="page-title">媒体节点</h1><p class="page-desc">从节点进入媒体流、网络会话、RTP 服务与代理任务，查看真实运行状态。</p></div><div class="head-actions"><button class="btn btn-primary" type="button" :disabled="loading" @click="load"><RefreshCw :class="{ 'animate-spin': loading }" aria-hidden="true" />{{ loading ? '正在刷新…' : '刷新节点' }}</button></div></header>
    <div v-if="loadError" class="warning-box mb-4" role="alert"><ShieldAlert aria-hidden="true" /><span>{{ loadError }}{{ updatedAt ? '，下方保留上次成功读取的节点。' : '' }}</span><button class="btn btn-sm" type="button" :disabled="loading" @click="load">重试</button></div>
    <section class="card node-list" aria-label="媒体节点列表" :aria-busy="loading">
      <div class="node-list-toolbar"><label class="node-list-search"><Search aria-hidden="true" /><input v-model="query" class="input plain" type="search" placeholder="搜索节点标识或地址" aria-label="搜索媒体节点" /></label><label class="node-state-filter"><span>状态</span><select v-model="state" class="select"><option value="">全部状态</option><option value="online">在线</option><option value="offline">离线</option></select></label><span class="node-list-summary">{{ updatedAt ? `${onlineCount} 在线 / ${rows.length} 个节点` : '正在读取节点' }}</span></div>
      <div v-if="loading && !updatedAt" class="empty-state" role="status">正在读取媒体节点…</div>
      <div v-else-if="!filtered.length" class="empty-state">{{ rows.length ? '没有匹配的节点，请调整筛选条件。' : loadError ? '节点列表暂不可用，请重试。' : '当前尚无媒体节点记录。' }}</div>
      <div v-else class="node-list-table-wrap"><table class="node-list-table"><thead><tr><th scope="col">节点</th><th scope="col">状态</th><th scope="col">API 地址</th><th scope="col">SDP 地址</th><th scope="col">RTP 端口范围</th><th scope="col">最近心跳</th><th scope="col">查看</th></tr></thead><tbody><tr v-for="item in filtered" :key="item.id"><td data-label="节点"><RouterLink class="node-name" :to="`/media-servers/${encodeURIComponent(item.id)}`"><Server aria-hidden="true" /><span>{{ item.id }}<small>{{ item.type || 'zlm' }}</small></span></RouterLink></td><td data-label="状态"><span class="status" :class="item.status ? 'online' : 'offline'">{{ item.status ? '在线' : '离线' }}</span></td><td data-label="API 地址">{{ item.ip || '—' }}:{{ item.ports?.http || '—' }}</td><td data-label="SDP 地址">{{ item.sdp_ip || '—' }}</td><td data-label="RTP 端口范围">{{ item.rtpport_range || '—' }}</td><td data-label="最近心跳">{{ formatDate(item.last_keepalive_at) }}</td><td data-label="查看"><RouterLink class="icon-btn node-open" :to="`/media-servers/${encodeURIComponent(item.id)}`" :aria-label="`查看节点 ${item.id} 的运行详情`" title="查看运行详情"><ArrowRight aria-hidden="true" /></RouterLink></td></tr></tbody></table></div>
      <footer v-if="updatedAt" class="node-list-footer"><span>{{ filtered.length }} 个匹配节点</span><span>列表更新于 {{ formatDate(updatedAt) }}</span></footer>
    </section>
    <p class="node-list-note">详情提供只读运行快照；不会修改 ZLM 配置、创建代理任务或中断媒体会话。</p>
  </main>
</template>

<style scoped>
.node-list { overflow: hidden; }.node-list-toolbar { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; padding: 16px; border-bottom: 1px solid var(--line); }
.node-list-search { position: relative; width: min(360px, 100%); }.node-list-search > svg { position: absolute; top: 10px; left: 10px; width: 15px; height: 15px; color: var(--muted); }.node-list-search .input { width: 100%; padding-left: 32px; }
.node-state-filter { display: flex; align-items: center; gap: 8px; color: var(--muted); font-size: 12px; }.node-list-summary { margin-left: auto; color: var(--muted); font-size: 12px; }
.node-list-table-wrap { overflow: auto; }.node-list-table { width: 100%; border-collapse: collapse; text-align: left; font-size: 13px; font-variant-numeric: tabular-nums; }.node-list-table th { padding: 12px 16px; color: var(--muted); background: var(--panel-2); font-weight: 500; white-space: nowrap; }.node-list-table td { padding: 14px 16px; border-top: 1px solid var(--line); white-space: nowrap; }.node-list-table tr:hover td { background: var(--panel-2); }
.node-name { display: inline-flex; align-items: center; gap: 10px; color: var(--blue); font-weight: 500; }.node-name > svg { width: 18px; height: 18px; color: var(--muted); }.node-name small { display: block; margin-top: 4px; font-size: 12px; font-weight: 400; color: var(--muted); }.node-open { border: 0; background: transparent; }.node-open svg { width: 20px; height: 20px; }
.node-list-footer { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 10px; padding: 12px 16px; color: var(--muted); font-size: 12px; border-top: 1px solid var(--line); }.node-list-note { margin: 14px 0 0; color: var(--muted); font-size: 12px; line-height: 1.7; }
@media (max-width: 900px) { .node-list-table, .node-list-table tbody, .node-list-table tr { display: block; }.node-list-table thead { display: none; }.node-list-table tr { padding: 12px 16px; border-top: 1px solid var(--line); }.node-list-table tr:first-child { border-top: 0; }.node-list-table td { display: grid; grid-template-columns: 90px minmax(0, 1fr); align-items: center; gap: 10px; padding: 6px 0; border: 0; white-space: normal; overflow-wrap: anywhere; }.node-list-table td::before { content: attr(data-label); color: var(--muted); }.node-open { width: 44px; height: 44px; } }
@media (max-width: 640px) { .node-list-toolbar { padding: 12px; }.node-list-search { width: 100%; }.node-state-filter .select, .head-actions .btn { min-height: 44px; }.node-list-summary { margin-left: 0; } }
</style>
