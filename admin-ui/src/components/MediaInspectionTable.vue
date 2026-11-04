<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { ChevronLeft, ChevronRight, RefreshCw, Search } from '@lucide/vue';
import type { MediaInspectionResult } from '../types/api';
import { inspectionRows, inspectionText, inspectionValue, type InspectionColumn, type InspectionRow } from '../utils/mediaInspection';
import { formatDate } from '../utils/format';

const props = defineProps<{
  title: string;
  description: string;
  result?: MediaInspectionResult;
  loading?: boolean;
  columns: InspectionColumn[];
  filterKey?: string;
  filterLabel?: string;
  inspectable?: boolean;
}>();
defineEmits<{ refresh: []; inspect: [row: InspectionRow] }>();
const query = ref('');
const filter = ref('');
const page = ref(1);
const size = ref(20);
const rows = computed(() => inspectionRows(props.result));
const filterOptions = computed(() => props.filterKey ? [...new Set(rows.value.map((row) => String(inspectionValue(row, props.filterKey!) ?? '')).filter(Boolean))].sort() : []);
const filtered = computed(() => rows.value.filter((row) => {
  if (filter.value && String(inspectionValue(row, props.filterKey || '')) !== filter.value) return false;
  return !query.value.trim() || JSON.stringify(row).toLowerCase().includes(query.value.trim().toLowerCase());
}));
const pages = computed(() => Math.max(1, Math.ceil(filtered.value.length / size.value)));
const visible = computed(() => filtered.value.slice((page.value - 1) * size.value, page.value * size.value));
watch([query, filter, size], () => { page.value = 1; });
watch(pages, (value) => { page.value = Math.min(page.value, value); });
const cell = (row: InspectionRow, column: InspectionColumn) => inspectionText(inspectionValue(row, column.key), column.kind);
</script>

<template>
  <section class="inspection-section" :aria-label="title" :aria-busy="loading">
    <header class="inspection-heading"><div><h2>{{ title }}</h2><p>{{ description }}</p></div><button class="btn" type="button" :disabled="loading" :aria-label="`刷新${title}`" @click="$emit('refresh')"><RefreshCw :class="{ 'animate-spin': loading }" aria-hidden="true" />刷新</button></header>
    <p v-if="loading && !result" class="inspection-loading" role="status">正在读取{{ title }}…</p>
    <div v-else-if="result && result.status !== 'ok'" class="inspection-message" :class="{ error: result.status === 'unavailable' }" :role="result.status === 'unavailable' ? 'alert' : 'status'">
      <strong>{{ result.status === 'unsupported' ? '当前节点不支持此项查询' : '暂时无法读取' }}</strong><p>{{ result.message }}</p>
    </div>
    <template v-else-if="result?.status === 'ok'">
      <div class="inspection-toolbar">
        <label class="inspection-search"><Search aria-hidden="true" /><input v-model="query" class="input plain" type="search" :aria-label="`搜索${title}`" placeholder="搜索当前快照的全部字段" /></label>
        <label v-if="filterKey && filterOptions.length" class="inspection-filter"><span>{{ filterLabel || '分类' }}</span><select v-model="filter" class="select"><option value="">全部</option><option v-for="option in filterOptions" :key="option" :value="option">{{ option }}</option></select></label>
        <span class="inspection-count">{{ filtered.length }} / {{ rows.length }} 条</span>
      </div>
      <div v-if="visible.length" class="inspection-table-wrap" role="region" :aria-label="`${title}数据表`" tabindex="0">
        <table class="inspection-table"><thead><tr><th v-for="column in columns" :key="column.key" scope="col">{{ column.label }}</th><th scope="col">详情</th></tr></thead>
          <tbody><template v-for="(row, index) in visible" :key="index"><tr><td v-for="column in columns" :key="column.key" :data-label="column.label"><span :title="cell(row, column)">{{ cell(row, column) }}</span></td><td data-label="详情"><button v-if="inspectable" class="btn btn-sm" type="button" @click="$emit('inspect', row)">实时详情</button><details class="inspection-raw"><summary>全部字段</summary><pre>{{ JSON.stringify(row, null, 2) }}</pre></details></td></tr></template></tbody>
        </table>
      </div>
      <div v-else class="inspection-empty"><strong>{{ rows.length ? '没有匹配的结果' : '当前没有记录' }}</strong><p>{{ rows.length ? '请调整搜索词或分类筛选。' : '本次查询成功；节点当前未返回此类运行记录。' }}</p></div>
      <footer class="inspection-footer"><span>采集于 {{ formatDate(result.fetched_at) }}</span><div><label>每页 <select v-model.number="size" class="select" :aria-label="`${title}每页条数`"><option :value="20">20</option><option :value="50">50</option><option :value="100">100</option></select> 条</label><button class="icon-btn" type="button" :disabled="page <= 1" aria-label="上一页" @click="page--"><ChevronLeft aria-hidden="true" /></button><span>{{ page }} / {{ pages }}</span><button class="icon-btn" type="button" :disabled="page >= pages" aria-label="下一页" @click="page++"><ChevronRight aria-hidden="true" /></button></div></footer>
    </template>
  </section>
</template>

<style scoped>
.inspection-section { min-width: 0; }
.inspection-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; margin-bottom: 14px; }
.inspection-heading h2 { margin: 0; color: var(--ink-strong); font-size: 14px; font-weight: 600; }
.inspection-heading p, .inspection-empty p, .inspection-message p { margin: 5px 0 0; color: var(--muted); font-size: 12px; line-height: 1.7; }
.inspection-toolbar { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; margin-bottom: 12px; }
.inspection-search { position: relative; flex: 1; min-width: 180px; max-width: 440px; }
.inspection-search > svg { position: absolute; inset: 10px auto auto 10px; width: 15px; height: 15px; color: var(--muted); }
.inspection-search .input { width: 100%; padding-left: 32px; }
.inspection-filter { display: flex; align-items: center; gap: 8px; font-size: 12px; }
.inspection-count { margin-left: auto; color: var(--muted); font-size: 12px; font-variant-numeric: tabular-nums; }
.inspection-table-wrap { max-width: 100%; overflow: auto; border: 1px solid var(--line); border-radius: var(--radius-sm); }
.inspection-table { width: 100%; border-collapse: collapse; font-size: 12px; text-align: left; font-variant-numeric: tabular-nums; }
.inspection-table th { padding: 10px 12px; white-space: nowrap; color: var(--muted); font-weight: 500; background: var(--panel-2); }
.inspection-table td { min-width: 82px; max-width: 320px; padding: 10px 12px; border-top: 1px solid var(--line); vertical-align: top; }
.inspection-table td > span { display: block; min-width: 60px; max-width: 300px; overflow-wrap: anywhere; line-height: 1.7; }
.inspection-table tbody tr:hover { background: var(--panel-2); }
.inspection-raw { margin-top: 5px; min-width: 74px; }
.inspection-raw summary { color: var(--blue); cursor: pointer; white-space: nowrap; }
.inspection-raw pre { min-width: 220px; max-width: 480px; max-height: 320px; padding: 10px; margin: 8px 0 0; overflow: auto; color: var(--ink); background: var(--panel-3); border-radius: var(--radius-sm); font: 12px/1.7 monospace; white-space: pre-wrap; overflow-wrap: anywhere; }
.inspection-empty, .inspection-loading, .inspection-message { padding: 28px 16px; background: var(--panel-2); border: 1px solid var(--line); border-radius: var(--radius-sm); font-size: 13px; }
.inspection-message.error { background: var(--red-soft); color: var(--red); }
.inspection-footer, .inspection-footer > div, .inspection-footer label { display: flex; align-items: center; gap: 8px; }
.inspection-footer { flex-wrap: wrap; justify-content: space-between; margin-top: 12px; color: var(--muted); font-size: 12px; }
.inspection-footer .select { min-height: 30px; padding: 0 6px; }
.inspection-footer .icon-btn { width: 30px; height: 30px; }
@media (max-width: 640px) {
  .inspection-heading { gap: 8px; }.inspection-toolbar { align-items: stretch; }.inspection-search { flex-basis: 100%; max-width: none; }
  .inspection-footer > div { flex-wrap: wrap; }.inspection-footer .icon-btn, .inspection-footer .select, .inspection-filter .select, .inspection-heading .btn { min-width: 44px; min-height: 44px; }
  .inspection-table, .inspection-table tbody, .inspection-table tr, .inspection-table td { display: block; }.inspection-table thead { display: none; }
  .inspection-table tr { padding: 8px 12px; border-bottom: 1px solid var(--line); }.inspection-table tr:last-child { border-bottom: 0; }
  .inspection-table td { display: grid; grid-template-columns: 84px minmax(0, 1fr); gap: 10px; min-width: 0; max-width: none; padding: 5px 0; border: 0; }
  .inspection-table td::before { content: attr(data-label); color: var(--muted); }.inspection-table td > span { max-width: none; }
  .inspection-raw pre { min-width: 0; max-width: 100%; }.inspection-raw { min-width: 0; }.inspection-raw summary { min-height: 44px; padding-top: 10px; }
}
</style>
