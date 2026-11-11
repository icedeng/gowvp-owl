<script setup lang="ts">
import { shallowRef, ref } from 'vue'
import { gatewayAPI } from '../api'
import { useVisiblePoll } from '../composables/useVisiblePoll'
import { errorMessage } from '../../../services/api'
import GatewayState from './GatewayState.vue'
import { stateLabel } from '../presentation'
import type { Operation } from '../types'
const props = defineProps<{ gatewayId: string; operationId: string }>()
const emit = defineEmits<{ finished: [] }>()
const operation = shallowRef<Operation>(); const error = ref(''); let finished = false
useVisiblePoll(async signal => {
  if (finished) return
  try {
    operation.value = await gatewayAPI.operation(props.gatewayId, props.operationId, signal); error.value = ''
    if (['succeeded', 'failed', 'cancelled'].includes(operation.value.status)) { finished = true; emit('finished') }
  } catch (e) { if (!signal.aborted) error.value = errorMessage(e) }
}, 2000)
</script>
<template>
  <aside class="card operation-status" aria-live="polite" aria-label="后台操作进度">
    <header><strong>{{ operation ? stateLabel(operation.type) : '正在读取操作进度' }}</strong><GatewayState v-if="operation" :value="operation.status" /><span v-if="operation && operation.progress.total > 0">已处理 {{ operation.progress.completed }} / {{ operation.progress.total }}</span></header>
    <p class="operation-id">操作 ID <span class="mono">{{ operationId }}</span></p>
    <p v-if="error" class="warning-box" role="alert">{{ error }}</p>
    <template v-if="operation">
      <p v-if="operation.step">当前步骤：{{ stateLabel(operation.step) }}</p>
      <p v-if="operation.status === 'waiting_external'">正在等待外部设备或网关响应，离开页面不会取消操作。</p>
      <p v-if="operation.error" class="operation-error" role="alert">{{ operation.error.code }}：{{ operation.error.message }}</p>
      <details v-if="operation.result"><summary>查看操作结果</summary><pre>{{ JSON.stringify(operation.result, null, 2) }}</pre></details>
    </template>
  </aside>
</template>

<style scoped>
.operation-status { padding: 14px; margin-bottom: 14px; font-size: 13px; }.operation-status header { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 12px; }.operation-status header strong { color: var(--ink-strong); font-weight: 500; }.operation-status header > span:last-child { font-size: 12px; }.operation-status p { margin: 8px 0 0; color: var(--muted); line-height: 1.7; overflow-wrap: anywhere; }.operation-status .operation-id { font-size: 12px; }.operation-status .operation-error { color: var(--red); }.operation-status details { margin-top: 10px; }.operation-status summary { color: var(--blue); cursor: pointer; font-size: 12px; }.operation-status pre { white-space: pre-wrap; overflow-wrap: anywhere; padding: 12px; background: var(--panel-2); font-size: 12px; line-height: 1.7; }
</style>
