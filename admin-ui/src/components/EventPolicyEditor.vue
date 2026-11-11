<script setup lang="ts">
import { ref, watch } from 'vue'
import { Camera, Clapperboard, LoaderCircle, Save, ShieldAlert } from '@lucide/vue'
import { api, errorMessage } from '../services/api'
import type { EventPolicy } from '../types/api'
import { formatDate } from '../utils/format'
const props = defineProps<{ deviceId: string }>()
const loading = ref(false), saving = ref(false), error = ref(''), saved = ref(false)
const policy = ref<EventPolicy | null>(null)
let sequence = 0
async function load() {
 const current = ++sequence; policy.value = null; saved.value = false; error.value = ''
 if (!props.deviceId || props.deviceId === 'all') return
 loading.value = true
 try { const { data } = await api.eventPolicy(props.deviceId); if (current === sequence) policy.value = data }
 catch (cause) { if (current === sequence) error.value = errorMessage(cause, '联动策略加载失败') }
 finally { if (current === sequence) loading.value = false }
}
async function save() {
 if (!policy.value || saving.value) return
 const current = sequence, did = props.deviceId
 saving.value = true; error.value = ''; saved.value = false
 try { const { data } = await api.saveEventPolicy(did, { ...policy.value }); if (current === sequence) { policy.value = data; saved.value = true } }
 catch (cause) { if (current === sequence) error.value = errorMessage(cause, '保存失败，请刷新策略后重试') }
 finally { saving.value = false }
}
watch(() => props.deviceId, load, { immediate: true })
</script>
<template>
 <section class="event-policy" :aria-busy="loading">
  <div class="detail-section-head"><div><h2>报警联动策略</h2><p>配置平台收到该设备国标报警后的取证动作，适用于设备下全部通道。</p></div><button v-if="policy" class="btn" :disabled="loading || saving" @click="load">重新加载</button></div>
  <div v-if="error" class="warning-box" role="alert"><ShieldAlert /><span>{{ error }}</span><button v-if="!policy" class="btn btn-sm" @click="load">重试</button></div>
  <p v-if="loading" class="section-note"><LoaderCircle class="animate-spin" />正在读取策略…</p>
  <form v-else-if="policy" class="policy-form" @submit.prevent="save">
   <fieldset :disabled="saving">
    <legend class="sr-only">报警取证设置</legend>
    <label class="policy-option"><input v-model="policy.snapshot" type="checkbox" @change="saved = false"><Camera aria-hidden="true" /><span><strong>报警时自动抓拍</strong><small>平台从实时流抓拍，将现场图片保存到对应事件。</small></span></label>
    <label class="policy-option"><input v-model="policy.record" type="checkbox" @change="saved = false"><Clapperboard aria-hidden="true" /><span><strong>报警时录制视频片段</strong><small>平台收到报警并取流后开始录制，不依赖设备 SD 卡，不包含报警前画面。</small></span></label>
    <div class="policy-fields">
     <label class="form-group"><span class="form-label">视频片段时长（秒）</span><input v-model.number="policy.clip_seconds" class="input" type="number" min="5" max="120" required :disabled="!policy.record" @input="saved = false"><small>5–120 秒；独立保存，不中断常规平台录像。</small></label>
     <label class="form-group"><span class="form-label">同通道取证间隔（秒）</span><input v-model.number="policy.cooldown_seconds" class="input" type="number" min="10" max="3600" required :disabled="!policy.record && !policy.snapshot" @input="saved = false"><small>10–3600 秒；间隔内或正在取证时仍记录报警，跳过重复取证。</small></label>
    </div>
   </fieldset>
   <p class="policy-note">策略保存后仅作用于新报警。未开启取证时只记录事件；设备级报警未指定通道、离线、排队超时或存储不足时，会显示跳过或失败原因。附件及处理记录随事件保留期清理。</p>
   <div class="policy-footer"><button class="btn btn-primary" :disabled="saving"><LoaderCircle v-if="saving" class="animate-spin" /><Save v-else />{{ saving ? '正在保存…' : '保存策略' }}</button><span v-if="saved" class="success-message" role="status">已保存，将对后续报警生效</span><span v-else-if="policy.updated_by" class="section-note">{{ policy.updated_by }} 更新于 {{ formatDate(policy.updated_at) }}</span></div>
  </form>
  <p v-else-if="!error && !loading" class="section-note">选择一台国标设备，查看或配置报警联动策略。</p>
 </section>
</template>
<style scoped>
.event-policy { min-width: 0; }
.detail-section-head { margin-bottom: 18px; }
.policy-form { max-width: 820px; }
fieldset { padding: 0; margin: 0; border: 0; min-width: 0; }
.policy-option { display: flex; align-items: flex-start; gap: 12px; padding: 16px 0; border-bottom: 1px solid var(--line); cursor: pointer; }
.policy-option > input { margin-top: 5px; }
.policy-option > svg { width: 18px; height: 18px; margin-top: 3px; color: var(--muted); flex-shrink: 0; }
.policy-option span { display: grid; gap: 4px; }
.policy-option small, .form-group small, .policy-note { color: var(--muted); font-size: 12px; line-height: 1.7; }
.policy-fields { display: grid; grid-template-columns: 1fr 1fr; gap: 20px; margin-top: 20px; }
.policy-note { margin: 20px 0; }
.policy-footer { display: flex; align-items: center; flex-wrap: wrap; gap: 12px; }
.success-message { color: var(--green); font-size: 13px; }
.section-note > svg { display: inline; width: 16px; height: 16px; vertical-align: middle; }
@media (max-width: 640px) { .policy-fields { grid-template-columns: 1fr; } .policy-option { min-height: 64px; } }
</style>
