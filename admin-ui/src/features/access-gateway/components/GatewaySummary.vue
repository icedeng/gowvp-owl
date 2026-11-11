<script setup lang="ts">
import { shallowRef } from 'vue'
import { RouterLink } from 'vue-router'
import { gatewayAPI } from '../api'
import { useVisiblePoll } from '../composables/useVisiblePoll'
const summary = shallowRef<{ total: number; ready: number; stale: number; failed: boolean }>()
useVisiblePoll(async signal => {
  try {
    let page=1, total=0, ready=0, stale=0, seen=0
    do {
      const data=await gatewayAPI.list({page,size:100},signal)
      if(signal.aborted)return
      total=data.total;seen+=data.items.length
      ready+=data.items.filter(g=>g.health_state==='ready'&&!g.stale).length
      stale+=data.items.filter(g=>g.stale).length
      if(!data.items.length)break
      page++
    }while(seen<total)
    summary.value={total,ready,stale,failed:false}
  }catch {if(!signal.aborted && summary.value)summary.value={...summary.value,failed:true}}
},15000)
</script>
<template><div v-if="summary" class="card gateway-summary"><RouterLink :to="{name:'access-gateways'}">公网接入网关 {{ summary.total }}</RouterLink><span>就绪 {{ summary.ready }} · 过期 {{ summary.stale }}{{ summary.failed ? ' · 刷新失败' : '' }}</span></div></template>
<style scoped>.gateway-summary{padding:16px 20px;margin-bottom:20px;display:flex;gap:24px;flex-wrap:wrap}</style>
