<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  ArrowLeft,
  Camera,
  Clock3,
  Download,
  LoaderCircle,
  Radio,
  RefreshCcw,
  Search,
  ShieldAlert,
  ShieldCheck,
  SlidersHorizontal,
  Trash2,
  UploadCloud,
} from "@lucide/vue";
import { api, errorMessage, typeLabel } from "../services/api";
import type { ApiChannel, ApiDevice } from "../types/api";
import { formatDate, relativeTime } from "../utils/format";
import { useUiStore } from "../stores/ui";
import ModalDialog from "../components/ModalDialog.vue";

const route = useRoute();
const router = useRouter();
const ui = useUiStore();
const tab = ref("概览");
const loading = ref(false);
const loadError = ref("");
const actionLoading = ref("");
const device = ref<ApiDevice | null>(null);
const relatedChannels = ref<ApiChannel[]>([]);
const diagnostics = ref<Record<string, unknown> | null>(null);
const a4 = ref<Record<string, unknown> | null>(null);
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
const tabs = [
  "概览",
  "通道",
  "订阅与同步",
  "国标配置",
  "协议档案",
  "诊断与高级操作",
];

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const id = String(route.params.id);
    const { data } = await api.device(id);
    device.value = data;
    basicForm.name = data.name || "";
    const channelResponse = await api.channels({
      page: 1,
      size: 99999,
      did: data.id,
    });
    relatedChannels.value = channelResponse.data.items || [];
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
) {
  if (!device.value) return;
  actionLoading.value = name;
  try {
    await fn();
    ui.toast(`${device.value.name || device.value.id} · ${name}成功`);
    if (refresh) await load();
  } catch (cause) {
    ui.toast(errorMessage(cause, `${name}失败`));
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
  await runAction(
    "保存设备",
    () =>
      api.editDevice(device.value!.id, {
        ...editForm,
        password: editForm.password || device.value?.password || "",
      }),
    true
  );
  editOpen.value = false;
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
  <main class="page-content">
    <RouterLink class="btn btn-ghost mb-3" to="/devices"
      ><ArrowLeft />返回设备列表</RouterLink
    >
    <div v-if="loadError" class="warning-box mb-4">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <div v-if="loading && !device" class="card empty-state">
      <LoaderCircle class="mx-auto mb-3 animate-spin" />正在加载设备详情…
    </div>
    <template v-if="device"
      ><section class="details-hero">
        <div class="details-identity">
          <span class="details-icon"><Camera /></span>
          <div>
            <div class="button-row">
              <h1>{{ device.name || "未命名设备" }}</h1>
              <span
                class="status"
                :class="device.is_online ? 'online' : 'offline'"
                >{{ device.is_online ? "在线" : "离线" }}</span
              ><span class="protocol-tag blue">{{
                typeLabel(device.type, device.device_id || device.id)
              }}</span>
            </div>
            <p>
              {{ device.device_id || device.id }} ·
              {{
                device.address ||
                [device.ip, device.port].filter(Boolean).join(":") ||
                "无地址"
              }}
              · 最近心跳 {{ relativeTime(device.keepalive_at) }}
            </p>
          </div>
        </div>
        <div class="head-actions">
          <button class="btn" @click="openEdit">
            <SlidersHorizontal />编辑</button
          ><button class="btn btn-danger" @click="deleteOpen = true">
            <Trash2 />删除</button
          ><button
            class="btn btn-primary"
            :disabled="!isGb || actionLoading === '同步目录'"
            @click="runAction('同步目录', () => api.catalog(device!.id), true)"
          >
            <LoaderCircle
              v-if="actionLoading === '同步目录'"
              class="animate-spin"
            /><RefreshCcw v-else />同步目录
          </button>
        </div>
      </section>
      <nav class="details-tabs" aria-label="设备详情导航">
        <button
          v-for="item in tabs"
          :key="item"
          :class="{ active: tab === item }"
          @click="tab = item"
        >
          {{ item }}
        </button>
      </nav>
      <template v-if="tab === '概览'"
        ><section class="grid two-col">
          <article class="card card-pad">
            <div class="card-head">
              <div>
                <h2 class="card-title">设备档案</h2>
                <p class="card-sub">当前注册与网络信息</p>
              </div>
              <span
                class="status"
                :class="device.is_online ? 'online' : 'offline'"
                >{{ device.is_online ? "已注册" : "离线" }}</span
              >
            </div>
            <dl class="definition-grid">
              <div>
                <dt>设备编码</dt>
                <dd class="mono">{{ device.device_id || device.id }}</dd>
              </div>
              <div>
                <dt>协议类型</dt>
                <dd>{{ typeLabel(device.type, device.device_id || device.id) }}</dd>
              </div>
              <div>
                <dt>厂商 / 型号</dt>
                <dd>
                  {{
                    [device.ext?.manufacturer, device.ext?.model]
                      .filter(Boolean)
                      .join(" / ") || "—"
                  }}
                </dd>
              </div>
              <div>
                <dt>注册地址</dt>
                <dd class="mono">
                  {{
                    device.address ||
                    [device.ip, device.port].filter(Boolean).join(":") ||
                    "—"
                  }}
                </dd>
              </div>
              <div>
                <dt>注册时间</dt>
                <dd>{{ formatDate(device.registered_at) }}</dd>
              </div>
              <div>
                <dt>通道数量</dt>
                <dd>{{ device.channels || relatedChannels.length }} 路</dd>
              </div>
              <div>
                <dt>心跳间隔</dt>
                <dd>{{ device.keepalives || "—" }} 秒</dd>
              </div>
              <div>
                <dt>协议版本</dt>
                <dd>
                  {{
                    device.ext?.gb_effective_version ||
                    device.ext?.gb_version ||
                    "—"
                  }}
                </dd>
              </div>
              <div>
                <dt>版本来源</dt>
                <dd>{{ device.ext?.gb_version_source || "—" }}</dd>
              </div>
            </dl>
          </article>
          <aside class="card card-pad">
            <div class="card-head">
              <div>
                <h2 class="card-title">能力摘要</h2>
                <p class="card-sub">按协议、版本与探测结果门控</p>
              </div>
              <ShieldCheck />
            </div>
            <div class="capability-grid">
              <div class="capability available">
                <strong>实时播放</strong><small>统一播放接口</small>
              </div>
              <div
                class="capability"
                :class="device.ptz_capable ? 'available' : 'unavailable'"
              >
                <strong>PTZ 控制</strong
                ><small>{{
                  device.ptz_verified
                    ? "已验证"
                    : device.ptz_capable
                    ? "声明支持"
                    : "未探测"
                }}</small>
              </div>
              <div
                class="capability"
                :class="isGb ? 'available' : 'unavailable'"
              >
                <strong>语音会话</strong
                ><small>{{ isGb ? "按通道能力开放" : "当前协议不适用" }}</small>
              </div>
              <div class="capability available">
                <strong>快照</strong><small>刷新并读取</small>
              </div>
              <div
                class="capability"
                :class="isGb ? 'available' : 'unavailable'"
              >
                <strong>远程录像</strong
                ><small>{{ isGb ? "设备端目录" : "当前协议不适用" }}</small>
              </div>
              <div
                class="capability"
                :class="
                  device.ext?.gb_version_capabilities?.length
                    ? 'available'
                    : 'unavailable'
                "
              >
                <strong>扩展能力</strong
                ><small
                  >{{
                    device.ext?.gb_version_capabilities?.length || 0
                  }}
                  项声明</small
                >
              </div>
            </div>
          </aside>
        </section></template
      >
      <section v-else-if="tab === '通道'" class="card table-card">
        <div class="card-head">
          <div>
            <h2 class="card-title">设备通道</h2>
            <p class="card-sub">目录同步会保留 AI、区域与录像配置</p>
          </div>
          <button
            class="btn btn-sm"
            :disabled="!isGb || actionLoading === '同步目录'"
            @click="runAction('同步目录', () => api.catalog(device!.id), true)"
          >
            <RefreshCcw />同步目录
          </button>
        </div>
        <div class="table-wrap">
          <table class="data-table">
            <thead>
              <tr>
                <th>通道</th>
                <th>协议</th>
                <th>在线</th>
                <th>PTZ</th>
                <th>AI</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="channel in relatedChannels" :key="channel.id">
                <td>
                  <div class="row-title">
                    <span class="device-glyph"><Radio /></span
                    ><span
                      ><strong>{{ channel.name || "未命名通道" }}</strong
                      ><small>{{
                        channel.channel_id || channel.id
                      }}</small></span
                    >
                  </div>
                </td>
                <td>
                  <span class="protocol-tag">{{
                    typeLabel(
                      channel.type,
                      channel.did ||
                        channel.device_id ||
                        channel.channel_id ||
                        channel.id
                    )
                  }}</span>
                </td>
                <td>
                  <span
                    class="status"
                    :class="channel.is_online ? 'online' : 'offline'"
                    >{{ channel.is_online ? "在线" : "离线" }}</span
                  >
                </td>
                <td>
                  {{
                    channel.ptz_verified
                      ? "已验证"
                      : channel.ptz_capable
                      ? "声明支持"
                      : "不支持"
                  }}
                </td>
                <td>{{ channel.ext?.enabled_ai ? "已启用" : "未启用" }}</td>
                <td>
                  <div class="row-actions">
                    <RouterLink
                      class="btn btn-sm"
                      :to="`/channels/${encodeURIComponent(channel.id)}`"
                      >详情</RouterLink
                    ><RouterLink
                      class="btn btn-sm"
                      :to="`/live?channel=${encodeURIComponent(channel.id)}`"
                      >预览</RouterLink
                    >
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
          <div v-if="!relatedChannels.length" class="empty-state">
            当前设备没有通道数据。
          </div>
        </div>
      </section>
      <section v-else-if="tab === '订阅与同步'" class="grid equal-col">
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">目录与时间</h2>
              <p class="card-sub">GB28181 设备级运维动作</p>
            </div>
            <Clock3 />
          </div>
          <div class="step-list">
            <div class="step-item">
              <span class="step-index">01</span
              ><span
                ><strong>目录同步</strong
                ><small>增量保存通道并保留用户配置</small></span
              ><button
                class="btn btn-sm"
                :disabled="!isGb"
                @click="
                  runAction('目录同步', () => api.catalog(device!.id), true)
                "
              >
                执行
              </button>
            </div>
            <div class="step-item">
              <span class="step-index">02</span
              ><span
                ><strong>设备校时</strong><small>下发平台当前时间</small></span
              ><button
                class="btn btn-sm"
                :disabled="!isGb"
                @click="runAction('设备校时', () => api.timeSync(device!.id))"
              >
                执行
              </button>
            </div>
          </div>
        </article>
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">事件订阅</h2>
              <p class="card-sub">每次操作订阅 3600 秒</p>
            </div>
            <Radio />
          </div>
          <div class="step-list">
            <div
              v-for="item in [
                { name: '目录订阅', event: 'catalog' },
                { name: '报警订阅', event: 'alarm' },
                { name: '位置订阅', event: 'mobile_position' },
              ]"
              :key="item.event"
              class="step-item"
            >
              <span class="step-index"><Radio /></span
              ><span
                ><strong>{{ item.name }}</strong
                ><small>{{ item.event }}</small></span
              ><button
                class="btn btn-sm"
                :disabled="!isGb"
                @click="
                  runAction(item.name, () =>
                    api.subscribe(device!.id, {
                      event: item.event,
                      expires: 3600,
                    })
                  )
                "
              >
                订阅
              </button>
            </div>
          </div>
        </article>
      </section>
      <section v-else-if="tab === '国标配置'" class="card form-section">
        <div class="card-head">
          <div>
            <h2 class="card-title">BasicParam 基础参数</h2>
            <p class="card-sub">GB/T 28181-2014+，下发后等待设备响应</p>
          </div>
          <span class="protocol-tag blue">2014+</span>
        </div>
        <div v-if="!isGb" class="empty-state">
          {{ typeLabel(device.type, device.device_id || device.id) }}
          设备不显示国标配置能力。
        </div>
        <form v-else @submit.prevent="saveBasic">
          <div class="form-grid">
            <label class="form-group"
              ><span class="form-label">设备名称</span
              ><input
                v-model="basicForm.name"
                class="input plain w-full" /></label
            ><label class="form-group"
              ><span class="form-label">过期时间</span
              ><input
                v-model.number="basicForm.expiration"
                class="input plain w-full"
                type="number" /></label
            ><label class="form-group"
              ><span class="form-label">心跳间隔</span
              ><input
                v-model.number="basicForm.heartbeat_interval"
                class="input plain w-full"
                type="number" /></label
            ><label class="form-group"
              ><span class="form-label">心跳超时次数</span
              ><input
                v-model.number="basicForm.heartbeat_count"
                class="input plain w-full"
                type="number"
            /></label>
          </div>
          <div class="settings-savebar">
            <span>该操作会向在线设备下发配置</span
            ><button
              class="btn btn-primary"
              :disabled="actionLoading === '下发 BasicParam'"
            >
              <LoaderCircle
                v-if="actionLoading === '下发 BasicParam'"
                class="animate-spin"
              />下发配置
            </button>
          </div>
        </form>
      </section>
      <section v-else-if="tab === '协议档案'" class="grid equal-col">
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">版本与能力矩阵</h2>
              <p class="card-sub">来自自动协商与手动覆盖</p>
            </div>
            <ShieldCheck />
          </div>
          <dl class="definition-grid">
            <div>
              <dt>有效版本</dt>
              <dd>
                {{
                  device.ext?.gb_effective_version ||
                  device.ext?.gb_version ||
                  "—"
                }}
              </dd>
            </div>
            <div>
              <dt>版本来源</dt>
              <dd>{{ device.ext?.gb_version_source || "—" }}</dd>
            </div>
            <div>
              <dt>声明版本</dt>
              <dd>{{ device.ext?.gb_declared_version || "—" }}</dd>
            </div>
            <div>
              <dt>手动覆盖</dt>
              <dd>{{ device.ext?.gb_manual_version || "未设置" }}</dd>
            </div>
          </dl>
          <div class="button-row mt-4">
            <span
              v-for="item in device.ext?.gb_version_capabilities || []"
              :key="item"
              class="protocol-tag blue"
              >{{ item }}</span
            >
          </div>
        </article>
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">能力探测</h2>
              <p class="card-sub">在线执行 OPTIONS 与 PTZ stop 探测</p>
            </div>
            <Search />
          </div>
          <div class="step-list">
            <div class="step-item">
              <span class="step-index">O</span
              ><span
                ><strong>OPTIONS</strong><small>刷新版本与能力协商</small></span
              ><button
                class="btn btn-sm"
                :disabled="!device.is_online"
                @click="
                  runAction(
                    'OPTIONS 探测',
                    () => api.optionsProbe(device!.id, { timeout: 5 }),
                    true
                  )
                "
              >
                重测
              </button>
            </div>
            <div class="step-item">
              <span class="step-index">P</span
              ><span
                ><strong>PTZ 控制</strong
                ><small>对设备通道执行 stop 探测</small></span
              ><button
                class="btn btn-sm"
                :disabled="!device.is_online"
                @click="
                  runAction(
                    'PTZ 探测',
                    () =>
                      api.devicePtzProbe(device!.id, {
                        action: 'stop',
                        speed: 30,
                        timeout: 5,
                      }),
                    true
                  )
                "
              >
                重测
              </button>
            </div>
          </div>
        </article>
      </section>
      <section v-else class="grid equal-col">
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">协议诊断</h2>
              <p class="card-sub">当前设备诊断快照</p>
            </div>
            <ShieldCheck />
          </div>
          <div class="read-only mono whitespace-pre-wrap break-all">
            {{
              diagnostics
                ? JSON.stringify(diagnostics, null, 2)
                : "当前后端未返回诊断快照。"
            }}
          </div>
          <RouterLink
            class="btn btn-sm mt-4"
            :to="`/diagnostics?device=${encodeURIComponent(device.id)}`"
            >打开完整诊断</RouterLink
          >
        </article>
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">扩展查询</h2>
              <p class="card-sub">只读查询已落库的协议扩展对象</p>
            </div>
            <UploadCloud />
          </div>
          <div class="step-list">
            <div class="step-item">
              <span class="step-index">A4</span
              ><span
                ><strong>获取 A.4 快照</strong
                ><small>GB/T 28181-2022 扩展对象</small></span
              ><button class="btn btn-sm" :disabled="!isGb" @click="queryA4">
                查询
              </button>
            </div>
            <div class="step-item">
              <span class="step-index"><Download /></span
              ><span
                ><strong>设备录像目录</strong
                ><small>请在具体通道中查询</small></span
              ><RouterLink
                class="btn btn-sm"
                :to="
                  relatedChannels[0]
                    ? `/channels/${encodeURIComponent(relatedChannels[0].id)}`
                    : '#'
                "
                >进入通道</RouterLink
              >
            </div>
          </div>
          <div
            v-if="a4"
            class="read-only mono mt-4 whitespace-pre-wrap break-all"
          >
            {{ JSON.stringify(a4, null, 2) }}
          </div>
        </article>
      </section>
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
            ><select
              v-model.number="editForm.stream_mode"
              class="select w-full"
            >
              <option :value="0">UDP</option>
              <option :value="1">TCP Passive</option>
              <option :value="2">TCP Active</option>
            </select></label
          ><label v-if="isGb" class="form-group"
            ><span class="form-label">GB 版本覆盖</span
            ><select v-model="editForm.gb_version" class="select w-full">
              <option value="">自动协商</option>
              <option value="1.0">1.0 / 2011</option>
              <option value="1.1">1.1 / 2014</option>
              <option value="2.0">2.0 / 2016</option>
              <option value="3.0">3.0 / 2022</option>
            </select></label
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
