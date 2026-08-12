<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useRoute } from "vue-router";
import {
  Aperture,
  ArrowLeft,
  Camera,
  Film,
  LoaderCircle,
  Mic,
  Play,
  Radio,
  Save,
  ShieldAlert,
  Video,
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

const route = useRoute();
const ui = useUiStore();
const tab = ref("实时视频");
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
const zoneForm = reactive({
  name: "",
  coordinates: "0.1,0.1,0.9,0.1,0.9,0.9,0.1,0.9",
  color: "#38bdf8",
  labels: "person,car",
});
const supportsPtz = computed(() => Boolean(channel.value?.ptz_capable));
const isGb = computed(
  () =>
    typeLabel(
      channel.value?.type,
      channel.value?.did ||
        channel.value?.device_id ||
        channel.value?.channel_id ||
        channel.value?.id
    ) === "GB28181"
);
const streamConfigRoute = computed(() => {
  const type = typeLabel(
    channel.value?.type,
    channel.value?.did ||
      channel.value?.device_id ||
      channel.value?.channel_id ||
      channel.value?.id
  );
  if (!channel.value || !["RTMP", "RTSP"].includes(type)) return null;
  return {
    path: type === "RTMP" ? "/push-streams" : "/pull-streams",
    query: { channel: channel.value.id },
  };
});
const tabs = [
  "实时视频",
  "PTZ / 快照 / 语音",
  "AI 与检测区域",
  "录像策略",
  "平台录像",
  "设备录像 / 下载",
];
const playAddresses = computed(() =>
  (play.value?.items || []).flatMap((item) =>
    Object.entries(item)
      .filter(
        ([key, value]) => key !== "label" && typeof value === "string" && value
      )
      .map(([key, value]) => ({
        label: `${String(item.label || "")} ${key}`.trim(),
        url: String(value),
      }))
  )
);

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const { data } = await api.channel(String(route.params.id));
    channel.value = data;
    aiEnabled.value = Boolean(data.ext?.enabled_ai);
    recordMode.value = data.ext?.record_mode || "always";
    const [recordResponse, zoneResponse, deviceResponse] =
      await Promise.allSettled([
        api.recordings({ page: 1, size: 100, cid: data.id }),
        api.zones(data.id),
        data.did
          ? api.device(data.did)
          : Promise.reject(new Error("no device")),
      ]);
    if (recordResponse.status === "fulfilled")
      recordings.value = recordResponse.value.data.items || [];
    if (zoneResponse.status === "fulfilled")
      zones.value = Array.isArray(zoneResponse.value.data)
        ? zoneResponse.value.data
        : zoneResponse.value.data.items || [];
    if (deviceResponse.status === "fulfilled")
      device.value = deviceResponse.value.data;
  } catch (cause) {
    loadError.value = errorMessage(cause, "通道详情加载失败");
  } finally {
    loading.value = false;
  }
}

async function runAction(
  name: string,
  fn: () => Promise<unknown>,
  refresh = false
) {
  if (!channel.value) return;
  actionLoading.value = name;
  try {
    await fn();
    ui.toast(`${channel.value.name || channel.value.id} · ${name}成功`);
    if (refresh) await load();
  } catch (cause) {
    ui.toast(errorMessage(cause, `${name}失败`));
    throw cause;
  } finally {
    actionLoading.value = "";
  }
}

async function startPlay() {
  if (!channel.value) return;
  actionLoading.value = "开始播放";
  try {
    play.value = (await api.play(channel.value.id)).data;
    ui.toast("播放地址已获取");
  } catch (cause) {
    ui.toast(errorMessage(cause, "播放失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function refreshSnapshot() {
  if (!channel.value) return;
  actionLoading.value = "刷新快照";
  try {
    const { data } = await api.snapshot(channel.value.id);
    snapshotUrl.value =
      data.link || `${api.snapshotImage(channel.value.id)}?t=${Date.now()}`;
    ui.toast(`快照已刷新${data.method ? ` · ${data.method}` : ""}`);
  } catch (cause) {
    ui.toast(errorMessage(cause, "快照刷新失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function toggleAI() {
  if (!channel.value) return;
  const next = !aiEnabled.value;
  try {
    await runAction(next ? "启用 AI" : "停用 AI", () =>
      next ? api.enableAI(channel.value!.id) : api.disableAI(channel.value!.id)
    );
    aiEnabled.value = next;
  } catch {
    /* 保持原状态 */
  }
}

async function saveRecordMode() {
  if (!channel.value) return;
  await runAction(
    "保存录像模式",
    () => api.recordMode(channel.value!.id, recordMode.value),
    true
  );
}

async function addZone() {
  if (!channel.value) return;
  const coordinates = zoneForm.coordinates
    .split(",")
    .map(Number)
    .filter(Number.isFinite);
  if (coordinates.length < 6 || coordinates.length % 2)
    return ui.toast("区域坐标需要至少 3 个点，格式为 x1,y1,x2,y2…");
  try {
    await runAction("新增检测区域", () =>
      api.addZone(channel.value!.id, {
        name: zoneForm.name,
        coordinates,
        color: zoneForm.color,
        labels: zoneForm.labels
          .split(",")
          .map((item) => item.trim())
          .filter(Boolean),
      })
    );
    const response = await api.zones(channel.value.id);
    zones.value = Array.isArray(response.data)
      ? response.data
      : response.data.items || [];
    zoneForm.name = "";
  } catch {
    /* 错误已提示 */
  }
}

async function queryRemoteRecords() {
  if (!channel.value) return;
  const end = Math.floor(Date.now() / 1000);
  actionLoading.value = "查询设备录像";
  try {
    remoteRecords.value = (
      await api.queryDeviceRecords(channel.value.id, {
        start_at: end - 86400,
        end_at: end,
        timeout: 10,
      })
    ).data;
    ui.toast("设备端录像目录查询完成");
  } catch (cause) {
    ui.toast(errorMessage(cause, "设备录像查询失败"));
  } finally {
    actionLoading.value = "";
  }
}

onMounted(load);
</script>

<template>
  <main class="page-content">
    <RouterLink class="btn btn-ghost mb-3" to="/channels"
      ><ArrowLeft />返回通道列表</RouterLink
    >
    <div v-if="loadError" class="warning-box mb-4">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <div v-if="loading && !channel" class="card empty-state">
      <LoaderCircle class="mx-auto mb-3 animate-spin" />正在加载通道详情…
    </div>
    <template v-if="channel"
      ><section class="details-hero">
        <div class="details-identity">
          <span class="details-icon"><Radio /></span>
          <div>
            <div class="button-row">
              <h1>{{ channel.name || "未命名通道" }}</h1>
              <span
                class="status"
                :class="channel.is_online ? 'online' : 'offline'"
                >{{ channel.is_online ? "在线" : "离线" }}</span
              ><span class="protocol-tag blue">{{
                typeLabel(
                  channel.type,
                  channel.did ||
                    channel.device_id ||
                    channel.channel_id ||
                    channel.id
                )
              }}</span>
            </div>
            <p>
              {{ channel.channel_id || channel.id }} ·
              {{ device?.name || channel.device_id || "未知设备" }}
            </p>
          </div>
        </div>
        <div class="head-actions">
          <RouterLink v-if="streamConfigRoute" class="btn" :to="streamConfigRoute">
            编辑流配置
          </RouterLink>
          <button
            class="btn btn-primary"
            :disabled="!channel.is_online || actionLoading === '开始播放'"
            @click="startPlay"
          >
            <LoaderCircle
              v-if="actionLoading === '开始播放'"
              class="animate-spin"
            /><Video v-else />获取播放地址
          </button>
        </div>
      </section>
      <nav class="details-tabs" aria-label="通道详情导航">
        <button
          v-for="item in tabs"
          :key="item"
          type="button"
          :class="{ active: tab === item }"
          :aria-current="tab === item ? 'page' : undefined"
          @click="tab = item"
        >
          {{ item }}
        </button>
      </nav>
      <section v-if="tab === '实时视频'" class="content-grid">
        <article class="card video-workspace">
          <div class="live-toolbar">
            <div>
              <strong>{{ channel.name }}</strong
              ><span
                >{{
                  typeLabel(
                    channel.type,
                    channel.did ||
                      channel.device_id ||
                      channel.channel_id ||
                      channel.id
                  )
                }} ·
                {{ play?.app || channel.app || "—" }} /
                {{ play?.stream || channel.stream || channel.id }}</span
              >
            </div>
            <span class="status" :class="channel.is_playing ? 'online' : ''">{{
              channel.is_playing ? "LIVE" : "空闲"
            }}</span>
          </div>
          <div class="video-tile active !border-0 !aspect-video">
            <StreamPlayer
              :result="play"
              :poster="snapshotUrl"
              :autoplay="Boolean(play)"
              @error="(message) => ui.toast(message)"
            />
            <span class="video-meta"
              ><span
                class="status"
                :class="channel.is_online ? 'online' : 'offline'"
                >{{ channel.is_online ? "在线" : "离线" }}</span
              ><span v-if="recordMode !== 'none'" class="rec">REC</span></span
            >
          </div>
          <div class="video-foot">
            <span>{{
              playAddresses.length
                ? "已获取可用播放地址"
                : "点击“获取播放地址”启动或查询实时流"
            }}</span
            ><button
              class="btn btn-sm"
              :disabled="actionLoading === '刷新快照'"
              @click="refreshSnapshot"
            >
              <LoaderCircle
                v-if="actionLoading === '刷新快照'"
                class="animate-spin"
              /><Aperture v-else />刷新画面
            </button>
          </div>
        </article>
        <aside class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">播放地址</h2>
              <p class="card-sub">后端返回的多协议地址</p>
            </div>
            <Camera />
          </div>
          <div class="step-list">
            <a
              v-for="item in playAddresses"
              :key="item.url"
              class="step-item"
              :href="item.url"
              target="_blank"
              rel="noreferrer"
              ><span class="step-index"><Play /></span
              ><span
                ><strong>{{ item.label }}</strong
                ><small class="break-all">{{ item.url }}</small></span
              ></a
            >
            <div v-if="!playAddresses.length" class="empty-state">
              尚未获取播放地址。
            </div>
          </div>
        </aside>
      </section>
      <section v-else-if="tab === 'PTZ / 快照 / 语音'" class="grid three-col">
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">PTZ 能力</h2>
              <p class="card-sub">
                {{
                  channel.ptz_verified ? "已通过实际命令验证" : "基于静态声明"
                }}
              </p>
            </div>
            <span class="status" :class="supportsPtz ? 'online' : ''">{{
              supportsPtz ? "支持" : "不支持"
            }}</span>
          </div>
          <button
            class="btn w-full"
            :disabled="!channel.is_online"
            @click="
              runAction(
                'PTZ 能力探测',
                () =>
                  api.ptzProbe(channel!.id, {
                    action: 'stop',
                    speed: 30,
                    timeout: 5,
                  }),
                true
              )
            "
          >
            重新探测
          </button>
          <div class="control-row mt-3">
            <button
              v-for="action in ['up', 'down', 'left', 'right', 'stop']"
              :key="action"
              class="btn btn-sm"
              :disabled="!supportsPtz"
              @click="
                runAction(`PTZ ${action}`, () =>
                  api.ptz(channel!.id, { action, speed: 30, timeout: 1 })
                )
              "
            >
              {{ action }}
            </button>
          </div>
        </article>
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">通道快照</h2>
              <p class="card-sub">所有协议统一刷新</p>
            </div>
            <Aperture />
          </div>
          <button class="btn w-full" @click="refreshSnapshot">刷新快照</button>
        </article>
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">语音会话</h2>
              <p class="card-sub">GB 通道开放对讲</p>
            </div>
            <Mic />
          </div>
          <div class="control-row">
            <button
              class="btn"
              :disabled="!isGb"
              @click="
                runAction('开始对讲', () =>
                  api.voiceStart(channel!.id, { mode: 'talk' })
                )
              "
            >
              开始</button
            ><button
              class="btn"
              :disabled="!isGb"
              @click="
                runAction('停止对讲', () =>
                  api.voiceStop(channel!.id, { mode: 'talk' })
                )
              "
            >
              停止
            </button>
          </div>
        </article>
      </section>
      <section v-else-if="tab === 'AI 与检测区域'" class="grid equal-col">
        <article class="card form-section">
          <div class="card-head">
            <div>
              <h2 class="card-title">AI 分析</h2>
              <p class="card-sub">任务状态定期与数据库对齐</p>
            </div>
            <span class="status" :class="aiEnabled ? 'online' : ''">{{
              aiEnabled ? "运行中" : "未启用"
            }}</span>
          </div>
          <label class="toggle-row"
            ><span
              ><strong class="text-[10px]">启用通道 AI</strong
              ><small class="block text-[8px] text-slate-500"
                >使用已配置的第一个区域</small
              ></span
            ><span class="switch"
              ><input
                :checked="aiEnabled"
                type="checkbox"
                @change="toggleAI" /><span class="slider" /></span
          ></label>
        </article>
        <article class="card form-section">
          <div class="card-head">
            <div>
              <h2 class="card-title">检测区域</h2>
              <p class="card-sub">
                当前 {{ zones.length }} 个；运行逻辑只读取第一个区域
              </p>
            </div>
            <span class="protocol-tag amber">单区域生效</span>
          </div>
          <div v-for="zone in zones" :key="zone.name" class="read-only mb-3">
            <strong>{{ zone.name }}</strong
            ><small class="block"
              >{{ zone.labels?.join(", ") || "默认标签" }} ·
              {{ zone.coordinates.length / 2 }} 个点</small
            >
          </div>
          <div class="form-grid">
            <label class="form-group"
              ><span class="form-label">区域名称</span
              ><input
                v-model="zoneForm.name"
                class="input plain w-full"
                required /></label
            ><label class="form-group"
              ><span class="form-label">检测标签</span
              ><input
                v-model="zoneForm.labels"
                class="input plain w-full" /></label
            ><label class="form-group full"
              ><span class="form-label">归一化坐标（逗号分隔）</span
              ><textarea v-model="zoneForm.coordinates" class="textarea mono" />
            </label>
          </div>
          <div class="settings-savebar">
            <span>API 仅支持查询与新增，不提供编辑/删除</span
            ><button
              class="btn btn-primary"
              :disabled="!zoneForm.name"
              @click="addZone"
            >
              <Save />新增区域
            </button>
          </div>
        </article>
      </section>
      <section v-else-if="tab === '录像策略'" class="grid equal-col">
        <article class="card form-section">
          <div class="card-head">
            <div>
              <h2 class="card-title">平台录像模式</h2>
              <p class="card-sub">保存到通道扩展字段并控制媒体录制</p>
            </div>
            <Film />
          </div>
          <div class="form-group">
            <span class="form-label">当前模式</span
            ><select v-model="recordMode" class="select w-full">
              <option value="always">持续录像（always）</option>
              <option value="none">不录制（none）</option>
              <option value="ai" disabled>AI 触发（语义待修复）</option>
            </select>
          </div>
          <div class="warning-box mt-4">
            <ShieldAlert /><span
              >AI 模式目前与持续录像行为相同，因此不允许新切换到该模式。</span
            >
          </div>
          <button class="btn btn-primary mt-4" @click="saveRecordMode">
            保存录像模式
          </button>
        </article>
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">录像摘要</h2>
              <p class="card-sub">当前通道平台录像</p>
            </div>
            <Film />
          </div>
          <dl class="definition-grid !grid-cols-1">
            <div>
              <dt>录像片段</dt>
              <dd>{{ recordings.length }} 条</dd>
            </div>
            <div>
              <dt>最近录像</dt>
              <dd>{{ formatDate(recordings[0]?.started_at) }}</dd>
            </div>
            <div>
              <dt>当前模式</dt>
              <dd>{{ recordMode }}</dd>
            </div>
          </dl>
        </article>
      </section>
      <section v-else-if="tab === '平台录像'" class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">平台录像</h2>
            <p class="card-sub">当前已查询到 {{ recordings.length }} 条</p>
          </div>
          <Film />
        </div>
        <RouterLink
          class="btn btn-primary"
          :to="`/recordings?channel=${encodeURIComponent(channel.id)}`"
          >打开录像中心</RouterLink
        >
      </section>
      <section v-else class="grid equal-col">
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">设备端录像</h2>
              <p class="card-sub">查询最近 24 小时设备录像目录</p>
            </div>
            <Film />
          </div>
          <button
            class="btn"
            :disabled="!isGb || actionLoading === '查询设备录像'"
            @click="queryRemoteRecords"
          >
            <LoaderCircle
              v-if="actionLoading === '查询设备录像'"
              class="animate-spin"
            />查询设备录像
          </button>
          <div
            v-if="remoteRecords"
            class="read-only mono mt-4 whitespace-pre-wrap break-all"
          >
            {{ JSON.stringify(remoteRecords, null, 2) }}
          </div>
        </article>
        <article class="card card-pad">
          <div class="card-head">
            <div>
              <h2 class="card-title">直连 TCP 下载</h2>
              <p class="card-sub">2014 附录 O · 会话级查询和取消</p>
            </div>
            <ShieldAlert />
          </div>
          <div class="read-only">
            后端没有全量任务列表接口。请先从设备录像查询结果创建具体历史会话；本页不会构造无来源的下载任务。
          </div>
        </article>
      </section></template
    >
  </main>
</template>
