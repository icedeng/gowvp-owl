<script setup lang="ts">
import { ref } from 'vue'
import { Activity, ArrowDownToLine, ArrowUpFromLine, HardDrive, MoreHorizontal, Plus, RadioTower, RefreshCcw, Server } from '@lucide/vue'
import { useUiStore } from '../stores/ui'

const ui = useUiStore()
const tab = ref('节点')
const streams = [
  { id: 'rtp/ch-east-01', type: 'GB28181', direction: '收流', viewers: 3, bitrate: '3.8 Mbps', uptime: '04:18:22', status: 'online' },
  { id: 'live/warehouse-01', type: 'ONVIF', direction: '代理', viewers: 1, bitrate: '2.6 Mbps', uptime: '01:42:16', status: 'warning' },
  { id: 'live/line-01', type: 'RTMP', direction: '推流', viewers: 8, bitrate: '5.1 Mbps', uptime: '12:09:44', status: 'online' },
  { id: 'live/cold-01', type: 'RTSP', direction: '代理', viewers: 0, bitrate: '2.2 Mbps', uptime: '00:36:07', status: 'online' },
]
</script>

<template>
  <main class="page-content">
    <header class="page-head">
      <div><h1 class="page-title">流媒体节点</h1><p class="page-desc">观察 ZLMediaKit 节点健康、活跃流、带宽和录像资源水位。</p></div>
      <div class="head-actions"><button class="btn" @click="ui.toast('节点状态已刷新')"><RefreshCcw />刷新节点</button><button class="btn btn-primary" @click="ui.toast('已打开新增推拉流表单')"><Plus />新增推拉流</button></div>
    </header>

    <div class="segmented mb-4"><button v-for="item in ['节点','活跃流','推流配置','拉流代理']" :key="item" :class="{ active: tab === item }" @click="tab = item">{{ item }}</button></div>

    <section class="grid three-col mb-4">
      <article class="card card-pad node-card"><div class="node-card-head"><span class="node-big-icon"><Server /></span><span class="status online">运行中</span></div><h2>local · ZLMediaKit</h2><p class="mono">127.0.0.1:80</p><dl class="node-metrics"><div><dt>活跃流</dt><dd>26</dd></div><div><dt>观看端</dt><dd>48</dd></div><div><dt>运行时间</dt><dd>17d</dd></div></dl></article>
      <article class="card card-pad resource-card"><div class="card-head"><div><h2 class="card-title">实时带宽</h2><p class="card-sub">入口 / 出口</p></div><Activity /></div><div class="resource-number">84.6 <small>Mbps</small></div><div class="resource-split"><span><ArrowDownToLine />52.1 Mbps</span><span><ArrowUpFromLine />32.5 Mbps</span></div></article>
      <article class="card card-pad resource-card"><div class="card-head"><div><h2 class="card-title">录像存储</h2><p class="card-sub">/configs/recordings</p></div><HardDrive /></div><div class="resource-number">72 <small>%</small></div><div class="progress warn mt-4"><i style="width:72%" /></div><p class="mt-3 text-[10px] text-slate-500">已用 3.6 TB / 总计 5 TB · 预计可用 11 天</p></article>
    </section>

    <section class="card card-pad">
      <div class="card-head"><div><h2 class="card-title">活跃流</h2><p class="card-sub">来自所有协议通道的当前媒体会话</p></div><span class="status online">26 路</span></div>
      <div class="table-wrap"><table class="data-table">
        <thead><tr><th>应用 / 流 ID</th><th>协议</th><th>方向</th><th>观看端</th><th>码率</th><th>持续时间</th><th>状态</th><th></th></tr></thead>
        <tbody><tr v-for="stream in streams" :key="stream.id"><td><div class="row-title"><span class="device-glyph"><RadioTower /></span><span><strong class="mono">{{ stream.id }}</strong><small>MEDIA SESSION</small></span></div></td><td><span class="protocol-tag">{{ stream.type }}</span></td><td>{{ stream.direction }}</td><td>{{ stream.viewers }}</td><td>{{ stream.bitrate }}</td><td class="mono">{{ stream.uptime }}</td><td><span class="status" :class="stream.status">{{ stream.status === 'warning' ? '抖动' : '稳定' }}</span></td><td><button class="more-btn" aria-label="更多操作" @click="ui.toast(`已打开 ${stream.id} 的流操作`)"><MoreHorizontal /></button></td></tr></tbody>
      </table></div>
    </section>
  </main>
</template>
