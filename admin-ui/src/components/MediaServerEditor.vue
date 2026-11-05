<script setup lang="ts">
import { reactive, ref, watch } from 'vue';
import ModalDialog from './ModalDialog.vue';
import { api, errorMessage } from '../services/api';
import type { MediaServer } from '../types/api';

const props = defineProps<{ open: boolean; node: MediaServer | null; defaultHook: string }>();
const emit = defineEmits<{ close: []; saved: [node: MediaServer] }>();
const saving = ref(false);
const error = ref('');
const form = reactive({ id: '', type: 'zlm', ip: '', http_port: 8080, hook_ip: '', sdp_ip: '', secret: '', rtpport_range: '30000-50000' });
watch(() => props.open, (open) => {
  if (!open) return;
  const node = props.node;
  Object.assign(form, { id: node?.id || '', type: node?.type || 'zlm', ip: node?.ip || '', http_port: node?.ports?.http || 8080, hook_ip: node?.hook_ip || props.defaultHook, sdp_ip: node?.sdp_ip || '', secret: '', rtpport_range: node?.rtpport_range || '30000-50000' });
  error.value = '';
});
async function save() {
  if (saving.value) return;
  saving.value = true;
  error.value = '';
  try {
    const body = { ...form, id: form.id.trim(), ip: form.ip.trim(), hook_ip: form.hook_ip.trim(), sdp_ip: form.sdp_ip.trim(), rtpport_range: form.rtpport_range.trim() };
    const response = props.node ? await api.editMediaServer(props.node.id, body) : await api.createMediaServer(body);
    emit('saved', response.data);
  } catch (cause) { error.value = errorMessage(cause, '媒体节点保存失败'); }
  finally { saving.value = false; }
}
</script>

<template>
  <ModalDialog :open="open" :title="node ? `编辑媒体节点 · ${node.id}` : '新增媒体节点'" description="配置平台与媒体服务的连接。节点标识创建后不可修改。" scroll-body @close="!saving && emit('close')">
    <form id="media-server-editor" class="node-editor" @submit.prevent="save">
      <div v-if="error" class="warning-box" role="alert">{{ error }}</div>
      <div class="form-grid">
        <label class="form-group"><span class="form-label">节点标识</span><input v-model="form.id" class="input" required maxlength="64" pattern="[A-Za-z0-9][A-Za-z0-9_-]{0,63}" :disabled="!!node || saving" placeholder="例如 edge-zlm-1" /><small class="form-hint">字母、数字、下划线或短横线；local 为保留标识。</small></label>
        <label class="form-group"><span class="form-label">媒体服务类型</span><select v-model="form.type" class="select" :disabled="saving"><option value="zlm">ZLMediaKit</option><option value="lalmax">Lalmax</option></select></label>
        <label class="form-group"><span class="form-label">API 地址</span><input v-model="form.ip" class="input" required :disabled="saving" placeholder="节点 IP 或域名" /><small class="form-hint">填写平台可访问的地址，不含协议和端口。</small></label>
        <label class="form-group"><span class="form-label">API 端口</span><input v-model.number="form.http_port" class="input" type="number" required min="1" max="65535" :disabled="saving" /></label>
        <label class="form-group"><span class="form-label">平台回调地址</span><input v-model="form.hook_ip" class="input" required :disabled="saving" placeholder="媒体节点可访问的平台 IP 或域名" /><small class="form-hint">使用平台服务的 HTTP 端口接收媒体回调。</small></label>
        <label class="form-group"><span class="form-label">SDP 收流地址</span><input v-model="form.sdp_ip" class="input" :disabled="saving" placeholder="留空使用 API 地址" /><small class="form-hint">摄像机可访问的媒体节点地址，跨网部署时请正确填写。</small></label>
        <label class="form-group"><span class="form-label">RTP 端口范围</span><input v-model="form.rtpport_range" class="input" required pattern="[0-9]+-[0-9]+" :disabled="saving" placeholder="30000-50000" /></label>
        <label class="form-group"><span class="form-label">API 密钥</span><input v-model="form.secret" class="input" type="password" autocomplete="new-password" :required="!node" :disabled="saving" :placeholder="node ? '留空保留当前密钥' : '填写媒体服务 API 密钥'" /></label>
      </div>
      <p class="form-hint">保存启用节点后会连接媒体服务，并同步平台运行所需的回调和收流参数；连接失败会保留为离线节点，可编辑后重试。正在使用的节点须先停止相关业务再修改。</p>
    </form>
    <template #footer><button class="btn" type="button" :disabled="saving" @click="emit('close')">取消</button><button class="btn btn-primary" type="submit" form="media-server-editor" :disabled="saving">{{ saving ? '正在保存…' : '保存节点' }}</button></template>
  </ModalDialog>
</template>

<style scoped>
.node-editor { padding: 20px; display: grid; gap: 20px; }
.node-editor .form-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 20px 16px; }
.node-editor .form-group { min-width: 0; }
.node-editor .form-hint { margin: 0; line-height: 1.65; color: var(--muted); font-size: 12px; }
@media (max-width: 640px) { .node-editor .form-grid { grid-template-columns: 1fr; } }
</style>
