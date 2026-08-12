<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { Aperture, ListFilter, LoaderCircle, Radio, RefreshCcw, Search, ShieldAlert, Video, X } from '@lucide/vue'
import { api, errorMessage, typeLabel } from '../services/api'
import type { ApiChannel, ApiDevice } from '../types/api'
import { useUiStore } from '../stores/ui'

const ui = useUiStore()
const query = ref('')
const type = ref('all')
const status = ref('all')
const loading = ref(false)
const loadError = ref('')
const rows = ref<ApiChannel[]>([])
const devices = ref<ApiDevice[]>([])
const total = ref(0)
const snapshotLoading = ref('')

const deviceNames = computed(() => Object.fromEntries(devices.value.map((item) => [item.id, item.name || item.device_id || item.id])))
const filtered = computed(() => rows.value.filter((item) => {
  const name = deviceNames.value[item.did || ''] || item.device_id || ''
  const matchText = `${item.name || ''}${item.id}${item.channel_id || ''}${name}`.toLowerCase().includes(query.value.toLowerCase())
  const matchType = type.value === 'all' || typeLabel(item.type) === type.value
  const matchStatus = status.value === 'all' || (status.value === 'online' ? item.is_online : !item.is_online)
  return matchText && matchType && matchStatus
}))
const onlineCount = computed(() => rows.value.filter((item) => item.is_online).length)
const aiCount = computed(() => rows.value.filter((item) => item.ext?.enabled_ai).length)
const recordingCount = computed(() => rows.value.filter((item) => (item.ext?.record_mode || 'always') !== 'none').length)
const hasFilters = computed(() => Boolean(query.value || type.value !== 'all' || status.value !== 'all'))

function resetFilters() {
  query.value = ''
  type.value = 'all'
  status.value = 'all'
}

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const [channelResponse, deviceResponse] = await Promise.all([
      api.channels({ page: 1, size: 99999 }),
      api.devices({ page: 1, size: 99999 }),
    ])
    rows.value = (channelResponse.data.items || []).map((item) => ({
      ...item,
      type: typeLabel(item.type, item.did || item.device_id || item.channel_id || item.id),
    }))
    total.value = channelResponse.data.total || rows.value.length
    devices.value = deviceResponse.data.items || []
  } catch (cause) {
    loadError.value = errorMessage(cause, '通道列表加载失败')
  } finally {
    loading.value = false
  }
}

async function refreshSnapshot(channel: ApiChannel) {
  snapshotLoading.value = channel.id
  try {
    const { data } = await api.snapshot(channel.id)
    ui.toast(`快照已刷新${data.method ? ` · ${data.method}` : ''}`)
  } catch (cause) {
    ui.toast(errorMessage(cause, '快照刷新失败'))
  } finally {
    snapshotLoading.value = ''
  }
}

onMounted(load)
</script>

<template>
  <main class="page-content"><header class="page-head"><div><h1 class="page-title">通道管理</h1><p class="page-desc">统一查看实时状态、PTZ 能力、AI 与录像模式；通道能力来自当前后端记录。</p></div><div class="head-actions"><RouterLink class="btn" to="/devices">设备管理</RouterLink><button class="btn" :disabled="loading" @click="load"><RefreshCcw :class="{ 'animate-spin': loading }" />刷新</button><RouterLink class="btn btn-primary" to="/live"><Video />实时监控</RouterLink></div></header>
    <div v-if="loadError" class="warning-box mb-4"><ShieldAlert /><span>{{ loadError }}</span><button class="btn btn-sm ml-auto" @click="load">重试</button></div>
    <section class="metric-line mb-4"><div class="metric-item"><div class="metric-label"><span>全部通道</span><Radio /></div><div class="metric-value">{{ total }}</div><div class="metric-foot">来自 {{ devices.length }} 台设备</div></div><div class="metric-item"><div class="metric-label"><span>在线</span><Radio /></div><div class="metric-value">{{ onlineCount }}</div><div class="metric-foot">{{ rows.length ? `${(onlineCount / rows.length * 100).toFixed(1)}% 可用` : '暂无通道' }}</div></div><div class="metric-item"><div class="metric-label"><span>AI 分析</span><Radio /></div><div class="metric-value">{{ aiCount }}</div><div class="metric-foot">已启用通道</div></div><div class="metric-item"><div class="metric-label"><span>录像开启</span><Radio /></div><div class="metric-value">{{ recordingCount }}</div><div class="metric-foot">always / ai 模式</div></div></section>
    <section class="card table-card"><div class="toolbar"><label class="field"><Search /><input v-model="query" class="input" aria-label="搜索通道" placeholder="搜索通道、编码或设备" /></label><select v-model="type" class="select" aria-label="按协议筛选"><option value="all">全部协议</option><option>GB28181</option><option>ONVIF</option><option>RTMP</option><option>RTSP</option></select><select v-model="status" class="select" aria-label="按在线状态筛选"><option value="all">全部状态</option><option value="online">在线</option><option value="offline">离线</option></select><button v-if="hasFilters" type="button" class="btn btn-sm filter-reset" @click="resetFilters"><X />清除筛选</button><span class="toolbar-spacer" /><span class="section-note" aria-live="polite">当前显示 {{ filtered.length }} / {{ total }} 路</span></div><p class="table-scroll-hint">左右滑动查看完整通道能力</p><div class="table-wrap"><table class="data-table"><thead><tr><th>通道</th><th>所属设备</th><th>协议</th><th>在线 / 播放</th><th>PTZ 能力</th><th>AI</th><th>录像模式</th><th>操作</th></tr></thead><tbody><tr v-for="channel in filtered" :key="channel.id"><td><div class="row-title"><span class="device-glyph" :class="{ off: !channel.is_online }"><Radio /></span><span><strong>{{ channel.name || '未命名通道' }}</strong><small>{{ channel.channel_id || channel.id }}</small></span></div></td><td>{{ deviceNames[channel.did || ''] || channel.device_id || '—' }}</td><td><span class="protocol-tag blue">{{ typeLabel(channel.type) }}</span></td><td><div class="button-row"><span class="status" :class="channel.is_online ? 'online' : 'offline'">{{ channel.is_online ? '在线' : '离线' }}</span><span class="status" :class="channel.is_playing ? 'info' : ''">{{ channel.is_playing ? 'LIVE' : '空闲' }}</span></div></td><td>{{ channel.ptz_verified ? '已验证' : channel.ptz_capable ? '声明支持' : '不支持' }}</td><td>{{ channel.ext?.enabled_ai ? '已启用' : '未启用' }}</td><td>{{ ({ always: '持续录像', ai: 'AI 录像', none: '不录制' } as Record<string,string>)[channel.ext?.record_mode || 'always'] || channel.ext?.record_mode }}</td><td><div class="row-actions"><button class="more-btn" :disabled="snapshotLoading === channel.id" aria-label="刷新快照" @click="refreshSnapshot(channel)"><LoaderCircle v-if="snapshotLoading === channel.id" class="animate-spin" /><Aperture v-else /></button><RouterLink class="btn btn-sm" :to="`/channels/${encodeURIComponent(channel.id)}`">详情</RouterLink><RouterLink class="btn btn-sm" :to="`/live?channel=${encodeURIComponent(channel.id)}`">预览</RouterLink></div></td></tr></tbody></table><div v-if="loading" class="empty-state"><LoaderCircle class="mx-auto mb-3 h-6 w-6 animate-spin" />正在加载通道…</div><div v-else-if="!filtered.length" class="empty-state empty-action"><ListFilter /><strong>{{ rows.length ? '没有符合当前条件的通道' : '当前环境尚无通道数据' }}</strong><span>{{ rows.length ? '清除筛选后可恢复全部通道。' : '通道由接入设备、RTMP 推流或 RTSP 拉流创建。' }}</span><div class="button-row"><button v-if="rows.length" class="btn" @click="resetFilters">清除筛选</button><RouterLink v-else class="btn btn-primary" to="/devices">前往设备管理</RouterLink></div></div></div><div class="pagination"><span>已加载全部 {{ rows.length }} 条通道记录</span><span class="section-note">筛选在当前结果内即时生效</span></div></section>
  </main>
</template>
