<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, shallowRef } from 'vue'
import { ChevronLeft, ChevronRight, Clock3, LoaderCircle, RefreshCw, Search } from '@lucide/vue'
import { api, errorMessage, typeLabel } from '../services/api'
import type { CaptureSchedulePage } from '../types/api'
import { formatDate } from '../utils/format'

const result = shallowRef<CaptureSchedulePage>({ items: [], total: 0, device_count: 0, standalone_count: 0 })
const query = ref('')
const appliedQuery = ref('')
const page = ref(1)
const size = ref(20)
const loading = ref(false)
const error = ref('')
const pages = computed(() => Math.max(1, Math.ceil(result.value.total / size.value)))
const start = computed(() => result.value.items.length ? (page.value - 1) * size.value + 1 : 0)
const end = computed(() => Math.min(result.value.total, (page.value - 1) * size.value + result.value.items.length))
let sequence = 0

async function load() {
  const current = ++sequence
  loading.value = true
  error.value = ''
  try {
    const { data } = await api.captureSchedules({ page: page.value, size: size.value, key: appliedQuery.value })
    if (current !== sequence) return
    result.value = data
    if (page.value > pages.value) {
      page.value = pages.value
      void load()
    }
  } catch (cause) {
    if (current === sequence) error.value = errorMessage(cause, '定时抓拍设备读取失败，请重试')
  } finally {
    if (current === sequence) loading.value = false
  }
}
function search() {
  appliedQuery.value = query.value.trim()
  page.value = 1
  result.value = { items: [], total: 0, device_count: 0, standalone_count: 0 }
  void load()
}
function changePage(value: number) {
  if (loading.value || value < 1 || value > pages.value) return
  page.value = value
  result.value = { ...result.value, items: [] }
  void load()
}
function changeSize() {
  page.value = 1
  result.value = { ...result.value, items: [] }
  void load()
}
onMounted(load)
onBeforeUnmount(() => { sequence += 1 })
</script>

<template>
  <div class="schedule-devices">
    <div class="schedule-heading">
      <div><h2 class="card-title">已开启定时抓拍的设备</h2><p class="card-sub">每行对应一个已启用的通道计划。离线设备仍显示，进入通道可修改或停用计划。</p></div>
      <button class="btn" type="button" :disabled="loading" @click="load"><LoaderCircle v-if="loading" class="animate-spin" aria-hidden="true" /><RefreshCw v-else aria-hidden="true" />刷新列表</button>
    </div>
    <form class="schedule-search" role="search" @submit.prevent="search">
      <label class="schedule-search-field"><Search aria-hidden="true" /><input v-model="query" class="input plain" type="search" maxlength="200" aria-label="搜索定时抓拍设备或通道" placeholder="搜索设备、通道名称或编码" /></label>
      <button class="btn" type="submit" :disabled="loading">搜索</button>
    </form>
    <p v-if="error" class="warning-box" role="alert">{{ error }} <button class="btn btn-sm" type="button" :disabled="loading" @click="load">重试</button></p>
    <p v-if="!error" class="schedule-summary" role="status">{{ appliedQuery ? '搜索结果：' : '' }}{{ result.device_count }} 台设备<template v-if="result.standalone_count">、{{ result.standalone_count }} 路独立通道</template>，{{ result.total }} 个已启用计划<span v-if="loading"> · 正在读取…</span></p>
    <div v-if="!result.items.length && !loading && !error" class="schedule-empty"><Clock3 aria-hidden="true" /><strong>{{ appliedQuery ? '没有匹配的定时抓拍计划' : '暂无设备开启定时抓拍' }}</strong><span>{{ appliedQuery ? '试试其他设备名称、通道名称或编码。' : '在通道详情的“抓拍与计划”中启用后，即可在这里查看。' }}</span><RouterLink class="btn" to="/devices">查看设备</RouterLink></div>
    <div v-if="result.items.length" class="schedule-table-wrap" :aria-busy="loading">
      <table class="schedule-table">
        <thead><tr><th scope="col">设备</th><th scope="col">通道</th><th scope="col">通道状态</th><th scope="col">抓拍间隔</th><th scope="col">下次执行</th><th scope="col">上次执行</th><th scope="col">最近错误</th><th scope="col">计划</th></tr></thead>
        <tbody>
          <tr v-for="item in result.items" :key="item.channel_id">
            <td><div class="schedule-resource"><RouterLink v-if="item.device_id" :to="`/devices/${encodeURIComponent(item.device_id)}`">{{ item.device_name || item.device_code || item.device_id }}</RouterLink><span v-else>独立通道</span><small v-if="item.device_id">{{ item.device_code || item.device_id }}</small><small v-else>{{ typeLabel(item.channel_type, item.channel_id) }}</small></div></td>
            <td><div class="schedule-resource"><RouterLink :to="{ path: `/channels/${encodeURIComponent(item.channel_id)}`, query: { tab: 'snapshots' } }">{{ item.channel_name || item.channel_code || item.channel_id }}</RouterLink><small>{{ item.channel_code || item.channel_id }}</small></div></td>
            <td><span class="status" :class="item.is_online ? 'online' : 'offline'">{{ item.is_online ? '在线' : '离线' }}</span></td>
            <td>{{ item.interval_seconds }} 秒</td>
            <td>{{ formatDate(item.next_run_at) }}</td>
            <td>{{ formatDate(item.last_run_at, '尚未执行') }}</td>
            <td><span v-if="item.last_error" class="schedule-error">{{ item.last_error }}</span><span v-else class="schedule-muted">—</span></td>
            <td><RouterLink class="btn btn-sm" :to="{ path: `/channels/${encodeURIComponent(item.channel_id)}`, query: { tab: 'snapshots' } }">查看计划</RouterLink></td>
          </tr>
        </tbody>
      </table>
    </div>
    <nav v-if="result.total" class="schedule-pagination" aria-label="定时抓拍设备分页">
      <div><span v-if="result.items.length">第 {{ start }}–{{ end }} 项，共 {{ result.total }} 项</span><label>每页<select v-model.number="size" class="select" :disabled="loading" aria-label="每页计划数量" @change="changeSize"><option :value="20">20 项</option><option :value="50">50 项</option><option :value="100">100 项</option></select></label></div>
      <div><button class="btn" type="button" :disabled="loading || page <= 1" aria-label="上一页" @click="changePage(page - 1)"><ChevronLeft aria-hidden="true" /></button><span>第 {{ page }} / {{ pages }} 页</span><button class="btn" type="button" :disabled="loading || page >= pages" aria-label="下一页" @click="changePage(page + 1)"><ChevronRight aria-hidden="true" /></button></div>
    </nav>
  </div>
</template>

<style scoped>
.schedule-devices { min-width: 0; }
.schedule-heading { display: flex; align-items: flex-start; justify-content: space-between; flex-wrap: wrap; gap: 16px; margin-bottom: 20px; }
.schedule-heading .card-sub { margin-top: 6px; }
.schedule-search { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }
.schedule-search-field { display: flex; align-items: center; gap: 8px; flex: 1 1 240px; max-width: 480px; }
.schedule-search-field svg { width: 16px; height: 16px; flex: 0 0 auto; color: var(--muted); }
.schedule-search-field input { min-width: 0; width: 100%; }
.schedule-summary { margin: 16px 0; color: var(--muted); font-size: 12px; }
.schedule-table-wrap { overflow-x: auto; }
.schedule-table { width: 100%; border-collapse: collapse; text-align: left; font-size: 13px; font-variant-numeric: tabular-nums; }
.schedule-table th { padding: 12px; background: var(--panel-2); color: var(--muted); font-weight: 500; white-space: nowrap; }
.schedule-table td { padding: 14px 12px; border-bottom: 1px solid var(--line); vertical-align: top; white-space: nowrap; }
.schedule-table tr:hover td { background: var(--panel-2); }
.schedule-resource { display: grid; gap: 4px; min-width: 140px; max-width: 240px; white-space: normal; overflow-wrap: anywhere; }
.schedule-resource a { color: var(--blue); text-decoration: underline; text-underline-offset: 3px; }
.schedule-resource small, .schedule-muted { color: var(--muted); }
.schedule-error { display: block; min-width: 160px; max-width: 280px; white-space: normal; overflow-wrap: anywhere; color: var(--red, #b42318); font-size: 12px; line-height: 1.6; }
.schedule-empty { display: flex; align-items: center; justify-content: center; flex-direction: column; gap: 12px; min-height: 220px; color: var(--muted); text-align: center; font-size: 13px; }
.schedule-empty svg { width: 28px; height: 28px; }
.schedule-empty strong { color: var(--ink-strong); }
.schedule-pagination, .schedule-pagination > div, .schedule-pagination label { display: flex; align-items: center; flex-wrap: wrap; gap: 12px; }
.schedule-pagination { justify-content: space-between; margin-top: 20px; font-size: 12px; color: var(--muted); }
.schedule-pagination .select { width: auto; }
@media (max-width: 640px) { .schedule-pagination { align-items: flex-start; gap: 16px; } }
</style>
