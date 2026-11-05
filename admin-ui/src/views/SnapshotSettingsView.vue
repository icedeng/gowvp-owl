<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { LoaderCircle, Save } from '@lucide/vue'
import { api, errorMessage } from '../services/api'
import type { SnapshotConfig } from '../types/api'
import { useUiStore } from '../stores/ui'
import { defaultSnapshotConfig, useSnapshotStore } from '../stores/snapshot'
import SnapshotWorkspace from '../components/SnapshotWorkspace.vue'
import CaptureScheduleDevices from '../components/CaptureScheduleDevices.vue'
const ui = useUiStore()
const store = useSnapshotStore()
const form = reactive<SnapshotConfig>({ ...defaultSnapshotConfig })
const loading = ref(true)
const saving = ref(false)
const ready = ref(false)
const error = ref('')
const route = useRoute()
const router = useRouter()
const tabs = [{ id: 'history', label: '抓拍历史' }, { id: 'schedules', label: '定时抓拍设备' }, { id: 'settings', label: '设置配置' }] as const
type SnapshotTab = typeof tabs[number]['id']
const activeTab = computed<SnapshotTab>(() => route.query.tab === 'settings' ? 'settings' : route.query.tab === 'schedules' ? 'schedules' : 'history')
const historyVisited = ref(false)
watch(activeTab, tab => { if (tab === 'history') historyVisited.value = true }, { immediate: true })
async function selectTab(tab: SnapshotTab) {
  await router.replace({ query: { ...route.query, tab } })
}
async function onTabKeydown(event: KeyboardEvent, index: number) {
  let next = index
  if (event.key === 'ArrowRight') next = (index + 1) % tabs.length
  else if (event.key === 'ArrowLeft') next = (index - 1 + tabs.length) % tabs.length
  else if (event.key === 'Home') next = 0
  else if (event.key === 'End') next = tabs.length - 1
  else return
  event.preventDefault()
  await selectTab(tabs[next]!.id)
  await nextTick()
  document.getElementById(`snapshot-tab-${activeTab.value}`)?.focus()
}
type NumericKey = { [K in keyof SnapshotConfig]: SnapshotConfig[K] extends number ? K : never }[keyof SnapshotConfig]
type Field = { key: NumericKey; label: string; min: number; max: number; hint: string }
const executionFields: Field[] = [
  { key: 'upload_timeout_seconds', label: '设备图片上传等待（秒）', min: 1, max: 3600, hint: '默认 120 秒，仅用于支持图片上传的 GB28181-2022 设备。' },
  { key: 'ffmpeg_concurrency', label: 'FFmpeg 最大并发数', min: 1, max: 32, hint: '默认 2 路；其余任务排队，排队不消耗取流超时。' },
  { key: 'scan_interval_ms', label: '队列扫描间隔（ms）', min: 100, max: 60000, hint: '默认 1000ms（1 秒），也作为立即执行模式的漏通知兜底。' },
  { key: 'poll_interval_ms', label: '管理端结果轮询间隔（ms）', min: 200, max: 60000, hint: '默认 2000ms；可设为 500ms。仅影响结果展示速度，弱网失败时自动延长间隔。' },
]
const multiFrameFields: Field[] = [
  { key: 'frame_count', label: '候选图片数量', min: 2, max: 30, hint: '默认最多生成 5 张，按顺序选择有效图片；最终只保存一张。' },
  { key: 'skip_frames', label: '跳过起始帧数', min: 1, max: 1000, hint: '默认跳过前 75 帧；数值越大，取图前等待越长。' },
  { key: 'frame_interval_ms', label: '候选帧采样间隔（ms）', min: 40, max: 10000, hint: '默认 1000ms，即每秒采样 1 帧。低帧率设备可能需要更长等待。' },
]
const advancedFields: Field[] = [
  { key: 'stream_timeout_seconds', label: '取流任务总超时（秒）', min: 1, max: 3600, hint: '默认 60 秒，包含起流、探测、取帧及保存；不含排队。各步骤均受此上限约束。' },
  { key: 'ffmpeg_timeout_seconds', label: 'FFmpeg 取帧超时（秒）', min: 1, max: 3600, hint: '默认 35 秒，不含多帧模式的 ffprobe 探测时间。不能大于任务总超时。' },
  { key: 'rtsp_timeout_seconds', label: 'RTSP 读写超时（秒）', min: 1, max: 3600, hint: '默认 10 秒。FFmpeg 到媒体节点使用 TCP，不改变设备到节点的传输协议。' },
  { key: 'probe_timeout_seconds', label: '视频流探测超时（秒）', min: 1, max: 3600, hint: '默认 10 秒，仅多帧模式执行 ffprobe；不能大于任务总超时。' },
  { key: 'analyze_duration_ms', label: '视频分析预算（ms）', min: 100, max: 60000, hint: '默认 5000ms，是分析上限而非固定等待；调小可能影响弱网和低帧率设备。' },
  { key: 'probe_size_bytes', label: '视频探测预算（字节）', min: 32768, max: 100000000, hint: '默认 5000000 字节。过小可能无法识别参数不完整的视频流。' },
  { key: 'jpeg_quality', label: 'JPEG 质量参数', min: 2, max: 31, hint: '默认 2；数值越小质量越高、文件通常越大。仅影响 FFmpeg 输出。' },
]
async function load() {
  loading.value = true
  try {
    Object.assign(form, await store.load())
    ready.value = true
    error.value = ''
  } catch (cause) { error.value = errorMessage(cause, '抓拍配置读取失败') }
  finally { loading.value = false }
}
async function save() {
  saving.value = true
  error.value = ''
  try {
    const { data } = await api.updateSnapshotConfig(form)
    Object.assign(form, data)
    store.apply(data)
    ui.toast('抓拍配置已保存')
  } catch (cause) { error.value = errorMessage(cause, '抓拍配置保存失败') }
  finally { saving.value = false }
}
onMounted(load)
</script>

<template>
  <main class="page-content snapshot-settings-page">
    <div class="page-head"><div><h1 class="page-title">抓拍设置</h1><p class="page-desc">配置任务调度、取帧策略和结果查询，查看所有通道保存的抓拍图片。</p></div></div>
    <div class="card snapshot-workbench">
      <div class="resource-detail-nav snapshot-tabs" role="tablist" aria-label="抓拍管理">
        <button v-for="(tab, index) in tabs" :id="`snapshot-tab-${tab.id}`" :key="tab.id" type="button" role="tab" :class="{ active: activeTab === tab.id }" :aria-selected="activeTab === tab.id" :aria-controls="`snapshot-panel-${tab.id}`" :tabindex="activeTab === tab.id ? 0 : -1" @click="selectTab(tab.id)" @keydown="onTabKeydown($event, index)">{{ tab.label }}</button>
      </div>
    <section v-show="activeTab === 'settings'" id="snapshot-panel-settings" class="snapshot-panel capture-settings" role="tabpanel" aria-labelledby="snapshot-tab-settings" tabindex="0">
      <p v-if="error" class="warning-box" role="alert">{{ error }} <button v-if="!ready" class="btn btn-sm" type="button" :disabled="loading" @click="load">重试</button></p>
      <p v-if="loading" class="snapshot-loading" role="status"><LoaderCircle class="animate-spin" aria-hidden="true" />正在读取抓拍配置…</p>
      <form class="snapshot-settings-form" :aria-busy="loading || saving" @submit.prevent="save">
        <div class="snapshot-settings-body">
        <fieldset :disabled="!ready || loading || saving">
          <legend>任务调度与结果查询</legend>
          <p class="section-description">默认每秒扫描队列；浏览器只查询后台任务状态，离开页面不会取消抓拍。</p>
          <div class="capture-dispatch-setting">
            <label class="capture-setting-toggle"><span>入队立即执行</span><span class="switch"><input v-model="form.immediate_dispatch" type="checkbox" role="switch" aria-describedby="capture-dispatch-help" /><span class="slider" /></span></label>
            <p id="capture-dispatch-help" class="field-note">默认关闭，等待下一轮扫描。开启后立即通知执行，扫描周期仍用于兜底；并发已满时继续排队。</p>
          </div>
          <div class="snapshot-field-grid">
            <label v-for="field in executionFields" :key="field.key" class="form-group"><span class="form-label">{{ field.label }}</span><input v-model.number="form[field.key]" class="input plain" type="number" :min="field.min" :max="field.max" step="1" required :aria-describedby="`capture-${field.key}-help`" /><small :id="`capture-${field.key}-help`">{{ field.hint }}</small></label>
          </div>
        </fieldset>
        <fieldset :disabled="!ready || loading || saving">
          <legend>实时流取帧方式</legend>
          <p class="section-description">按协议直接选择设备上传或 FFmpeg。以下模式用于 FFmpeg 取帧，不改变设备上传流程。</p>
          <label class="form-group mode-field"><span class="form-label">FFmpeg 取帧模式</span><select v-model="form.ffmpeg_mode" class="select" aria-describedby="capture-mode-help"><option value="multi_frame">多帧兼容模式（默认）</option><option value="single_frame">单帧快速模式</option></select><small id="capture-mode-help">{{ form.ffmpeg_mode === 'multi_frame' ? '沿用原多帧处理：先探测视频流，再跳过起始帧并采样候选图片，耗时相对较长。' : '只协商视频，以单线程解码取首帧，不预先运行 ffprobe；速度受设备起流、关键帧和网络状态影响。' }}</small></label>
          <div v-if="form.ffmpeg_mode === 'multi_frame'" class="snapshot-field-grid">
            <label v-for="field in multiFrameFields" :key="field.key" class="form-group"><span class="form-label">{{ field.label }}</span><input v-model.number="form[field.key]" class="input plain" type="number" :min="field.min" :max="field.max" step="1" required :aria-describedby="`capture-${field.key}-help`" /><small :id="`capture-${field.key}-help`">{{ field.hint }}</small></label>
          </div>
        </fieldset>
        </div>
        <details class="capture-advanced">
          <summary>高级参数 · 超时、探测与图片质量</summary>
          <fieldset :disabled="!ready || loading || saving">
            <legend class="sr-only">超时、探测与图片质量</legend>
            <p class="section-description">弱网下可按实际耗时调整等待预算。缩短超时或减小探测预算，可能降低抓拍成功率；持续断流仍可能失败。</p>
            <div class="snapshot-field-grid">
              <label v-for="field in advancedFields" :key="field.key" class="form-group"><span class="form-label">{{ field.label }}</span><input v-model.number="form[field.key]" class="input plain" type="number" :min="field.min" :max="field.max" step="1" required :disabled="field.key === 'probe_timeout_seconds' && form.ffmpeg_mode !== 'multi_frame'" :title="field.key === 'probe_timeout_seconds' && form.ffmpeg_mode !== 'multi_frame' ? '单帧模式不运行 ffprobe' : undefined" :aria-describedby="`capture-${field.key}-help`" /><small :id="`capture-${field.key}-help`">{{ field.hint }}</small></label>
            </div>
          </fieldset>
        </details>
        <div class="capture-settings-footer"><p>保存到当前节点配置文件，无需重启。执行参数用于后续任务；扫描和本管理端轮询立即更新。定时计划在各通道中配置。</p><button class="btn btn-primary" type="submit" :disabled="!ready || loading || saving"><LoaderCircle v-if="loading || saving" class="animate-spin" aria-hidden="true" /><Save v-else aria-hidden="true" />{{ saving ? '保存中' : '保存配置' }}</button></div>
      </form>
    </section>
    <section v-show="activeTab === 'history'" id="snapshot-panel-history" class="snapshot-panel snapshot-history" role="tabpanel" aria-labelledby="snapshot-tab-history" tabindex="0"><SnapshotWorkspace v-if="historyVisited" /></section>
    <section v-if="activeTab === 'schedules'" id="snapshot-panel-schedules" class="snapshot-panel" role="tabpanel" aria-labelledby="snapshot-tab-schedules" tabindex="0"><CaptureScheduleDevices /></section>
    </div>
  </main>
</template>

<style scoped>
.snapshot-workbench { min-width: 0; }
.snapshot-tabs button { font-weight: 600; }
.snapshot-panel { min-width: 0; padding: 16px; }
.snapshot-panel:focus-visible { outline: 2px solid var(--blue); outline-offset: -2px; }
.snapshot-settings-form { display: grid; gap: 20px; }
.snapshot-settings-body { display: grid; grid-template-columns: minmax(0, 1fr); gap: 24px; }
.snapshot-loading { display: flex; align-items: center; gap: 8px; margin: 0 0 16px; color: var(--muted); font-size: 13px; }
.snapshot-loading svg { width: 16px; height: 16px; }
.snapshot-settings-form fieldset { min-width: 0; padding: 0; margin: 0; border: 0; }
.snapshot-settings-body > fieldset + fieldset { padding-top: 20px; border-top: 1px solid var(--line); }
.snapshot-settings-form legend { margin-bottom: 6px; padding: 0; color: var(--ink-strong); font-size: 14px; font-weight: 600; }
.section-description, .field-note, .snapshot-settings-form small { color: var(--muted); font-size: 12px; line-height: 1.6; }
.section-description { margin: 0 0 16px; }
.capture-dispatch-setting { margin-bottom: 16px; padding: 12px; background: var(--panel-2); border-radius: var(--radius-sm); }
.capture-setting-toggle { display: flex; align-items: center; justify-content: space-between; gap: 12px; min-height: var(--control-height); font-size: 13px; cursor: pointer; }
.capture-setting-toggle input:disabled + .slider { opacity: .6; cursor: not-allowed; }
.field-note { margin: 4px 0 0; }
.snapshot-field-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; }
.snapshot-field-grid .form-group, .mode-field { margin: 0; }
.mode-field { margin-bottom: 16px; }
.capture-advanced { border-top: 1px solid var(--line); padding-top: 4px; }
.capture-advanced summary { padding-block: 12px; color: var(--ink-strong); font-size: 13px; font-weight: 600; cursor: pointer; }
.capture-advanced[open] summary { margin-bottom: 4px; }
.sr-only { position: absolute; width: 1px; height: 1px; padding: 0; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
.capture-settings-footer { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 16px; padding-top: 16px; border-top: 1px solid var(--line); }
.capture-settings-footer p { flex: 1 1 360px; margin: 0; color: var(--muted); font-size: 12px; line-height: 1.6; }
.capture-settings-footer > .btn { flex: 0 0 auto; margin-left: auto; }
@media (min-width: 1280px) {
  .snapshot-settings-body { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 32px; }
  .snapshot-settings-body > fieldset + fieldset { padding: 0 0 0 32px; border-top: 0; border-left: 1px solid var(--line); }
}
@media (max-width: 640px) {
  .snapshot-panel { padding: 12px; }
  .snapshot-field-grid { grid-template-columns: minmax(0, 1fr); }
  .capture-settings-footer { align-items: stretch; }
  .capture-settings-footer > .btn { width: 100%; margin-left: 0; }
}
</style>
