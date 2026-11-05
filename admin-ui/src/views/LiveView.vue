<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  Aperture,
  Camera,
  ChevronDown,
  ChevronUp,
  CircleStop,
  ExternalLink,
  LoaderCircle,
  Mic,
  Minus,
  PanelLeftClose,
  PanelLeftOpen,
  PanelRight,
  Plus,
  Radio,
  RefreshCcw,
  Search,
  ShieldAlert,
  Volume2,
  X,
} from "@lucide/vue";
import { api, collectPages, errorMessage, typeLabel } from "../services/api";
import type { ApiChannel, ApiDevice, PlayResult } from "../types/api";
import { useUiStore } from "../stores/ui";
import { canAttemptPTZ, ptzCapabilityLabel } from "../utils/ptz";
import SnapshotWorkspace from "../components/SnapshotWorkspace.vue";
import type { CaptureRecord } from "../types/api";
import StreamPlayer from "../components/StreamPlayer.vue";

const ui = useUiStore();
const route = useRoute();
const router = useRouter();
const selected = ref("");
const layout = ref("4");
const resourcesOpen = ref(true);
const controlsOpen = ref(false);
const activeSlot = ref(0);
const previewSlots = ref<(ApiChannel | null)[]>([]);
const previewLoadingId = ref("");
const resourceSearch = ref<HTMLInputElement>();
const controlsPanel = ref<HTMLElement>();
const resourceToggle = ref<HTMLButtonElement>();
const controlsToggle = ref<HTMLButtonElement>();
const muted = ref(false);
const query = ref("");
const loading = ref(false);
const loadingMore = ref(false);
const loadError = ref("");
const actionLoading = ref("");
const channels = ref<ApiChannel[]>([]);
const devices = ref<ApiDevice[]>([]);
const channelPage = ref(1);
const channelTotal = ref(0);
const loadedChannelCount = ref(0);
const playResults = ref<Record<string, PlayResult>>({});
const snapshots = ref<Record<string, string>>({});
const captureWorkspace = ref<InstanceType<typeof SnapshotWorkspace>>();
const captureChannel = ref<ApiChannel | null>(null);
const captureRegion = ref<HTMLElement>();
const collapsedResourceGroups = ref<Set<string>>(new Set());
const CHANNEL_PAGE_SIZE = 200;
let searchTimer: number | undefined;
let channelLoadSequence = 0;
const selectedChannel = computed(() => previewSlots.value[activeSlot.value] || undefined);
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
const canLoadMoreChannels = computed(
  () => loadedChannelCount.value < channelTotal.value
);
const wallChannels = computed(() => Array.from(
  { length: Number(layout.value) },
  (_, index) => previewSlots.value[index] || null
));
const supportsPtz = computed(() => Boolean(selectedChannel.value?.is_online) && canAttemptPTZ(selectedChannel.value));
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

function channelsForDevice(deviceID: string) {
  return filteredChannels.value.filter((item) => item.did === deviceID);
}

function unassignedChannels() {
  return filteredChannels.value.filter(
    (item) => !devices.value.some((device) => device.id === item.did)
  );
}

function resourceGroupExpanded(groupID: string) {
  return Boolean(query.value.trim()) || !collapsedResourceGroups.value.has(groupID);
}

function toggleResourceGroup(groupID: string) {
  const next = new Set(collapsedResourceGroups.value);
  if (next.has(groupID)) next.delete(groupID);
  else next.add(groupID);
  collapsedResourceGroups.value = next;
}

function selectSlot(index: number) {
  activeSlot.value = index;
  selected.value = previewSlots.value[index]?.id || "";
}

function selectResource(channel: ApiChannel) {
  // 已在墙上的通道直接定位；新通道只替换选中窗口，避免其他画面跳位。
  const existing = previewSlots.value.findIndex((item) => item?.id === channel.id);
  if (existing >= 0 && existing < Number(layout.value)) selectSlot(existing);
  else {
    if (existing >= 0) previewSlots.value[existing] = previewSlots.value[activeSlot.value] || null;
    previewSlots.value[activeSlot.value] = channel;
    selected.value = channel.id;
  }
}

async function openResourcePicker() {
  resourcesOpen.value = true;
  await nextTick();
  resourceSearch.value?.focus();
}

function closeResources() {
  resourcesOpen.value = false;
  resourceToggle.value?.focus();
}

function closeControls() {
  controlsOpen.value = false;
  controlsToggle.value?.focus();
}

async function toggleControls() {
  controlsOpen.value = !controlsOpen.value;
  if (controlsOpen.value) {
    await nextTick();
    controlsPanel.value?.scrollIntoView({ block: "nearest" });
  }
}

async function loadChannels(reset = true) {
  const sequence = ++channelLoadSequence;
  const requestedPage = reset ? 1 : channelPage.value + 1;
  if (reset) loading.value = true;
  else loadingMore.value = true;
  try {
    const { data } = await api.channels({
      page: requestedPage,
      size: CHANNEL_PAGE_SIZE,
      key: query.value.trim() || undefined,
    });
    if (sequence !== channelLoadSequence) return;
    const nextItems = data?.items || [];
    channels.value = reset
      ? nextItems
      : [
          ...new Map(
            [...channels.value, ...nextItems].map((item) => [item.id, item])
          ).values(),
        ];
    channelPage.value = requestedPage;
    channelTotal.value = Number(data?.total ?? channels.value.length);
    loadedChannelCount.value = reset
      ? nextItems.length
      : Math.min(channelTotal.value, loadedChannelCount.value + nextItems.length);
    previewSlots.value = previewSlots.value.map((slot) => slot
      ? nextItems.find((item) => item.id === slot.id) || slot
      : null);
    await syncRouteContext();
  } finally {
    if (sequence === channelLoadSequence) {
      loading.value = false;
      loadingMore.value = false;
    }
  }
}

async function load() {
  loadError.value = "";
  loading.value = true;
  const deviceTask = collectPages(api.devices)
    .then((result) => {
      devices.value = result.items;
    })
    .catch((cause) => {
      devices.value = [];
      loadError.value = `设备分组加载失败，通道已按未分组展示：${errorMessage(cause)}`;
    });
  try {
    await Promise.all([loadChannels(true), deviceTask]);
  } catch (cause) {
    loadError.value = errorMessage(cause, "实时通道加载失败");
  } finally {
    loading.value = false;
  }
}

async function syncRouteContext() {
  const target = String(route.query.channel || route.query.stream || "");
  const device = String(route.query.device || "");
  let matched =
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
  if (target && !matched && !query.value.trim()) {
    try {
      const { data } = await api.channel(target);
      matched = data;
      channels.value = [
        data,
        ...channels.value.filter((item) => item.id !== data.id),
      ];
    } catch {
      // 路由目标可能已删除，继续选中首个可用通道。
    }
  }
  if (!previewSlots.value.length) {
    const first = matched || channels.value.find((item) => item.is_online) || channels.value[0];
    if (first) {
      previewSlots.value = [first, ...channels.value.filter((item) => item.id !== first.id)].slice(0, Number(layout.value));
      selectSlot(0);
    }
  } else if (matched && selected.value !== matched.id) {
    selectResource(matched);
  }
}

function loadMoreChannels() {
  if (!loading.value && !loadingMore.value && canLoadMoreChannels.value)
    void loadChannels(false).catch((cause) => {
      loadError.value = errorMessage(cause, "更多通道加载失败");
    });
}

async function action(name: string, fn: () => Promise<unknown>) {
  if (!selectedChannel.value || actionLoading.value) return;
  const channel = selectedChannel.value;
  actionLoading.value = name;
  try {
    const result = await fn();
    ui.toast(
      name.startsWith("云台") || name.startsWith("焦距") || name === "停止云台"
        ? (result as { data?: { msg?: string } })?.data?.msg || "设备已接受控制信令，请观察画面确认是否执行"
        : `${channel.name || channel.id} · ${name}成功`
    );
  } catch (cause) {
    ui.toast(errorMessage(cause, `${name}失败`));
    throw cause;
  } finally {
    actionLoading.value = "";
  }
}

async function startPlay(channel = selectedChannel.value) {
  if (!channel?.is_online || actionLoading.value) return;
  const channelID = channel.id;
  previewLoadingId.value = channelID;
  actionLoading.value = "加载预览";
  try {
    playResults.value[channelID] = (
      await api.play(channelID)
    ).data;
    ui.toast("实时播放地址已获取");
  } catch (cause) {
    ui.toast(errorMessage(cause, "实时播放失败"));
  } finally {
    previewLoadingId.value = "";
    actionLoading.value = "";
  }
}

async function snapshot() {
  if (!selectedChannel.value || actionLoading.value) return;
  captureChannel.value = selectedChannel.value;
  await nextTick();
  captureRegion.value?.scrollIntoView({ behavior: "smooth", block: "start" });
  await captureWorkspace.value?.capture();
}
function captureSaved(row: CaptureRecord) {
  snapshots.value[row.channel_id] = api.captureImage(row.id, false, row.updated_at);
}
function refreshCaptureCover() {
  if (captureChannel.value) snapshots.value[captureChannel.value.id] = api.snapshotImage(captureChannel.value.id, Date.now());
}

function openVoiceControls() {
  if (!selectedChannel.value) return;
  void router.push({ name: "channel-detail", params: { id: selectedChannel.value.id } });
}

async function toggleAI() {
  if (!selectedChannel.value || actionLoading.value) return;
  const channel = selectedChannel.value;
  const next = !channel.ext?.enabled_ai;
  try {
    await action(next ? "启用 AI" : "停用 AI", () =>
      next
        ? api.enableAI(channel.id)
        : api.disableAI(channel.id)
    );
    channel.ext = {
      ...channel.ext,
      enabled_ai: next,
    };
  } catch {
    /* 保持原状态 */
  }
}

async function toggleRecording() {
  if (!selectedChannel.value || actionLoading.value) return;
  const channel = selectedChannel.value;
  const next =
    (channel.ext?.record_mode || "none") === "none"
      ? "always"
      : "none";
  try {
    await action(next === "always" ? "开启手动录像" : "停止录像", () =>
      api.recordMode(channel.id, next)
    );
    channel.ext = {
      ...channel.ext,
      record_mode: next,
    };
  } catch {
    /* 保持原状态 */
  }
}

watch(() => route.query, () => void syncRouteContext());
watch(layout, () => {
  if (activeSlot.value >= Number(layout.value)) selectSlot(0);
});
watch(query, () => {
  window.clearTimeout(searchTimer);
  searchTimer = window.setTimeout(() => {
    loadError.value = "";
    void loadChannels(true).catch((cause) => {
      loadError.value = errorMessage(cause, "通道搜索失败");
    });
  }, 350);
});
onMounted(() => {
  resourcesOpen.value = window.matchMedia('(min-width: 901px)').matches;
  void load();
});
onBeforeUnmount(() => window.clearTimeout(searchTimer));
</script>

<template>
  <main class="page-content live-page">
    <header class="page-head">
      <div>
        <h1 class="page-title">实时监控</h1>
        <p class="page-desc">选择窗口后切换通道，点击画面开始预览。</p>
      </div>
      <div class="head-actions">
        <button ref="resourceToggle" class="btn" type="button" :aria-expanded="resourcesOpen" aria-controls="live-resources" :aria-label="resourcesOpen ? '收起通道资源' : '展开通道资源'" :title="resourcesOpen ? '收起通道资源' : '展开通道资源'" @click="resourcesOpen = !resourcesOpen"><PanelLeftClose v-if="resourcesOpen" aria-hidden="true" /><PanelLeftOpen v-else aria-hidden="true" />通道资源</button>
        <div class="segmented" aria-label="分屏布局">
          <button
            v-for="item in ['1', '4', '9', '16']"
            :key="item"
            type="button"
            :class="{ active: layout === item }"
            :aria-pressed="layout === item"
            @click="layout = item"
          >
            {{ item }} 分屏
          </button>
        </div>
        <button class="btn" :disabled="loading" @click="load">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新</button>
      </div>
    </header>
    <div v-if="loadError" class="warning-box mb-4" role="alert">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <section v-if="channels.length || previewSlots.length || query" class="live-monitor-layout" :class="{ 'resources-hidden': !resourcesOpen, 'has-controls': controlsOpen && selectedChannel }">
      <aside v-if="resourcesOpen" id="live-resources" class="card resource-panel" aria-label="通道资源">
        <div class="card-head live-resource-heading">
          <h2 class="card-title">通道资源</h2>
          <button class="icon-btn" type="button" aria-label="收起通道资源" title="收起通道资源" aria-controls="live-resources" :aria-expanded="resourcesOpen" @click="closeResources"><PanelLeftClose aria-hidden="true" /></button>
          <p class="card-sub">
              {{ channels.filter((item) => item.is_online).length }} 路在线 /
              已加载 {{ loadedChannelCount }} / {{ channelTotal }} 路
          </p>
        </div>
        <label class="field mb-3"
          ><Search /><input
            ref="resourceSearch"
            v-model="query"
            class="input w-full"
            aria-label="搜索通道名称、编号或流"
            placeholder="搜索通道名称、编号或流"
        /></label>
        <div class="resource-tree">
          <template v-for="device in devices" :key="device.id">
            <button
              v-if="channelsForDevice(device.id).length"
              type="button"
              class="tree-group"
              :aria-expanded="resourceGroupExpanded(device.id)"
              :aria-controls="`resource-group-${device.id}`"
              @click="toggleResourceGroup(device.id)"
            >
              <span>{{ device.name || device.device_id || device.id }}</span>
              <small>{{ channelsForDevice(device.id).length }}</small>
              <ChevronDown :class="{ rotated: resourceGroupExpanded(device.id) }" />
            </button>
            <div
              v-if="channelsForDevice(device.id).length"
              v-show="resourceGroupExpanded(device.id)"
              :id="`resource-group-${device.id}`"
              class="tree-group-items"
            >
              <button
                v-for="channel in channelsForDevice(device.id)"
                :key="channel.id"
                class="tree-item"
                :class="{ active: selected === channel.id }"
                :aria-pressed="selected === channel.id"
                @click="selectResource(channel)"
              >
                <Radio aria-hidden="true" /><span class="resource-channel-name" :title="channel.name || channel.channel_id || channel.id">{{ channel.name || channel.channel_id || channel.id }}</span><i
                  class="slot-led"
                  :class="{ warn: !channel.is_online }"
                  :aria-label="channel.is_online ? '在线' : '离线'"
                />
              </button>
            </div>
          </template>
          <button
            v-if="unassignedChannels().length"
            type="button"
            class="tree-group"
            :aria-expanded="resourceGroupExpanded('__unassigned__')"
            aria-controls="resource-group-unassigned"
            @click="toggleResourceGroup('__unassigned__')"
          >
            <span>未归属设备</span>
            <small>{{ unassignedChannels().length }}</small>
            <ChevronDown :class="{ rotated: resourceGroupExpanded('__unassigned__') }" />
          </button>
          <div
            v-if="unassignedChannels().length"
            v-show="resourceGroupExpanded('__unassigned__')"
            id="resource-group-unassigned"
            class="tree-group-items"
          >
            <button
              v-for="channel in unassignedChannels()"
              :key="channel.id"
              class="tree-item"
              :class="{ active: selected === channel.id }"
              :aria-pressed="selected === channel.id"
              @click="selectResource(channel)"
            >
              <Radio aria-hidden="true" /><span class="resource-channel-name" :title="channel.name || channel.id">{{ channel.name || channel.id }}</span><i class="slot-led" :class="{ warn: !channel.is_online }" :aria-label="channel.is_online ? '在线' : '离线'" />
            </button>
          </div>
          <button
            v-if="canLoadMoreChannels"
            type="button"
            class="btn resource-load-more"
            :disabled="loadingMore"
            @click="loadMoreChannels"
          >
            <LoaderCircle v-if="loadingMore" class="animate-spin" />
            {{ loadingMore ? "正在加载…" : `加载更多（剩余 ${channelTotal - loadedChannelCount} 路）` }}
          </button>
          <p v-if="!filteredChannels.length" class="live-resource-empty" role="status">{{ loading ? '正在搜索通道…' : '未找到匹配通道，请调整关键词。' }}</p>
        </div>
        <p class="live-resource-hint">选择通道以替换窗口 {{ activeSlot + 1 }}</p>
      </aside>
      <article class="card video-workspace channel-video-workspace live-video-workspace">
        <div class="live-toolbar">
          <div class="live-current-channel">
            <strong>{{ selectedChannel ? selectedChannel.name || "未命名通道" : `窗口 ${activeSlot + 1} · 尚未选择通道` }}</strong
            ><span v-if="selectedChannel"
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
          <div class="live-workspace-actions">
            <button v-if="playResults[selectedChannel?.id || '']" class="btn" type="button" :disabled="!selectedChannel?.is_online || Boolean(actionLoading)" @click="startPlay()"><LoaderCircle v-if="previewLoadingId === selectedChannel?.id" class="animate-spin" aria-hidden="true" /><RefreshCcw v-else aria-hidden="true" />重新预览</button>
            <button ref="controlsToggle" class="btn" type="button" :disabled="!selectedChannel" :aria-expanded="controlsOpen && Boolean(selectedChannel)" aria-controls="live-controls" @click="toggleControls"><PanelRight aria-hidden="true" />通道控制</button>
          </div>
        </div>
        <div class="live-video-wall" :class="`grid-${layout}`" aria-label="视频分屏">
          <section
            v-for="(channel, index) in wallChannels"
            :key="index"
            class="live-tile"
            :class="{ active: activeSlot === index }"
            :aria-label="`窗口 ${index + 1} · ${channel?.name || channel?.channel_id || '未选择通道'}`"
            @click.capture="selectSlot(index)"
          >
            <div class="live-tile-picture">
            <StreamPlayer
              v-if="channel"
              :key="channel.id"
              :result="playResults[channel.id]"
              :poster="snapshots[channel.id] || api.snapshotImage(channel.id)"
              :muted="muted"
              :autoplay="Boolean(playResults[channel.id])"
              :loading="previewLoadingId === channel.id"
              loading-text="正在开始预览…"
              idle-action="开始预览"
              :idle-hint="channel.is_online ? '点击开始预览，查看实时画面' : '通道离线，暂时无法预览'"
              :disabled="!channel.is_online || Boolean(actionLoading)"
              @request-play="startPlay(channel)"
              @retry="startPlay(channel)"
              @error="(message) => ui.toast(message)"
            />
            <div v-else class="live-empty-slot">
              <button class="channel-preview-start" type="button" :aria-label="`为窗口 ${index + 1} 选择通道`" @click="openResourcePicker"><Plus aria-hidden="true" /><span class="live-preview-label">选择通道</span></button>
              <small class="live-preview-hint">从通道资源添加画面</small>
            </div>
            <span v-if="channel" class="video-meta"
              ><span
                class="status"
                :class="channel.is_online ? 'online' : 'offline'"
                >{{ channel.is_online ? "在线" : "离线" }}</span
              ><span
                v-if="(channel.ext?.record_mode || 'none') !== 'none'"
                class="rec"
                >REC</span
              ></span
            >
            </div>
            <button class="live-tile-caption" type="button" :aria-pressed="activeSlot === index" :title="channel ? `${channel.name || '未命名通道'} · ${channel.channel_id || channel.id}` : '选择此窗口'" @click="selectSlot(index)">
              <span class="live-window-index">{{ index + 1 }}</span><span>{{ channel ? channel.name || channel.channel_id || channel.id : '未选择通道' }}</span>
            </button>
          </section>
        </div>
        <div class="video-foot channel-video-foot">
          <div class="stream-context">
            <strong>当前窗口 {{ activeSlot + 1 }}<template v-if="selectedChannel"> · {{ selectedChannel.name || selectedChannel.channel_id || selectedChannel.id }}</template></strong>
            <span>{{ selectedChannel ? playResults[selectedChannel.id] ? '可在播放器中调节音量或切换全屏' : '点击画面中的开始预览，查看实时视频' : '打开通道资源，为当前窗口选择通道' }}</span>
          </div>
          <button class="stream-link" type="button" :disabled="!selectedChannel?.is_online || Boolean(actionLoading)" @click="snapshot"><LoaderCircle v-if="actionLoading === '抓拍'" class="animate-spin" aria-hidden="true" /><Aperture v-else aria-hidden="true" />抓拍</button>
        </div>
        <details v-if="playResults[selectedChannel?.id || '']" :key="selectedChannel?.id" class="live-play-addresses">
          <summary>播放地址 <span>{{ playAddresses.length }} 个地址</span></summary>
          <div v-if="playAddresses.length" class="live-address-list">
            <a
              v-for="item in playAddresses"
              :key="item.url"
              :href="item.url"
              target="_blank"
              rel="noopener noreferrer"
              ><ExternalLink />{{ item.label }}</a
            >
          </div>
          <span
            v-else class="live-resource-empty"
            >后端未返回可打开地址</span
          >
        </details>
      </article>
      <aside v-if="controlsOpen && selectedChannel" id="live-controls" ref="controlsPanel" class="card live-controls" aria-label="当前通道控制">
        <div class="live-controls-heading"><div><h2 class="card-title">通道控制</h2><p class="card-sub">{{ selectedChannel.name || selectedChannel.channel_id || selectedChannel.id }}</p></div><button class="icon-btn" type="button" aria-label="收起通道控制" @click="closeControls"><X aria-hidden="true" /></button></div>
        <section class="control-panel">
          <div class="card-head">
            <div>
              <h2 class="card-title">云台控制</h2>
              <p class="card-sub">
                {{ ptzCapabilityLabel(selectedChannel) }}
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
        <section class="control-panel">
          <div class="card-head">
            <div>
              <h2 class="card-title">通道动作</h2>
              <p class="card-sub">作用于当前选中通道</p>
            </div>
          </div>
          <div class="control-row">
            <button
              class="btn"
              :disabled="!selectedChannel.is_online || Boolean(actionLoading)"
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
              @click="openVoiceControls"
            >
              <Mic />配置语音
            </button>
          </div>
          <label class="toggle-row"
            ><span
              ><strong>静音播放</strong
              ><small class="text-slate-500"
                >仅影响当前浏览器</small
              ></span
            ><span class="switch"
              ><input v-model="muted" type="checkbox" /><span
                class="slider" /></span></label
          ><label class="toggle-row"
            ><span
              ><strong>AI 分析</strong
              ><small class="text-slate-500"
                >使用通道已配置区域</small
              ></span
            ><span class="switch"
              ><input
                :checked="selectedChannel?.ext?.enabled_ai"
                :disabled="Boolean(actionLoading)"
                type="checkbox"
                @change="toggleAI" /><span class="slider" /></span></label
          ><label class="toggle-row"
            ><span
              ><strong>手动录像</strong
              ><small class="text-slate-500"
                >开启后录制，手动停止后结束</small
              ></span
            ><span class="switch"
              ><input
                :disabled="Boolean(actionLoading)"
                :checked="
                  (selectedChannel?.ext?.record_mode || 'none') !== 'none'
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
    <section v-if="captureChannel" ref="captureRegion" class="card form-section live-capture-region" aria-label="通道抓拍管理">
      <div class="card-head"><h2 class="card-title">{{ captureChannel.name || captureChannel.id }} · 抓拍与计划</h2><button class="btn btn-sm" type="button" @click="captureChannel = null"><X />收起</button></div>
      <SnapshotWorkspace ref="captureWorkspace" :channel-id="captureChannel.id" :disabled="!captureChannel.is_online" @captured="captureSaved" @changed="refreshCaptureCover" />
    </section>
  </main>
</template>

<style scoped>
.live-capture-region { margin-top: 20px; scroll-margin-top: 80px; }
.live-page .page-head { gap: 12px; }
.live-page .head-actions { gap: 8px; }
.live-monitor-layout { display: grid; grid-template-columns: 228px minmax(0, 1fr); align-items: start; gap: 12px; }
.live-monitor-layout.resources-hidden { grid-template-columns: minmax(0, 1fr); }
.live-monitor-layout.has-controls { grid-template-columns: 228px minmax(0, 1fr) 272px; }
.live-monitor-layout.resources-hidden.has-controls { grid-template-columns: minmax(0, 1fr) 272px; }
.resource-panel { display: flex; flex-direction: column; max-height: max(320px, calc(100dvh - 188px)); }
.live-page .icon-btn { flex-shrink: 0; }
.resource-panel > .card-head, .resource-panel > .field { flex-shrink: 0; }
.live-resource-heading { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: center; column-gap: 8px; row-gap: 4px; }
.live-resource-heading .card-title { min-width: 0; }
.live-resource-heading .card-sub { grid-column: 1 / -1; margin-top: 0; overflow-wrap: anywhere; }
.resource-tree { min-height: 0; overflow-y: auto; overscroll-behavior: contain; scrollbar-gutter: stable; scrollbar-color: var(--line-strong) transparent; }
.resource-channel-name { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.tree-item > svg, .tree-item > .slot-led { flex-shrink: 0; }
.tree-item { font-size: 12px; }
.live-resource-hint { margin: 12px 0 0; padding-top: 10px; color: var(--muted); border-top: 1px solid var(--line); font-size: 12px; }
.live-resource-empty { padding: 16px 8px; color: var(--muted); font-size: 12px; line-height: 1.6; }
.live-video-workspace { min-width: 0; }
.live-toolbar { gap: 12px; color: var(--ink); background: var(--panel); border-bottom: 1px solid var(--line); flex-wrap: wrap; }
.live-current-channel { flex: 1 1 160px; min-width: 0; }
.live-current-channel strong { overflow: hidden; color: var(--ink-strong); font-size: 14px; text-overflow: ellipsis; white-space: nowrap; }
.live-current-channel > span { margin-top: 3px; color: var(--muted); font-size: 12px; overflow-wrap: anywhere; }
.live-workspace-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.live-video-wall { display: grid; gap: 8px; padding: 8px; background: var(--panel-2); }
.live-video-wall.grid-1 { grid-template-columns: minmax(0, 1fr); }
.live-video-wall.grid-4 { grid-template-columns: repeat(2, minmax(0, 1fr)); }
.live-video-wall.grid-9 { grid-template-columns: repeat(3, minmax(0, 1fr)); }
.live-video-wall.grid-16 { grid-template-columns: repeat(4, minmax(0, 1fr)); }
.live-tile { min-width: 0; overflow: hidden; background: var(--panel); border: 1px solid var(--line); border-radius: var(--radius-sm); container-type: inline-size; container-name: live-tile; }
.live-tile.active { border-color: var(--blue); }
.live-tile:focus-within { outline: 2px solid var(--blue); outline-offset: 1px; }
.live-tile-picture { position: relative; aspect-ratio: 16 / 9; background: var(--panel-3); }
.live-empty-slot { position: absolute; inset: 0; display: grid; place-content: center; justify-items: center; gap: 8px; padding: 16px; color: var(--muted); }
.live-empty-slot small { font-size: 12px; }
.live-tile-caption { display: flex; align-items: center; gap: 8px; width: 100%; min-height: 36px; padding: 6px 10px; color: var(--ink); background: var(--panel); border: 0; border-top: 1px solid var(--line); text-align: left; font-size: 12px; cursor: pointer; }
.live-tile-caption > span:last-child { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.live-tile-caption:hover, .live-tile.active .live-tile-caption { color: var(--blue); background: var(--blue-soft); }
.live-window-index { flex-shrink: 0; color: var(--muted); font-variant-numeric: tabular-nums; }
.video-meta .status { font-size: 11px; }
.channel-video-foot { flex-wrap: wrap; }
.live-play-addresses { padding: 10px 13px; border-top: 1px solid var(--line); font-size: 12px; }
.live-play-addresses summary { color: var(--muted); cursor: pointer; }
.live-play-addresses summary > span { margin-left: 8px; }
.live-play-addresses summary:focus-visible { outline: 2px solid var(--blue); outline-offset: 3px; }
.live-address-list { display: flex; flex-wrap: wrap; gap: 8px 16px; padding-top: 12px; }
.live-address-list a { display: inline-flex; align-items: center; gap: 6px; color: var(--blue); text-decoration: underline; text-underline-offset: 3px; overflow-wrap: anywhere; }
.live-address-list svg { width: 14px; height: 14px; flex-shrink: 0; }
.live-controls { min-width: 0; overflow: hidden; }
.live-controls-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 14px; border-bottom: 1px solid var(--line); }
.live-controls-heading > div { min-width: 0; }
.live-controls-heading .card-sub { overflow-wrap: anywhere; }
.live-controls .control-panel + .control-panel { border-top: 1px solid var(--line); }
.live-controls .button-row { flex-wrap: wrap; }
@container live-tile (max-width: 240px) {
  .live-preview-hint, :deep(.stream-player-hint) { display: none; }
  .channel-preview-start { min-width: 44px; padding-inline: 10px; font-size: 12px; }
  .live-tile-caption { padding-inline: 8px; }
}
@container live-tile (max-width: 175px) {
  .live-preview-label { display: none; }
  .video-meta .status { padding: 2px 5px; font-size: 10px; }
}
@media (max-width: 1280px) {
  .live-monitor-layout.has-controls { grid-template-columns: 228px minmax(0, 1fr); }
  .live-monitor-layout.resources-hidden.has-controls { grid-template-columns: minmax(0, 1fr); }
  .live-controls { grid-column: 2; }
  .resources-hidden .live-controls { grid-column: 1; }
}
@media (max-width: 900px) {
  .live-monitor-layout, .live-monitor-layout.resources-hidden, .live-monitor-layout.has-controls, .live-monitor-layout.resources-hidden.has-controls { grid-template-columns: minmax(0, 1fr); }
  .live-controls { grid-column: 1; }
  .resource-panel { order: initial; max-height: 360px; }
  .live-workspace-actions .btn, .live-tile-caption, .live-page .icon-btn, .channel-video-foot .stream-link { min-height: 44px; }
  .live-video-wall.grid-9, .live-video-wall.grid-16 { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}
@media (max-width: 480px) {
  .live-video-wall { gap: 4px; padding: 4px; }
  .live-video-wall.grid-4 { grid-template-columns: minmax(0, 1fr); }
  .live-toolbar { padding: 12px; }
  .live-workspace-actions { width: 100%; }
  .live-workspace-actions .btn { flex: 1; justify-content: center; }
}
</style>
