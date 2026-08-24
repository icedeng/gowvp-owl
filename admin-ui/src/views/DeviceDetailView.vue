<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  Activity,
  ArrowLeft,
  Camera,
  ChevronDown,
  ChevronRight,
  Clock3,
  Download,
  FileCog,
  History,
  Info,
  ListVideo,
  LoaderCircle,
  Play,
  Radio,
  RefreshCcw,
  Search,
  Settings2,
  ShieldAlert,
  ShieldCheck,
  SlidersHorizontal,
  Trash2,
  UploadCloud,
  Wifi,
  Wrench,
} from "@lucide/vue";
import { api, collectPages, errorMessage, typeLabel } from "../services/api";
import type { ApiChannel, ApiDevice, DeviceHistoryRecord } from "../types/api";
import { formatDate, relativeTime } from "../utils/format";
import { useUiStore } from "../stores/ui";
import ModalDialog from "../components/ModalDialog.vue";

const route = useRoute();
const router = useRouter();
const ui = useUiStore();
type DetailTab = "overview" | "channels" | "operations" | "diagnostics";
type HistoryKind = "heartbeat" | "register";

const tab = ref<DetailTab>("overview");
const loading = ref(false);
const loadError = ref("");
const actionLoading = ref("");
const device = ref<ApiDevice | null>(null);
const relatedChannels = ref<ApiChannel[]>([]);
const diagnostics = ref<Record<string, unknown> | null>(null);
const a4 = ref<Record<string, unknown> | null>(null);
const heartbeatHistory = ref<DeviceHistoryRecord[]>([]);
const registerHistory = ref<DeviceHistoryRecord[]>([]);
const historyKind = ref<HistoryKind | null>(null);
const historyRows = ref<DeviceHistoryRecord[]>([]);
const historyLoading = ref(false);
const historyError = ref("");
const editOpen = ref(false);
const deleteOpen = ref(false);
const deleting = ref(false);
const editForm = reactive({
  name: "",
  device_id: "",
  username: "",
  password: "",
  ip: "",
  port: 0,
  stream_mode: 1,
  gb_version: "",
});
const basicForm = reactive({
  name: "",
  expiration: 3600,
  heartbeat_interval: 60,
  heartbeat_count: 3,
});
const isGb = computed(
  () =>
    typeLabel(
      device.value?.type,
      device.value?.device_id || device.value?.id
    ) === "GB28181"
);
const tabs: Array<{ key: DetailTab; name: string; note: string; icon: typeof Activity }> = [
  { key: "overview", name: "运行概览", note: "状态、档案与最近活动", icon: Activity },
  { key: "channels", name: "通道资源", note: "预览与通道能力", icon: ListVideo },
  { key: "operations", name: "设备运维", note: "同步、订阅与参数下发", icon: Wrench },
  { key: "diagnostics", name: "协议诊断", note: "版本、探测与高级查询", icon: ShieldCheck },
];
const deviceAddress = computed(() =>
  device.value?.address ||
  [device.value?.ip, device.value?.port].filter(Boolean).join(":") ||
  "—"
);
const protocolVersion = computed(() =>
  device.value?.ext?.gb_effective_version || device.value?.ext?.gb_version || "—"
);
const streamMode = computed(() => {
  const mode = Number(device.value?.stream_mode);
  return ({ 0: "UDP", 1: "TCP 被动", 2: "TCP 主动" } as Record<number, string>)[mode] || device.value?.transport?.toUpperCase() || "—";
});
const onlineChannels = computed(() => relatedChannels.value.filter((item) => item.is_online).length);
const recentActivity = computed(() => {
  const rows = [...heartbeatHistory.value, ...registerHistory.value];
  const kinds = new Set(rows.map((item) => item.kind));
  if (!kinds.has("heartbeat") && device.value?.keepalive_at) {
    rows.push({
      id: -1,
      device_id: device.value.device_id || device.value.id,
      kind: "heartbeat",
      recorded_at: device.value.keepalive_at,
      address: deviceAddress.value,
      status: device.value.is_online ? "在线" : "最近状态",
    });
  }
  if (!kinds.has("register") && device.value?.registered_at) {
    rows.push({
      id: -2,
      device_id: device.value.device_id || device.value.id,
      kind: "register",
      recorded_at: device.value.registered_at,
      address: deviceAddress.value,
      status: "已注册",
    });
  }
  return rows
    .sort((a, b) => new Date(b.recorded_at || 0).getTime() - new Date(a.recorded_at || 0).getTime())
    .slice(0, 6);
});
const historyTitle = computed(() =>
  historyKind.value === "heartbeat" ? "心跳记录" : "注册记录"
);

function latestHistorySnapshot(kind: HistoryKind): DeviceHistoryRecord[] {
  if (!device.value) return [];
  const recordedAt = kind === "heartbeat" ? device.value.keepalive_at : device.value.registered_at;
  if (!recordedAt) return [];
  return [{
    id: kind === "heartbeat" ? -1 : -2,
    device_id: device.value.device_id || device.value.id,
    kind,
    recorded_at: recordedAt,
    address: deviceAddress.value,
    status: kind === "heartbeat" ? (device.value.is_online ? "在线" : "最近状态") : "已注册",
  }];
}

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const id = String(route.params.id);
    const { data } = await api.device(id);
    device.value = data;
    basicForm.name = data.name || "";
    const [channelResponse, heartbeatResponse, registerResponse] = await Promise.allSettled([
      collectPages(api.channels, { did: data.id }),
      api.deviceHistory(data.id, "heartbeat", { page: 1, size: 6 }),
      api.deviceHistory(data.id, "register", { page: 1, size: 6 }),
    ]);
    relatedChannels.value = channelResponse.status === "fulfilled" ? channelResponse.value.items : [];
    heartbeatHistory.value = heartbeatResponse.status === "fulfilled" ? heartbeatResponse.value.data?.items || [] : [];
    registerHistory.value = registerResponse.status === "fulfilled" ? registerResponse.value.data?.items || [] : [];
    if (isGb.value) {
      try {
        diagnostics.value = (await api.gbDiagnostics(data.id)).data;
      } catch {
        diagnostics.value = null;
      }
    }
  } catch (cause) {
    loadError.value = errorMessage(cause, "设备详情加载失败");
  } finally {
    loading.value = false;
  }
}

async function runAction(
  name: string,
  fn: () => Promise<unknown>,
  refresh = false
): Promise<boolean> {
  if (!device.value) return false;
  actionLoading.value = name;
  try {
    await fn();
    ui.toast(`${device.value.name || device.value.id} · ${name}成功`);
    if (refresh) await load();
    return true;
  } catch (cause) {
    ui.toast(errorMessage(cause, `${name}失败`));
    return false;
  } finally {
    actionLoading.value = "";
  }
}

function openEdit() {
  if (!device.value) return;
  Object.assign(editForm, {
    name: device.value.name || "",
    device_id: device.value.device_id || "",
    username: device.value.username || "",
    password: "",
    ip: device.value.ip || "",
    port: device.value.port || 0,
    stream_mode: device.value.stream_mode ?? 1,
    gb_version: device.value.ext?.gb_manual_version || "",
  });
  editOpen.value = true;
}

async function saveEdit() {
  if (!device.value) return;
  const saved = await runAction(
    "保存设备",
    () =>
      api.editDevice(device.value!.id, {
        ...editForm,
        password: editForm.password || device.value?.password || "",
      }),
    true
  );
  if (saved) editOpen.value = false;
}

async function deleteDevice() {
  if (!device.value) return;
  deleting.value = true;
  const name = device.value.name || device.value.device_id || device.value.id;
  try {
    await api.deleteDevice(device.value.id);
    ui.toast(`设备 ${name} 已删除`);
    await router.push("/devices");
  } catch (cause) {
    ui.toast(errorMessage(cause, "设备删除失败"));
  } finally {
    deleting.value = false;
  }
}

async function openHistory(kind: HistoryKind) {
  if (!device.value) return;
  historyKind.value = kind;
  const loadedRows = kind === "heartbeat" ? heartbeatHistory.value : registerHistory.value;
  historyRows.value = loadedRows.length ? loadedRows : latestHistorySnapshot(kind);
  historyError.value = "";
  historyLoading.value = true;
  try {
    const { data } = await api.deviceHistory(device.value.id, kind, { page: 1, size: 100 });
    historyRows.value = data?.items || [];
  } catch (cause) {
    historyError.value = "历史明细接口暂不可用，当前展示设备最近状态。";
  } finally {
    historyLoading.value = false;
  }
}

async function saveBasic() {
  if (!device.value) return;
  await runAction("下发 BasicParam", () =>
    api.gbConfig(device.value!.id, {
      target_id: device.value?.device_id,
      timeout: 8,
      basic_param: basicForm,
    })
  );
}

async function queryA4() {
  if (!device.value) return;
  actionLoading.value = "查询 A.4 快照";
  try {
    a4.value = (await api.gbA4Snapshot(device.value.id, { limit: 50 })).data;
    ui.toast("A.4 快照查询完成");
  } catch (cause) {
    ui.toast(errorMessage(cause, "A.4 快照查询失败"));
  } finally {
    actionLoading.value = "";
  }
}

onMounted(load);
</script>

<template>
  <main class="page-content device-detail-page">
    <div v-if="loadError" class="warning-box mb-4" role="alert">
      <ShieldAlert /><span>{{ loadError }}</span><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <div v-if="loading && !device" class="detail-skeleton" aria-live="polite">
      <span /><span /><span />
      <p><LoaderCircle class="animate-spin" />正在加载设备运行状态…</p>
    </div>

    <template v-if="device">
      <section class="device-command-header">
        <div class="device-command-identity">
          <RouterLink
            class="device-command-back"
            to="/devices"
            aria-label="返回国标设备列表"
            title="返回国标设备"
          ><ArrowLeft /></RouterLink>
          <span class="device-command-icon"><Camera /></span>
          <div>
            <div class="device-command-title">
              <h1>{{ device.name || "未命名设备" }}</h1>
              <span class="status" :class="device.is_online ? 'online' : 'offline'">{{ device.is_online ? "在线" : "离线" }}</span>
              <span class="protocol-tag blue">GB28181 · {{ protocolVersion }}</span>
            </div>
            <div class="device-command-meta">
              <span class="mono">{{ device.device_id || device.id }}</span>
              <span><Wifi />{{ deviceAddress }}</span>
              <span><Clock3 />最近心跳 {{ relativeTime(device.keepalive_at) }}</span>
            </div>
          </div>
        </div>
        <div class="head-actions">
          <button class="btn" @click="openEdit"><SlidersHorizontal />编辑档案</button>
          <button class="btn btn-danger" type="button" @click="deleteOpen = true"><Trash2 />删除设备</button>
        </div>
      </section>

      <section class="device-pulse" aria-label="设备运行摘要">
        <div class="device-pulse-lead" :class="{ offline: !device.is_online }">
          <span class="pulse-led" />
          <div><strong>{{ device.is_online ? "信令链路正常" : "设备当前离线" }}</strong><small>{{ device.is_online ? "注册与心跳持续更新" : "请检查注册、网络及鉴权配置" }}</small></div>
        </div>
        <div><small>通道在线</small><strong>{{ onlineChannels }} / {{ relatedChannels.length }}</strong><span>当前目录资源</span></div>
        <div><small>最近心跳</small><strong>{{ relativeTime(device.keepalive_at) }}</strong><span>{{ heartbeatHistory.length ? "历史已记录" : "等待新记录" }}</span></div>
        <div><small>流传输</small><strong>{{ streamMode }}</strong><span>{{ device.transport?.toUpperCase() || "SIP" }} 信令</span></div>
        <div><small>注册有效期</small><strong>{{ device.expires || "—" }}<em v-if="device.expires">s</em></strong><span>{{ formatDate(device.registered_at) }}</span></div>
      </section>

      <section class="device-workbench">
        <aside class="device-workbench-nav">
          <nav class="card detail-task-nav" aria-label="设备详情工作区">
            <button v-for="item in tabs" :key="item.key" :class="{ active: tab === item.key }" @click="tab = item.key">
              <component :is="item.icon" /><span><strong>{{ item.name }}</strong><small>{{ item.note }}</small></span><ChevronRight />
            </button>
          </nav>
          <article class="card detail-side-facts">
            <div><span>厂商</span><strong>{{ device.ext?.manufacturer || "—" }}</strong></div>
            <div><span>型号</span><strong>{{ device.ext?.model || "—" }}</strong></div>
            <div><span>接入地址</span><strong class="mono">{{ deviceAddress }}</strong></div>
          </article>
        </aside>

        <div class="device-workspace-content">
          <template v-if="tab === 'overview'">
            <section class="detail-section-head">
              <div><h2>运行概览</h2><p>核对设备注册档案、协议能力与最近信令活动。</p></div>
            </section>
            <section class="detail-overview-grid">
              <article class="card detail-profile-card">
                <div class="card-head"><div><h3 class="card-title">设备档案</h3><p class="card-sub">注册、网络与协议基础信息</p></div><FileCog /></div>
                <dl class="detail-definition-list">
                  <div><dt>设备编码</dt><dd class="mono">{{ device.device_id || device.id }}</dd></div>
                  <div><dt>厂商 / 型号</dt><dd>{{ [device.ext?.manufacturer, device.ext?.model].filter(Boolean).join(" / ") || "—" }}</dd></div>
                  <div><dt>注册地址</dt><dd class="mono">{{ deviceAddress }}</dd></div>
                  <div><dt>流传输模式</dt><dd>{{ streamMode }}</dd></div>
                  <div><dt>注册时间</dt><dd>{{ formatDate(device.registered_at) }}</dd></div>
                  <div><dt>协议版本</dt><dd>{{ protocolVersion }} <small>{{ device.ext?.gb_version_source || "自动识别" }}</small></dd></div>
                </dl>
              </article>
              <article class="card detail-capability-card">
                <div class="card-head"><div><h3 class="card-title">能力就绪度</h3><p class="card-sub">按版本声明与探测结果汇总</p></div><ShieldCheck /></div>
                <div class="detail-capability-list">
                  <div class="ready"><Play /><span><strong>实时播放</strong><small>统一播放链路可用</small></span><b>可用</b></div>
                  <div :class="device.ptz_capable ? 'ready' : 'pending'"><Activity /><span><strong>PTZ 控制</strong><small>{{ device.ptz_verified ? "设备通道已验证" : device.ptz_capable ? "设备声明支持" : "尚未完成探测" }}</small></span><b>{{ device.ptz_capable ? "支持" : "待探测" }}</b></div>
                  <div class="ready"><Camera /><span><strong>快照与录像</strong><small>进入具体通道执行</small></span><b>可用</b></div>
                  <div :class="device.ext?.gb_version_capabilities?.length ? 'ready' : 'pending'"><Settings2 /><span><strong>版本扩展</strong><small>{{ device.ext?.gb_version_capabilities?.length || 0 }} 项能力声明</small></span><b>{{ protocolVersion }}</b></div>
                </div>
              </article>
              <article class="card detail-activity-card">
                <div class="card-head detail-activity-head">
                  <div><h3 class="card-title">最近信令活动</h3><p class="card-sub">设备心跳与注册事件</p></div>
                  <div class="detail-history-actions">
                    <button type="button" @click="openHistory('heartbeat')"><Activity />心跳记录</button>
                    <button type="button" @click="openHistory('register')"><Radio />注册记录</button>
                  </div>
                </div>
                <div v-if="recentActivity.length" class="device-activity-list">
                  <div v-for="record in recentActivity" :key="`${record.kind}-${record.id}`">
                    <span class="activity-kind" :class="record.kind"><Activity v-if="record.kind === 'heartbeat'" /><Radio v-else /></span>
                    <span><strong>{{ record.kind === "heartbeat" ? "心跳上报" : "设备注册" }}</strong><small class="mono">{{ record.address || "来源地址未记录" }}</small></span>
                    <time>{{ relativeTime(record.recorded_at) }}</time>
                  </div>
                </div>
                <div v-else class="compact-empty"><History /><span><strong>暂无信令时间</strong><small>设备产生心跳或注册事件后会显示在这里。</small></span></div>
              </article>
            </section>
          </template>

          <template v-else-if="tab === 'channels'">
            <section class="detail-section-head">
              <div><h2>通道资源</h2><p>查看通道目录、在线状态并进入通道详情。</p></div>
              <button class="btn" :disabled="actionLoading === '同步通道'" @click="runAction('同步通道', () => api.catalog(device!.id), true)">
                <LoaderCircle v-if="actionLoading === '同步通道'" class="animate-spin" /><RefreshCcw v-else />同步通道
              </button>
            </section>
            <section class="card detail-channel-list">
              <div v-for="channel in relatedChannels" :key="channel.id" class="detail-channel-row">
                <span class="channel-status-icon" :class="{ offline: !channel.is_online }"><Camera /></span>
                <span class="channel-main"><strong>{{ channel.name || "未命名通道" }}</strong><small class="mono">{{ channel.channel_id || channel.id }}</small></span>
                <span><small>状态</small><b class="status" :class="channel.is_online ? 'online' : 'offline'">{{ channel.is_online ? "在线" : "离线" }}</b></span>
                <span><small>PTZ</small><b>{{ channel.ptz_verified ? "已验证" : channel.ptz_capable ? "声明支持" : "不支持" }}</b></span>
                <span><small>AI</small><b>{{ channel.ext?.enabled_ai ? "已启用" : "未启用" }}</b></span>
                <div class="device-row-actions channel-row-actions">
                  <RouterLink class="device-row-detail" :to="`/channels/${encodeURIComponent(channel.id)}`">详情<ChevronRight /></RouterLink>
                </div>
              </div>
              <div v-if="!relatedChannels.length" class="compact-empty"><ListVideo /><span><strong>暂无通道资源</strong><small>执行同步通道，从设备获取最新通道。</small></span></div>
            </section>
          </template>

          <template v-else-if="tab === 'operations'">
            <section class="detail-section-head"><div><h2>设备运维</h2><p>执行设备级同步、校时、订阅和基础参数下发。</p></div></section>
            <section class="detail-operations-grid">
              <article class="card detail-operation-card">
                <div class="card-head"><div><h3 class="card-title">同步与校时</h3><p class="card-sub">更新设备目录与平台时间</p></div><RefreshCcw /></div>
                <div class="operation-row"><span><strong>目录同步</strong><small>增量保存通道并保留业务配置</small></span><button class="btn btn-sm" :disabled="actionLoading === '目录同步'" @click="runAction('目录同步', () => api.catalog(device!.id), true)">执行</button></div>
                <div class="operation-row"><span><strong>设备校时</strong><small>向设备下发平台当前时间</small></span><button class="btn btn-sm" :disabled="actionLoading === '设备校时'" @click="runAction('设备校时', () => api.timeSync(device!.id))">执行</button></div>
              </article>
              <article class="card detail-operation-card">
                <div class="card-head"><div><h3 class="card-title">事件订阅</h3><p class="card-sub">单次订阅有效期 3600 秒</p></div><Radio /></div>
                <div v-for="item in [{ name: '目录订阅', event: 'catalog' }, { name: '报警订阅', event: 'alarm' }, { name: '位置订阅', event: 'mobile_position' }]" :key="item.event" class="operation-row">
                  <span><strong>{{ item.name }}</strong><small class="mono">{{ item.event }}</small></span><button class="btn btn-sm" :disabled="actionLoading === item.name" @click="runAction(item.name, () => api.subscribe(device!.id, { event: item.event, expires: 3600 }))">订阅</button>
                </div>
              </article>
              <article class="card detail-basic-config">
                <div class="card-head"><div><h3 class="card-title">BasicParam 基础参数</h3><p class="card-sub">配置将通过 SIP 下发至在线设备</p></div><span class="protocol-tag blue">2014+</span></div>
                <div class="operation-notice"><Info />下发前请确认设备在线并支持 GB/T 28181-2014 或更高版本。</div>
                <form @submit.prevent="saveBasic">
                  <div class="form-grid">
                    <label class="form-group"><span class="form-label">设备名称</span><input v-model="basicForm.name" class="input plain w-full" required /></label>
                    <label class="form-group"><span class="form-label">注册过期时间（秒）</span><input v-model.number="basicForm.expiration" class="input plain w-full" type="number" min="1" required /></label>
                    <label class="form-group"><span class="form-label">心跳间隔（秒）</span><input v-model.number="basicForm.heartbeat_interval" class="input plain w-full" type="number" min="1" required /></label>
                    <label class="form-group"><span class="form-label">心跳超时次数</span><input v-model.number="basicForm.heartbeat_count" class="input plain w-full" type="number" min="1" required /></label>
                  </div>
                  <div class="detail-form-actions"><span>设备返回响应后才表示配置生效</span><button class="btn btn-primary" :disabled="!device.is_online || actionLoading === '下发 BasicParam'"><LoaderCircle v-if="actionLoading === '下发 BasicParam'" class="animate-spin" /><UploadCloud v-else />下发配置</button></div>
                </form>
              </article>
            </section>
          </template>

          <template v-else>
            <section class="detail-section-head"><div><h2>协议诊断</h2><p>核验版本能力，执行在线探测并查看协议扩展结果。</p></div><RouterLink class="btn" :to="`/diagnostics?device=${encodeURIComponent(device.id)}`">打开完整诊断</RouterLink></section>
            <section class="detail-diagnostics-grid">
              <article class="card detail-version-card">
                <div class="card-head"><div><h3 class="card-title">协议档案</h3><p class="card-sub">自动协商与手动覆盖结果</p></div><ShieldCheck /></div>
                <dl class="detail-definition-list compact"><div><dt>有效版本</dt><dd>{{ protocolVersion }}</dd></div><div><dt>声明版本</dt><dd>{{ device.ext?.gb_declared_version || "—" }}</dd></div><div><dt>版本来源</dt><dd>{{ device.ext?.gb_version_source || "—" }}</dd></div><div><dt>手动覆盖</dt><dd>{{ device.ext?.gb_manual_version || "未设置" }}</dd></div></dl>
                <div class="capability-tags"><span v-for="item in device.ext?.gb_version_capabilities || []" :key="item" class="protocol-tag blue">{{ item }}</span><span v-if="!device.ext?.gb_version_capabilities?.length" class="section-note">暂无能力声明</span></div>
              </article>
              <article class="card detail-operation-card">
                <div class="card-head"><div><h3 class="card-title">能力探测</h3><p class="card-sub">仅在线设备可执行</p></div><Search /></div>
                <div class="operation-row"><span><strong>OPTIONS 探测</strong><small>刷新版本和能力协商</small></span><button class="btn btn-sm" :disabled="!device.is_online || actionLoading === 'OPTIONS 探测'" @click="runAction('OPTIONS 探测', () => api.optionsProbe(device!.id, { timeout: 5 }), true)">重测</button></div>
                <div class="operation-row"><span><strong>PTZ stop 探测</strong><small>核验通道控制能力</small></span><button class="btn btn-sm" :disabled="!device.is_online || actionLoading === 'PTZ 探测'" @click="runAction('PTZ 探测', () => api.devicePtzProbe(device!.id, { action: 'stop', speed: 30, timeout: 5 }), true)">重测</button></div>
                <div class="operation-row"><span><strong>A.4 扩展快照</strong><small>GB/T 28181-2022 扩展对象</small></span><button class="btn btn-sm" :disabled="actionLoading === '查询 A.4 快照'" @click="queryA4">查询</button></div>
              </article>
              <article class="card detail-raw-card">
                <details open><summary><span><Settings2 /><strong>诊断快照</strong></span><small>后端实时诊断结果</small></summary><pre>{{ diagnostics ? JSON.stringify(diagnostics, null, 2) : "当前后端未返回诊断快照。" }}</pre></details>
                <details v-if="a4" open><summary><span><Download /><strong>A.4 查询结果</strong></span><small>最近一次查询</small></summary><pre>{{ JSON.stringify(a4, null, 2) }}</pre></details>
              </article>
            </section>
          </template>
        </div>
      </section>
      <ModalDialog
        :open="Boolean(historyKind)"
        :title="`${device.name || device.device_id || '设备'} · ${historyTitle}`"
        description="按时间倒序展示后端持久化的最近 100 条记录。"
        @close="historyKind = null"
      >
        <div class="history-summary">
          <span>{{ historyKind === "heartbeat" ? "最近心跳" : "最近注册" }}</span>
          <strong>{{ relativeTime(historyKind === "heartbeat" ? device.keepalive_at : device.registered_at) }}</strong>
        </div>
        <div class="table-wrap history-table-wrap">
          <table class="data-table history-table">
            <thead><tr><th>序号</th><th>时间</th><th>间隔（秒）</th><th>来源地址</th><th>状态</th></tr></thead>
            <tbody>
              <tr v-for="(record, index) in historyRows" :key="record.id">
                <td>{{ index + 1 }}</td>
                <td>{{ formatDate(record.recorded_at) }}</td>
                <td>{{ record.interval_seconds || "—" }}</td>
                <td class="mono">{{ record.address || "—" }}</td>
                <td>{{ record.status || "—" }}</td>
              </tr>
            </tbody>
          </table>
          <div v-if="historyLoading" class="empty-state"><LoaderCircle class="mx-auto mb-2 animate-spin" />正在加载历史记录…</div>
          <div v-else-if="historyError && historyRows.length" class="history-inline-warning"><ShieldAlert /><span>{{ historyError }}</span><button class="btn btn-sm" @click="historyKind && openHistory(historyKind)">重试</button></div>
          <div v-else-if="historyError" class="empty-state empty-action"><ShieldAlert /><strong>历史记录加载失败</strong><span>{{ historyError }}</span><button class="btn" @click="historyKind && openHistory(historyKind)">重试</button></div>
          <div v-else-if="!historyRows.length" class="empty-state">当前仅有最近{{ historyKind === "heartbeat" ? "心跳" : "注册" }}时间，尚无已持久化的明细记录。</div>
        </div>
      </ModalDialog>
      <ModalDialog
        :open="deleteOpen"
        title="删除设备及关联通道"
        description="该操作会删除设备记录及其全部关联通道，无法撤销。"
        @close="deleteOpen = false"
      >
        <div class="danger-confirm">
          <span class="danger-confirm-icon"><Trash2 /></span>
          <div>
            <strong>{{ device.name || device.device_id || device.id }}</strong>
            <p>
              将同时删除 {{ relatedChannels.length }} 路关联通道；录像和事件历史不会在此同步删除。
            </p>
          </div>
        </div>
        <template #footer>
          <button class="btn" :disabled="deleting" @click="deleteOpen = false">
            取消
          </button>
          <button class="btn btn-danger" :disabled="deleting" @click="deleteDevice">
            <LoaderCircle v-if="deleting" class="animate-spin" />
            <Trash2 v-else />{{ deleting ? "正在删除…" : "确认删除设备" }}
          </button>
        </template>
      </ModalDialog>
      <ModalDialog
        :open="editOpen"
        title="编辑设备"
        description="保存会更新当前后端设备记录；密码留空时保留现值。"
        @close="editOpen = false"
        ><form class="form-grid" @submit.prevent="saveEdit">
          <label class="form-group"
            ><span class="form-label">设备名称</span
            ><input
              v-model="editForm.name"
              class="input plain w-full"
              required /></label
          ><label class="form-group"
            ><span class="form-label">国标编码</span
            ><input
              v-model="editForm.device_id"
              class="input plain w-full mono" /></label
          ><label class="form-group"
            ><span class="form-label">IP</span
            ><input v-model="editForm.ip" class="input plain w-full" /></label
          ><label class="form-group"
            ><span class="form-label">端口</span
            ><input
              v-model.number="editForm.port"
              class="input plain w-full"
              type="number" /></label
          ><label class="form-group"
            ><span class="form-label">用户名</span
            ><input
              v-model="editForm.username"
              class="input plain w-full" /></label
          ><label class="form-group"
            ><span class="form-label">新密码</span
            ><input
              v-model="editForm.password"
              class="input plain w-full"
              type="password"
              autocomplete="new-password"
              placeholder="留空保留" /></label
          ><label class="form-group"
            ><span class="form-label">流模式</span
            ><span class="select-control">
              <select
                v-model.number="editForm.stream_mode"
                class="select w-full"
              >
                <option :value="0">UDP</option>
                <option :value="1">TCP 被动模式</option>
                <option :value="2">TCP 主动模式</option>
              </select>
              <ChevronDown aria-hidden="true" />
            </span></label
          ><label v-if="isGb" class="form-group"
            ><span class="form-label">GB 版本覆盖</span
            ><span class="select-control">
              <select v-model="editForm.gb_version" class="select w-full">
                <option value="">自动协商</option>
                <option value="1.0">1.0 / 2011</option>
                <option value="1.1">1.1 / 2014</option>
                <option value="2.0">2.0 / 2016</option>
                <option value="3.0">3.0 / 2022</option>
              </select>
              <ChevronDown aria-hidden="true" />
            </span></label
          >
          <div class="modal-foot full">
            <button type="button" class="btn" @click="editOpen = false">
              取消</button
            ><button
              class="btn btn-primary"
              :disabled="actionLoading === '保存设备'"
            >
              <LoaderCircle
                v-if="actionLoading === '保存设备'"
                class="animate-spin"
              />保存设备
            </button>
          </div>
        </form></ModalDialog
      ></template
    >
  </main>
</template>
