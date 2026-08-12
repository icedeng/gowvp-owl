<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { Activity, HardDrive, LoaderCircle, RefreshCcw, Server, Settings2, ShieldAlert } from '@lucide/vue'
import { api, errorMessage, typeLabel } from '../services/api'
import type { ApiChannel, MediaServer, ResourceStats } from '../types/api'
import { formatBytes, relativeTime } from '../utils/format'
import { useUiStore } from '../stores/ui'
import ModalDialog from '../components/ModalDialog.vue'

const ui = useUiStore()
const loading = ref(false)
const saving = ref(false)
const loadError = ref('')
const rows = ref<MediaServer[]>([])
const channels = ref<ApiChannel[]>([])
const stats = ref<ResourceStats>({})
const editOpen = ref(false)
const editing = ref<MediaServer | null>(null)
const form = reactive({ type: 'zlm', ip: '', hook_ip: '', sdp_ip: '', secret: '' })
const disk = computed(() => stats.value.disk?.[0])
const diskPercent = computed(() => disk.value?.total ? Number(disk.value.used || 0) / disk.value.total * 100 : 0)
const streamCounts = computed(() => Object.fromEntries(['GB28181','ONVIF','RTMP','RTSP'].map((type) => [type, channels.value.filter((item) => typeLabel(item.type) === type && item.is_playing).length])))

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const [mediaResponse, channelResponse, statResponse] = await Promise.all([api.mediaServers({ page: 1, size: 100 }), api.channels({ page: 1, size: 99999 }), api.stats()])
    rows.value = mediaResponse.data.items || []
    channels.value = (channelResponse.data.items || []).map((item) => ({
      ...item,
      type: typeLabel(item.type, item.did || item.device_id || item.channel_id || item.id),
    }))
    stats.value = statResponse.data
  } catch (cause) { loadError.value = errorMessage(cause, '媒体节点加载失败') } finally { loading.value = false }
}

function edit(item: MediaServer) {
  editing.value = item
  Object.assign(form, { type: item.type || 'zlm', ip: item.ip || '', hook_ip: item.hook_ip || '', sdp_ip: item.sdp_ip || '', secret: '' })
  editOpen.value = true
}

async function save() {
  if (!editing.value) return
  saving.value = true
  try {
    await api.editMediaServer(editing.value.id, { type: form.type, ip: form.ip, hook_ip: form.hook_ip, sdp_ip: form.sdp_ip, secret: form.secret || editing.value.secret || '' })
    editOpen.value = false
    ui.toast(`媒体节点 ${editing.value.id} 已更新`)
    await load()
  } catch (cause) { ui.toast(errorMessage(cause, '媒体节点更新失败')) } finally { saving.value = false }
}

onMounted(load)
</script>

<template><main class="page-content"><header class="page-head"><div><h1 class="page-title">媒体节点</h1><p class="page-desc">查看当前媒体节点连接、端口、录像目录与活跃流摘要；HTTP API 只开放已有节点编辑。</p></div><div class="head-actions"><button class="btn" :disabled="loading" @click="load"><RefreshCcw :class="{ 'animate-spin': loading }" />刷新</button><RouterLink class="btn btn-primary" to="/system-status"><Activity />系统状态</RouterLink></div></header><div v-if="loadError" class="warning-box mb-4"><ShieldAlert /><span>{{ loadError }}</span><button class="btn btn-sm ml-auto" @click="load">重试</button></div><div class="warning-box mb-4"><ShieldAlert /><span>多节点新增和删除路由尚未开放，因此本页不提供“添加节点”或“删除节点”。</span></div><section class="grid three-col mb-4"><article v-for="item in rows" :key="item.id" class="card card-pad"><div class="card-head"><span class="details-icon"><Server /></span><span class="status" :class="item.status ? 'online' : 'offline'">{{ item.status ? '运行中' : '离线' }}</span></div><h2 class="section-title">{{ item.id }} · {{ item.type || '未知驱动' }}</h2><p class="mono text-slate-500">{{ item.ip || '—' }}:{{ item.ports?.http || '—' }}</p><dl class="definition-grid !grid-cols-1 mt-3"><div><dt>最近心跳</dt><dd>{{ relativeTime(item.last_keepalive_at) }}</dd></div><div><dt>RTP 端口</dt><dd>{{ item.rtpport_range || '—' }}</dd></div></dl></article><article v-if="!rows.length" class="card card-pad"><div class="card-head"><span class="details-icon"><Server /></span><span class="status offline">无节点</span></div><div class="empty-state"><LoaderCircle v-if="loading" class="mx-auto mb-2 animate-spin" />{{ loading ? '正在加载媒体节点…' : '当前环境没有媒体节点记录。' }}</div></article><article class="card card-pad"><div class="card-head"><div><h2 class="card-title">录像存储</h2><p class="card-sub">{{ rows[0]?.record_path || disk?.name || '—' }}</p></div><HardDrive /></div><div class="metric-value">{{ diskPercent.toFixed(1) }}%</div><div class="progress mt-4" :class="{ warn: diskPercent >= 70 }"><i :style="{ width: `${Math.min(100,diskPercent)}%` }" /></div><p class="section-note mt-3">{{ formatBytes(disk?.used) }} / {{ formatBytes(disk?.total) }}</p></article></section><section class="grid equal-col"><article class="card card-pad"><div class="card-head"><div><h2 class="card-title">节点连接配置</h2><p class="card-sub">Secret 不显示原值，修改时留空将保留当前值</p></div><Settings2 /></div><div v-for="item in rows" :key="item.id" class="mb-4"><dl class="definition-grid"><div><dt>节点</dt><dd>{{ item.id }}</dd></div><div><dt>类型</dt><dd>{{ item.type || '—' }}</dd></div><div><dt>HTTP 地址</dt><dd class="mono">{{ item.ip || '—' }}:{{ item.ports?.http || '—' }}</dd></div><div><dt>Hook IP</dt><dd class="mono">{{ item.hook_ip || '—' }}</dd></div><div><dt>SDP IP</dt><dd class="mono">{{ item.sdp_ip || '—' }}</dd></div><div><dt>Secret</dt><dd>••••••••</dd></div></dl><button class="btn mt-4" @click="edit(item)">编辑节点</button></div></article><article class="card card-pad"><div class="card-head"><div><h2 class="card-title">媒体会话摘要</h2><p class="card-sub">按通道 Hook 状态统计播放中的流</p></div><Activity /></div><div class="matrix"><div v-for="type in ['GB28181','RTMP','RTSP','ONVIF']" :key="type" class="matrix-slot"><span class="protocol-tag blue">{{ type }}</span><h3>{{ streamCounts[type] || 0 }} 路</h3><p>活跃播放</p></div></div></article></section><ModalDialog :open="editOpen" title="编辑媒体节点" description="保存会重新连接媒体服务，请确认地址与密钥正确。" @close="editOpen = false"><form class="form-grid" @submit.prevent="save"><label class="form-group"><span class="form-label">驱动类型</span><select v-model="form.type" class="select w-full"><option value="zlm">ZLMediaKit</option><option value="lalmax">Lalmax</option></select></label><label class="form-group"><span class="form-label">服务 IP</span><input v-model="form.ip" class="input plain w-full" required /></label><label class="form-group"><span class="form-label">Hook IP</span><input v-model="form.hook_ip" class="input plain w-full" required /></label><label class="form-group"><span class="form-label">SDP IP</span><input v-model="form.sdp_ip" class="input plain w-full" required /></label><label class="form-group full"><span class="form-label">新 Secret</span><input v-model="form.secret" class="input plain w-full" type="password" autocomplete="new-password" placeholder="留空保留当前值" /></label><div class="modal-foot full"><button type="button" class="btn" @click="editOpen = false">取消</button><button class="btn btn-primary" :disabled="saving"><LoaderCircle v-if="saving" class="animate-spin" />{{ saving ? '正在保存…' : '保存配置' }}</button></div></form></ModalDialog></main></template>
