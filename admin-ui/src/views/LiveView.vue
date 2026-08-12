<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { useRoute } from "vue-router";
import {
  Aperture,
  Camera,
  ChevronUp,
  CircleStop,
  ExternalLink,
  LoaderCircle,
  Mic,
  Minus,
  Play,
  Plus,
  Radio,
  RefreshCcw,
  Search,
  ShieldAlert,
  Volume2,
} from "@lucide/vue";
import { api, errorMessage, typeLabel } from "../services/api";
import type { ApiChannel, ApiDevice, PlayResult } from "../types/api";
import { useUiStore } from "../stores/ui";

const ui = useUiStore();
const route = useRoute();
const selected = ref("");
const layout = ref("4");
const muted = ref(false);
const query = ref("");
const loading = ref(false);
const loadError = ref("");
const actionLoading = ref("");
const channels = ref<ApiChannel[]>([]);
const devices = ref<ApiDevice[]>([]);
const playResults = ref<Record<string, PlayResult>>({});
const snapshots = ref<Record<string, string>>({});
const selectedChannel = computed(
  () =>
    channels.value.find((item) => item.id === selected.value) ||
    channels.value[0]
);
const deviceNames = computed(() =>
  Object.fromEntries(
    devices.value.map((item) => [
      item.id,
      item.name || item.device_id || item.id,
    ])
  )
);
const filteredChannels = computed(() =>
  channels.value.filter((item) =>
    `${item.name || ""}${item.channel_id || ""}${item.id}${
      deviceNames.value[item.did || ""] || ""
    }`
      .toLowerCase()
      .includes(query.value.toLowerCase())
  )
);
const wallChannels = computed(() => {
  if (!filteredChannels.value.length) return [];
  const current =
    selectedChannel.value &&
    filteredChannels.value.some((item) => item.id === selectedChannel.value?.id)
      ? selectedChannel.value
      : filteredChannels.value[0];
  const ordered = [
    current,
    ...filteredChannels.value.filter((item) => item.id !== current.id),
  ];
  return Array.from(
    { length: Math.min(Number(layout.value), ordered.length) },
    (_, index) => ordered[index]
  );
});
const supportsPtz = computed(() => Boolean(selectedChannel.value?.ptz_capable));
const selectedDevice = computed(() =>
  devices.value.find((item) => item.id === selectedChannel.value?.did)
);
const playAddresses = computed(() => {
  const result = playResults.value[selectedChannel.value?.id || ""];
  return (result?.items || []).flatMap((item) =>
    Object.entries(item)
      .filter(
        ([key, value]) => key !== "label" && typeof value === "string" && value
      )
      .map(([key, value]) => ({
        label: `${String(item.label || "")} ${key}`.trim(),
        url: String(value),
      }))
  );
});

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const [channelResponse, deviceResponse] = await Promise.all([
      api.channels({ page: 1, size: 99999 }),
      api.devices({ page: 1, size: 99999 }),
    ]);
    channels.value = channelResponse.data.items || [];
    devices.value = deviceResponse.data.items || [];
    syncRouteContext();
  } catch (cause) {
    loadError.value = errorMessage(cause, "实时通道加载失败");
  } finally {
    loading.value = false;
  }
}

function syncRouteContext() {
  const target = String(route.query.channel || route.query.stream || "");
  const device = String(route.query.device || "");
  const matched =
    (target
      ? channels.value.find(
          (item) =>
            item.id === target ||
            item.channel_id === target ||
            item.stream === target
        )
      : undefined) ||
    (device
      ? channels.value.find((item) => item.did === device)
      : undefined);
  selected.value =
    matched?.id ||
    channels.value.find((item) => item.is_online)?.id ||
    selected.value ||
    channels.value[0]?.id ||
    "";
}

async function action(name: string, fn: () => Promise<unknown>) {
  if (!selectedChannel.value) return;
  actionLoading.value = name;
  try {
    await fn();
    ui.toast(
      `${selectedChannel.value.name || selectedChannel.value.id} · ${name}成功`
    );
  } catch (cause) {
    ui.toast(errorMessage(cause, `${name}失败`));
    throw cause;
  } finally {
    actionLoading.value = "";
  }
}

async function startPlay() {
  if (!selectedChannel.value) return;
  actionLoading.value = "加载预览";
  try {
    playResults.value[selectedChannel.value.id] = (
      await api.play(selectedChannel.value.id)
    ).data;
    ui.toast("实时播放地址已获取");
  } catch (cause) {
    ui.toast(errorMessage(cause, "实时播放失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function snapshot() {
  if (!selectedChannel.value) return;
  actionLoading.value = "抓拍";
  try {
    const { data } = await api.snapshot(selectedChannel.value.id);
    snapshots.value[selectedChannel.value.id] =
      data.link ||
      `${api.snapshotImage(selectedChannel.value.id)}?t=${Date.now()}`;
    ui.toast(`快照已刷新${data.method ? ` · ${data.method}` : ""}`);
  } catch (cause) {
    ui.toast(errorMessage(cause, "抓拍失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function toggleAI() {
  if (!selectedChannel.value) return;
  const next = !selectedChannel.value.ext?.enabled_ai;
  try {
    await action(next ? "启用 AI" : "停用 AI", () =>
      next
        ? api.enableAI(selectedChannel.value!.id)
        : api.disableAI(selectedChannel.value!.id)
    );
    selectedChannel.value.ext = {
      ...selectedChannel.value.ext,
      enabled_ai: next,
    };
  } catch {
    /* 保持原状态 */
  }
}

async function toggleRecording() {
  if (!selectedChannel.value) return;
  const next =
    (selectedChannel.value.ext?.record_mode || "always") === "none"
      ? "always"
      : "none";
  try {
    await action(next === "always" ? "开启持续录像" : "停止录像", () =>
      api.recordMode(selectedChannel.value!.id, next)
    );
    selectedChannel.value.ext = {
      ...selectedChannel.value.ext,
      record_mode: next,
    };
  } catch {
    /* 保持原状态 */
  }
}

watch(() => route.query, syncRouteContext);
onMounted(load);
</script>

<template>
  <main class="page-content live-page">
    <header class="page-head">
      <div>
        <h1 class="page-title">实时监控</h1>
        <p class="page-desc">
          加载当前环境的四类协议通道，并按真实能力显示 PTZ、语音、AI
          与录像控制。
        </p>
      </div>
      <div class="head-actions">
        <div class="segmented" aria-label="分屏布局">
          <button
            v-for="item in ['1', '4', '9', '16']"
            :key="item"
            type="button"
            :class="{ active: layout === item }"
            @click="layout = item"
          >
            {{ item }} 分屏
          </button>
        </div>
        <button class="btn" :disabled="loading" @click="load">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新</button
        ><button
          class="btn btn-primary"
          :disabled="
            !selectedChannel?.is_online || actionLoading === '加载预览'
          "
          @click="startPlay"
        >
          <LoaderCircle
            v-if="actionLoading === '加载预览'"
            class="animate-spin"
          /><Play v-else />获取播放地址
        </button>
      </div>
    </header>
    <div v-if="loadError" class="warning-box mb-4">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <section v-if="channels.length" class="live-layout">
      <aside class="card resource-panel">
        <div class="card-head">
          <div>
            <h2 class="card-title">通道资源</h2>
            <p class="card-sub">
              {{ channels.filter((item) => item.is_online).length }} 路在线 /
              {{ channels.length }} 路
            </p>
          </div>
          <Radio />
        </div>
        <label class="field mb-3"
          ><Search /><input
            v-model="query"
            class="input w-full"
            aria-label="搜索设备或通道"
            placeholder="搜索设备或通道"
        /></label>
        <div class="resource-tree">
          <template v-for="device in devices" :key="device.id"
            ><p
              v-if="filteredChannels.some((item) => item.did === device.id)"
              class="tree-group"
            >
              {{ device.name || device.device_id || device.id }}
            </p>
            <button
              v-for="channel in filteredChannels.filter(
                (item) => item.did === device.id
              )"
              :key="channel.id"
              class="tree-item"
              :class="{ active: selected === channel.id }"
              :aria-pressed="selected === channel.id"
              @click="selected = channel.id"
            >
              <Radio />{{ channel.name || channel.channel_id || channel.id
              }}<i
                class="slot-led"
                :class="{ warn: !channel.is_online }"
              /></button
          ></template>
          <p
            v-if="
              filteredChannels.some(
                (item) => !devices.some((device) => device.id === item.did)
              )
            "
            class="tree-group"
          >
            未归属设备
          </p>
          <button
            v-for="channel in filteredChannels.filter(
              (item) => !devices.some((device) => device.id === item.did)
            )"
            :key="channel.id"
            class="tree-item"
            :class="{ active: selected === channel.id }"
            :aria-pressed="selected === channel.id"
            @click="selected = channel.id"
          >
            <Radio />{{ channel.name || channel.id
            }}<i class="slot-led" :class="{ warn: !channel.is_online }" />
          </button>
        </div>
      </aside>
      <article class="card video-workspace">
        <div class="live-toolbar">
          <div>
            <strong>{{ selectedChannel?.name || "未命名通道" }}</strong
            ><span
              >{{
                selectedDevice?.name || selectedChannel?.device_id || "未知设备"
              }}
              ·
              {{
                typeLabel(
                  selectedChannel?.type,
                  selectedChannel?.did ||
                    selectedChannel?.device_id ||
                    selectedChannel?.channel_id ||
                    selectedChannel?.id
                )
              }}
              ·
              {{
                playResults[selectedChannel?.id || ""] ? "地址已加载" : "待加载"
              }}</span
            >
          </div>
          <span
            class="status"
            :class="selectedChannel?.is_playing ? 'online' : ''"
            >{{ selectedChannel?.is_playing ? "LIVE" : "空闲" }}</span
          >
        </div>
        <div class="video-wall" :class="`grid-${layout}`">
          <button
            v-for="(channel, index) in wallChannels"
            :key="channel.id"
            class="video-tile"
            :class="{ active: selected === channel.id }"
            :aria-label="'选择' + (channel.name || channel.channel_id || channel.id)"
            :aria-pressed="selected === channel.id"
            @click="selected = channel.id"
          >
            <img
              v-if="snapshots[channel.id]"
              :src="snapshots[channel.id]"
              class="absolute inset-0 h-full w-full object-cover"
              alt="通道快照"
            /><span v-else class="video-placeholder"
              ><Camera /><small>{{
                channel.is_online ? "等待快照" : "通道离线"
              }}</small></span
            ><span class="video-meta"
              ><span
                class="status"
                :class="channel.is_online ? 'online' : 'offline'"
                >{{ channel.is_online ? "在线" : "离线" }}</span
              ><span
                v-if="(channel.ext?.record_mode || 'always') !== 'none'"
                class="rec"
                >REC</span
              ></span
            ><span class="video-name"
              ><span
                ><strong>{{ channel.name || "未命名通道" }}</strong
                ><small>{{ channel.channel_id || channel.id }}</small></span
              ><span class="tile-index">{{ index + 1 }}</span></span
            >
          </button>
        </div>
        <div class="video-foot">
          <div class="stream-context">
            <span>{{
              playResults[selectedChannel?.id || ""]
                ? "播放地址已准备，可选择协议打开"
                : "当前显示快照占位，需主动获取播放地址"
            }}</span>
            <strong class="mono"
              >{{
                selectedChannel?.app ||
                playResults[selectedChannel?.id || ""]?.app ||
                "—"
              }}
              /
              {{
                selectedChannel?.stream ||
                playResults[selectedChannel?.id || ""]?.stream ||
                selectedChannel?.id
              }}</strong
            >
          </div>
          <div v-if="playAddresses.length" class="stream-actions">
            <a
              v-for="item in playAddresses.slice(0, 3)"
              :key="item.url"
              class="stream-link"
              :href="item.url"
              target="_blank"
              rel="noopener noreferrer"
              ><ExternalLink />{{ item.label }}</a
            >
          </div>
          <span
            v-else-if="playResults[selectedChannel?.id || '']"
            class="stream-empty"
            >后端未返回可打开地址</span
          >
        </div>
      </article>
      <aside class="control-stack">
        <section class="card control-panel">
          <div class="card-head">
            <div>
              <h2 class="card-title">云台控制</h2>
              <p class="card-sub">
                {{
                  selectedChannel?.ptz_verified
                    ? "已验证支持"
                    : supportsPtz
                    ? "声明支持"
                    : "当前通道不支持"
                }}
              </p>
            </div>
            <span class="status" :class="supportsPtz ? 'online' : ''">PTZ</span>
          </div>
          <div class="ptz" :class="{ 'opacity-40': !supportsPtz }">
            <button
              class="up"
              :disabled="!supportsPtz"
              aria-label="向上"
              @click="
                action('云台向上', () =>
                  api.ptz(selectedChannel!.id, {
                    action: 'up',
                    speed: 30,
                    timeout: 1,
                  })
                )
              "
            >
              <ChevronUp /></button
            ><button
              class="right"
              :disabled="!supportsPtz"
              aria-label="向右"
              @click="
                action('云台向右', () =>
                  api.ptz(selectedChannel!.id, {
                    action: 'right',
                    speed: 30,
                    timeout: 1,
                  })
                )
              "
            >
              <ChevronUp /></button
            ><button
              class="down"
              :disabled="!supportsPtz"
              aria-label="向下"
              @click="
                action('云台向下', () =>
                  api.ptz(selectedChannel!.id, {
                    action: 'down',
                    speed: 30,
                    timeout: 1,
                  })
                )
              "
            >
              <ChevronUp /></button
            ><button
              class="left"
              :disabled="!supportsPtz"
              aria-label="向左"
              @click="
                action('云台向左', () =>
                  api.ptz(selectedChannel!.id, {
                    action: 'left',
                    speed: 30,
                    timeout: 1,
                  })
                )
              "
            >
              <ChevronUp /></button
            ><button
              class="center"
              :disabled="!supportsPtz"
              aria-label="停止"
              @click="
                action('云台停止', () =>
                  api.ptz(selectedChannel!.id, {
                    action: 'stop',
                    speed: 30,
                    timeout: 1,
                  })
                )
              "
            >
              <CircleStop />
            </button>
          </div>
          <div class="control-row">
            <button
              class="btn btn-sm"
              :disabled="!supportsPtz"
              @click="
                action('焦距缩小', () =>
                  api.ptz(selectedChannel!.id, {
                    action: 'zoom_out',
                    speed: 30,
                    timeout: 1,
                  })
                )
              "
            >
              <Minus />缩小</button
            ><button
              class="btn btn-sm"
              :disabled="!supportsPtz"
              @click="
                action('焦距放大', () =>
                  api.ptz(selectedChannel!.id, {
                    action: 'zoom_in',
                    speed: 30,
                    timeout: 1,
                  })
                )
              "
            >
              <Plus />放大
            </button>
          </div>
        </section>
        <section class="card control-panel">
          <div class="card-head">
            <div>
              <h2 class="card-title">通道动作</h2>
              <p class="card-sub">作用于当前选中通道</p>
            </div>
          </div>
          <div class="control-row">
            <button
              class="btn"
              :disabled="actionLoading === '抓拍'"
              @click="snapshot"
            >
              <LoaderCircle
                v-if="actionLoading === '抓拍'"
                class="animate-spin"
              /><Aperture v-else />抓拍</button
            ><button
              class="btn"
              :disabled="
                typeLabel(
                  selectedChannel?.type,
                  selectedChannel?.did ||
                    selectedChannel?.device_id ||
                    selectedChannel?.channel_id ||
                    selectedChannel?.id
                ) !== 'GB28181'
              "
              @click="
                action('开始对讲', () =>
                  api.voiceStart(selectedChannel!.id, { mode: 'talk' })
                )
              "
            >
              <Mic />对讲
            </button>
          </div>
          <label class="toggle-row"
            ><span
              ><strong class="block text-[10px]">静音播放</strong
              ><small class="text-[8px] text-slate-500"
                >仅影响当前浏览器</small
              ></span
            ><span class="switch"
              ><input v-model="muted" type="checkbox" /><span
                class="slider" /></span></label
          ><label class="toggle-row"
            ><span
              ><strong class="block text-[10px]">AI 分析</strong
              ><small class="text-[8px] text-slate-500"
                >使用通道已配置区域</small
              ></span
            ><span class="switch"
              ><input
                :checked="selectedChannel?.ext?.enabled_ai"
                type="checkbox"
                @change="toggleAI" /><span class="slider" /></span></label
          ><label class="toggle-row"
            ><span
              ><strong class="block text-[10px]">持续录像</strong
              ><small class="text-[8px] text-slate-500"
                >AI 录制暂不开放</small
              ></span
            ><span class="switch"
              ><input
                :checked="
                  (selectedChannel?.ext?.record_mode || 'always') !== 'none'
                "
                type="checkbox"
                @change="toggleRecording" /><span class="slider" /></span
          ></label>
          <div class="button-row mt-3">
            <RouterLink
              class="btn btn-sm flex-1"
              :to="`/channels/${encodeURIComponent(selectedChannel?.id || '')}`"
              >通道详情</RouterLink
            ><RouterLink
              class="btn btn-sm flex-1"
              :to="`/recordings?channel=${encodeURIComponent(
                selectedChannel?.id || ''
              )}`"
              ><Volume2 />查看录像</RouterLink
            >
          </div>
        </section>
      </aside>
    </section>
    <div v-else class="card empty-state empty-action">
      <LoaderCircle v-if="loading" class="animate-spin" />
      <template v-if="loading">
        <strong>正在加载实时通道</strong>
        <span>正在核对设备、通道与能力信息…</span>
      </template>
      <template v-else>
        <Radio />
        <strong>当前环境没有可用通道</strong>
        <span>请先接入设备，或创建 RTMP / RTSP 流通道。</span>
        <RouterLink class="btn btn-primary" to="/devices"
          >前往设备管理</RouterLink
        >
      </template>
    </div>
  </main>
</template>
