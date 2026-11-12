<script setup lang="ts">
import { createRequestKey } from '../requestKey'
import { computed, onMounted, reactive, ref, shallowRef, toRaw } from 'vue'
import { Check, FileKey, Settings2, PlugZap, Upload } from '@lucide/vue'
import GatewayState from './GatewayState.vue'
import { probeLabels } from '../presentation'
import { useRouter } from 'vue-router'
import ModalDialog from '../../../components/ModalDialog.vue'
import { errorMessage } from '../../../services/api'
import { gatewayAPI } from '../api'
import type { DesiredConfig, Gateway, Operation } from '../types'
const props = defineProps<{ gateway?: Gateway }>()
const emit = defineEmits<{ close: []; saved: [Gateway | Operation] }>()
const router = useRouter()
const open = ref(false)
onMounted(() => { open.value = true })
const steps = [{ label: '基本配置', icon: Settings2 }, { label: '导入证书', icon: FileKey }, { label: '测试与启用', icon: PlugZap }]
const timeouts = { heartbeat_interval_ms: '心跳间隔', heartbeat_timeout_ms: '心跳超时', lease_ttl_ms: '租约有效期', media_connect_timeout_ms: '媒体连接超时', media_first_packet_timeout_ms: '媒体首包超时' } as const
const gateway = shallowRef(props.gateway)
const id = ref(props.gateway?.id || '')
const stage = ref(1)
const busy = ref(false)
const error = ref('')
const ca = shallowRef<File>(); const cert = shallowRef<File>(); const keyFile = shallowRef<File>()
const filesInputKey = ref(0)
const uploadedCredential = ref('')
const probe = shallowRef<Awaited<ReturnType<typeof gatewayAPI.probe>>>()
const draft: DesiredConfig = props.gateway?.desired_config ? structuredClone(props.gateway.desired_config) : {
  name: '', group: '', description: '', url: '', platform_id: '', credential_id: null, default_media_server_id: null, proxy_ref: null,
  required_capabilities: ['sip_udp', 'sip_tcp', 'rtp_udp'], public_ip: '', sip_port: 15060, media_port_start: 30000, media_port_end: 30199,
  max_media_sessions: 64, max_transfer_sessions: 8, max_to_a_bps: '100000000', max_to_device_bps: '100000000',
  heartbeat_interval_ms: 15000, heartbeat_timeout_ms: 45000, lease_ttl_ms: 60000, media_connect_timeout_ms: 10000, media_first_packet_timeout_ms: 30000,
  source_cidrs: [], task_public_base_url: null,
}
const config = reactive(draft)
const savedConfig = ref(JSON.stringify(draft))
const configDirty = computed(() => JSON.stringify(config) !== savedConfig.value)
const cidrs = computed({ get: () => config.source_cidrs.join('\n'), set: v => { config.source_cidrs = v.split(/[\s,]+/).filter(Boolean) } })
const caps = computed({ get: () => config.required_capabilities.join(', '), set: v => { config.required_capabilities = v.split(/[\s,]+/).filter(Boolean) } })
// 失败重试保留相同请求和幂等键；参数改变才开始新操作。
const attempts = new Map<string, { body: string; key: string }>()
function requestKey(action: string, body: unknown) { const json = JSON.stringify(body); const old = attempts.get(action); if (old?.body === json) return old.key; const key = createRequestKey(); attempts.set(action, { body: json, key }); return key }
async function run(fn: () => Promise<void>) { if (busy.value) return; busy.value = true; error.value = ''; try { await fn() } catch (e) { error.value = errorMessage(e) } finally { busy.value = false } }
async function save() { await run(async () => {
  if (config.media_port_end < config.media_port_start) throw new Error('媒体结束端口不能小于起始端口')
  probe.value = undefined
  const body = structuredClone({ ...toRaw(config), default_media_server_id: config.default_media_server_id || null, proxy_ref: config.proxy_ref || null, task_public_base_url: config.task_public_base_url || null })
  if (!gateway.value) gateway.value = await gatewayAPI.create(id.value, body, requestKey('create', { id: id.value, body }))
  else { const result = await gatewayAPI.update(gateway.value, body, requestKey('update', { revision: gateway.value.revision, body })); if ('operation_id' in result) { emit('saved', result); emit('close'); return } gateway.value = result; if (props.gateway) { emit('saved', result); emit('close'); return } }
  savedConfig.value = JSON.stringify(config)
  emit('saved', gateway.value!); stage.value = 2
}) }
async function upload() { await run(async () => {
  probe.value = undefined
  if (!gateway.value) return
  if (!uploadedCredential.value) {
    if (!ca.value || !cert.value || !keyFile.value) throw new Error('请选择完整的 CA、客户端证书及私钥文件')
    const form = new FormData(); form.append('ca_pem', ca.value); form.append('client_cert_pem', cert.value); form.append('client_key_pem', keyFile.value)
    const result = await gatewayAPI.credential(gateway.value.id, form, requestKey('credential', [ca.value.name, cert.value.name, keyFile.value.name, ca.value.lastModified, cert.value.lastModified, keyFile.value.lastModified]))
    uploadedCredential.value = result.id
    keyFile.value = undefined; ca.value = undefined; cert.value = undefined; filesInputKey.value++
  }
  config.credential_id = uploadedCredential.value
  if (gateway.value.admin_state === 'enabled') {
    emit('saved', await gatewayAPI.action(gateway.value, 'certificate-rotation', { new_credential_id: uploadedCredential.value, drain_timeout_ms: 30000 }, requestKey('rotation', uploadedCredential.value))); emit('close'); return
  }
  gateway.value = await gatewayAPI.get(gateway.value.id)
  const saved = await gatewayAPI.update(gateway.value, { ...config }, requestKey('bind', { revision: gateway.value.revision, config }))
  if ('operation_id' in saved) { emit('saved', saved); emit('close'); return }
  gateway.value = saved; uploadedCredential.value = ''; savedConfig.value = JSON.stringify(config); emit('saved', saved); stage.value = 3
}) }
async function test() { await run(async () => { if (gateway.value) probe.value = await gatewayAPI.probe(gateway.value.id, createRequestKey()) }) }
async function enable() { await run(async () => { if (!gateway.value) return; emit('saved', await gatewayAPI.action(gateway.value, 'state', { state: 'enabled' }, requestKey('enable', gateway.value.revision))); emit('close'); await router.push({ name: 'access-gateway-detail', params: { id: gateway.value.id } }) }) }
function revealInvalidField(event: Event) { if (event.target instanceof HTMLElement) event.target.closest('details')?.setAttribute('open', '') }
function file(event: Event) { return (event.target as HTMLInputElement).files?.[0] }
</script>
<template>
  <ModalDialog :open="open" :title="props.gateway ? `配置接入网关 · ${props.gateway.name}` : '添加接入网关'" description="保存配置、导入证书并完成连接测试后启用。" size="wide" scroll-body @close="!busy && emit('close')">
    <div class="gateway-editor">
      <nav class="editor-steps" aria-label="网关配置步骤">
        <button v-for="(step, index) in steps" :key="step.label" type="button" :class="{ active: stage === index + 1, complete: stage > index + 1 }" :aria-current="stage === index + 1 ? 'step' : undefined" :disabled="busy || !!uploadedCredential || (index > 0 && (!gateway || configDirty || !!uploadedCredential)) || (index === 2 && !config.credential_id)" @click="stage = index + 1"><Check v-if="stage > index + 1" aria-hidden="true" /><component :is="step.icon" v-else aria-hidden="true" /><span>{{ step.label }}</span></button>
      </nav>
      <p v-if="stage === 1 && gateway && configDirty" class="editor-help">配置有未保存的修改，请先保存，再进行证书或连接操作。</p>
      <p v-if="error" class="warning-box" role="alert">{{ error }}</p>
      <form v-if="stage === 1" id="gateway-config" @invalid.capture="revealInvalidField" @submit.prevent="save">
        <fieldset :disabled="busy" class="editor-section"><legend>基本信息</legend><div class="form-grid">
          <label class="form-group">网关 ID <span class="required-note">必填</span><input v-model.trim="id" class="input" required :disabled="!!gateway" maxlength="64" placeholder="例如 gateway-01" /><small>须与 B 端配置中的网关 ID 一致，创建后不可修改。</small></label>
          <label class="form-group">网关名称 <span class="required-note">必填</span><input v-model.trim="config.name" class="input" required maxlength="80" placeholder="输入便于识别的名称" /></label>
          <label class="form-group">分组<input v-model.trim="config.group" class="input" maxlength="64" placeholder="例如区域或项目名称（可选）" /></label>
          <label class="form-group">A 平台身份 <span class="required-note">必填</span><input v-model.trim="config.platform_id" class="input" required placeholder="与 A 端平台身份配置一致" /></label>
          <label class="form-group full">WSS 地址 <span class="required-note">必填</span><input v-model.trim="config.url" class="input" type="url" required pattern="wss://.+" placeholder="wss://gateway.example.com/gateway/v1/control" /><small>B 端的公网控制连接地址，使用 WSS 安全连接。</small></label>
        </div></fieldset>
        <fieldset :disabled="busy" class="editor-section"><legend>公网接入</legend><div class="form-grid">
          <label class="form-group">公网通告 IP <span class="required-note">必填</span><input v-model.trim="config.public_ip" class="input" required placeholder="B 端对外提供服务的公网 IP" /></label>
          <label class="form-group">SIP 端口<input v-model.number="config.sip_port" class="input" type="number" min="1" max="65535" required /></label>
          <label class="form-group">媒体起始端口<input v-model.number="config.media_port_start" class="input" type="number" min="1" max="65535" required /></label>
          <label class="form-group">媒体结束端口<input v-model.number="config.media_port_end" class="input" type="number" :min="config.media_port_start || 1" max="65535" required /></label>
          <label class="form-group">默认媒体节点 ID<input v-model.trim="config.default_media_server_id" class="input" placeholder="留空使用现有选择策略" /></label>
          <label class="form-group">任务 HTTPS 公网地址<input v-model.trim="config.task_public_base_url" class="input" type="url" placeholder="https://gateway.example.com:8443" /></label>
        </div></fieldset>
        <details class="editor-advanced"><summary>高级策略<span>容量、带宽、来源限制与超时</span></summary><fieldset :disabled="busy" class="editor-section"><legend class="sr-only">高级策略</legend><div class="form-grid">
          <label class="form-group">媒体会话上限<input v-model.number="config.max_media_sessions" class="input" type="number" min="1" required /></label>
          <label class="form-group">文件任务上限<input v-model.number="config.max_transfer_sessions" class="input" type="number" min="0" required /></label>
          <label class="form-group">到 A 带宽上限（bps）<input v-model="config.max_to_a_bps" class="input" inputmode="numeric" pattern="[0-9]+" required /></label>
          <label class="form-group">到设备带宽上限（bps）<input v-model="config.max_to_device_bps" class="input" inputmode="numeric" pattern="[0-9]+" required /></label>
          <label class="form-group full">受控代理引用<input v-model.trim="config.proxy_ref" class="input" maxlength="64" pattern="[A-Za-z0-9_-]+" placeholder="留空直连，例如 corporate-egress" /><small>由管理员在 A 端预配置代理及允许连接的网关地址，此处仅选择引用。</small></label>
          <label class="form-group full">必要能力<input v-model="caps" class="input" placeholder="多个能力用逗号分隔" /><small>使用协议能力标识，例如 sip_udp、sip_tcp、rtp_udp。</small></label>
          <label class="form-group full">允许的媒体来源 CIDR<textarea v-model="cidrs" class="textarea" rows="2" placeholder="每行一个 CIDR，例如 203.0.113.0/24" /><small>留空时，仅允许已认证 SIP 对端的 IP。</small></label>
          <label v-for="(label, field) in timeouts" :key="field" class="form-group">{{ label }}（ms）<input v-model.number="config[field]" type="number" class="input" min="1" required /></label>
        </div></fieldset></details>
        <label class="form-group editor-note">备注<textarea v-model="config.description" class="textarea" rows="2" maxlength="500" placeholder="补充网关的用途或运维说明（可选）" /></label>
        <p class="editor-help">保存后以实际生效版本和后台操作结果为准。</p>
      </form>
      <section v-else-if="stage === 2" :key="filesInputKey" class="credential-step">
        <h3 class="section-title">导入连接证书</h3>
        <p class="editor-help">{{ props.gateway ? '重新导入证书时，运行中的网关将先排空业务，再轮换证书。' : '网关草稿已保存。请选择 CA 证书、客户端证书和对应私钥。' }}私钥仅用于本次上传，不会保存到浏览器存储。</p>
        <div v-if="!uploadedCredential" class="file-fields">
          <label class="form-group">CA 证书<input :disabled="busy" type="file" accept=".pem,.crt" @change="ca = file($event)" /></label>
          <label class="form-group">客户端证书<input :disabled="busy" type="file" accept=".pem,.crt" @change="cert = file($event)" /></label>
          <label class="form-group">客户端私钥<input :disabled="busy" type="file" accept=".pem,.key" @change="keyFile = file($event)" /></label>
        </div>
        <p v-if="uploadedCredential" class="editor-help">证书已上传，绑定或轮换尚未完成。可重试此步骤，无需再次上传文件。</p>
        <p v-else-if="config.credential_id" class="editor-help">已绑定凭据，可保留当前证书，直接继续连接测试。</p>
      </section>
      <section v-else class="probe-step">
        <h3 class="section-title">检查连接并启用</h3><p class="editor-help">连接测试检查网关身份与协议能力。媒体链路还需在设备实际发流后确认。</p>
        <button class="btn" :disabled="busy" @click="test"><PlugZap aria-hidden="true" />{{ busy ? '正在检查…' : probe ? '重新测试连接' : '开始连接测试' }}</button>
        <div v-if="probe" class="probe-result" aria-live="polite"><p class="probe-conclusion">{{ probe.can_enable ? '连接检查通过，可以启用网关。' : '连接检查尚未通过，请根据下方结果检查配置和证书后重试。' }}</p><ul><li v-for="check in probe.checks" :key="check.name"><span>{{ probeLabels[check.name] || check.name }}</span><GatewayState :value="check.status" /><p>{{ check.msg }}</p></li></ul></div>
        <p v-else class="editor-help">完成连接测试后，平台会根据检查结果开放启用操作。</p>
      </section>
    </div>
    <template #footer>
      <button class="btn" :disabled="busy" @click="emit('close')">{{ gateway ? '关闭' : '取消' }}</button>
      <button v-if="stage > 1" class="btn" :disabled="busy || !!uploadedCredential" @click="stage--">上一步</button>
      <button v-if="stage === 1" class="btn btn-primary" type="submit" form="gateway-config" :disabled="busy">{{ busy ? '正在保存…' : props.gateway ? '保存配置' : '保存并继续' }}</button>
      <template v-else-if="stage === 2"><button v-if="config.credential_id && !uploadedCredential" class="btn" :disabled="busy" @click="stage = 3">使用当前凭据</button><button class="btn btn-primary" :disabled="busy || (!uploadedCredential && (!ca || !cert || !keyFile))" @click="upload"><Upload aria-hidden="true" />{{ busy ? '正在处理…' : uploadedCredential ? '重试绑定凭据' : '上传并继续' }}</button></template>
      <button v-else class="btn btn-primary" :disabled="busy || !probe?.can_enable" @click="enable">{{ busy ? '正在处理…' : '启用网关' }}</button>
    </template>
  </ModalDialog>
</template>

<style scoped>
.gateway-editor { padding: 0 2px 4px; }.editor-steps { display: flex; gap: 0; margin: 0 0 20px; border-bottom: 1px solid var(--line); }.editor-steps button { display: flex; flex: 1; align-items: center; justify-content: center; gap: 8px; min-height: 44px; padding: 8px; background: transparent; border: 0; border-bottom: 2px solid transparent; color: var(--muted); font-size: 13px; cursor: pointer; }.editor-steps button:hover:not(:disabled) { background: var(--panel-2); color: var(--blue); }.editor-steps button.active { color: var(--blue); border-bottom-color: var(--blue); font-weight: 500; }.editor-steps button.complete { color: var(--green); }.editor-steps button:disabled { cursor: not-allowed; color: var(--muted-2); }.editor-steps svg { width: 16px; height: 16px; }
.editor-section { margin: 0; padding: 0 0 18px; border: 0; min-width: 0; }.editor-section legend { padding: 0; margin-bottom: 12px; font-size: 14px; font-weight: 600; color: var(--ink-strong); }.form-group { font-size: 13px; position: relative; }.form-group small, .editor-help { color: var(--muted); font-size: 12px; line-height: 1.7; font-weight: 400; }.required-note { position: absolute; right: 0; top: 0; color: var(--muted); font-size: 11px; font-weight: 400; }.editor-help { margin: 0 0 14px; }.editor-note { margin-top: 16px; }
.editor-advanced { border-top: 1px solid var(--line); border-bottom: 1px solid var(--line); }.editor-advanced summary { padding: 12px 0; font-size: 13px; font-weight: 500; cursor: pointer; }.editor-advanced summary span { margin-left: 10px; color: var(--muted); font-size: 12px; font-weight: 400; }.editor-advanced .editor-section { padding-top: 4px; }.editor-note + .editor-help { margin: 10px 0 0; }
.file-fields { display: grid; gap: 16px; margin: 18px 0; }.file-fields input { width: 100%; padding: 10px; border: 1px solid var(--line); border-radius: var(--radius-sm); background: var(--panel-2); font-size: 13px; }.file-fields input::file-selector-button { margin-right: 10px; padding: 6px 10px; border: 1px solid var(--line-strong); border-radius: var(--radius-sm); background: var(--panel); color: var(--ink); cursor: pointer; }.probe-result { margin-top: 16px; }.probe-conclusion { font-size: 13px; line-height: 1.7; }.probe-result ul { padding: 0; list-style: none; border-top: 1px solid var(--line); }.probe-result li { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 16px; padding: 12px 0; border-bottom: 1px solid var(--line); }.probe-result li p { flex-basis: 100%; margin: 0; color: var(--muted); font-size: 12px; overflow-wrap: anywhere; }.probe-step > .editor-help:last-child { margin-top: 12px; }
@media (max-width: 640px) { .editor-steps button { gap: 5px; padding-inline: 2px; font-size: 12px; }.editor-advanced summary span { display: block; margin: 4px 0 0 16px; } }
</style>
