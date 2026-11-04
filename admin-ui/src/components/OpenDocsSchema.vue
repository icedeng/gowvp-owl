<script setup lang="ts">
import { computed } from 'vue'
import { schemaFields, type Schema, type Spec } from '../services/open-platform-docs'
const props=defineProps<{spec:Spec;schema:Schema;response?:boolean}>()
const fields=computed(()=>schemaFields(props.spec,props.schema))
</script>
<template><div v-if="fields.length" class="schema-scroll"><table><thead><tr><th>字段</th><th>类型</th><th>{{ response?'必需 / 可空':'是否必填' }}</th><th>含义与限制</th></tr></thead><tbody><tr v-for="field in fields" :key="field.name"><td><code>{{ field.name }}</code></td><td>{{ field.type }}</td><td>{{ field.required?'必需':response?'按响应返回':'可选' }}</td><td>{{ field.description }}<small v-if="field.constraints">{{ field.constraints }}</small></td></tr></tbody></table></div><p v-else class="empty-schema">无额外字段；使用接口说明中的原始字节或空对象。</p></template>
<style scoped>
.schema-scroll{overflow:auto;margin:14px 0;border:1px solid #dce2ea;border-radius:8px}table{width:100%;border-collapse:collapse;font-size:13px;line-height:1.7;min-width:620px}th,td{padding:11px 14px;text-align:left;border-bottom:1px solid #e7ebf1;vertical-align:top}th{background:#f5f7fa;font-weight:600;color:#46556d}td:first-child{max-width:250px;overflow-wrap:anywhere}td:nth-child(2),td:nth-child(3){white-space:nowrap}tr:last-child td{border-bottom:0}small{display:block;color:#53637a;margin-top:4px;font-size:12px}code{color:#234d83}.empty-schema{color:#59677b}
</style>
