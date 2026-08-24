<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from "vue";
import { useRoute } from "vue-router";
import {
  ChevronLeft,
  ChevronRight,
  Eye,
  LoaderCircle,
  Plus,
  RadioTower,
  RefreshCcw,
  Search,
  ShieldAlert,
  Trash2,
  X,
} from "@lucide/vue";
import { api, countDevicesByType, errorMessage } from "../services/api";
import type { ApiChannel, MediaServer } from "../types/api";
import { useUiStore } from "../stores/ui";
import ModalDialog from "../components/ModalDialog.vue";
import RemoteChannelSelect from "../components/RemoteChannelSelect.vue";

const ui = useUiStore();
const route = useRoute();
const open = ref(false);
const editing = ref<ApiChannel | null>(null);
const deleteTarget = ref<ApiChannel | null>(null);
const loading = ref(false);
const saving = ref(false);
const deleting = ref(false);
const loadError = ref("");
const query = ref("");
const status = ref("all");
const rows = ref<ApiChannel[]>([]);
const currentPage = ref(1);
const total = ref(0);
const deviceTotal = ref(0);
const media = ref<MediaServer[]>([]);
const form = reactive({
  name: "",
  source_url: "",
  transport: 0,
  app: "live",
  stream: "",
  device_mode: "new",
  device_id: "",
  device_name: "RTSP 拉流设备",
  media_server_id: "local",
  timeout_s: 10,
  enabled_audio: true,
  enabled: true,
});
let routeEditHandled = false;
let searchTimer: number | undefined;
let loadSequence = 0;
const PAGE_SIZE = 30;
const pageCount = computed(() =>
  Math.max(1, Math.ceil(total.value / PAGE_SIZE))
);
const pagedRows = computed(() => rows.value);
const hasFilters = computed(
  () => Boolean(query.value || status.value !== "all")
);

function resetFilters() {
  query.value = "";
  status.value = "all";
  currentPage.value = 1;
}

function maskSource(value?: string) {
  if (!value) return "—";
  return value
    .replace(/\/\/([^/@]+)@/, "//***@")
    .replace(/(rtsp:\/\/[^/]+\/).+/, "$1***");
}

function listParams() {
  return {
    type: "RTSP",
    key: query.value.trim() || undefined,
    is_online:
      status.value === "all" ? undefined : status.value === "online",
  };
}

async function loadList() {
  const sequence = ++loadSequence;
  loading.value = true;
  loadError.value = "";
  try {
    const response = await api.channels({
      page: currentPage.value,
      size: PAGE_SIZE,
      ...listParams(),
    });
    if (sequence !== loadSequence) return;
    rows.value = response.data?.items || [];
    total.value = Number(response.data?.total ?? rows.value.length);
  } catch (cause) {
    if (sequence === loadSequence)
      loadError.value = errorMessage(cause, "RTSP 拉流列表加载失败");
  } finally {
    if (sequence === loadSequence) loading.value = false;
  }
}

async function load() {
  const sequence = ++loadSequence;
  loading.value = true;
  loadError.value = "";
  try {
    const [channelResponse, deviceResponse, mediaResponse] = await Promise.allSettled([
      api.channels({ page: currentPage.value, size: PAGE_SIZE, ...listParams() }),
      countDevicesByType("RTSP"),
      api.mediaServers({ page: 1, size: 100 }),
    ]);
    if (sequence !== loadSequence) return;
    if (channelResponse.status === "rejected") throw channelResponse.reason;
    rows.value = channelResponse.value.data?.items || [];
    total.value = Number(channelResponse.value.data?.total ?? rows.value.length);
    if (deviceResponse.status === "fulfilled") deviceTotal.value = deviceResponse.value;
    if (mediaResponse.status === "fulfilled") media.value = mediaResponse.value.data?.items || [];
    const auxiliaryFailure = [deviceResponse, mediaResponse].find((item) => item.status === "rejected");
    if (auxiliaryFailure?.status === "rejected") {
      loadError.value = `拉流列表已加载，部分表单选项暂不可用：${errorMessage(auxiliaryFailure.reason)}`;
    }
    if (!routeEditHandled) {
      const target = String(route.query.channel || route.query.stream || "");
      let matched = rows.value.find((item) => item.id === target || item.stream === target);
      if (target && !matched) {
        try {
          const response = await api.channels({ page: 1, size: 20, type: "RTSP", key: target });
          matched = (response.data?.items || []).find((item) => item.id === target || item.stream === target);
        } catch {
          // 路由目标可能已删除，保留当前列表。
        }
      }
      if (matched) edit(matched);
      routeEditHandled = true;
    }
  } catch (cause) {
    if (sequence === loadSequence)
      loadError.value = errorMessage(cause, "RTSP 拉流列表加载失败");
  } finally {
    if (sequence === loadSequence) loading.value = false;
  }
}

function changePage(next: number) {
  const target = Math.min(pageCount.value, Math.max(1, next));
  if (target === currentPage.value) return;
  currentPage.value = target;
  void loadList();
}

function create() {
  editing.value = null;
  Object.assign(form, {
    name: "",
    source_url: "",
    transport: 0,
    app: "live",
    stream: "",
    device_mode: deviceTotal.value ? "existing" : "new",
    device_id: "",
    device_name: "RTSP 拉流设备",
    media_server_id: media.value[0]?.id || "local",
    timeout_s: 10,
    enabled_audio: true,
    enabled: true,
  });
  open.value = true;
}

function edit(row: ApiChannel) {
  editing.value = row;
  Object.assign(form, {
    name: row.name || "",
    source_url: "",
    transport: row.config?.transport || 0,
    app: row.app || "live",
    stream: row.stream || "",
    device_mode: "existing",
    device_id: row.did || "",
    device_name: "",
    media_server_id: row.config?.media_server_id || "local",
    timeout_s: row.config?.timeout_s || 10,
    enabled_audio: row.config?.enabled_audio ?? true,
    enabled: row.config?.enabled ?? true,
  });
  open.value = true;
}

async function save() {
  if (form.source_url && !/^rtsp:\/\//i.test(form.source_url.trim())) {
    ui.toast("RTSP 源地址必须以 rtsp:// 开头");
    return;
  }
  if (!editing.value && form.device_mode === "existing" && !form.device_id) {
    ui.toast("请选择已有 RTSP 设备");
    return;
  }
  saving.value = true;
  try {
    const config = {
      ...(editing.value?.config || {}),
      media_server_id: form.media_server_id,
      source_url: form.source_url || editing.value?.config?.source_url || "",
      transport: Number(form.transport),
      timeout_s: Number(form.timeout_s),
      enabled_audio: form.enabled_audio,
      enabled: form.enabled,
    };
    if (editing.value)
      await api.editChannel(editing.value.id, {
        device_id: editing.value.device_id || "",
        name: form.name,
        ptztype: editing.value.ptztype || 0,
        is_online: Boolean(editing.value.is_online),
        ext: editing.value.ext || {},
        app: form.app,
        stream: form.stream,
        config,
      });
    else
      await api.addChannel({
        type: "RTSP",
        name: form.name,
        device_id: form.device_mode === "existing" ? form.device_id : "",
        device_name: form.device_mode === "new" ? form.device_name : "",
        app: form.app,
        stream: form.stream,
        config,
      });
    open.value = false;
    ui.toast(editing.value ? "拉流配置已更新" : "拉流配置已创建");
    await load();
  } catch (cause) {
    ui.toast(errorMessage(cause, "拉流配置保存失败"));
  } finally {
    saving.value = false;
  }
}

async function remove() {
  if (!deleteTarget.value) return;
  deleting.value = true;
  const name = deleteTarget.value.name || deleteTarget.value.stream || deleteTarget.value.id;
  try {
    await api.deleteChannel(deleteTarget.value.id);
    deleteTarget.value = null;
    ui.toast(`拉流配置 ${name} 已删除`);
    await load();
  } catch (cause) {
    ui.toast(errorMessage(cause, "拉流配置删除失败"));
  } finally {
    deleting.value = false;
  }
}

onMounted(load);
watch([query, status], () => {
  currentPage.value = 1;
  window.clearTimeout(searchTimer);
  searchTimer = window.setTimeout(() => void loadList(), 350);
});
watch(pageCount, (count) => {
  if (currentPage.value > count) {
    currentPage.value = count;
    void loadList();
  }
});
onBeforeUnmount(() => window.clearTimeout(searchTimer));
</script>

<template>
  <main class="page-content">
    <header class="page-head">
      <div>
        <h1 class="page-title">RTSP 拉流</h1>
        <p class="page-desc">
          管理源地址、传输方式与按需代理；源地址在列表中始终脱敏。
        </p>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新</button
        ><button class="btn btn-primary" @click="create">
          <Plus />新增拉流
        </button>
      </div>
    </header>
    <div v-if="loadError" class="warning-box mb-4" role="alert">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <div class="warning-box mb-4">
      <ShieldAlert /><span
        >超时、音频、启用状态和无人观看策略已进入数据模型，但当前运行适配器尚未完整消费；当前有效配置以源
        URL 与 TCP/UDP 为主。</span
      >
    </div>
    <section class="card table-card">
      <div class="toolbar">
        <label class="field"
          ><Search /><input
            v-model="query"
            class="input"
            aria-label="搜索 RTSP 拉流"
            placeholder="搜索名称、App 或 Stream" /></label
        ><select v-model="status" class="select" aria-label="按在线状态筛选">
          <option value="all">全部状态</option>
          <option value="online">在线</option>
          <option value="offline">离线</option></select
        ><button
          v-if="hasFilters"
          type="button"
          class="btn btn-sm filter-reset"
          @click="resetFilters"
        >
          <X />清除筛选</button
        ><span class="toolbar-spacer" /><span class="section-note"
          >本页 {{ rows.length }} / 共 {{ total }} 个拉流通道</span
        >
      </div>
      <div class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th>拉流名称</th>
              <th>源地址摘要</th>
              <th>App / Stream</th>
              <th>传输方式</th>
              <th>在线</th>
              <th>播放</th>
              <th>媒体节点</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in pagedRows" :key="row.id">
              <td>
                <div class="row-title">
                  <span class="device-glyph"><RadioTower /></span
                  ><span
                    ><strong>{{ row.name || "未命名拉流" }}</strong
                    ><small>{{ row.id }}</small></span
                  >
                </div>
              </td>
              <td class="mono">{{ maskSource(row.config?.source_url) }}</td>
              <td class="mono">
                {{ row.app || "—" }} / {{ row.stream || "—" }}
              </td>
              <td>{{ row.config?.transport === 1 ? "UDP" : "TCP" }}</td>
              <td>
                <span
                  class="status"
                  :class="row.is_online ? 'online' : 'offline'"
                  >{{ row.is_online ? "在线" : "离线" }}</span
                >
              </td>
              <td>
                <span class="status" :class="row.is_playing ? 'info' : ''">{{
                  row.is_playing ? "LIVE" : "空闲"
                }}</span>
              </td>
              <td>{{ row.config?.media_server_id || "local" }}</td>
              <td>
                <div class="row-actions">
                  <RouterLink
                    class="btn btn-sm"
                    :to="`/live?stream=${encodeURIComponent(
                      row.stream || row.id
                    )}`"
                    ><Eye />预览</RouterLink
                  ><button class="btn btn-sm" @click="edit(row)">编辑</button
                  ><button
                    class="more-btn danger"
                    aria-label="删除拉流配置"
                    @click="deleteTarget = row"
                  >
                    <Trash2 />
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-if="loading" class="empty-state">
          <LoaderCircle class="mx-auto mb-2 animate-spin" />正在加载拉流配置…
        </div>
        <div v-else-if="!rows.length" class="empty-state empty-action">
          <RadioTower />
          <strong>{{
            hasFilters
              ? "没有符合条件的拉流"
              : "当前环境尚无 RTSP 拉流配置"
          }}</strong>
          <span>{{
            hasFilters
              ? "清除筛选后可恢复全部拉流配置。"
              : "创建拉流配置后，媒体节点会按需代理 RTSP 源。"
          }}</span>
          <button v-if="hasFilters" class="btn" @click="resetFilters">
            清除筛选
          </button>
          <button v-else class="btn btn-primary" @click="create">
            <Plus />新增拉流
          </button>
        </div>
      </div>
      <div class="pagination">
        <span>本页 {{ pagedRows.length }} 条 · 共 {{ total }} 条匹配配置</span>
        <div v-if="pageCount > 1" class="pagination-actions" aria-label="拉流列表分页">
          <button type="button" class="page-btn" :disabled="currentPage === 1" aria-label="上一页" @click="changePage(currentPage - 1)"><ChevronLeft /></button>
          <span>第 {{ currentPage }} / {{ pageCount }} 页</span>
          <button type="button" class="page-btn" :disabled="currentPage === pageCount" aria-label="下一页" @click="changePage(currentPage + 1)"><ChevronRight /></button>
        </div>
      </div>
    </section>
    <ModalDialog
      :open="open"
      :title="editing ? '编辑 RTSP 拉流' : '新增 RTSP 拉流'"
      description="编辑时源 URL 留空将保留现值，管理端不会回显其中的账号密码。"
      @close="open = false"
      ><form class="form-grid" @submit.prevent="save">
        <label class="form-group full"
          ><span class="form-label">拉流名称</span
          ><input
            v-model="form.name"
            class="input plain w-full"
            required /></label
        ><label class="form-group full"
          ><span class="form-label">{{
            editing ? "新 RTSP 源 URL" : "RTSP 源 URL"
          }}</span
          ><input
            v-model="form.source_url"
            class="input plain w-full mono"
            :required="!editing"
            :placeholder="
              editing ? '留空保留现值' : 'rtsp://user:password@host/path'
            "
          /><span class="form-help"
            >源地址不会以明文显示在列表或日志中。</span
          ></label
        ><label class="form-group"
          ><span class="form-label">传输方式</span
          ><select v-model.number="form.transport" class="select w-full">
            <option :value="0">TCP</option>
            <option :value="1">UDP</option>
          </select></label
        ><template v-if="!editing"
          ><label class="form-group"
            ><span class="form-label">虚拟设备</span
            ><select v-model="form.device_mode" class="select w-full">
              <option value="existing" :disabled="!deviceTotal">
                使用已有 RTSP 设备
              </option>
              <option value="new">新建 RTSP 虚拟设备</option>
            </select></label
          ><div v-if="form.device_mode === 'existing'" class="form-group"
            ><span class="form-label">选择设备</span
            ><RemoteChannelSelect
              v-model="form.device_id"
              resource="device"
              type="RTSP"
              aria-label="选择已有 RTSP 设备"
              :all-label="''"
              placeholder-label="请选择 RTSP 设备"
            /></div
          ><label v-else class="form-group"
            ><span class="form-label">新设备名称</span
            ><input
              v-model="form.device_name"
              class="input plain w-full"
              required /></label></template
        ><label class="form-group"
          ><span class="form-label">App</span
          ><input
            v-model="form.app"
            class="input plain w-full mono"
            required /></label
        ><label class="form-group"
          ><span class="form-label">Stream</span
          ><input
            v-model="form.stream"
            class="input plain w-full mono"
            required /></label
        ><label class="form-group"
          ><span class="form-label">媒体节点</span
          ><select v-model="form.media_server_id" class="select w-full">
            <option v-for="item in media" :key="item.id" :value="item.id">
              {{ item.id }}
            </option>
            <option v-if="!media.length" value="local">local</option>
          </select></label
        >
        <div class="read-only full">
          高级参数可保存到模型，但在运行适配器完整消费前不作为生效承诺。
        </div>
        <div class="modal-foot full">
          <button type="button" class="btn" @click="open = false">取消</button
          ><button class="btn btn-primary" :disabled="saving">
            <LoaderCircle v-if="saving" class="animate-spin" />{{
              saving ? "正在保存…" : "保存配置"
            }}
          </button>
        </div>
      </form></ModalDialog
    >
    <ModalDialog
      :open="Boolean(deleteTarget)"
      title="删除 RTSP 拉流配置"
      description="该操作会删除统一通道记录，并停止后续按需拉流。"
      @close="deleteTarget = null"
    >
      <div class="danger-confirm">
        <span class="danger-confirm-icon"><Trash2 /></span>
        <div>
          <strong>{{ deleteTarget?.name || deleteTarget?.id }}</strong>
          <p class="mono">
            {{ deleteTarget?.app || "—" }} / {{ deleteTarget?.stream || "—" }}
          </p>
        </div>
      </div>
      <template #footer>
        <button class="btn" :disabled="deleting" @click="deleteTarget = null">
          取消
        </button>
        <button class="btn btn-danger" :disabled="deleting" @click="remove">
          <LoaderCircle v-if="deleting" class="animate-spin" />
          <Trash2 v-else />{{ deleting ? "正在删除…" : "确认删除" }}
        </button>
      </template>
    </ModalDialog>
  </main>
</template>
