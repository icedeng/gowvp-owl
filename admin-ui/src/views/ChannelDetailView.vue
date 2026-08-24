<script setup lang="ts">
import { computed, nextTick, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  Aperture,
  ArrowLeft,
  Camera,
  ChevronUp,
  Copy,
  Cpu,
  ExternalLink,
  Film,
  Link2,
  LoaderCircle,
  Mic,
  Radio,
  RefreshCw,
  Save,
  ShieldAlert,
  Square,
  Video,
  Volume2,
  VolumeX,
} from "@lucide/vue";
import { api, errorMessage, typeLabel } from "../services/api";
import type {
  ApiChannel,
  ApiDevice,
  ApiRecording,
  PlayResult,
  Zone,
} from "../types/api";
import { formatDate } from "../utils/format";
import { useUiStore } from "../stores/ui";
import StreamPlayer from "../components/StreamPlayer.vue";
import type { StreamProtocol } from "../player";

const route = useRoute();
const router = useRouter();
const ui = useUiStore();
const loading = ref(false);
const actionLoading = ref("");
const loadError = ref("");
const channel = ref<ApiChannel | null>(null);
const device = ref<ApiDevice | null>(null);
const recordings = ref<ApiRecording[]>([]);
const zones = ref<Zone[]>([]);
const play = ref<PlayResult | null>(null);
const snapshotUrl = ref("");
const aiEnabled = ref(false);
const recordMode = ref("always");
const remoteRecords = ref<Record<string, unknown> | null>(null);
const resourceErrors = reactive({ device: "", recordings: "", zones: "", remote: "" });
const resourceLoading = reactive({ recordings: false, zones: false, remote: false });
const zoneError = ref("");
type ChannelTab = "live" | "ai" | "recordings" | "technical";
const activeTab = ref<ChannelTab>("live");
const liveProtocolPriority: StreamProtocol[] = ["ws-flv", "http-flv", "webrtc", "hls"];
let loadSequence = 0;

const detailTabs = [
  { id: "live" as const, label: "实时值守", description: "预览与控制", icon: Video },
  { id: "ai" as const, label: "智能分析", description: "AI 与检测区域", icon: Cpu },
  { id: "recordings" as const, label: "录像与回看", description: "策略与录像目录", icon: Film },
  { id: "technical" as const, label: "通道档案", description: "协议与流信息", icon: Link2 },
];

const zoneForm = reactive({
  name: "",
  coordinates: "0.1,0.1,0.9,0.1,0.9,0.9,0.1,0.9",
  color: "#38bdf8",
  labels: "person,car",
});

const protocol = computed(() =>
  typeLabel(
    channel.value?.type,
    channel.value?.did || channel.value?.device_id || channel.value?.channel_id || channel.value?.id
  )
);
const isGb = computed(() => protocol.value === "GB28181");
const supportsPtz = computed(() => Boolean(channel.value?.ptz_capable));
const canUseRealtime = computed(() => Boolean(channel.value?.is_online) && !actionLoading.value);
const ptzDisabledReason = computed(() => {
  if (!channel.value?.is_online) return "通道离线，无法发送控制指令";
  if (!supportsPtz.value) return "当前设备未声明 PTZ 能力";
  return "";
});
const voiceDisabledReason = computed(() => {
  if (!isGb.value) return "语音对讲仅支持 GB28181 通道";
  if (!channel.value?.is_online) return "通道离线，无法建立语音会话";
  return "";
});
const streamConfigRoute = computed(() => {
  if (!channel.value || !["RTMP", "RTSP"].includes(protocol.value)) return null;
  return {
    path: protocol.value === "RTMP" ? "/push-streams" : "/pull-streams",
    query: { channel: channel.value.id },
  };
});
const playAddresses = computed(() =>
  (play.value?.items || []).flatMap((item) =>
    Object.entries(item)
      .filter(([key, value]) => key !== "label" && typeof value === "string" && value)
      .map(([key, value]) => ({
        label: `${String(item.label || "")} ${key}`.trim(),
        url: String(value),
      }))
  )
);
const previousRoute = computed(() => {
  const back = window.history.state?.back;
  if (typeof back !== "string" || !back.startsWith("/") || back === route.fullPath) return null;
  const resolved = router.resolve(back);
  if (!resolved.matched.length || resolved.name === "login" || resolved.name === "channel-detail") return null;
  return resolved;
});
const backLabel = computed(() =>
  previousRoute.value?.meta.title ? `返回${String(previousRoute.value.meta.title)}` : "返回国标设备"
);

function goBack() {
  if (previousRoute.value) router.back();
  else router.push("/devices");
}

function selectTab(tab: ChannelTab) {
  activeTab.value = tab;
}

async function onTabKeydown(event: KeyboardEvent, index: number) {
  let nextIndex = index;
  if (event.key === "ArrowRight") nextIndex = (index + 1) % detailTabs.length;
  else if (event.key === "ArrowLeft") nextIndex = (index - 1 + detailTabs.length) % detailTabs.length;
  else if (event.key === "Home") nextIndex = 0;
  else if (event.key === "End") nextIndex = detailTabs.length - 1;
  else return;
  event.preventDefault();
  activeTab.value = detailTabs[nextIndex].id;
  await nextTick();
  document.getElementById(`channel-tab-${activeTab.value}`)?.focus();
}

function clearContext() {
  channel.value = null;
  device.value = null;
  recordings.value = [];
  zones.value = [];
  play.value = null;
  snapshotUrl.value = "";
  remoteRecords.value = null;
  Object.assign(resourceErrors, { device: "", recordings: "", zones: "", remote: "" });
  activeTab.value = "live";
}

async function loadRecordings(id: string, sequence = loadSequence) {
  resourceLoading.recordings = true;
  resourceErrors.recordings = "";
  try {
    const response = await api.recordings({ page: 1, size: 100, cid: id });
    if (sequence === loadSequence) recordings.value = response.data?.items || [];
  } catch (cause) {
    if (sequence === loadSequence) resourceErrors.recordings = errorMessage(cause, "平台录像暂时无法读取");
  } finally {
    if (sequence === loadSequence) resourceLoading.recordings = false;
  }
}

async function loadZones(id: string, sequence = loadSequence) {
  resourceLoading.zones = true;
  resourceErrors.zones = "";
  try {
    const response = await api.zones(id);
    const data = response.data;
    if (sequence === loadSequence) zones.value = Array.isArray(data) ? data : data?.items || [];
  } catch (cause) {
    if (sequence === loadSequence) resourceErrors.zones = errorMessage(cause, "检测区域暂时无法读取");
  } finally {
    if (sequence === loadSequence) resourceLoading.zones = false;
  }
}

async function load() {
  const sequence = ++loadSequence;
  const id = String(route.params.id || "");
  clearContext();
  loading.value = true;
  loadError.value = "";
  try {
    const { data } = await api.channel(id);
    if (sequence !== loadSequence) return;
    if (!data?.id) throw new Error("通道不存在或响应格式错误");
    channel.value = data;
    aiEnabled.value = Boolean(data.ext?.enabled_ai);
    recordMode.value = data.ext?.record_mode || "always";

    void loadRecordings(data.id, sequence);
    void loadZones(data.id, sequence);
    if (data.did) {
      api.device(data.did).then(
        (response) => {
          if (sequence === loadSequence) device.value = response.data || null;
        },
        (cause) => {
          if (sequence === loadSequence) resourceErrors.device = errorMessage(cause, "所属设备信息暂时无法读取");
        }
      );
    }
  } catch (cause) {
    if (sequence === loadSequence) loadError.value = errorMessage(cause, "通道数据暂不可用");
  } finally {
    if (sequence === loadSequence) loading.value = false;
  }
}

async function runAction(name: string, fn: () => Promise<unknown>) {
  if (!channel.value || actionLoading.value) return false;
  actionLoading.value = name;
  try {
    await fn();
    ui.toast(`${channel.value.name || channel.value.id} · ${name}成功`);
    return true;
  } catch (cause) {
    ui.toast(errorMessage(cause, `${name}失败`));
    return false;
  } finally {
    actionLoading.value = "";
  }
}

async function startPlay() {
  if (!channel.value || actionLoading.value) return;
  actionLoading.value = "开始预览";
  try {
    play.value = (await api.play(channel.value.id)).data || null;
    ui.toast("实时流已建立");
  } catch (cause) {
    ui.toast(errorMessage(cause, "实时流建立失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function copyPlayAddress(url: string) {
  try {
    await navigator.clipboard.writeText(url);
    ui.toast("播放地址已复制");
  } catch {
    ui.toast("复制失败，请检查浏览器剪贴板权限");
  }
}

async function refreshSnapshot() {
  if (!channel.value || actionLoading.value) return;
  actionLoading.value = "刷新快照";
  try {
    const { data } = await api.snapshot(channel.value.id);
    snapshotUrl.value = data?.link || api.snapshotImage(channel.value.id, Date.now());
    ui.toast(`快照已刷新${data?.method ? ` · ${data.method}` : ""}`);
  } catch (cause) {
    ui.toast(errorMessage(cause, "快照刷新失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function toggleAI() {
  if (!channel.value) return;
  const next = !aiEnabled.value;
  const succeeded = await runAction(next ? "启用 AI" : "停用 AI", () =>
    next ? api.enableAI(channel.value!.id) : api.disableAI(channel.value!.id)
  );
  if (succeeded) aiEnabled.value = next;
}

async function saveRecordMode() {
  if (!channel.value) return;
  await runAction("保存录像模式", () => api.recordMode(channel.value!.id, recordMode.value));
}

async function addZone() {
  if (!channel.value) return;
  zoneError.value = "";
  if (!zoneForm.name.trim()) {
    zoneError.value = "请输入区域名称";
    return;
  }
  const rawCoordinates = zoneForm.coordinates.split(",").map((item) => item.trim());
  const coordinates = rawCoordinates.map(Number);
  if (coordinates.length < 6 || coordinates.length % 2 || coordinates.some((value) => !Number.isFinite(value))) {
    zoneError.value = "区域坐标需要至少 3 个点，格式为 x1,y1,x2,y2…";
    return;
  }
  const succeeded = await runAction("新增检测区域", () =>
    api.addZone(channel.value!.id, {
      name: zoneForm.name.trim(),
      coordinates,
      color: zoneForm.color,
      labels: zoneForm.labels.split(",").map((item) => item.trim()).filter(Boolean),
    })
  );
  if (succeeded) {
    zoneForm.name = "";
    await loadZones(channel.value.id);
  }
}

async function queryRemoteRecords() {
  if (!channel.value || actionLoading.value) return;
  const end = Math.floor(Date.now() / 1000);
  resourceLoading.remote = true;
  resourceErrors.remote = "";
  actionLoading.value = "查询设备录像";
  try {
    remoteRecords.value = (await api.queryDeviceRecords(channel.value.id, {
      start_at: end - 86400,
      end_at: end,
      timeout: 10,
    })).data || {};
    ui.toast("设备端录像目录查询完成");
  } catch (cause) {
    resourceErrors.remote = errorMessage(cause, "设备录像查询失败");
  } finally {
    resourceLoading.remote = false;
    actionLoading.value = "";
  }
}

watch(() => route.params.id, load, { immediate: true });
</script>

<template>
  <main class="page-content channel-detail-page" :aria-busy="loading">
    <section v-if="loadError && !channel" class="channel-core-error card" role="alert">
      <ShieldAlert />
      <div>
        <h1>通道数据暂不可用</h1>
        <p>{{ loadError }}</p>
      </div>
      <div class="head-actions">
        <button class="btn" type="button" @click="goBack"><ArrowLeft />{{ backLabel }}</button>
        <button class="btn btn-primary" type="button" @click="load"><RefreshCw />重试</button>
      </div>
    </section>

    <div v-else-if="loading && !channel" class="card empty-state" role="status" aria-live="polite">
      <LoaderCircle class="mx-auto mb-3 animate-spin" />正在加载通道详情…
    </div>

    <template v-if="channel">
      <section class="device-command-header channel-command-header">
        <div class="device-command-identity">
          <button class="device-command-back" type="button" :aria-label="backLabel" :title="backLabel" @click="goBack">
            <ArrowLeft />
          </button>
          <span class="device-command-icon"><Radio /></span>
          <div class="min-w-0">
            <div class="device-command-title">
              <h1>{{ channel.name || "未命名通道" }}</h1>
              <span class="status" :class="channel.is_online ? 'online' : 'offline'">{{ channel.is_online ? "在线" : "离线" }}</span>
              <span class="status" :class="channel.is_playing ? 'online' : ''">{{ channel.is_playing ? "LIVE" : "空闲" }}</span>
              <span class="protocol-tag blue">{{ protocol }}</span>
            </div>
            <div class="device-command-meta">
              <span class="mono">{{ channel.channel_id || channel.id }}</span>
              <RouterLink v-if="channel.did" :to="`/devices/${encodeURIComponent(channel.did)}`">
                <Camera />{{ device?.name || channel.device_id || "所属设备" }}
              </RouterLink>
              <span v-else><Camera />{{ device?.name || channel.device_id || "未关联设备" }}</span>
              <small v-if="resourceErrors.device">所属设备信息未加载</small>
            </div>
          </div>
        </div>
        <div v-if="streamConfigRoute" class="head-actions"><RouterLink class="btn" :to="streamConfigRoute">编辑流配置</RouterLink></div>
      </section>

      <div class="channel-detail-tabs" role="tablist" aria-label="通道详情功能">
        <button
          v-for="(item, index) in detailTabs"
          :id="`channel-tab-${item.id}`"
          :key="item.id"
          type="button"
          role="tab"
          :class="{ active: activeTab === item.id }"
          :aria-selected="activeTab === item.id"
          :aria-controls="`channel-panel-${item.id}`"
          :tabindex="activeTab === item.id ? 0 : -1"
          @click="selectTab(item.id)"
          @keydown="onTabKeydown($event, index)"
        >
          <span class="channel-tab-icon"><component :is="item.icon" /></span>
          <span><strong>{{ item.label }}</strong><small>{{ item.description }}</small></span>
        </button>
      </div>

      <section v-if="activeTab === 'live'" id="channel-panel-live" class="channel-detail-section" role="tabpanel" aria-labelledby="channel-tab-live" tabindex="0">
        <div class="detail-section-head">
          <div><h2>实时值守</h2><p>在同一工作区完成预览、云台控制、快照和语音对讲。</p></div>
        </div>
        <div class="channel-live-grid">
          <article class="card video-workspace">
            <div class="video-tile active !border-0 !aspect-video">
              <StreamPlayer :result="play" :poster="snapshotUrl" :autoplay="Boolean(play)" :protocol-priority="liveProtocolPriority" @error="(message) => ui.toast(message)" />
              <span class="video-meta"><span class="status" :class="channel.is_online ? 'online' : 'offline'">{{ channel.is_online ? "在线" : "离线" }}</span><span v-if="recordMode !== 'none'" class="rec">REC</span></span>
            </div>
            <div class="video-foot channel-video-foot">
              <div class="stream-context">
                <strong>{{ playAddresses.length ? "实时流已建立" : channel.is_online ? "尚未开始预览" : "通道离线，暂时无法预览" }}</strong>
                <span>{{ playAddresses.length ? `${playAddresses.length} 个播放地址可用于联调测试` : "开始预览后将显示可用协议地址" }}</span>
              </div>
              <div class="stream-actions">
                <button class="stream-link stream-primary" type="button" :disabled="!channel.is_online || Boolean(actionLoading)" @click="startPlay"><LoaderCircle v-if="actionLoading === '开始预览'" class="animate-spin" /><Video v-else />{{ play ? "重新预览" : "开始预览" }}</button>
                <button class="stream-link" type="button" :disabled="!canUseRealtime" @click="refreshSnapshot"><Aperture />抓拍</button>
              </div>
            </div>
          </article>

          <section class="card channel-stream-addresses" aria-labelledby="channel-stream-address-title">
            <div class="card-head">
              <div><h3 id="channel-stream-address-title" class="card-title">播放地址</h3><p class="card-sub">完整地址可复制到 VLC、FFplay 或接口测试工具</p></div>
              <span class="protocol-tag blue">{{ playAddresses.length }} 个</span>
            </div>
            <div v-if="playAddresses.length" class="channel-stream-address-list">
              <div v-for="item in playAddresses" :key="item.url" class="channel-stream-address-row">
                <span class="protocol-tag">{{ item.label }}</span>
                <code>{{ item.url }}</code>
                <div class="channel-stream-address-actions">
                  <a class="device-row-action" :href="item.url" target="_blank" rel="noreferrer" :aria-label="`打开 ${item.label} 地址`" title="打开测试"><ExternalLink /></a>
                  <button class="device-row-action" type="button" :aria-label="`复制 ${item.label} 地址`" title="复制地址" @click="copyPlayAddress(item.url)"><Copy /></button>
                </div>
              </div>
            </div>
            <div v-else class="empty-state compact">开始预览后显示后端返回的完整播放地址。</div>
          </section>

          <aside class="control-stack channel-control-stack">
            <article class="card control-panel">
              <div class="card-head"><div><h3 class="card-title">云台控制</h3><p class="card-sub">方向控制与能力探测</p></div><span class="status" :class="supportsPtz ? 'online' : ''">{{ supportsPtz ? "可用" : "不可用" }}</span></div>
              <div class="ptz" :aria-disabled="Boolean(ptzDisabledReason)">
                <button class="up" type="button" aria-label="云台向上" :disabled="Boolean(ptzDisabledReason) || Boolean(actionLoading)" @click="runAction('云台向上', () => api.ptz(channel!.id, { action: 'up', speed: 30, timeout: 1 }))"><ChevronUp /></button>
                <button class="right" type="button" aria-label="云台向右" :disabled="Boolean(ptzDisabledReason) || Boolean(actionLoading)" @click="runAction('云台向右', () => api.ptz(channel!.id, { action: 'right', speed: 30, timeout: 1 }))"><ChevronUp /></button>
                <button class="down" type="button" aria-label="云台向下" :disabled="Boolean(ptzDisabledReason) || Boolean(actionLoading)" @click="runAction('云台向下', () => api.ptz(channel!.id, { action: 'down', speed: 30, timeout: 1 }))"><ChevronUp /></button>
                <button class="left" type="button" aria-label="云台向左" :disabled="Boolean(ptzDisabledReason) || Boolean(actionLoading)" @click="runAction('云台向左', () => api.ptz(channel!.id, { action: 'left', speed: 30, timeout: 1 }))"><ChevronUp /></button>
                <button class="center" type="button" aria-label="停止云台" :disabled="Boolean(ptzDisabledReason) || Boolean(actionLoading)" @click="runAction('停止云台', () => api.ptz(channel!.id, { action: 'stop', speed: 30, timeout: 1 }))"><Square /></button>
              </div>
              <p class="capability-reason">{{ ptzDisabledReason || (channel.ptz_verified ? "能力已通过实际命令验证" : "能力来自设备静态声明") }}</p>
              <button class="btn w-full" type="button" :disabled="!channel.is_online || Boolean(actionLoading)" @click="runAction('PTZ 能力探测', () => api.ptzProbe(channel!.id, { action: 'stop', speed: 30, timeout: 5 }))"><RefreshCw />重新探测</button>
            </article>

            <article class="card control-panel">
              <div class="card-head"><div><h3 class="card-title">快捷操作</h3><p class="card-sub">保持当前画面持续可见</p></div><Mic /></div>
              <div class="channel-quick-actions">
                <button class="btn" type="button" :disabled="!canUseRealtime" @click="refreshSnapshot"><Aperture />刷新快照</button>
                <button class="btn" type="button" :disabled="Boolean(voiceDisabledReason) || Boolean(actionLoading)" @click="runAction('开始对讲', () => api.voiceStart(channel!.id, { mode: 'talk' }))"><Volume2 />开始对讲</button>
                <button class="btn" type="button" :disabled="Boolean(voiceDisabledReason) || Boolean(actionLoading)" @click="runAction('停止对讲', () => api.voiceStop(channel!.id, { mode: 'talk' }))"><VolumeX />停止对讲</button>
              </div>
              <p class="capability-reason">{{ voiceDisabledReason || "当前通道支持 GB28181 语音会话" }}</p>
            </article>
          </aside>
        </div>
      </section>

      <section v-else-if="activeTab === 'ai'" id="channel-panel-ai" class="channel-detail-section" role="tabpanel" aria-labelledby="channel-tab-ai" tabindex="0">
        <div class="detail-section-head"><div><h2>智能分析</h2><p>管理通道 AI 状态与实际生效的检测区域。</p></div></div>
        <div class="channel-ai-grid">
          <article class="card form-section channel-ai-summary">
            <div class="card-head"><div><h3 class="card-title">AI 分析状态</h3><p class="card-sub">启用前请确认已配置检测区域</p></div><span class="status" :class="aiEnabled ? 'online' : ''">{{ aiEnabled ? "运行中" : "未启用" }}</span></div>
            <div class="channel-ai-state"><Cpu /><div><strong>{{ zones[0]?.name || "尚未配置检测区域" }}</strong><span>{{ zones[0] ? `${zones[0].labels?.join('、') || '默认标签'} · ${zones[0].coordinates.length / 2} 个点` : "新增区域后，第一个区域将作为当前生效区域" }}</span></div></div>
            <button class="btn btn-primary w-full" type="button" :disabled="Boolean(actionLoading) || (!aiEnabled && !zones.length)" @click="toggleAI">{{ aiEnabled ? "停用 AI" : "启用 AI" }}</button>
          </article>

          <article class="card form-section">
            <div class="card-head"><div><h3 class="card-title">检测区域</h3><p class="card-sub">当前 {{ zones.length }} 个，后端运行逻辑读取第一个区域</p></div><span class="protocol-tag amber">单区域生效</span></div>
            <div v-if="resourceErrors.zones" class="inline-resource-error" role="alert"><span>{{ resourceErrors.zones }}</span><button class="btn btn-sm" type="button" @click="loadZones(channel.id)">重试</button></div>
            <div v-else-if="resourceLoading.zones" class="empty-state compact" role="status"><LoaderCircle class="animate-spin" />正在读取检测区域…</div>
            <div v-else class="zone-list">
              <div v-for="(zone, index) in zones" :key="`${zone.name}-${index}`" class="read-only zone-row"><span class="zone-swatch" :style="{ background: zone.color || '#38bdf8' }" /><div><strong>{{ zone.name }}</strong><small>{{ zone.labels?.join("、") || "默认标签" }} · {{ zone.coordinates.length / 2 }} 个点</small></div><span class="protocol-tag" :class="index === 0 ? 'blue' : ''">{{ index === 0 ? "当前生效" : "备用" }}</span></div>
              <div v-if="!zones.length" class="empty-state compact">尚未配置检测区域</div>
            </div>
            <form class="channel-zone-form" @submit.prevent="addZone">
              <div class="form-grid">
                <label class="form-group"><span class="form-label">区域名称</span><input v-model="zoneForm.name" class="input plain w-full" :aria-invalid="Boolean(zoneError)" aria-describedby="zone-error" /></label>
                <label class="form-group"><span class="form-label">检测标签</span><input v-model="zoneForm.labels" class="input plain w-full" /></label>
                <label class="form-group full"><span class="form-label">归一化坐标（逗号分隔）</span><textarea v-model="zoneForm.coordinates" class="textarea mono" :aria-invalid="Boolean(zoneError)" aria-describedby="zone-error" /></label>
              </div>
              <p v-if="zoneError" id="zone-error" class="field-error" role="alert">{{ zoneError }}</p>
              <div class="settings-savebar"><span>API 当前支持查询与新增，不提供编辑或删除。</span><button class="btn btn-primary" type="submit" :disabled="Boolean(actionLoading)"><Save />新增区域</button></div>
            </form>
          </article>
        </div>
      </section>

      <section v-else-if="activeTab === 'recordings'" id="channel-panel-recordings" class="channel-detail-section" role="tabpanel" aria-labelledby="channel-tab-recordings" tabindex="0">
        <div class="detail-section-head"><div><h2>录像与回看</h2><p>集中管理平台录像策略、录像片段和 GB28181 设备端录像。</p></div><RouterLink class="btn btn-primary" :to="`/recordings?channel=${encodeURIComponent(channel.id)}`"><Film />进入录像中心</RouterLink></div>
        <div class="channel-recording-summary">
          <div><small>当前策略</small><strong>{{ recordMode === "always" ? "持续录像" : "不录制" }}</strong></div>
          <div><small>平台片段</small><strong>{{ resourceErrors.recordings ? "—" : recordings.length }}</strong></div>
          <div><small>最近录像</small><strong>{{ resourceErrors.recordings ? "读取失败" : formatDate(recordings[0]?.started_at, "暂无") }}</strong></div>
        </div>
        <div class="channel-recording-grid">
          <article class="card form-section">
            <div class="card-head"><div><h3 class="card-title">平台录像策略</h3><p class="card-sub">控制当前通道的媒体录制行为</p></div><Film /></div>
            <label class="form-group"><span class="form-label">录像模式</span><select v-model="recordMode" class="select w-full"><option value="always">持续录像（always）</option><option value="none">不录制（none）</option><option value="ai" disabled>AI 触发（暂不可用）</option></select></label>
            <div class="warning-box mt-4"><ShieldAlert /><span>AI 模式当前与持续录像行为相同，因此暂不允许切换。</span></div>
            <button class="btn btn-primary mt-4" type="button" :disabled="Boolean(actionLoading)" @click="saveRecordMode"><Save />保存策略</button>
          </article>

          <article class="card card-pad">
            <div class="card-head"><div><h3 class="card-title">平台录像</h3><p class="card-sub">最近录制的通道片段</p></div><Film /></div>
            <div v-if="resourceErrors.recordings" class="inline-resource-error" role="alert"><span>{{ resourceErrors.recordings }}</span><button class="btn btn-sm" type="button" @click="loadRecordings(channel.id)">重试</button></div>
            <div v-else-if="resourceLoading.recordings" class="empty-state compact" role="status"><LoaderCircle class="animate-spin" />正在读取录像…</div>
            <div v-else-if="recordings.length" class="channel-recording-list"><div v-for="item in recordings.slice(0, 5)" :key="item.id"><span><strong>{{ formatDate(item.started_at) }}</strong><small>{{ item.app || channel.app || "—" }} / {{ item.stream || channel.stream || channel.id }}</small></span><a class="device-row-action" :href="api.recordingDownloadUrl(item.id)" download aria-label="下载录像"><ExternalLink /></a></div></div>
            <div v-else class="empty-state compact">{{ recordMode === "none" ? "当前未开启平台录像" : "已开启录像，但暂时没有片段" }}</div>
          </article>

          <article class="card card-pad">
            <div class="card-head"><div><h3 class="card-title">设备端录像</h3><p class="card-sub">查询最近 24 小时的 GB28181 设备录像目录</p></div><Camera /></div>
            <button class="btn" type="button" :disabled="!isGb || !channel.is_online || Boolean(actionLoading)" @click="queryRemoteRecords"><LoaderCircle v-if="resourceLoading.remote" class="animate-spin" /><RefreshCw v-else />查询设备录像</button>
            <p v-if="!isGb" class="capability-reason">设备端录像查询仅支持 GB28181 通道。</p>
            <p v-else-if="!channel.is_online" class="capability-reason">通道离线，暂时无法查询设备录像。</p>
            <div v-if="resourceErrors.remote" class="inline-resource-error mt-4" role="alert">{{ resourceErrors.remote }}</div>
            <details v-else-if="remoteRecords" class="detail-raw-card mt-4"><summary>查看设备技术响应</summary><pre>{{ JSON.stringify(remoteRecords, null, 2) }}</pre></details>
            <div v-else class="read-only mt-4">尚未查询。查询完成后可核验设备端录像目录；当前后端没有全量下载任务列表接口。</div>
          </article>
        </div>
      </section>

      <section v-else id="channel-panel-technical" class="channel-detail-section" role="tabpanel" aria-labelledby="channel-tab-technical" tabindex="0">
        <div class="detail-section-head"><div><h2>通道档案</h2><p>用于运维核验的协议、归属和媒体流标识。</p></div></div>
        <article class="card card-pad"><dl class="channel-technical-grid"><div><dt>协议</dt><dd>{{ protocol }}</dd></div><div><dt>通道编号</dt><dd class="mono">{{ channel.channel_id || channel.id }}</dd></div><div><dt>所属设备</dt><dd>{{ device?.name || channel.device_id || "—" }}</dd></div><div><dt>设备 ID</dt><dd class="mono">{{ channel.did || channel.device_id || "—" }}</dd></div><div><dt>应用 / 流</dt><dd class="mono">{{ channel.app || "—" }} / {{ channel.stream || "—" }}</dd></div><div><dt>更新时间</dt><dd>{{ formatDate(channel.updated_at) }}</dd></div></dl><details v-if="playAddresses.length" class="detail-raw-card mt-4"><summary><Link2 />查看全部播放地址（{{ playAddresses.length }}）</summary><div class="technical-link-list"><a v-for="item in playAddresses" :key="item.url" :href="item.url" target="_blank" rel="noreferrer"><strong>{{ item.label }}</strong><span class="mono">{{ item.url }}</span></a></div></details></article>
      </section>
    </template>
  </main>
</template>
