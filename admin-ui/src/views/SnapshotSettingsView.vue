<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { LoaderCircle, Save } from '@lucide/vue'
import { api, errorMessage } from '../services/api'
import type { SnapshotConfig } from '../types/api'
import { useUiStore } from '../stores/ui'
import { defaultSnapshotConfig, useSnapshotStore } from '../stores/snapshot'
import SnapshotWorkspace from '../components/SnapshotWorkspace.vue'
const ui = useUiStore()
const store = useSnapshotStore()
const form = reactive<SnapshotConfig>({ ...defaultSnapshotConfig })
const loading = ref(true)
const saving = ref(false)
const ready = ref(false)
const error = ref('')
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
  <div>
    <div class="page-head"><div><h1 class="page-title">抓拍设置</h1><p class="page-desc">配置任务调度、取帧策略和结果查询，查看所有通道保存的抓拍图片。</p></div></div>
    <section class="card form-section capture-settings">
      <p v-if="error" class="warning-box" role="alert">{{ error }} <button v-if="!ready" class="btn btn-sm" type="button" :disabled="loading" @click="load">重试</button></p>
      <form class="snapshot-settings-form" @submit.prevent="save">
        <fieldset :disabled="!ready || loading || saving">
          <legend>任务调度与结果查询</legend>
          <p class="section-description">默认每秒扫描队列；浏览器只查询后台任务状态，离开页面不会取消抓拍。</p>
          <label class="capture-setting-toggle"><span>入队立即执行</span><span class="switch"><input v-model="form.immediate_dispatch" type="checkbox" role="switch" /><span class="slider" /></span></label>
          <p class="field-note">默认关闭，等待下一轮扫描。开启后立即通知执行，扫描周期仍用于兜底；并发已满时继续排队。</p>
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
        <div class="capture-settings-footer"><p>保存到当前节点配置文件，无需重启。执行参数用于后续任务；扫描和本管理端轮询立即更新。定时计划在各通道中配置。</p><button class="btn btn-primary" type="submit" :disabled="!ready || loading || saving"><LoaderCircle v-if="loading || saving" class="animate-spin" /><Save v-else />{{ saving ? '保存中' : '保存配置' }}</button></div>
      </form>
    </section>
    <section class="card form-section"><SnapshotWorkspace /></section>
  </div>
</template>

<style scoped>
.capture-settings { margin-bottom: 24px; }
.snapshot-settings-form { display: grid; gap: 24px; }
.snapshot-settings-form fieldset { min-width: 0; padding: 0; margin: 0; border: 0; }
.snapshot-settings-form > fieldset + fieldset { padding-top: 24px; border-top: 1px solid var(--line); }
.snapshot-settings-form legend { margin-bottom: 8px; padding: 0; color: var(--ink-strong); font-size: 16px; font-weight: 700; }
.section-description, .field-note, .snapshot-settings-form small { color: var(--muted); font-size: 12px; line-height: 1.6; }
.section-description { margin: 0 0 20px; }
.capture-setting-toggle { display: inline-flex; align-items: center; gap: 16px; min-height: var(--control-height); font-size: 13px; cursor: pointer; }
.field-note { margin: 6px 0 20px; }
.snapshot-field-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 20px 24px; }
.snapshot-field-grid .form-group, .mode-field { margin: 0; }
.mode-field { max-width: 600px; margin-bottom: 20px; }
.capture-advanced { border: 1px solid var(--line); border-radius: var(--radius-sm); padding: 16px; }
.capture-advanced summary { color: var(--ink-strong); font-weight: 600; cursor: pointer; }
.capture-advanced[open] summary { margin-bottom: 20px; }
.sr-only { position: absolute; width: 1px; height: 1px; padding: 0; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
.capture-settings-footer { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 16px; padding-top: 20px; border-top: 1px solid var(--line); }
.capture-settings-footer p { flex: 1 1 360px; margin: 0; color: var(--muted); font-size: 12px; line-height: 1.6; }
@media (max-width: 720px) { .snapshot-field-grid { grid-template-columns: minmax(0, 1fr); } }
</style>
