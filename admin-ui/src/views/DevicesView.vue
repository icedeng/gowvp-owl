<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from "vue";
import {
  Clock3,
  ChevronLeft,
  ChevronRight,
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
import {
  api,
  collectPages,
  countItems,
  errorMessage,
  typeLabel,
} from "../services/api";
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
const legacyRows = ref<ApiDevice[]>([]);
const currentPage = ref(1);
const total = ref(0);
const deviceTotal = ref(0);
const onlineTotal = ref(0);
const offlineTotal = ref(0);
const channelTotal = ref(0);
const supportsServerFilters = ref<boolean | null>(null);
const statisticDevice = ref<ApiDevice | null>(null);
const statisticKind = ref<StatisticKind>("heartbeat");
const statisticLoading = ref(false);
const statisticError = ref("");
const statisticRows = ref<DeviceHistoryRecord[]>([]);
const PAGE_SIZE = 20;
let loadSequence = 0;
let searchTimer: number | undefined;
const form = reactive({ name: "", device_id: "", password: "", stream_mode: 1 });
const accessInfo = reactive({
  serverIp: "",
  id: "",
  domain: "",
  port: 0,
  password: "",
});

const filtered = computed(() => rows.value);
const pageCount = computed(() =>
  Math.max(1, Math.ceil(total.value / PAGE_SIZE))
);
const pagedRows = computed(() => rows.value);
const hasFilters = computed(() => Boolean(query.value || status.value !== "all"));
const statisticTitle = computed(() =>
  statisticKind.value === "heartbeat" ? "心跳记录" : "注册记录"
);

function resetFilters() {
  query.value = "";
  status.value = "all";
  currentPage.value = 1;
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
    statisticRows.value = data?.items || [];
  } catch (cause) {
    statisticError.value = errorMessage(cause, "历史记录加载失败");
  } finally {
    statisticLoading.value = false;
  }
}

function isGbDevice(device: ApiDevice) {
  return typeLabel(device.type, device.device_id || device.id) === "GB28181";
}

function matchesFilters(device: ApiDevice) {
  const text = `${device.name || ""}${device.id}${device.device_id || ""}${
    device.address || ""
  }${device.ip || ""}`.toLowerCase();
  const matchStatus =
    status.value === "all" ||
    (status.value === "online"
      ? device.is_online === true
      : device.is_online !== true);
  return text.includes(query.value.trim().toLowerCase()) && matchStatus;
}

async function probeServerFilters() {
  const [allResponse, onlineResponse, offlineResponse, channels] =
    await Promise.all([
      api.devices({ page: 1, size: 1, type: "GB28181" }),
      api.devices({ page: 1, size: 1, type: "GB28181", is_online: true }),
      api.devices({ page: 1, size: 1, type: "GB28181", is_online: false }),
      countItems(api.channels, { type: "GB28181" }),
    ]);
  const allItems = allResponse.data?.items || [];
  const onlineItems = onlineResponse.data?.items || [];
  const offlineItems = offlineResponse.data?.items || [];
  const all = Number(allResponse.data?.total ?? allItems.length);
  const online = Number(onlineResponse.data?.total ?? onlineItems.length);
  const offline = Number(offlineResponse.data?.total ?? offlineItems.length);
  const supported =
    online + offline === all &&
    allItems.every(isGbDevice) &&
    onlineItems.every((item) => isGbDevice(item) && item.is_online === true) &&
    offlineItems.every(
      (item) => isGbDevice(item) && item.is_online === false
    );
  return { supported, all, online, offline, channels };
}

async function load(refreshSummary = false) {
  const sequence = ++loadSequence;
  loading.value = true;
  loadError.value = "";
  try {
    if (supportsServerFilters.value === null || refreshSummary) {
      const summary = await probeServerFilters();
      if (sequence !== loadSequence) return;
      supportsServerFilters.value = summary.supported;
      channelTotal.value = summary.channels;
      if (summary.supported) {
        deviceTotal.value = summary.all;
        onlineTotal.value = summary.online;
        offlineTotal.value = summary.offline;
        legacyRows.value = [];
      }
    }

    if (supportsServerFilters.value) {
      const response = await api.devices({
        page: currentPage.value,
        size: PAGE_SIZE,
        type: "GB28181",
        key: query.value.trim() || undefined,
        is_online:
          status.value === "all" ? undefined : status.value === "online",
      });
      if (sequence !== loadSequence) return;
      rows.value = (response.data?.items || []).filter(isGbDevice);
      total.value = Number(response.data?.total ?? rows.value.length);
    } else {
      if (!legacyRows.value.length || refreshSummary) {
        const data = await collectPages(api.devices);
        if (sequence !== loadSequence) return;
        legacyRows.value = data.items.filter(isGbDevice);
        deviceTotal.value = legacyRows.value.length;
        onlineTotal.value = legacyRows.value.filter(
          (item) => item.is_online === true
        ).length;
        offlineTotal.value = deviceTotal.value - onlineTotal.value;
      }
      const matched = legacyRows.value.filter(matchesFilters);
      total.value = matched.length;
      rows.value = matched.slice(
        (currentPage.value - 1) * PAGE_SIZE,
        currentPage.value * PAGE_SIZE
      );
    }
  } catch (cause) {
    if (sequence === loadSequence)
      loadError.value = errorMessage(cause, "国标设备列表加载失败");
  } finally {
    if (sequence === loadSequence) loading.value = false;
  }
}

function changePage(next: number) {
  const target = Math.min(pageCount.value, Math.max(1, next));
  if (target === currentPage.value) return;
  currentPage.value = target;
  void load();
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
    await load(true);
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
    await load(true);
  } catch (cause) {
    ui.toast(errorMessage(cause, "添加国标设备失败"));
  } finally {
    saving.value = false;
  }
}

onMounted(() => load(true));
onBeforeUnmount(() => window.clearTimeout(searchTimer));
watch([query, status], () => {
  currentPage.value = 1;
  window.clearTimeout(searchTimer);
  searchTimer = window.setTimeout(() => void load(), 280);
});
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
        <button class="btn" :disabled="loading" @click="load(true)">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新
        </button>
        <button class="btn btn-primary" @click="addOpen = true">
          <Plus />添加国标设备
        </button>
      </div>
    </header>

    <div v-if="loadError" class="warning-box mb-4" role="alert">
      <ShieldAlert /><span>{{ loadError }}</span>
      <button class="btn btn-sm ml-auto" @click="load(true)">重试</button>
    </div>

    <section class="device-summary" aria-label="国标设备统计">
      <div>
        <span>设备</span>
        <strong>{{ deviceTotal }}</strong>
      </div>
      <div>
        <span>在线</span>
        <strong class="text-green-700">{{ onlineTotal }}</strong>
      </div>
      <div>
        <span>离线</span>
        <strong class="text-red-700">{{ offlineTotal }}</strong>
      </div>
      <div>
        <span>通道</span>
        <strong>{{ channelTotal }}</strong>
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
          本页 {{ pagedRows.length }} 台 · 匹配 {{ total }} 台
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
            <tr v-for="device in pagedRows" :key="device.id">
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
                <div class="device-row-actions">
                  <button
                    type="button"
                    class="device-row-action"
                    :disabled="actionLoading === device.id"
                    :aria-label="`刷新${device.name || device.device_id || '设备'}目录`"
                    title="刷新目录"
                    @click="refreshDevice(device)"
                  >
                    <LoaderCircle v-if="actionLoading === device.id" class="animate-spin" />
                    <RefreshCcw v-else />
                  </button>
                  <RouterLink
                    class="device-row-detail"
                    :to="`/devices/${encodeURIComponent(device.id)}`"
                  >详情<ChevronRight /></RouterLink>
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
          <strong>{{ deviceTotal ? "没有符合当前条件的国标设备" : "当前环境尚未接入国标设备" }}</strong>
          <span>{{ deviceTotal ? "清除筛选后可恢复全部设备。" : "设备注册成功后会自动出现在此列表。" }}</span>
          <div class="button-row">
            <button v-if="deviceTotal" class="btn" @click="resetFilters">清除筛选</button>
            <button v-else class="btn btn-primary" @click="addOpen = true"><Plus />添加国标设备</button>
          </div>
        </div>
      </div>
      <div class="pagination">
        <span>共 {{ total }} 条匹配记录</span>
        <div v-if="pageCount > 1" class="pagination-actions" aria-label="设备列表分页">
          <button
            type="button"
            class="page-btn"
            :disabled="currentPage === 1"
            aria-label="上一页"
            @click="changePage(currentPage - 1)"
          ><ChevronLeft /></button>
          <span>第 {{ currentPage }} / {{ pageCount }} 页</span>
          <button
            type="button"
            class="page-btn"
            :disabled="currentPage === pageCount"
            aria-label="下一页"
            @click="changePage(currentPage + 1)"
          ><ChevronRight /></button>
        </div>
        <span v-else class="section-note">共 1 页</span>
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
