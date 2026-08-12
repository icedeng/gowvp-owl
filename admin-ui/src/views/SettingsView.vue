<script setup lang="ts">
import { ref } from 'vue'
import { Database, KeyRound, RadioTower, Save, Server, ShieldCheck, SlidersHorizontal } from '@lucide/vue'
import { useUiStore } from '../stores/ui'

const ui = useUiStore()
const section = ref('SIP 服务')
const autoConfig = ref(true)
const aiEnabled = ref(true)

function save() { ui.toast(`${section.value}配置已保存，相关服务将在下次心跳后生效`) }
</script>

<template>
  <main class="page-content">
    <header class="page-head">
      <div><h1 class="page-title">平台设置</h1><p class="page-desc">集中维护 SIP、媒体服务、录像、AI 与安全参数，明确配置生效范围。</p></div>
      <div class="head-actions"><button class="btn btn-primary" @click="save"><Save />保存当前配置</button></div>
    </header>

    <section class="settings-layout">
      <nav class="card settings-nav" aria-label="设置分类">
        <button v-for="item in [
          { label:'SIP 服务', icon:RadioTower }, { label:'媒体节点', icon:Server }, { label:'录像策略', icon:Database },
          { label:'AI 分析', icon:SlidersHorizontal }, { label:'账号安全', icon:KeyRound },
        ]" :key="item.label" :class="{ active: section === item.label }" @click="section = item.label"><component :is="item.icon" />{{ item.label }}</button>
      </nav>

      <form class="card form-section" @submit.prevent="save">
        <div v-if="section === 'SIP 服务'">
          <div class="card-head"><div><h2 class="card-title">SIP 服务</h2><p class="card-sub">GB28181 设备注册入口。修改 ID 或域前请先确认设备侧配置。</p></div><span class="status online">15060 / TCP+UDP</span></div>
          <div class="form-grid">
            <label class="form-group"><span class="form-label">监听地址</span><input class="input plain w-full" value="0.0.0.0" /><span class="form-help">监听所有网卡。</span></label>
            <label class="form-group"><span class="form-label">监听端口</span><input class="input plain w-full" type="number" value="15060" /></label>
            <label class="form-group full"><span class="form-label">SIP 服务 ID</span><input class="input plain w-full mono" value="34020000002000000001" /><span class="form-help">设备侧 Server ID 必须与此值一致。</span></label>
            <label class="form-group"><span class="form-label">域</span><input class="input plain w-full mono" value="3402000000" /></label>
            <label class="form-group"><span class="form-label">注册密码</span><input class="input plain w-full" type="password" value="12345678" /></label>
          </div>
        </div>
        <div v-else-if="section === '媒体节点'">
          <div class="card-head"><div><h2 class="card-title">媒体节点</h2><p class="card-sub">配置 Owl 与 ZLMediaKit 之间的 Hook 和收流地址。</p></div><span class="status online">local</span></div>
          <div class="form-grid"><label class="form-group"><span class="form-label">节点 IP</span><input class="input plain w-full" value="127.0.0.1" /></label><label class="form-group"><span class="form-label">Hook IP</span><input class="input plain w-full" value="127.0.0.1" /></label><label class="form-group"><span class="form-label">SDP IP</span><input class="input plain w-full" value="192.168.1.10" /></label><label class="form-group"><span class="form-label">RTP 端口范围</span><input class="input plain w-full" value="20000-20100" /></label><label class="toggle-row full"><span><strong class="block text-[11px]">自动配置节点</strong><small class="text-[9px] text-slate-500">服务启动时同步 Hook 与录像配置</small></span><span class="switch"><input v-model="autoConfig" type="checkbox" /><span class="slider" /></span></label></div>
        </div>
        <div v-else-if="section === '录像策略'">
          <div class="card-head"><div><h2 class="card-title">录像策略</h2><p class="card-sub">设置默认保留时间和存储路径。</p></div></div>
          <div class="form-grid"><label class="form-group"><span class="form-label">默认保留天数</span><input class="input plain w-full" type="number" value="30" /></label><label class="form-group"><span class="form-label">清理阈值</span><input class="input plain w-full" value="85%" /></label><label class="form-group full"><span class="form-label">录像路径</span><input class="input plain w-full mono" value="configs/recordings" /></label></div>
        </div>
        <div v-else-if="section === 'AI 分析'">
          <div class="card-head"><div><h2 class="card-title">AI 分析</h2><p class="card-sub">控制全局分析服务和默认置信度阈值。</p></div></div>
          <label class="toggle-row"><span><strong class="block text-[11px]">启用 AI 服务</strong><small class="text-[9px] text-slate-500">每路分析流会持续占用计算与内存资源</small></span><span class="switch"><input v-model="aiEnabled" type="checkbox" /><span class="slider" /></span></label>
          <div class="form-grid mt-4"><label class="form-group"><span class="form-label">默认置信度阈值</span><input class="input plain w-full" type="number" step="0.01" value="0.75" /></label><label class="form-group"><span class="form-label">模型文件</span><input class="input plain w-full mono" value="configs/owl.onnx" /></label></div>
        </div>
        <div v-else>
          <div class="card-head"><div><h2 class="card-title">账号安全</h2><p class="card-sub">更新当前管理员凭据和会话策略。</p></div><ShieldCheck /></div>
          <div class="form-grid"><label class="form-group"><span class="form-label">当前密码</span><input class="input plain w-full" type="password" /></label><span /><label class="form-group"><span class="form-label">新密码</span><input class="input plain w-full" type="password" /></label><label class="form-group"><span class="form-label">确认新密码</span><input class="input plain w-full" type="password" /></label></div>
        </div>
        <div class="settings-savebar"><span>修改仅在保存后生效</span><button class="btn btn-primary"><Save />保存 {{ section }}</button></div>
      </form>
    </section>
  </main>
</template>
