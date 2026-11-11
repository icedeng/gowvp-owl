<script setup lang="ts">
import { computed, onMounted, ref, shallowRef } from 'vue'
import { Copy, Eye, EyeOff, RefreshCw } from '@lucide/vue'
import { api, errorMessage } from '../../../services/api'
import type { SipAccessInfo } from '../../../types/api'
import { copyText } from '../../../utils/clipboard'
import { useUiStore } from '../../../stores/ui'
import type { Gateway } from '../types'

const props = defineProps<{ gateway: Gateway }>()
const ui = useUiStore()
const info = shallowRef<SipAccessInfo>()
const loading = ref(false)
const error = ref('')
const reveal = ref(false)
const applied = computed(() => props.gateway.applied_config)
const address = computed(() => typeof applied.value?.public_ip === 'string' ? applied.value.public_ip : '')
const port = computed(() => typeof applied.value?.sip_port === 'number' ? applied.value.sip_port : undefined)
const transport = computed(() => [
  props.gateway.capabilities.includes('sip_udp') ? 'UDP' : '',
  props.gateway.capabilities.includes('sip_tcp') ? 'TCP' : '',
].filter(Boolean).join(' / '))
const fields = computed(() => [
  { label: 'SIP 服务器地址', value: address.value },
  { label: 'SIP 服务器端口', value: port.value ? String(port.value) : '' },
  { label: 'SIP 服务器 ID', value: info.value?.id || '' },
  { label: 'SIP 服务器域', value: info.value?.domain || '' },
  { label: '信令传输协议', value: transport.value },
])
const canCopy = computed(() => !loading.value && !error.value && fields.value.every(field => field.value))
async function load() {
  if (loading.value) return
  loading.value = true; error.value = ''; reveal.value = false; info.value = undefined
  try {
    const result = (await api.configInfo()).data.access_info
    if (!result) throw new Error('服务器未返回国标接入信息')
    info.value = result
  }
  catch (cause) { error.value = errorMessage(cause, '国标接入信息读取失败，请重试') }
  finally { loading.value = false }
}
async function copy(value: string) {
  try { await copyText(value); ui.toast('已复制') }
  catch { ui.toast('复制失败，请选中文本手动复制') }
}
function copyParameters() {
  void copy(fields.value.map(field => `${field.label}：${field.value}`).join('\n'))
}
onMounted(load)
</script>

<template>
  <section class="access-section" aria-labelledby="gateway-access-title">
    <header class="access-head">
      <div><h2 id="gateway-access-title">国标设备接入信息</h2><p>在设备的 GB/T 28181 平台配置中填写以下参数。</p></div>
      <div class="access-actions">
        <button class="btn btn-sm" :disabled="loading" @click="load"><RefreshCw :class="{ 'animate-spin': loading }" aria-hidden="true" />{{ loading ? '读取中…' : '刷新参数' }}</button>
        <button class="btn btn-sm" :disabled="!canCopy" @click="copyParameters"><Copy aria-hidden="true" />复制接入参数</button>
      </div>
    </header>
    <p v-if="error" class="warning-box" role="alert">{{ error }}，可点击“刷新参数”重试。</p>
    <p v-if="!applied" class="warning-box">网关尚未上报已应用配置，服务器地址和端口暂不可用。请完成配置下发后重试。</p>
    <p v-else-if="gateway.config_revision !== gateway.applied_revision" class="warning-box">配置变更待应用，下方地址与端口来自已应用版本 {{ gateway.applied_revision }}。期望配置请在“配置”中查看。</p>
    <p v-if="gateway.stale || gateway.connection_state !== 'connected' || gateway.admin_state !== 'enabled' || gateway.health_state !== 'ready'" class="access-note">当前网关尚未就绪或状态已过期，以下参数仅供核对；恢复连接并确认就绪后再接入设备。</p>
    <dl class="access-grid">
      <div v-for="field in fields" :key="field.label"><dt>{{ field.label }}</dt><dd><span class="mono">{{ field.value || (loading ? '读取中…' : '—') }}</span><button v-if="field.value" class="access-copy" :aria-label="`复制${field.label}`" :title="`复制${field.label}`" @click="copy(field.value)"><Copy aria-hidden="true" /></button></dd></div>
      <div><dt>注册密码</dt><dd><span class="mono">{{ loading ? '读取中…' : info?.password ? (reveal ? info.password : '••••••') : info ? '未配置' : '—' }}</span><button v-if="info?.password" class="access-copy" :aria-label="reveal ? '隐藏注册密码' : '显示注册密码'" :title="reveal ? '隐藏密码' : '显示密码'" :aria-pressed="reveal" @click="reveal = !reveal"><EyeOff v-if="reveal" aria-hidden="true" /><Eye v-else aria-hidden="true" /></button></dd></div>
    </dl>
    <p class="access-note">设备编码使用设备自身的 20 位国标 ID。服务器 ID、域和注册密码与 A 端一致，服务器地址与端口使用本网关；复制内容不包含密码。</p>
  </section>
</template>

<style scoped>
.access-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; margin-bottom: 12px; }
.access-head h2 { margin: 0; font-size: 14px; color: var(--ink-strong); }.access-head p, .access-note { margin: 5px 0 0; color: var(--muted); font-size: 12px; line-height: 1.7; }
.access-actions { display: flex; flex-wrap: wrap; gap: 8px; }.access-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); margin: 0; }
.access-grid > div { display: grid; grid-template-columns: 132px minmax(0, 1fr); align-items: center; gap: 10px; padding: 9px 12px 9px 0; border-bottom: 1px solid var(--line); font-size: 13px; }
.access-grid dt { color: var(--muted); }.access-grid dd { display: flex; align-items: center; gap: 6px; min-width: 0; margin: 0; }.access-grid dd > span { overflow-wrap: anywhere; }
.access-copy { display: inline-flex; flex: 0 0 auto; align-items: center; justify-content: center; width: 28px; height: 28px; padding: 0; color: var(--muted); background: transparent; border: 0; border-radius: var(--radius-sm); cursor: pointer; }.access-copy:hover { color: var(--blue); background: var(--blue-soft); }.access-copy svg { width: 14px; height: 14px; }
.access-note { margin-top: 12px; }.warning-box { margin: 0 0 12px; }
@media (max-width: 1100px) { .access-grid { grid-template-columns: 1fr; } }
@media (max-width: 640px) { .access-head { flex-direction: column; }.access-grid > div { grid-template-columns: 114px minmax(0, 1fr); padding-right: 0; }.access-copy { width: 44px; height: 44px; }.access-actions .btn { min-height: 44px; } }
</style>
