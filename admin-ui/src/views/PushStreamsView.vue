<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useRoute } from "vue-router";
import {
  Copy,
  KeyRound,
  LoaderCircle,
  Plus,
  RefreshCcw,
  Search,
  ShieldAlert,
  Trash2,
  UploadCloud,
  X,
} from "@lucide/vue";
import { api, errorMessage, typeLabel } from "../services/api";
import type { ApiChannel, ApiDevice, MediaServer } from "../types/api";
import { relativeTime } from "../utils/format";
import { useUiStore } from "../stores/ui";
import ModalDialog from "../components/ModalDialog.vue";

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
const devices = ref<ApiDevice[]>([]);
const media = ref<MediaServer[]>([]);
const form = reactive({
  name: "",
  app: "live",
  stream: "",
  device_mode: "new",
  device_id: "",
  device_name: "RTMP 推流设备",
  media_server_id: "local",
  auth_enabled: true,
});
let routeEditHandled = false;
const rtmpDevices = computed(() =>
  devices.value.filter(
    (item) => typeLabel(item.type, item.device_id || item.id) === "RTMP"
  )
);
const filtered = computed(() =>
  rows.value.filter(
    (item) =>
      `${item.name || ""}${item.app || ""}${item.stream || ""}${item.id}`
        .toLowerCase()
        .includes(query.value.toLowerCase()) &&
      (status.value === "all" ||
        (status.value === "online" ? item.is_online : !item.is_online))
  )
);
const online = computed(
  () => rows.value.filter((item) => item.is_online).length
);
const protectedCount = computed(
  () => rows.value.filter((item) => !item.config?.is_auth_disabled).length
);
const hasFilters = computed(
  () => Boolean(query.value || status.value !== "all")
);

function resetFilters() {
  query.value = "";
  status.value = "all";
}

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const [channelResponse, deviceResponse, mediaResponse] = await Promise.all([
      api.channels({ page: 1, size: 99999, type: "RTMP" }),
      api.devices({ page: 1, size: 99999 }),
      api.mediaServers({ page: 1, size: 100 }),
    ]);
    rows.value = channelResponse.data.items || [];
    devices.value = deviceResponse.data.items || [];
    media.value = mediaResponse.data.items || [];
    if (!routeEditHandled) {
      const target = String(route.query.channel || route.query.stream || "");
      const matched = rows.value.find((item) => item.id === target || item.stream === target);
      if (matched) edit(matched);
      routeEditHandled = true;
    }
  } catch (cause) {
    loadError.value = errorMessage(cause, "RTMP 推流列表加载失败");
  } finally {
    loading.value = false;
  }
}

function create() {
  editing.value = null;
  Object.assign(form, {
    name: "",
    app: "live",
    stream: "",
    device_mode: rtmpDevices.value.length ? "existing" : "new",
    device_id: rtmpDevices.value[0]?.id || "",
    device_name: "RTMP 推流设备",
    media_server_id: media.value[0]?.id || "local",
    auth_enabled: true,
  });
  open.value = true;
}

function edit(row: ApiChannel) {
  editing.value = row;
  Object.assign(form, {
    name: row.name || "",
    app: row.app || "live",
    stream: row.stream || "",
    device_mode: "existing",
    device_id: row.did || "",
    device_name: "",
    media_server_id: row.config?.media_server_id || "local",
    auth_enabled: !row.config?.is_auth_disabled,
  });
  open.value = true;
}

async function save() {
  if (form.app.trim().toLowerCase() === "rtp") {
    ui.toast("App 不能使用系统保留值 rtp");
    return;
  }
  saving.value = true;
  try {
    if (editing.value) {
      await api.editChannel(editing.value.id, {
        device_id: editing.value.device_id || "",
        name: form.name,
        ptztype: editing.value.ptztype || 0,
        is_online: Boolean(editing.value.is_online),
        ext: editing.value.ext || {},
        app: form.app,
        stream: form.stream,
        config: {
          ...editing.value.config,
          media_server_id: form.media_server_id,
          is_auth_disabled: !form.auth_enabled,
        },
      });
    } else {
      await api.addChannel({
        type: "RTMP",
        name: form.name,
        device_id: form.device_mode === "existing" ? form.device_id : "",
        device_name: form.device_mode === "new" ? form.device_name : "",
        app: form.app,
        stream: form.stream,
        config: {
          media_server_id: form.media_server_id,
          is_auth_disabled: !form.auth_enabled,
        },
      });
    }
    open.value = false;
    ui.toast(editing.value ? "推流配置已更新" : "推流配置已创建");
    await load();
  } catch (cause) {
    ui.toast(errorMessage(cause, "推流配置保存失败"));
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
    ui.toast(`推流配置 ${name} 已删除`);
    await load();
  } catch (cause) {
    ui.toast(errorMessage(cause, "推流配置删除失败"));
  } finally {
    deleting.value = false;
  }
}

async function copyAddress(row: ApiChannel) {
  if (!row.config?.push_addr) return ui.toast("后端未返回推流地址");
  try {
    await navigator.clipboard.writeText(row.config.push_addr);
    ui.toast("推流地址已复制，请妥善保管鉴权参数");
  } catch {
    ui.toast("复制失败，请检查浏览器剪贴板权限");
  }
}

onMounted(load);
</script>

<template>
  <main class="page-content">
    <header class="page-head">
      <div>
        <h1 class="page-title">RTMP 推流</h1>
        <p class="page-desc">
          基于统一通道模型管理推流入口、鉴权与在线状态；列表不直接暴露完整
          Session。
        </p>
      </div>
      <div class="head-actions">
        <RouterLink class="btn" to="/channels">全部通道</RouterLink
        ><button class="btn" :disabled="loading" @click="load">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新</button
        ><button class="btn btn-primary" @click="create">
          <Plus />新增推流
        </button>
      </div>
    </header>
    <div v-if="loadError" class="warning-box mb-4">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <section class="metric-line mb-4">
      <div class="metric-item">
        <div class="metric-label"><span>推流配置</span><UploadCloud /></div>
        <div class="metric-value">{{ rows.length }}</div>
        <div class="metric-foot">RTMP 虚拟设备 {{ rtmpDevices.length }} 台</div>
      </div>
      <div class="metric-item">
        <div class="metric-label"><span>在线推流</span><UploadCloud /></div>
        <div class="metric-value">{{ online }}</div>
        <div class="metric-foot">当前活跃</div>
      </div>
      <div class="metric-item">
        <div class="metric-label"><span>启用鉴权</span><KeyRound /></div>
        <div class="metric-value">{{ protectedCount }}</div>
        <div class="metric-foot">
          {{
            rows.length
              ? `${((protectedCount / rows.length) * 100).toFixed(1)}% 已保护`
              : "暂无配置"
          }}
        </div>
      </div>
      <div class="metric-item">
        <div class="metric-label"><span>媒体节点</span><UploadCloud /></div>
        <div class="metric-value">{{ media.length }}</div>
        <div class="metric-foot">
          {{ media.filter((item) => item.status).length }} 个在线
        </div>
      </div>
    </section>
    <section class="card table-card">
      <div class="toolbar">
        <label class="field"
          ><Search /><input
            v-model="query"
            class="input"
            aria-label="搜索 RTMP 推流"
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
          >{{ filtered.length }} / {{ rows.length }} 个配置</span
        >
      </div>
      <div class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th>推流名称</th>
              <th>App / Stream</th>
              <th>状态</th>
              <th>鉴权</th>
              <th>媒体节点</th>
              <th>最近推流</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in filtered" :key="row.id">
              <td>
                <div class="row-title">
                  <span class="device-glyph"><UploadCloud /></span
                  ><span
                    ><strong>{{ row.name || "未命名推流" }}</strong
                    ><small>{{ row.id }}</small></span
                  >
                </div>
              </td>
              <td class="mono">
                {{ row.app || "—" }} / {{ row.stream || "—" }}
              </td>
              <td>
                <span
                  class="status"
                  :class="row.is_online ? 'online' : 'offline'"
                  >{{ row.is_online ? "推流中" : "未推流" }}</span
                >
              </td>
              <td>{{ row.config?.is_auth_disabled ? "未启用" : "已启用" }}</td>
              <td>{{ row.config?.media_server_id || "local" }}</td>
              <td>{{ relativeTime(row.config?.pushed_at) }}</td>
              <td>
                <div class="row-actions">
                  <button
                    class="more-btn"
                    aria-label="复制推流地址"
                    @click="copyAddress(row)"
                  >
                    <Copy /></button
                  ><RouterLink
                    class="btn btn-sm"
                    :to="`/live?stream=${encodeURIComponent(
                      row.stream || row.id
                    )}`"
                    >预览</RouterLink
                  ><button class="btn btn-sm" @click="edit(row)">编辑</button
                  ><button
                    class="more-btn danger"
                    aria-label="删除推流配置"
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
          <LoaderCircle class="mx-auto mb-2 animate-spin" />正在加载推流配置…
        </div>
        <div v-else-if="!filtered.length" class="empty-state empty-action">
          <UploadCloud />
          <strong>{{
            rows.length
              ? "没有符合条件的推流"
              : "当前环境尚无 RTMP 推流配置"
          }}</strong>
          <span>{{
            rows.length
              ? "清除筛选后可恢复全部推流配置。"
              : "创建推流入口后即可向媒体节点推送视频。"
          }}</span>
          <button v-if="rows.length" class="btn" @click="resetFilters">
            清除筛选
          </button>
          <button v-else class="btn btn-primary" @click="create">
            <Plus />新增推流
          </button>
        </div>
      </div>
    </section>
    <ModalDialog
      :open="open"
      :title="editing ? '编辑 RTMP 推流' : '新增 RTMP 推流'"
      description="App 不能使用保留值 rtp；地址由后端根据媒体节点生成。"
      @close="open = false"
      ><form class="form-grid" @submit.prevent="save">
        <label class="form-group full"
          ><span class="form-label">推流名称</span
          ><input
            v-model="form.name"
            class="input plain w-full"
            required /></label
        ><template v-if="!editing"
          ><label class="form-group"
            ><span class="form-label">虚拟设备</span
            ><select v-model="form.device_mode" class="select w-full">
              <option value="existing" :disabled="!rtmpDevices.length">
                使用已有 RTMP 设备
              </option>
              <option value="new">新建 RTMP 虚拟设备</option>
            </select></label
          ><label v-if="form.device_mode === 'existing'" class="form-group"
            ><span class="form-label">选择设备</span
            ><select v-model="form.device_id" class="select w-full" required>
              <option
                v-for="item in rtmpDevices"
                :key="item.id"
                :value="item.id"
              >
                {{ item.name || item.id }}
              </option>
            </select></label
          ><label v-else class="form-group"
            ><span class="form-label">新设备名称</span
            ><input
              v-model="form.device_name"
              class="input plain w-full"
              required /></label></template
        ><label class="form-group"
          ><span class="form-label">媒体节点</span
          ><select v-model="form.media_server_id" class="select w-full">
            <option v-for="item in media" :key="item.id" :value="item.id">
              {{ item.id }}
            </option>
            <option v-if="!media.length" value="local">local</option>
          </select></label
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
        ><label class="toggle-row full"
          ><span
            ><strong>启用推流鉴权</strong
            ><small class="block text-slate-500"
              >复制地址时才临时使用鉴权参数</small
            ></span
          ><span class="switch"
            ><input v-model="form.auth_enabled" type="checkbox" /><span
              class="slider" /></span
        ></label>
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
      title="删除 RTMP 推流配置"
      description="该操作会删除统一通道记录，已有推流地址将立即失效。"
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
