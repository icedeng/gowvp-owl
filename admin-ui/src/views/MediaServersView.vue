<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { Activity, CircleCheck, CircleDashed, HardDrive, LoaderCircle, RefreshCcw, Server, Settings2, ShieldAlert } from "@lucide/vue";
import { api, collectPages, errorMessage, typeLabel } from "../services/api";
import type { MediaServer, ResourceStats } from "../types/api";
import { formatBytes, relativeTime } from "../utils/format";
import { useUiStore } from "../stores/ui";
import ModalDialog from "../components/ModalDialog.vue";

const ui = useUiStore();
const loading = ref(false);
const saving = ref(false);
const loadError = ref("");
const rows = ref<MediaServer[]>([]);
const streamCounts = ref<Record<string, number>>({});
const stats = ref<ResourceStats>({});
const editOpen = ref(false);
const editing = ref<MediaServer | null>(null);
const form = reactive({ type: "zlm", ip: "", hook_ip: "", sdp_ip: "", secret: "" });
const disk = computed(() => stats.value.disk?.[0]);
const diskPercent = computed(() => disk.value?.total ? Number(disk.value.used || 0) / disk.value.total * 100 : 0);
const engineProfiles = {
  zlm: [
    { name: "协商 SSRC 过滤", state: "ready", detail: "收流端口与 INVITE y= 使用同一会话 SSRC" },
    { name: "RFC 4571 RTP / RTCP", state: "verify", detail: "应用仓库补丁已构建；真实 2016/2022 TCP 抓包待验收" },
    { name: "主动 TCP 连接", state: "ready", detail: "平台收流与级联发送均接入生产调用" },
    { name: "来源地址隔离", state: "verify", detail: "仍需在目标网络以真实设备抓包确认" },
  ],
  lalmax: [
    { name: "协商 SSRC 过滤", state: "limited", detail: "公开 API 无 SSRC 字段，不宣称等价防串流" },
    { name: "RFC 4571 RTP / RTCP", state: "limited", detail: "TCP RTCP 能力未形成与 ZLM 等价证据" },
    { name: "主动 TCP 连接", state: "limited", detail: "不支持的主动模式会由业务层提前拒绝" },
    { name: "来源地址隔离", state: "verify", detail: "依赖部署网络并需真实链路验证" },
  ],
} as const;

function engineProfile(type?: string) {
  return engineProfiles[String(type || "").toLowerCase() as keyof typeof engineProfiles] || [
    { name: "GB28181 媒体能力", state: "verify", detail: "未知媒体驱动，需按实际实现单独验收" },
  ];
}

async function loadStreamCounts() {
  const protocols = ["GB28181", "ONVIF", "RTMP", "RTSP"];
  const [allResponse, activeResponse, idleResponse, ...protocolResponses] = await Promise.all([
    api.channels({ page: 1, size: 1 }),
    api.channels({ page: 1, size: 1, is_playing: true }),
    api.channels({ page: 1, size: 1, is_playing: false }),
    ...protocols.map((type) => api.channels({ page: 1, size: 1, type, is_playing: true })),
  ]);
  const pageTotal = (data?: { items?: unknown[]; total?: number }) => Number(data?.total ?? data?.items?.length ?? 0);
  const allTotal = pageTotal(allResponse.data);
  const activeTotal = pageTotal(activeResponse.data);
  const idleTotal = pageTotal(idleResponse.data);
  const supportsFilters = activeTotal + idleTotal === allTotal
    && (activeResponse.data?.items || []).every((item) => item.is_playing === true)
    && (idleResponse.data?.items || []).every((item) => item.is_playing === false)
    && protocolResponses.every((response, index) => (response.data?.items || []).every((item) =>
      item.is_playing === true && typeLabel(item.type, item.channel_id || item.id) === protocols[index]
    ));
  if (supportsFilters) return Object.fromEntries(protocols.map((type, index) => [type, pageTotal(protocolResponses[index].data)]));
  const legacy = await collectPages(api.channels);
  return Object.fromEntries(protocols.map((type) => [type, legacy.items.filter((item) =>
    item.is_playing === true && typeLabel(item.type, item.channel_id || item.id) === type
  ).length]));
}

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const [mediaResponse, channelResponse, statResponse] = await Promise.allSettled([
      api.mediaServers({ page: 1, size: 100 }), loadStreamCounts(), api.stats(),
    ]);
    if (mediaResponse.status === "rejected") throw mediaResponse.reason;
    rows.value = mediaResponse.value.data?.items || [];
    if (channelResponse.status === "fulfilled") streamCounts.value = channelResponse.value;
    if (statResponse.status === "fulfilled") stats.value = statResponse.value.data || {};
    const auxiliaryFailure = [channelResponse, statResponse].find((item) => item.status === "rejected");
    if (auxiliaryFailure?.status === "rejected") loadError.value = `媒体节点已加载，部分摘要暂不可用：${errorMessage(auxiliaryFailure.reason)}`;
  } catch (cause) {
    loadError.value = errorMessage(cause, "媒体节点加载失败");
  } finally {
    loading.value = false;
  }
}

function edit(item: MediaServer) {
  editing.value = item;
  Object.assign(form, { type: item.type || "zlm", ip: item.ip || "", hook_ip: item.hook_ip || "", sdp_ip: item.sdp_ip || "", secret: "" });
  editOpen.value = true;
}

async function save() {
  if (!editing.value) return;
  saving.value = true;
  try {
    await api.editMediaServer(editing.value.id, {
      type: form.type, ip: form.ip, hook_ip: form.hook_ip, sdp_ip: form.sdp_ip,
      secret: form.secret || editing.value.secret || "",
    });
    editOpen.value = false;
    ui.toast(`媒体节点 ${editing.value.id} 已更新`);
    await load();
  } catch (cause) {
    ui.toast(errorMessage(cause, "媒体节点更新失败"));
  } finally {
    saving.value = false;
  }
}

onMounted(load);
</script>

<template>
  <main class="page-content media-server-page">
    <header class="page-head">
      <div><h1 class="page-title">媒体节点</h1><p class="page-desc">查看节点连接、端口、录像存储，以及 GB28181 收流能力与验收边界。</p></div>
      <div class="head-actions"><button class="btn" :disabled="loading" @click="load"><RefreshCcw :class="{ 'animate-spin': loading }" />刷新</button><RouterLink class="btn btn-primary" to="/system-status"><Activity />系统状态</RouterLink></div>
    </header>
    <div v-if="loadError" class="warning-box mb-4" role="alert"><ShieldAlert /><span>{{ loadError }}</span><button class="btn btn-sm ml-auto" @click="load">重试</button></div>
    <div class="warning-box mb-4"><ShieldAlert /><span>多节点新增和删除路由尚未开放，因此本页只提供已有节点编辑；媒体能力状态不会替代目标网络的真实 RTP/RTCP 抓包。</span></div>

    <section class="grid three-col mb-4">
      <article v-for="item in rows" :key="item.id" class="card card-pad">
        <div class="card-head"><span class="details-icon"><Server /></span><span class="status" :class="item.status ? 'online' : 'offline'">{{ item.status ? "运行中" : "离线" }}</span></div>
        <h2 class="section-title">{{ item.id }} · {{ item.type || "未知驱动" }}</h2>
        <p class="mono text-slate-500">{{ item.ip || "—" }}:{{ item.ports?.http || "—" }}</p>
        <dl class="definition-grid !grid-cols-1 mt-3"><div><dt>最近心跳</dt><dd>{{ relativeTime(item.last_keepalive_at) }}</dd></div><div><dt>RTP 端口</dt><dd>{{ item.rtpport_range || "—" }}</dd></div></dl>
      </article>
      <article v-if="!rows.length" class="card card-pad"><div class="card-head"><span class="details-icon"><Server /></span><span class="status offline">无节点</span></div><div class="empty-state"><LoaderCircle v-if="loading" class="mx-auto mb-2 animate-spin" />{{ loading ? "正在加载媒体节点…" : "当前环境没有媒体节点记录。" }}</div></article>
      <article class="card card-pad"><div class="card-head"><div><h2 class="card-title">录像存储</h2><p class="card-sub">{{ rows[0]?.record_path || disk?.name || "—" }}</p></div><HardDrive /></div><div class="metric-value">{{ diskPercent.toFixed(1) }}%</div><div class="progress mt-4" :class="{ warn: diskPercent >= 70 }"><i :style="{ width: `${Math.min(100, diskPercent)}%` }" /></div><p class="section-note mt-3">{{ formatBytes(disk?.used) }} / {{ formatBytes(disk?.total) }}</p></article>
    </section>

    <section class="card card-pad mb-4">
      <div class="card-head"><div><h2 class="card-title">GB28181 收流能力边界</h2><p class="card-sub">按当前媒体驱动展示已接入能力、受限项与外部验收项。</p></div><Activity /></div>
      <div v-if="rows.length" class="engine-grid">
        <article v-for="item in rows" :key="`capability-${item.id}`" class="engine-profile">
          <header><span><Server /><strong>{{ item.id }}</strong></span><span class="protocol-tag blue">{{ item.type || "未知" }}</span></header>
          <div v-for="capability in engineProfile(item.type)" :key="capability.name" class="engine-capability" :class="capability.state">
            <CircleCheck v-if="capability.state === 'ready'" /><ShieldAlert v-else-if="capability.state === 'limited'" /><CircleDashed v-else />
            <span><strong>{{ capability.name }}</strong><small>{{ capability.detail }}</small></span>
            <b>{{ capability.state === "ready" ? "已接入" : capability.state === "limited" ? "受限" : "待验收" }}</b>
          </div>
        </article>
      </div>
      <div v-else class="empty-state">媒体节点加载后显示对应引擎的 GB28181 能力边界。</div>
    </section>

    <section class="grid equal-col">
      <article class="card card-pad">
        <div class="card-head"><div><h2 class="card-title">节点连接配置</h2><p class="card-sub">Secret 不显示原值，修改时留空将保留当前值</p></div><Settings2 /></div>
        <div v-for="item in rows" :key="item.id" class="mb-4"><dl class="definition-grid"><div><dt>节点</dt><dd>{{ item.id }}</dd></div><div><dt>类型</dt><dd>{{ item.type || "—" }}</dd></div><div><dt>HTTP 地址</dt><dd class="mono">{{ item.ip || "—" }}:{{ item.ports?.http || "—" }}</dd></div><div><dt>Hook IP</dt><dd class="mono">{{ item.hook_ip || "—" }}</dd></div><div><dt>SDP IP</dt><dd class="mono">{{ item.sdp_ip || "—" }}</dd></div><div><dt>Secret</dt><dd>••••••••</dd></div></dl><button class="btn mt-4" @click="edit(item)">编辑节点</button></div>
      </article>
      <article class="card card-pad"><div class="card-head"><div><h2 class="card-title">媒体会话摘要</h2><p class="card-sub">按通道 Hook 状态统计播放中的流</p></div><Activity /></div><div class="matrix"><div v-for="type in ['GB28181', 'RTMP', 'RTSP', 'ONVIF']" :key="type" class="matrix-slot"><span class="protocol-tag blue">{{ type }}</span><h3>{{ streamCounts[type] || 0 }} 路</h3><p>活跃播放</p></div></div></article>
    </section>

    <ModalDialog :open="editOpen" title="编辑媒体节点" description="保存会重新连接媒体服务，请确认地址与密钥正确。" @close="editOpen = false">
      <form class="form-grid" @submit.prevent="save"><label class="form-group"><span class="form-label">驱动类型</span><select v-model="form.type" class="select w-full"><option value="zlm">ZLMediaKit</option><option value="lalmax">Lalmax</option></select></label><label class="form-group"><span class="form-label">服务 IP</span><input v-model="form.ip" class="input plain w-full" required /></label><label class="form-group"><span class="form-label">Hook IP</span><input v-model="form.hook_ip" class="input plain w-full" required /></label><label class="form-group"><span class="form-label">SDP IP</span><input v-model="form.sdp_ip" class="input plain w-full" required /></label><label class="form-group full"><span class="form-label">新 Secret</span><input v-model="form.secret" class="input plain w-full" type="password" autocomplete="new-password" placeholder="留空保留当前值" /></label><div class="modal-foot full"><button type="button" class="btn" @click="editOpen = false">取消</button><button class="btn btn-primary" :disabled="saving"><LoaderCircle v-if="saving" class="animate-spin" />{{ saving ? "正在保存…" : "保存配置" }}</button></div></form>
    </ModalDialog>
  </main>
</template>

<style scoped>
.engine-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(360px, 1fr)); gap: 12px; }
.engine-profile { min-width: 0; border: 1px solid var(--line); border-radius: 10px; overflow: hidden; }
.engine-profile > header { min-height: 46px; display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 0 12px; background: #f7f9fb; border-bottom: 1px solid var(--line); }
.engine-profile > header > span:first-child { display: inline-flex; align-items: center; gap: 8px; }.engine-profile > header svg { width: 16px; color: var(--blue); }
.engine-capability { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; align-items: center; gap: 9px; padding: 10px 12px; border-bottom: 1px solid #edf0f4; }
.engine-capability:last-child { border-bottom: 0; }.engine-capability > svg { width: 16px; }.engine-capability span, .engine-capability strong, .engine-capability small { display: block; }.engine-capability strong { color: var(--ink); font-size: 12px; }.engine-capability small { margin-top: 2px; color: var(--muted); font-size: 10px; }.engine-capability b { font-size: 11px; }
.engine-capability.ready > svg, .engine-capability.ready > b { color: #167653; }.engine-capability.verify > svg, .engine-capability.verify > b { color: #9a5b08; }.engine-capability.limited > svg, .engine-capability.limited > b { color: #a33f32; }
@media (max-width: 600px) { .engine-grid { grid-template-columns: 1fr; }.engine-capability { align-items: start; }.engine-capability b { grid-column: 2; } }
</style>
