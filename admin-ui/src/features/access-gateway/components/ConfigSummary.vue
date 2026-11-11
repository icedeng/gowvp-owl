<script setup lang="ts">
import { configLabels } from '../presentation'
defineProps<{ config?: object | null }>()
function display(value: unknown) {
  if (value == null || value === '') return '—'
  if (Array.isArray(value)) return value.join('、') || '—'
  return typeof value === 'object' ? JSON.stringify(value) : String(value)
}
</script>

<template>
  <template v-if="config">
    <dl class="config-summary"><div v-for="(value, key) in config" :key="key"><dt>{{ configLabels[key] || key }}</dt><dd>{{ display(value) }}</dd></div></dl>
    <details class="config-source"><summary>查看原始配置</summary><pre>{{ JSON.stringify(config, null, 2) }}</pre></details>
  </template>
  <p v-else class="config-empty">暂无配置，网关应用成功后会显示实际生效参数。</p>
</template>

<style scoped>
.config-summary { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); margin: 0; font-size: 13px; }.config-summary > div { display: grid; grid-template-columns: 150px minmax(0, 1fr); gap: 12px; padding: 10px 12px; border-bottom: 1px solid var(--line); }.config-summary dt { color: var(--muted); }.config-summary dd { margin: 0; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }.config-source { margin: 14px 0 20px; }.config-source summary { color: var(--blue); cursor: pointer; font-size: 12px; }.config-source pre { white-space: pre-wrap; overflow-wrap: anywhere; background: var(--panel-2); padding: 12px; font-size: 12px; line-height: 1.7; }.config-empty { color: var(--muted); font-size: 13px; }
@media (max-width: 1100px) { .config-summary { grid-template-columns: 1fr; } }
@media (max-width: 640px) { .config-summary > div { grid-template-columns: 120px minmax(0, 1fr); padding-inline: 0; } }
</style>
