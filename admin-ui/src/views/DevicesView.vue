<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import {
  Clock3,
  Eye,
  EyeOff,
  History,
  ListFilter,
  LoaderCircle,
  Plus,
  RadioTower,
  RefreshCcw,
  Search,
  ShieldAlert,
  X,
} from "@lucide/vue";
import { api, errorMessage, typeLabel } from "../services/api";
import type { ApiDevice, DeviceHistoryRecord } from "../types/api";
import { formatDate, relativeTime } from "../utils/format";
import { useUiStore } from "../stores/ui";
import ModalDialog from "../components/ModalDialog.vue";

type StatisticKind = "heartbeat" | "register";

const ui = useUiStore();
const query = ref("");
const status = ref("all");
const addOpen = ref(false);
const accessOpen = ref(false);
const accessLoading = ref(false);
const accessError = ref("");
const revealAccessPassword = ref(false);
const loading = ref(false);
const saving = ref(false);
const loadError = ref("");
const actionLoading = ref("");
const rows = ref<ApiDevice[]>([]);
const statisticDevice = ref<ApiDevice | null>(null);
const statisticKind = ref<StatisticKind>("heartbeat");
const statisticLoading = ref(false);
const statisticError = ref("");
const statisticRows = ref<DeviceHistoryRecord[]>([]);
const form = reactive({ name: "", device_id: "", password: "", stream_mode: 1 });
const accessInfo = reactive({
  serverIp: "",
  id: "",
  domain: "",
  port: 0,
  password: "",
});

const gbRows = computed(() =>
  rows.value.filter(
    (device) =>
      typeLabel(device.type, device.device_id || device.id) === "GB28181"
  )
);
const filtered = computed(() =>
  gbRows.value.filter((device) => {
    const text = `${device.name || ""}${device.id}${device.device_id || ""}${
      device.address || ""
    }${device.ip || ""}`.toLowerCase();
    const matchQuery = text.includes(query.value.toLowerCase());
    const matchStatus =
      status.value === "all" ||
      (status.value === "online" ? device.is_online : !device.is_online);
    return matchQuery && matchStatus;
  })
);
const onlineCount = computed(
  () => gbRows.value.filter((item) => item.is_online).length
);
const offlineCount = computed(() => gbRows.value.length - onlineCount.value);
const channelCount = computed(() =>
  gbRows.value.reduce(
    (sum, item) => sum + Number(item.channels || item.children?.length || 0),
    0
  )
);
const hasFilters = computed(() => Boolean(query.value || status.value !== "all"));
const statisticTitle = computed(() =>
  statisticKind.value === "heartbeat" ? "心跳记录" : "注册记录"
);

function resetFilters() {
  query.value = "";
  status.value = "all";
}

function deviceAddress(device: ApiDevice) {
  const address =
    device.address || [device.ip, device.port].filter(Boolean).join(":");
  if (!address) return "—";
  if (/^[a-z][a-z\d+.-]*:\/\//i.test(address)) return address;
  return `${String(device.transport || "udp").toLowerCase()}://${address}`;
}

function streamMode(device: ApiDevice) {
  return (
    ({ 0: "UDP", 1: "TCP 被动", 2: "TCP 主动" } as Record<number, string>)[
      Number(device.stream_mode)
    ] || device.transport?.toUpperCase() || "—"
  );
}

async function openStatistic(device: ApiDevice, kind: StatisticKind) {
  statisticDevice.value = device;
  statisticKind.value = kind;
  statisticRows.value = [];
  statisticError.value = "";
  statisticLoading.value = true;
  try {
    const { data } = await api.deviceHistory(device.id, kind, { page: 1, size: 100 });
    statisticRows.value = data.items || [];
  } catch (cause) {
    statisticError.value = errorMessage(cause, "历史记录加载失败");
  } finally {
    statisticLoading.value = false;
  }
}

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const { data } = await api.devices({ page: 1, size: 99999 });
    rows.value = data.items || [];
  } catch (cause) {
    loadError.value = errorMessage(cause, "国标设备列表加载失败");
  } finally {
    loading.value = false;
  }
}

async function openAccessInfo() {
  accessOpen.value = true;
  accessLoading.value = true;
  accessError.value = "";
  revealAccessPassword.value = false;
  Object.assign(accessInfo, { serverIp: "", id: "", domain: "", port: 0, password: "" });
  try {
    const { data } = await api.configInfo();
    const info = data.access_info || {};
    Object.assign(accessInfo, {
      serverIp: info.server_ip || "",
      id: info.id || "",
      domain: info.domain || "",
      port: Number(info.port || 0),
      password: info.password || "",
    });
  } catch (cause) {
    accessError.value = errorMessage(cause, "国标接入信息加载失败");
  } finally {
    accessLoading.value = false;
  }
}

async function refreshDevice(device: ApiDevice) {
  actionLoading.value = device.id;
  try {
    await api.catalog(device.id);
    ui.toast(`${device.name || device.device_id || device.id} · 目录刷新指令已发送`);
    await load();
  } catch (cause) {
    ui.toast(errorMessage(cause, "刷新设备失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function addDevice() {
  saving.value = true;
  try {
    await api.addDevice({ type: "GB28181", ...form });
    addOpen.value = false;
    ui.toast(`${form.name} 已添加`);
    Object.assign(form, { name: "", device_id: "", password: "", stream_mode: 1 });
    await load();
  } catch (cause) {
    ui.toast(errorMessage(cause, "添加国标设备失败"));
  } finally {
    saving.value = false;
  }
}

onMounted(load);
</script>

<template>
  <main class="page-content device-page gb-device-page">
    <header class="page-head">
      <div>
        <h1 class="page-title">国标设备</h1>
        <p class="page-desc">
          管理 GB/T 28181 设备的注册状态、信令地址、流传输模式、通道规模与协议版本。
        </p>
      </div>
      <div class="head-actions">
        <button class="btn" @click="openAccessInfo">
          <RadioTower />接入信息
        </button>
        <button class="btn" :disabled="loading" @click="load">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新
        </button>
        <button class="btn btn-primary" @click="addOpen = true">
          <Plus />添加国标设备
        </button>
      </div>
    </header>

    <div v-if="loadError" class="warning-box mb-4" role="alert">
      <ShieldAlert /><span>{{ loadError }}</span>
      <button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>

    <section class="device-summary" aria-label="国标设备统计">
      <div>
        <span>设备</span>
        <strong>{{ gbRows.length }}</strong>
      </div>
      <div>
        <span>在线</span>
        <strong class="text-green-700">{{ onlineCount }}</strong>
      </div>
      <div>
        <span>离线</span>
        <strong class="text-red-700">{{ offlineCount }}</strong>
      </div>
      <div>
        <span>通道</span>
        <strong>{{ channelCount }}</strong>
      </div>
    </section>

    <section class="card table-card">
      <div class="toolbar">
        <label class="field">
          <Search /><input
            v-model="query"
            class="input"
            aria-label="搜索国标设备"
            placeholder="搜索名称、设备编号或地址"
          />
        </label>
        <select v-model="status" class="select" aria-label="按在线状态筛选">
          <option value="all">全部状态</option>
          <option value="online">在线</option>
          <option value="offline">离线</option>
        </select>
        <button
          v-if="hasFilters"
          type="button"
          class="btn btn-sm filter-reset"
          @click="resetFilters"
        >
          <X />清除筛选
        </button>
        <span class="toolbar-spacer" />
        <span class="section-note" aria-live="polite">
          当前显示 {{ filtered.length }} / {{ gbRows.length }} 台
        </span>
      </div>
      <div class="table-wrap">
        <table class="data-table gb-device-data-table">
          <thead>
            <tr>
              <th>设备</th>
              <th>厂商 / 型号</th>
              <th>接入链路</th>
              <th>通道数</th>
              <th>状态</th>
              <th>统计</th>
              <th>协议版本</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="device in filtered" :key="device.id">
              <td data-label="设备">
                <div class="row-title compact">
                  <span>
                    <strong>{{ device.name || "未命名设备" }}</strong>
                    <small class="mono">{{ device.device_id || device.id }}</small>
                  </span>
                </div>
              </td>
              <td data-label="厂商 / 型号">
                <span class="stacked-value">
                  <strong>{{ device.ext?.manufacturer || "—" }}</strong>
                  <small>{{ device.ext?.model || "—" }}</small>
                </span>
              </td>
              <td data-label="接入链路">
                <span class="stacked-value">
                  <strong class="mono">{{ deviceAddress(device) }}</strong>
                  <small>{{ streamMode(device) }}</small>
                </span>
              </td>
              <td data-label="通道数">{{ device.channels || device.children?.length || 0 }} 路</td>
              <td data-label="状态">
                <span class="status" :class="device.is_online ? 'online' : 'offline'">
                  {{ device.is_online ? "在线" : "离线" }}
                </span>
              </td>
              <td data-label="统计">
                <div class="stat-actions">
                  <button
                    type="button"
                    :aria-label="`${device.name || device.device_id || '设备'}心跳记录`"
                    @click="openStatistic(device, 'heartbeat')"
                  >
                    <Clock3 /><span class="stat-name">心跳</span><time>{{ relativeTime(device.keepalive_at) }}</time>
                  </button>
                  <button
                    type="button"
                    :aria-label="`${device.name || device.device_id || '设备'}注册记录`"
                    @click="openStatistic(device, 'register')"
                  >
                    <History /><span class="stat-name">注册</span><time>{{ relativeTime(device.registered_at) }}</time>
                  </button>
                </div>
              </td>
              <td data-label="协议版本">
                <strong>{{ device.ext?.gb_effective_version || device.ext?.gb_version || "—" }}</strong>
                <small v-if="device.ext?.gb_version_source" class="block text-slate-500">
                  {{ device.ext.gb_version_source }}
                </small>
              </td>
              <td data-label="操作">
                <div class="row-actions">
                  <button
                    type="button"
                    class="btn btn-sm"
                    :disabled="actionLoading === device.id"
                    @click="refreshDevice(device)"
                  >
                    <LoaderCircle v-if="actionLoading === device.id" class="animate-spin" />
                    <RefreshCcw v-else />刷新
                  </button>
                  <RouterLink
                    class="btn btn-sm"
                    :to="`/devices/${encodeURIComponent(device.id)}`"
                  >详情</RouterLink>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-if="loading" class="empty-state">
          <LoaderCircle class="mx-auto mb-3 h-6 w-6 animate-spin" />正在加载国标设备…
        </div>
        <div v-else-if="!filtered.length" class="empty-state empty-action">
          <ListFilter />
          <strong>{{ gbRows.length ? "没有符合当前条件的国标设备" : "当前环境尚未接入国标设备" }}</strong>
          <span>{{ gbRows.length ? "清除筛选后可恢复全部设备。" : "设备注册成功后会自动出现在此列表。" }}</span>
          <div class="button-row">
            <button v-if="gbRows.length" class="btn" @click="resetFilters">清除筛选</button>
            <button v-else class="btn btn-primary" @click="addOpen = true"><Plus />添加国标设备</button>
          </div>
        </div>
      </div>
      <div class="pagination">
        <span>已加载 {{ gbRows.length }} 条国标设备记录</span>
        <span class="section-note">筛选在当前结果内即时生效</span>
      </div>
    </section>

    <ModalDialog
      :open="accessOpen"
      title="国标接入信息"
      description="下级设备注册到本平台时使用以下参数。"
      @close="accessOpen = false"
    >
      <div v-if="accessError" class="access-info-warning" role="alert">
        <ShieldAlert /><span>{{ accessError }}</span>
        <button type="button" class="btn btn-sm" @click="openAccessInfo">重试</button>
      </div>
      <div v-if="accessLoading" class="access-info-loading" aria-live="polite">
        <LoaderCircle class="animate-spin" />正在获取接入信息…
      </div>
      <div v-else-if="!accessError" class="access-info-section" aria-live="polite">
        <dl class="access-info-grid">
          <div><dt>服务器 IP</dt><dd class="mono">{{ accessInfo.serverIp || "—" }}</dd></div>
          <div><dt>端口（UDP / TCP）</dt><dd class="mono">{{ accessInfo.port || "—" }}</dd></div>
          <div class="wide"><dt>国标 ID</dt><dd class="mono">{{ accessInfo.id || "—" }}</dd></div>
          <div><dt>国标域</dt><dd class="mono">{{ accessInfo.domain || "—" }}</dd></div>
          <div>
            <dt>密码</dt>
            <dd class="access-secret">
              <span class="mono">{{ revealAccessPassword ? accessInfo.password || "—" : accessInfo.password ? "••••••" : "—" }}</span>
              <button
                v-if="accessInfo.password"
                type="button"
                class="access-secret-toggle"
                :aria-label="revealAccessPassword ? '隐藏国标接入密码' : '显示国标接入密码'"
                :title="revealAccessPassword ? '隐藏密码' : '显示密码'"
                @click="revealAccessPassword = !revealAccessPassword"
              >
                <EyeOff v-if="revealAccessPassword" /><Eye v-else />
              </button>
            </dd>
          </div>
        </dl>
      </div>
      <template #footer>
        <button type="button" class="btn btn-primary" @click="accessOpen = false">完成</button>
      </template>
    </ModalDialog>

    <ModalDialog
      :open="Boolean(statisticDevice)"
      :title="`${statisticDevice?.name || statisticDevice?.device_id || '设备'} · ${statisticTitle}`"
      description="按时间倒序展示后端持久化的最近 100 条记录。"
      @close="statisticDevice = null"
    >
      <div class="history-summary">
        <span>{{ statisticKind === "heartbeat" ? "最近心跳" : "最近注册" }}</span>
        <strong>{{ relativeTime(statisticKind === "heartbeat" ? statisticDevice?.keepalive_at : statisticDevice?.registered_at) }}</strong>
      </div>
      <div class="table-wrap history-table-wrap">
        <table class="data-table history-table">
          <thead><tr><th>序号</th><th>时间</th><th>间隔（秒）</th><th>来源地址</th><th>状态</th></tr></thead>
          <tbody>
            <tr v-for="(record, index) in statisticRows" :key="record.id">
              <td>{{ index + 1 }}</td>
              <td>{{ formatDate(record.recorded_at) }}</td>
              <td>{{ record.interval_seconds || "—" }}</td>
              <td class="mono">{{ record.address || "—" }}</td>
              <td>{{ record.status || "—" }}</td>
            </tr>
          </tbody>
        </table>
        <div v-if="statisticLoading" class="empty-state"><LoaderCircle class="mx-auto mb-2 animate-spin" />正在加载历史记录…</div>
        <div v-else-if="statisticError" class="empty-state empty-action"><ShieldAlert /><strong>历史记录加载失败</strong><span>{{ statisticError }}</span><button class="btn" @click="statisticDevice && openStatistic(statisticDevice, statisticKind)">重试</button></div>
        <div v-else-if="!statisticRows.length" class="empty-state">暂无已持久化的{{ statisticKind === "heartbeat" ? "心跳" : "注册" }}记录，新事件到达后会显示在这里。</div>
      </div>
    </ModalDialog>

    <ModalDialog
      :open="addOpen"
      title="添加国标设备"
      description="自动注册设备无需重复创建；此操作会写入当前连接的后端环境。"
      @close="addOpen = false"
    >
      <form class="form-grid" @submit.prevent="addDevice">
        <label class="form-group full">
          <span class="form-label">设备名称</span>
          <input v-model="form.name" class="input plain w-full" placeholder="例如：园区东门 NVR" required />
        </label>
        <label class="form-group full">
          <span class="form-label">国标设备编号</span>
          <input v-model="form.device_id" class="input plain w-full mono" minlength="18" maxlength="20" placeholder="18–20 位设备编码" required />
        </label>
        <label class="form-group full">
          <span class="form-label">注册密码</span>
          <input v-model="form.password" class="input plain w-full" type="password" autocomplete="new-password" />
        </label>
        <label class="form-group full">
          <span class="form-label">收流模式</span>
          <select
            v-model.number="form.stream_mode"
            class="select w-full"
            aria-describedby="stream-mode-help"
          >
            <option :value="0">UDP</option>
            <option :value="2">TCP 主动模式</option>
            <option :value="1">TCP 被动模式</option>
          </select>
          <span id="stream-mode-help" class="form-help">决定设备实时点播时 RTP 媒体流的传输方式。</span>
        </label>
        <div class="modal-foot full">
          <button type="button" class="btn" @click="addOpen = false">取消</button>
          <button class="btn btn-primary" :disabled="saving">
            <LoaderCircle v-if="saving" class="animate-spin" />{{ saving ? "正在保存…" : "保存设备" }}
          </button>
        </div>
      </form>
    </ModalDialog>
  </main>
</template>
