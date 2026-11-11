<script setup lang="ts">
import { onMounted, ref, shallowRef } from 'vue'
import { gatewayAPI } from '../api'
import type { Gateway } from '../types'
import { errorMessage } from '../../../services/api'
withDefaults(defineProps<{ modelValue?: string; allowAll?: boolean; directValue?: string; label?: string }>(), { directValue: '', label: '接入网关' })
const emit = defineEmits<{ 'update:modelValue': [string] }>()
const rows = shallowRef<Gateway[]>([]); const error = ref('')
onMounted(async () => { try { let page = 1; let total = 0; const items: Gateway[] = []; do { const data = await gatewayAPI.list({page, size:100});items.push(...data.items);total=data.total;page++ } while(items.length<total);rows.value=items } catch(e) { error.value=errorMessage(e) } })
</script>
<template><label><span class="sr-only">{{ label }}</span><select class="select" :aria-label="label" :value="modelValue || ''" @change="emit('update:modelValue',($event.target as HTMLSelectElement).value)"><option v-if="allowAll" value="">全部入口</option><option :value="directValue">直连</option><option v-for="g in rows" :key="g.id" :value="g.id">{{ g.name }}（{{ g.id }}）</option><option v-if="modelValue && !rows.some(g=>g.id===modelValue) && modelValue!==directValue" :value="modelValue">{{ modelValue }}</option></select><small v-if="error" :title="error">网关列表不可用</small></label></template>
