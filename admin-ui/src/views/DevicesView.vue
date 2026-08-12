<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from "vue";
import {
  Camera,
  ListFilter,
  LoaderCircle,
  Plus,
  Radar,
  RefreshCcw,
  Search,
  ShieldAlert,
  X,
} from "@lucide/vue";
import { api, apiUrl, errorMessage, typeLabel } from "../services/api";
import type { ApiDevice } from "../types/api";
import { relativeTime } from "../utils/format";
import { useUiStore } from "../stores/ui";
import ModalDialog from "../components/ModalDialog.vue";

const ui = useUiStore();
const query = ref("");
const status = ref("all");
const protocol = ref("all");
const addOpen = ref(false);
const discoveryOpen = ref(false);
const discovering = ref(false);
const discovered = ref<string[]>([]);
const loading = ref(false);
const saving = ref(false);
const loadError = ref("");
const rows = ref<ApiDevice[]>([]);
const total = ref(0);
const form = reactive({
  type: "GB28181",
  name: "",
  device_id: "",
  ip: "",
  port: 80,
  username: "",
  password: "",
});
let discoverySource: EventSource | undefined;

const filtered = computed(() =>
  rows.value.filter((device) => {
    const text = `${device.name || ""}${device.id}${device.device_id || ""}${
      device.address || ""
    }${device.ip || ""}`.toLowerCase();
    const matchQuery = text.includes(query.value.toLowerCase());
    const matchStatus =
      status.value === "all" ||
      (status.value === "online" ? device.is_online : !device.is_online);
    const matchProtocol =
      protocol.value === "all" ||
      typeLabel(device.type, device.device_id || device.id) === protocol.value;
    return matchQuery && matchStatus && matchProtocol;
  })
);
const onlineCount = computed(
  () => rows.value.filter((item) => item.is_online).length
);
const offlineCount = computed(() => rows.value.length - onlineCount.value);
const channelCount = computed(() =>
  rows.value.reduce(
    (sum, item) => sum + Number(item.channels || item.children?.length || 0),
    0
  )
);
const hasFilters = computed(() =>
  Boolean(query.value || status.value !== "all" || protocol.value !== "all")
);

function resetFilters() {
  query.value = "";
  status.value = "all";
  protocol.value = "all";
}

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const { data } = await api.devices({ page: 1, size: 99999 });
    rows.value = data.items || [];
    total.value = data.total || rows.value.length;
  } catch (cause) {
    loadError.value = errorMessage(cause, "设备列表加载失败");
  } finally {
    loading.value = false;
  }
}

async function addDevice() {
  saving.value = true;
  try {
    const body =
      form.type === "GB28181"
        ? {
            type: form.type,
            name: form.name,
            device_id: form.device_id,
            password: form.password,
          }
        : {
            type: form.type,
            name: form.name,
            ip: form.ip,
            port: Number(form.port),
            username: form.username,
            password: form.password,
          };
    await api.addDevice(body);
    addOpen.value = false;
    ui.toast(`${form.name} 已添加`);
    Object.assign(form, {
      type: "GB28181",
      name: "",
      device_id: "",
      ip: "",
      port: 80,
      username: "",
      password: "",
    });
    await load();
  } catch (cause) {
    ui.toast(errorMessage(cause, "添加设备失败"));
  } finally {
    saving.value = false;
  }
}

function discover() {
  discoveryOpen.value = true;
  discovered.value = [];
  discovering.value = true;
  discoverySource?.close();
  discoverySource = new EventSource(apiUrl("/onvif/discover"));
  discoverySource.addEventListener("discover", (event) => {
    try {
      const data = JSON.parse((event as MessageEvent).data) as {
        addr?: string;
      };
      if (data.addr && !discovered.value.includes(data.addr))
        discovered.value.push(data.addr);
    } catch {
      /* 忽略无法解析的厂商发现数据。 */
    }
  });
  discoverySource.addEventListener("end", () => {
    discovering.value = false;
    discoverySource?.close();
  });
  discoverySource.onerror = () => {
    discovering.value = false;
    discoverySource?.close();
  };
}

function useDiscovered(address: string) {
  const normalized = address.replace(/^https?:\/\//, "");
  const [ip, port] = normalized.split(":");
  Object.assign(form, {
    type: "ONVIF",
    name: `ONVIF ${ip}`,
    ip,
    port: Number(port || 80),
    username: "",
    password: "",
  });
  discoveryOpen.value = false;
  addOpen.value = true;
}

onMounted(load);
onBeforeUnmount(() => discoverySource?.close());
</script>

<template>
  <main class="page-content">
    <header class="page-head">
      <div>
        <h1 class="page-title">设备管理</h1>
        <p class="page-desc">
          统一查看 GB28181、ONVIF、RTMP 与 RTSP
          设备的在线状态、通道规模和协议档案。
        </p>
      </div>
      <div class="head-actions">
        <button class="btn" @click="discover"><Radar />ONVIF 发现</button
        ><button class="btn" :disabled="loading" @click="load">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新</button
        ><button class="btn btn-primary" @click="addOpen = true">
          <Plus />添加设备
        </button>
      </div>
    </header>

    <div v-if="loadError" class="warning-box mb-4">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <section class="metric-line mb-4">
      <div class="metric-item">
        <div class="metric-label"><span>全部设备</span><Camera /></div>
        <div class="metric-value">{{ total }}</div>
        <div class="metric-foot">四类协议统一管理</div>
      </div>
      <div class="metric-item">
        <div class="metric-label"><span>在线</span><Camera /></div>
        <div class="metric-value">{{ onlineCount }}</div>
        <div class="metric-foot">
          {{
            rows.length
              ? `${((onlineCount / rows.length) * 100).toFixed(1)}% 在线率`
              : "暂无设备"
          }}
        </div>
      </div>
      <div class="metric-item">
        <div class="metric-label"><span>离线</span><Camera /></div>
        <div class="metric-value">{{ offlineCount }}</div>
        <div class="metric-foot">需要检查注册与心跳</div>
      </div>
      <div class="metric-item">
        <div class="metric-label"><span>通道总数</span><Camera /></div>
        <div class="metric-value">{{ channelCount }}</div>
        <div class="metric-foot">来自当前设备记录</div>
      </div>
    </section>

    <section class="card table-card">
      <div class="toolbar">
        <label class="field"
          ><Search /><input
            v-model="query"
            class="input"
            aria-label="搜索设备"
            placeholder="搜索名称、编码或地址" /></label
        ><select v-model="status" class="select" aria-label="按在线状态筛选">
          <option value="all">全部状态</option>
          <option value="online">在线</option>
          <option value="offline">离线</option></select
        ><select v-model="protocol" class="select" aria-label="按协议筛选">
          <option value="all">全部协议</option>
          <option>GB28181</option>
          <option>ONVIF</option>
          <option>RTSP</option>
          <option>RTMP</option></select
        ><button
          v-if="hasFilters"
          type="button"
          class="btn btn-sm filter-reset"
          @click="resetFilters"
        >
          <X />清除筛选</button
        ><span class="toolbar-spacer" /><span
          class="section-note"
          aria-live="polite"
          >当前显示 {{ filtered.length }} / {{ total }} 台</span
        >
      </div>
      <p class="table-scroll-hint">左右滑动查看完整设备信息</p>
      <div class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th>设备</th>
              <th>协议</th>
              <th>状态</th>
              <th>厂商 / 型号</th>
              <th>地址</th>
              <th>通道</th>
              <th>最近心跳</th>
              <th>协议版本</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="device in filtered" :key="device.id">
              <td>
                <div class="row-title">
                  <span class="device-glyph" :class="{ off: !device.is_online }"
                    ><Camera /></span
                  ><span
                    ><strong>{{ device.name || "未命名设备" }}</strong
                    ><small>{{ device.device_id || device.id }}</small></span
                  >
                </div>
              </td>
              <td>
                <span class="protocol-tag blue">{{
                  typeLabel(device.type, device.device_id || device.id)
                }}</span>
              </td>
              <td>
                <span
                  class="status"
                  :class="device.is_online ? 'online' : 'offline'"
                  >{{ device.is_online ? "在线" : "离线" }}</span
                >
              </td>
              <td>
                {{
                  [device.ext?.manufacturer, device.ext?.model]
                    .filter(Boolean)
                    .join(" / ") ||
                  (["RTMP", "RTSP"].includes(
                    typeLabel(device.type, device.device_id || device.id)
                  )
                    ? "虚拟设备"
                    : "—")
                }}
              </td>
              <td class="mono">
                {{
                  device.address ||
                  [device.ip, device.port].filter(Boolean).join(":") ||
                  "—"
                }}
              </td>
              <td>{{ device.channels || device.children?.length || 0 }} 路</td>
              <td :title="device.keepalive_at">
                {{ relativeTime(device.keepalive_at) }}
              </td>
              <td>
                {{
                  device.ext?.gb_effective_version ||
                  device.ext?.gb_version ||
                  "—"
                }}<small
                  v-if="device.ext?.gb_version_source"
                  class="block text-slate-500"
                  >{{ device.ext.gb_version_source }}</small
                >
              </td>
              <td>
                <div class="row-actions">
                  <RouterLink
                    class="btn btn-sm"
                    :to="`/devices/${encodeURIComponent(device.id)}`"
                    >详情</RouterLink
                  ><RouterLink
                    class="btn btn-sm"
                    :to="`/live?device=${encodeURIComponent(device.id)}`"
                    >预览</RouterLink
                  >
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-if="loading" class="empty-state">
          <LoaderCircle
            class="mx-auto mb-3 h-6 w-6 animate-spin"
          />正在加载设备…
        </div>
        <div v-else-if="!filtered.length" class="empty-state empty-action">
          <ListFilter /><strong>{{
            rows.length ? "没有符合当前条件的设备" : "当前环境尚未接入设备"
          }}</strong
          ><span>{{
            rows.length
              ? "清除筛选后可恢复全部设备。"
              : "可手动添加设备，或先扫描局域网中的 ONVIF 设备。"
          }}</span>
          <div class="button-row">
            <button v-if="rows.length" class="btn" @click="resetFilters">
              清除筛选</button
            ><template v-else
              ><button class="btn" @click="discover"><Radar />ONVIF 发现</button
              ><button class="btn btn-primary" @click="addOpen = true">
                <Plus />添加设备
              </button></template
            >
          </div>
        </div>
      </div>
      <div class="pagination">
        <span>已加载全部 {{ rows.length }} 条设备记录</span
        ><span class="section-note">筛选在当前结果内即时生效</span>
      </div>
    </section>

    <ModalDialog
      :open="addOpen"
      title="添加接入设备"
      description="设备添加会写入当前连接的后端环境。RTMP/RTSP 请从对应流页面创建。"
      @close="addOpen = false"
    >
      <div class="segmented mb-4">
        <button
          v-for="item in ['GB28181', 'ONVIF']"
          :key="item"
          type="button"
          :class="{ active: form.type === item }"
          @click="form.type = item"
        >
          {{ item }}
        </button>
      </div>
      <form class="form-grid" @submit.prevent="addDevice">
        <label class="form-group"
          ><span class="form-label">设备名称</span
          ><input
            v-model="form.name"
            class="input plain w-full"
            placeholder="例如：园区东门 NVR"
            required /></label
        ><label class="form-group"
          ><span class="form-label">接入协议</span
          ><input
            class="input plain w-full"
            :value="form.type"
            disabled /></label
        ><template v-if="form.type === 'GB28181'"
          ><label class="form-group full"
            ><span class="form-label">国标设备 ID</span
            ><input
              v-model="form.device_id"
              class="input plain w-full mono"
              minlength="18"
              maxlength="20"
              placeholder="18–20 位设备编码"
              required
            /><span class="form-help"
              >自动注册设备无需在此重复创建。</span
            ></label
          ><label class="form-group full"
            ><span class="form-label">注册密码</span
            ><input
              v-model="form.password"
              class="input plain w-full"
              type="password"
              autocomplete="new-password" /></label></template
        ><template v-else
          ><label class="form-group"
            ><span class="form-label">设备 IP</span
            ><input
              v-model="form.ip"
              class="input plain w-full"
              placeholder="10.0.0.8"
              required /></label
          ><label class="form-group"
            ><span class="form-label">端口</span
            ><input
              v-model.number="form.port"
              class="input plain w-full"
              type="number"
              min="1"
              max="65535"
              required /></label
          ><label class="form-group"
            ><span class="form-label">用户名</span
            ><input
              v-model="form.username"
              class="input plain w-full"
              autocomplete="off"
              required /></label
          ><label class="form-group"
            ><span class="form-label">密码</span
            ><input
              v-model="form.password"
              class="input plain w-full"
              type="password"
              autocomplete="new-password"
              required /></label
        ></template>
        <div class="modal-foot full">
          <button type="button" class="btn" @click="addOpen = false">
            取消</button
          ><button class="btn btn-primary" :disabled="saving">
            <LoaderCircle v-if="saving" class="animate-spin" />{{
              saving ? "正在保存…" : "保存设备"
            }}
          </button>
        </div>
      </form>
    </ModalDialog>
    <ModalDialog
      :open="discoveryOpen"
      title="ONVIF 局域网发现"
      description="扫描由 Owl 服务端网卡执行，通常在 3 秒无新结果后结束。"
      @close="
        discoveryOpen = false;
        discoverySource?.close();
      "
      ><div class="step-list">
        <div v-for="address in discovered" :key="address" class="step-item">
          <span class="step-index"><Radar /></span
          ><span
            ><strong>发现设备</strong
            ><small class="mono">{{ address }}</small></span
          ><button class="btn btn-sm" @click="useDiscovered(address)">
            填写账号
          </button>
        </div>
        <div v-if="discovering" class="empty-state">
          <LoaderCircle class="mx-auto mb-2 animate-spin" />正在扫描局域网…
        </div>
        <div v-else-if="!discovered.length" class="empty-state">
          未发现新设备，请检查服务端局域网与容器网络模式。
        </div>
      </div>
      <div class="modal-foot">
        <button
          class="btn"
          @click="
            discoveryOpen = false;
            discoverySource?.close();
          "
        >
          关闭</button
        ><button class="btn" @click="discover"><RefreshCcw />重新扫描</button>
      </div></ModalDialog
    >
  </main>
</template>
