<script setup lang="ts">
import { RouterLink } from 'vue-router'
export interface IngressSummary { type: string; subject_type?: 'device' | 'platform'; gateway_id: string | null; gateway_name: string | null; binding_revision: string; effective: boolean }
defineProps<{ ingress?: IngressSummary }>()
</script>
<template><span v-if="ingress"><RouterLink v-if="ingress.type === 'gateway' && ingress.gateway_id" :to="{name:'access-gateway-detail',params:{id:ingress.gateway_id}}">{{ ingress.gateway_name || ingress.gateway_id }}</RouterLink><span v-else>直连</span><small v-if="ingress.type === 'gateway' && !ingress.effective"> · 等待重新注册</small></span><span v-else>入口信息不可用</span></template>
