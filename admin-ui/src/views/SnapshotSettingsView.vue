<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { LoaderCircle, Save } from '@lucide/vue'
import { api, errorMessage } from '../services/api'
import { useUiStore } from '../stores/ui'
import SnapshotWorkspace from '../components/SnapshotWorkspace.vue'
const ui = useUiStore()
const form = reactive({ upload_timeout_seconds: 120, ffmpeg_concurrency: 2 })
const loading = ref(true)
const saving = ref(false)
const ready = ref(false)
const error = ref('')
async function load() {
  loading.value = true
  try {
    const { data } = await api.configInfo()
    if (!data.snapshot) throw new Error('服务未返回抓拍配置，请确认后端版本')
    Object.assign(form, data.snapshot)
    ready.value = true
    error.value = ''
  } catch (cause) { error.value = errorMessage(cause, '抓拍配置读取失败') }
  finally { loading.value = false }
}
async function save() {
  saving.value = true
  error.value = ''
  try {
    Object.assign(form, (await api.updateSnapshotConfig(form)).data)
    ui.toast('抓拍配置已保存，新任务立即生效')
  } catch (cause) { error.value = errorMessage(cause, '抓拍配置保存失败') }
  finally { saving.value = false }
}
onMounted(load)
</script>

<template>
  <div>
    <div class="page-head"><div><h1 class="page-title">抓拍设置</h1><p class="page-desc">配置设备上传等待与取帧并发，查看所有通道保存的抓拍图片。</p></div></div>
    <section class="card form-section capture-settings">
      <div class="card-head"><div><h2 class="card-title">抓拍执行</h2><p class="card-sub">根据协议与能力选择设备上传或实时流取帧，失败时记录原因。</p></div></div>
      <p v-if="error" class="warning-box" role="alert">{{ error }} <button v-if="!ready" class="btn btn-sm" type="button" :disabled="loading" @click="load">重试</button></p>
      <form class="snapshot-settings-form" @submit.prevent="save">
        <label class="form-group"><span class="form-label">设备图片上传等待（秒）</span><input v-model.number="form.upload_timeout_seconds" class="input plain" type="number" min="1" max="3600" required :disabled="!ready || loading || saving" /><small>默认 120 秒，适用于支持抓拍上传的 GB28181-2022 设备。</small></label>
        <label class="form-group"><span class="form-label">FFmpeg 最大并发数</span><input v-model.number="form.ffmpeg_concurrency" class="input plain" type="number" min="1" max="32" required :disabled="!ready || loading || saving" /><small>默认 2 路；其余取帧任务排队等待，范围 1–32。</small></label>
        <button class="btn btn-primary" type="submit" :disabled="!ready || loading || saving"><LoaderCircle v-if="loading || saving" class="animate-spin" /><Save v-else />{{ saving ? '保存中' : '保存配置' }}</button>
      </form>
      <p class="capture-settings-note">保存后用于新任务，无需重启。调低并发时，已执行的取帧会完成后再接新任务。定时抓拍在各通道的“抓拍与计划”中配置。</p>
    </section>
    <section class="card form-section"><SnapshotWorkspace /></section>
  </div>
</template>

<style scoped>
.capture-settings { margin-bottom: 24px; }
.snapshot-settings-form { display: flex; flex-wrap: wrap; gap: 20px; align-items: flex-end; }
.snapshot-settings-form .form-group { flex: 1 1 250px; max-width: 460px; margin: 0; }
.capture-settings-note { margin-top: 18px; color: var(--muted); font-size: 13px; }
@media (max-width: 640px) { .snapshot-settings-form .form-group { flex-basis: 100%; max-width: none; } }
</style>
