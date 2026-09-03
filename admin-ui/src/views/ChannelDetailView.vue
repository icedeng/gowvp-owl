<script setup lang="ts">
import { computed, nextTick, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  Aperture,
  ArrowLeft,
  Camera,
  ChevronUp,
  Copy,
  Cpu,
  ExternalLink,
  Film,
  FileUp,
  Gauge,
  Link2,
  LoaderCircle,
  Mic,
  Pause,
  Play,
  Radio,
  RefreshCw,
  Save,
  Search,
  Server,
  ShieldAlert,
  Square,
  Video,
  Volume2,
  VolumeX,
} from "@lucide/vue";
import { api, collectPages, errorMessage, typeLabel } from "../services/api";
import type {
  ApiChannel,
  ApiDevice,
  ApiRecording,
  GBHistoryDownloadState,
  GBOperationOutput,
  GBSnapshotState,
  GBUpgradeState,
  MediaServer,
  PlayResult,
  Zone,
} from "../types/api";
import { formatBytes, formatDate } from "../utils/format";
import { useUiStore } from "../stores/ui";
import StreamPlayer from "../components/StreamPlayer.vue";
import type { StreamProtocol } from "../player";

const route = useRoute();
const router = useRouter();
const ui = useUiStore();
const loading = ref(false);
const actionLoading = ref("");
const loadError = ref("");
const channel = ref<ApiChannel | null>(null);
const device = ref<ApiDevice | null>(null);
const recordings = ref<ApiRecording[]>([]);
const recordingTotal = ref(0);
const zones = ref<Zone[]>([]);
const play = ref<PlayResult | null>(null);
const snapshotUrl = ref("");
const aiEnabled = ref(false);
const recordMode = ref("always");
const remoteRecords = ref<Record<string, unknown> | null>(null);
const gbQueryResult = ref<GBOperationOutput | null>(null);
const gbControlResult = ref<GBOperationOutput | null>(null);
const snapshotSessionId = ref("");
const snapshotState = ref<GBSnapshotState | null>(null);
const upgradeSessionId = ref("");
const upgradeState = ref<GBUpgradeState | null>(null);
const historyActive = ref(false);
const historyState = ref<GBHistoryDownloadState | null>(null);
const mediaServers = ref<MediaServer[]>([]);
const selectedMediaServerId = ref("local");
const resourceErrors = reactive({ device: "", recordings: "", zones: "", remote: "", history: "", media: "" });
const resourceLoading = reactive({ recordings: false, zones: false, remote: false, media: false });
const zoneError = ref("");
type ChannelTab = "live" | "ai" | "recordings" | "gb" | "technical";
const activeTab = ref<ChannelTab>("live");
const liveProtocolPriority: StreamProtocol[] = ["ws-flv", "http-flv", "webrtc", "hls"];
let loadSequence = 0;

const detailTabs = [
  { id: "live" as const, label: "实时值守", description: "预览与控制", icon: Video },
  { id: "ai" as const, label: "智能分析", description: "AI 与检测区域", icon: Cpu },
  { id: "recordings" as const, label: "录像与回看", description: "策略与录像目录", icon: Film },
  { id: "gb" as const, label: "国标扩展", description: "查询、控制与任务", icon: Radio },
  { id: "technical" as const, label: "通道档案", description: "协议与流信息", icon: Link2 },
];

const zoneForm = reactive({
  name: "",
  coordinates: "0.1,0.1,0.9,0.1,0.9,0.9,0.1,0.9",
  color: "#38bdf8",
  labels: "person,car",
});
const voiceForm = reactive({
  mode: "talk_standard" as "talk_standard" | "talk" | "broadcast",
  mediaServerId: "local",
  sourceVhost: "__defaultVhost__",
  sourceApp: "live",
  sourceStream: "",
});
const gbQueryForm = reactive({
  action: "device_status",
  configType: "basic_param",
  number: 0,
  startAt: "",
  endAt: "",
  filePath: "",
  address: "",
  secrecy: "",
  recordType: "",
  recorderId: "",
  indistinctQuery: "",
  streamNumber: "",
  startAlarmPriority: "",
  endAlarmPriority: "",
  alarmMethod: "",
  alarmType: "",
  startAlarmTime: "",
  endAlarmTime: "",
});
const gbControlForm = reactive({
  action: "teleboot",
  streamNumber: 0,
  alarmMethod: "",
  alarmType: "",
  ptzCmd: "stop",
  ptzSpeed: 40,
  ptzPreset: 1,
  ptzGroup: 1,
  ptzAux: 1,
  ptzValue: 40,
  homeEnabled: 1,
  homeResetTime: 60,
  homePresetIndex: 1,
  sdcardId: 0,
  pan: 0,
  tilt: 0,
  zoom: 1,
  frameLength: 1920,
  frameWidth: 1080,
  frameMidPointX: 960,
  frameMidPointY: 540,
  frameLengthX: 400,
  frameLengthY: 300,
  targetTrackMode: "Auto",
  targetTrackDeviceId2: "",
});
const upgradeForm = reactive({ firmware: "", fileUrl: "", manufacturer: "", sessionId: "" });
function localDateTimeInput(value: Date) {
  return new Date(value.getTime() - value.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
}
const historyNow = new Date();
const historyForm = reactive({
  mode: "playback" as "playback" | "download",
  startAt: localDateTimeInput(new Date(historyNow.getTime() - 60 * 60_000)),
  endAt: localDateTimeInput(historyNow),
  transport: "rtp" as "rtp" | "direct_tcp",
  downloadSpeed: 1,
  recordType: 3,
  controlAction: "pause" as "play" | "pause" | "speed" | "seek",
  scale: 2,
  seekAt: localDateTimeInput(new Date(historyNow.getTime() - 30 * 60_000)),
  speedAt: "",
});

const protocol = computed(() =>
  typeLabel(
    channel.value?.type,
    channel.value?.did || channel.value?.device_id || channel.value?.channel_id || channel.value?.id
  )
);
const isGb = computed(() => protocol.value === "GB28181");
const boundMediaServerId = computed(() => channel.value?.config?.media_server_id?.trim() || "local");
const boundMediaServer = computed(() => mediaServers.value.find((item) => item.id === boundMediaServerId.value));
const boundMediaServerState = computed(() => {
  if (resourceLoading.media) return { tone: "pending", label: "读取中" };
  if (resourceErrors.media || !boundMediaServer.value || boundMediaServer.value.status === undefined) {
    return { tone: "pending", label: "状态未知" };
  }
  return boundMediaServer.value.status
    ? { tone: "online", label: "在线" }
    : { tone: "offline", label: "离线" };
});
const mediaBindingChanged = computed(() => selectedMediaServerId.value !== boundMediaServerId.value);
const mediaBindingDisabledReason = computed(() => {
  if (!isGb.value) return "媒体节点绑定仅适用于 GB28181 通道";
  if (channel.value?.is_playing || historyActive.value) return "通道存在活动媒体会话，请先停止预览、回放、下载或语音会话";
  if (resourceLoading.media) return "正在读取媒体节点";
  if (resourceErrors.media) return resourceErrors.media;
  return "";
});
const supportsPtz = computed(() => Boolean(channel.value?.ptz_capable));
const canUseRealtime = computed(() => Boolean(channel.value?.is_online) && !actionLoading.value);
const gbCapabilityDeclaration = computed<string[] | undefined>(() => {
  const deviceCapabilities = device.value?.ext?.gb_version_capabilities;
  if (Array.isArray(deviceCapabilities)) return deviceCapabilities;
  const channelCapabilities = channel.value?.ext?.gb_version_capabilities;
  return Array.isArray(channelCapabilities) ? channelCapabilities : undefined;
});
const gbCapabilities = computed(() => gbCapabilityDeclaration.value || []);
const hasDeclaredGBCapabilities = computed(() => gbCapabilityDeclaration.value !== undefined);
const rawGbVersion = computed(() =>
  device.value?.ext?.gb_effective_version ||
  device.value?.ext?.gb_version ||
  channel.value?.ext?.gb_effective_version ||
  channel.value?.ext?.gb_version ||
  ""
);
const effectiveGbVersion = computed(() => {
  switch (String(rawGbVersion.value).trim()) {
    case "1.0":
    case "2011":
      return "1.0";
    case "1.1":
    case "2014":
    case "2011-supplement-2014":
      return "1.1";
    case "2.0":
    case "2016":
      return "2.0";
    case "3.0":
    case "2022":
      return "3.0";
    default:
      return "";
  }
});
const gbVersionLabel = computed(() => {
  const labels: Record<string, string> = {
    "1.0": "2011（1.0）",
    "1.1": "2014（1.1）",
    "2.0": "2016（2.0）",
    "3.0": "2022（3.0）",
  };
  return labels[effectiveGbVersion.value] || "版本待识别";
});
function hasGBCapability(name: string) {
  if (hasDeclaredGBCapabilities.value) return gbCapabilities.value.includes(name);
  const version = effectiveGbVersion.value;
  if (["snapshot", "upgrade", "home_position_query", "cruise_track_query", "ptz_position", "sdcard", "target_track"].includes(name)) {
    return version === "3.0";
  }
  if (["mobile_position", "home_position", "iframe_control"].includes(name)) {
    return ["2.0", "3.0"].includes(version);
  }
  if (["config_query", "preset_query", "drag_zoom_control", "download_speed"].includes(name)) {
    return ["1.1", "2.0", "3.0"].includes(version);
  }
  if (name === "direct_tcp_download") return version === "1.1";
  return false;
}
const supportsUpgrade = computed(() =>
  hasDeclaredGBCapabilities.value ? hasGBCapability("upgrade") : effectiveGbVersion.value === "3.0"
);
const supportsDeviceSnapshot = computed(() =>
  hasDeclaredGBCapabilities.value ? hasGBCapability("snapshot") : effectiveGbVersion.value === "3.0"
);
const queryCapability = computed(() => ({
  config_download: "config_query",
  preset_query: "preset_query",
  mobile_position: "mobile_position",
  home_position_query: "home_position_query",
  cruise_track_list: "cruise_track_query",
  cruise_track: "cruise_track_query",
  ptz_position: "ptz_position",
  sdcard_status: "sdcard",
} as Record<string, string>)[gbQueryForm.action] || "");
const alarmTypeQuerySupported = computed(() => ["2.0", "3.0"].includes(effectiveGbVersion.value));
const alarmQueryValidationMessage = computed(() => {
  if (gbQueryForm.action !== "alarm") return "";
  const startPriority = gbQueryForm.startAlarmPriority.trim();
  const endPriority = gbQueryForm.endAlarmPriority.trim();
  if (startPriority && !/^\d$/.test(startPriority)) return "起始报警级别必须为 0–4";
  if (endPriority && !/^\d$/.test(endPriority)) return "结束报警级别必须为 0–4";
  if ((startPriority && Number(startPriority) > 4) || (endPriority && Number(endPriority) > 4)) {
    return "报警级别必须为 0–4";
  }
  if (gbQueryForm.alarmType.trim() && !alarmTypeQuerySupported.value) {
    return "报警类型仅 GB/T 28181-2016/2022 支持";
  }
  const startTime = gbQueryForm.startAlarmTime.trim();
  const endTime = gbQueryForm.endAlarmTime.trim();
  const start = startTime ? Date.parse(startTime) : NaN;
  const end = endTime ? Date.parse(endTime) : NaN;
  if (startTime && Number.isNaN(start)) return "起始报警时间格式无效";
  if (endTime && Number.isNaN(end)) return "结束报警时间格式无效";
  if (!Number.isNaN(start) && !Number.isNaN(end) && end < start) return "起始报警时间不能晚于结束时间";
  if (startPriority && endPriority && Number(startPriority) > Number(endPriority)) {
    return "起始报警级别不能高于结束级别";
  }
  return "";
});
const alarmQueryValid = computed(() => !alarmQueryValidationMessage.value);
const recordIndistinctQuerySupported = computed(() => effectiveGbVersion.value !== "" && effectiveGbVersion.value !== "1.0");
const recordStreamNumberSupported = computed(() => effectiveGbVersion.value === "3.0");
function queryDateValue(value: string) {
  const trimmed = value.trim();
  if (!trimmed) return 0;
  const parsed = Date.parse(trimmed);
  return Number.isNaN(parsed) ? 0 : Math.floor(parsed / 1000);
}
const standardQueryValidationMessage = computed(() => {
  if (!["catalog", "record_info"].includes(gbQueryForm.action)) return "";
  const startText = gbQueryForm.startAt.trim();
  const endText = gbQueryForm.endAt.trim();
  const start = startText ? Date.parse(startText) : NaN;
  const end = endText ? Date.parse(endText) : NaN;
  if (startText && Number.isNaN(start)) return "起始时间格式无效";
  if (endText && Number.isNaN(end)) return "结束时间格式无效";
  if (gbQueryForm.action === "record_info" && ["2.0", "3.0"].includes(effectiveGbVersion.value) && (!startText || !endText)) {
    return "GB/T 28181-2016/2022 的录像查询必须填写起止时间";
  }
  if (!Number.isNaN(start) && !Number.isNaN(end) && end <= start) {
    return gbQueryForm.action === "record_info" ? "录像查询结束时间必须晚于起始时间" : "目录查询结束时间必须晚于起始时间";
  }
  if (gbQueryForm.action === "record_info" && gbQueryForm.indistinctQuery && !recordIndistinctQuerySupported.value) {
    return "模糊查询仅 GB/T 28181-2014 及以后版本支持";
  }
  if (gbQueryForm.action === "record_info" && gbQueryForm.streamNumber && !recordStreamNumberSupported.value) {
    return "码流编号仅 GB/T 28181-2022 支持";
  }
  if (gbQueryForm.action === "record_info" && gbQueryForm.alarmType.trim() && !alarmTypeQuerySupported.value) {
    return "录像报警类型仅 GB/T 28181-2016/2022 支持";
  }
  return "";
});
const queryValidationMessage = computed(() => alarmQueryValidationMessage.value || standardQueryValidationMessage.value);
const controlCapability = computed(() => ({
  iframe_send: "iframe_control",
  drag_zoom_in: "drag_zoom_control",
  drag_zoom_out: "drag_zoom_control",
  home_position: "home_position",
  ptz_precise: "ptz_position",
  format_sdcard: "sdcard",
  target_track: "target_track",
} as Record<string, string>)[gbControlForm.action] || "");
const queryAvailable = computed(() => (!queryCapability.value || hasGBCapability(queryCapability.value)) &&
  (gbQueryForm.action !== "config_download" || gbQueryForm.configType !== "snapshot" || hasGBCapability("snapshot")) &&
  !queryValidationMessage.value);
const queryUnavailableCapability = computed(() =>
  gbQueryForm.action === "config_download" && gbQueryForm.configType === "snapshot" && !hasGBCapability("snapshot")
    ? "snapshot"
    : queryCapability.value
);
const controlAvailable = computed(() => !controlCapability.value || hasGBCapability(controlCapability.value));
const supportsDirectDownload = computed(() => hasGBCapability("direct_tcp_download"));
const historyScaleValid = computed(() => {
  if (historyForm.controlAction !== "speed") return true;
  const scale = Number(historyForm.scale);
  return Number.isFinite(scale) && scale !== 0 && Math.abs(scale) >= 0.25 && Math.abs(scale) <= 16;
});
const supportsPositionedSpeed = computed(() => historyForm.controlAction === "speed" &&
  (effectiveGbVersion.value === "1.0" || effectiveGbVersion.value === "3.0" && Number(historyForm.scale) < 0));
const historyControlValid = computed(() => {
  if (!historyScaleValid.value) return false;
  if (historyForm.controlAction === "seek") return historyUnix(historyForm.seekAt) > 0;
  if (historyForm.controlAction === "speed" && supportsPositionedSpeed.value && historyForm.speedAt) {
    return historyUnix(historyForm.speedAt) > 0;
  }
  return true;
});
const historyDownloadSpeedValid = computed(() => {
  if (historyForm.mode !== "download" || !hasGBCapability("download_speed")) return true;
  const speed = Number(historyForm.downloadSpeed);
  return Number.isInteger(speed) && speed >= 1 && speed <= 16;
});
const historyStartAllowed = computed(() => historyDownloadSpeedValid.value &&
  (historyForm.mode !== "download" || historyForm.transport !== "direct_tcp" || supportsDirectDownload.value));
const historyProgress = computed(() => {
  const state = historyState.value;
  if (!state) return 0;
  if (state.progress_known && Number.isFinite(state.progress_percent)) return Math.min(100, Math.max(0, Number(state.progress_percent)));
  if (state.file_size_known && Number(state.file_size) > 0) return Math.min(100, Math.max(0, Number(state.received || 0) / Number(state.file_size) * 100));
  return 0;
});
const historyProgressKnown = computed(() => Boolean(historyState.value?.progress_known ||
  (historyState.value?.file_size_known && Number(historyState.value.file_size) > 0)));
const historyStatusLabel = computed(() => ({
  waiting_media: "等待媒体",
  connecting: "正在连接",
  receiving: "接收中",
  completed: "已完成",
  stopped: "已停止",
  cancelled: "已取消",
  failed: "失败",
} as Record<string, string>)[historyState.value?.status || ""] || (historyActive.value ? "会话中" : "未启动"));
const historyStatusClass = computed(() => {
  const status = historyState.value?.status;
  if (status === "completed") return "done";
  if (["failed", "cancelled"].includes(status || "")) return "danger";
  if (["waiting_media", "connecting", "receiving"].includes(status || "") || historyActive.value) return "pending";
  return "";
});
const supportsVoiceTalk = computed(() =>
  hasDeclaredGBCapabilities.value
    ? gbCapabilities.value.includes("voice_intercom")
    : ["1.0", "1.1", "2.0", "3.0"].includes(effectiveGbVersion.value)
);
const supportsStandardVoiceTalk = computed(() =>
  supportsVoiceTalk.value && supportsVoiceBroadcast.value && ["2.0", "3.0"].includes(effectiveGbVersion.value)
);
const supportsVoiceBroadcast = computed(() =>
  hasDeclaredGBCapabilities.value
    ? gbCapabilities.value.includes("voice_broadcast")
    : ["1.1", "2.0", "3.0"].includes(effectiveGbVersion.value)
);
const ptzDisabledReason = computed(() => {
  if (!channel.value?.is_online) return "通道离线，无法发送控制指令";
  if (!supportsPtz.value) return "当前设备未声明 PTZ 能力";
  return "";
});
const voiceDisabledReason = computed(() => {
  if (!isGb.value) return "语音对讲仅支持 GB28181 通道";
  if (!channel.value?.is_online) return "通道离线，无法建立语音会话";
  if (voiceForm.mode === "talk_standard" && !supportsStandardVoiceTalk.value) return "标准双流程语音对讲仅支持 2016/2022";
  if (voiceForm.mode === "talk" && !supportsVoiceTalk.value) return "当前协议版本不支持兼容语音对讲";
  if (voiceForm.mode === "broadcast" && !supportsVoiceBroadcast.value) return "当前协议版本不支持语音广播";
  if (!voiceForm.sourceStream.trim()) return "请填写已就绪的 ZLMediaKit G.711 A-law 音频源流 ID";
  return "";
});
const voiceStopDisabledReason = computed(() => {
  if (!isGb.value) return "语音会话仅支持 GB28181 通道";
  if (!channel.value?.is_online) return "通道离线，无法停止语音会话";
  return "";
});
const streamConfigRoute = computed(() => {
  if (!channel.value || !["RTMP", "RTSP"].includes(protocol.value)) return null;
  return {
    path: protocol.value === "RTMP" ? "/push-streams" : "/pull-streams",
    query: { channel: channel.value.id },
  };
});
const playAddresses = computed(() =>
  (play.value?.items || []).flatMap((item) =>
    Object.entries(item)
      .filter(([key, value]) => key !== "label" && typeof value === "string" && value)
      .map(([key, value]) => ({
        label: `${String(item.label || "")} ${key}`.trim(),
        url: String(value),
      }))
  )
);
const previousRoute = computed(() => {
  const back = window.history.state?.back;
  if (typeof back !== "string" || !back.startsWith("/") || back === route.fullPath) return null;
  const resolved = router.resolve(back);
  if (!resolved.matched.length || resolved.name === "login" || resolved.name === "channel-detail") return null;
  return resolved;
});
const backLabel = computed(() =>
  previousRoute.value?.meta.title ? `返回${String(previousRoute.value.meta.title)}` : "返回国标设备"
);

function goBack() {
  if (previousRoute.value) router.back();
  else router.push("/devices");
}

function selectTab(tab: ChannelTab) {
  activeTab.value = tab;
}

async function onTabKeydown(event: KeyboardEvent, index: number) {
  let nextIndex = index;
  if (event.key === "ArrowRight") nextIndex = (index + 1) % detailTabs.length;
  else if (event.key === "ArrowLeft") nextIndex = (index - 1 + detailTabs.length) % detailTabs.length;
  else if (event.key === "Home") nextIndex = 0;
  else if (event.key === "End") nextIndex = detailTabs.length - 1;
  else return;
  event.preventDefault();
  activeTab.value = detailTabs[nextIndex].id;
  await nextTick();
  document.getElementById(`channel-tab-${activeTab.value}`)?.focus();
}

function clearContext() {
  channel.value = null;
  device.value = null;
  recordings.value = [];
  recordingTotal.value = 0;
  zones.value = [];
  play.value = null;
  snapshotUrl.value = "";
  remoteRecords.value = null;
  gbQueryResult.value = null;
  gbControlResult.value = null;
  snapshotSessionId.value = "";
  snapshotState.value = null;
  upgradeSessionId.value = "";
  upgradeState.value = null;
  historyActive.value = false;
  historyState.value = null;
  mediaServers.value = [];
  selectedMediaServerId.value = "local";
  voiceForm.mediaServerId = "local";
  Object.assign(resourceErrors, { device: "", recordings: "", zones: "", remote: "", history: "", media: "" });
  activeTab.value = "live";
}

function mediaServerStatusLabel(status?: boolean) {
  if (status === true) return "在线";
  if (status === false) return "离线";
  return "状态未知";
}

async function loadMediaServers(sequence = loadSequence) {
  resourceLoading.media = true;
  resourceErrors.media = "";
  try {
    const response = await collectPages(api.mediaServers, {}, 1000);
    if (sequence === loadSequence) mediaServers.value = response.items;
  } catch (cause) {
    if (sequence === loadSequence) resourceErrors.media = errorMessage(cause, "媒体节点暂时无法读取");
  } finally {
    if (sequence === loadSequence) resourceLoading.media = false;
  }
}

async function loadRecordings(id: string, sequence = loadSequence) {
  resourceLoading.recordings = true;
  resourceErrors.recordings = "";
  try {
    const response = await api.recordings({ page: 1, size: 100, cid: id });
    if (sequence === loadSequence) {
      recordings.value = response.data?.items || [];
      recordingTotal.value = Number(response.data?.total ?? recordings.value.length);
    }
  } catch (cause) {
    if (sequence === loadSequence) resourceErrors.recordings = errorMessage(cause, "平台录像暂时无法读取");
  } finally {
    if (sequence === loadSequence) resourceLoading.recordings = false;
  }
}

async function loadZones(id: string, sequence = loadSequence) {
  resourceLoading.zones = true;
  resourceErrors.zones = "";
  try {
    const response = await api.zones(id);
    const data = response.data;
    if (sequence === loadSequence) zones.value = Array.isArray(data) ? data : data?.items || [];
  } catch (cause) {
    if (sequence === loadSequence) resourceErrors.zones = errorMessage(cause, "检测区域暂时无法读取");
  } finally {
    if (sequence === loadSequence) resourceLoading.zones = false;
  }
}

function applyHistoryState(state: GBHistoryDownloadState | null) {
  historyState.value = state;
  historyActive.value = Boolean(state && !["completed", "stopped", "cancelled", "failed"].includes(state.status));
  if (!state) return;
  historyForm.mode = "download";
  if (state.transport === "direct_tcp" || state.transport === "rtp") historyForm.transport = state.transport;
}

async function restoreHistoryState(id: string, sequence = loadSequence) {
  try {
    const response = await api.historyStatus(id);
    if (sequence === loadSequence) applyHistoryState(response.data || null);
  } catch (cause) {
    const status = Number((cause as { response?: { status?: number } })?.response?.status || 0);
    if (sequence === loadSequence && status !== 400 && status !== 404) {
      resourceErrors.history = errorMessage(cause, "历史下载状态暂时无法读取");
    }
  }
}

async function load() {
  const sequence = ++loadSequence;
  const id = String(route.params.id || "");
  clearContext();
  loading.value = true;
  loadError.value = "";
  try {
    const { data } = await api.channel(id);
    if (sequence !== loadSequence) return;
    if (!data?.id) throw new Error("通道不存在或响应格式错误");
    channel.value = data;
    selectedMediaServerId.value = data.config?.media_server_id?.trim() || "local";
    voiceForm.mediaServerId = selectedMediaServerId.value;
    aiEnabled.value = Boolean(data.ext?.enabled_ai);
    recordMode.value = data.ext?.record_mode || "always";

    void loadRecordings(data.id, sequence);
    void loadZones(data.id, sequence);
    void restoreHistoryState(data.id, sequence);
    void loadMediaServers(sequence);
    if (data.did) {
      api.device(data.did).then(
        (response) => {
          if (sequence === loadSequence) device.value = response.data || null;
        },
        (cause) => {
          if (sequence === loadSequence) resourceErrors.device = errorMessage(cause, "所属设备信息暂时无法读取");
        }
      );
    }
  } catch (cause) {
    if (sequence === loadSequence) loadError.value = errorMessage(cause, "通道数据暂不可用");
  } finally {
    if (sequence === loadSequence) loading.value = false;
  }
}

async function runAction(name: string, fn: () => Promise<unknown>) {
  if (!channel.value || actionLoading.value) return false;
  actionLoading.value = name;
  try {
    await fn();
    ui.toast(`${channel.value.name || channel.value.id} · ${name}成功`);
    return true;
  } catch (cause) {
    ui.toast(errorMessage(cause, `${name}失败`));
    return false;
  } finally {
    actionLoading.value = "";
  }
}

async function startPlay() {
  if (!channel.value || actionLoading.value) return;
  actionLoading.value = "开始预览";
  try {
    play.value = (await api.play(channel.value.id)).data || null;
    ui.toast("实时流已建立");
  } catch (cause) {
    ui.toast(errorMessage(cause, "实时流建立失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function saveMediaServerBinding() {
  if (!channel.value || mediaBindingDisabledReason.value || !mediaBindingChanged.value || actionLoading.value) return;
  const mediaServerId = selectedMediaServerId.value.trim() || "local";
  const succeeded = await runAction("保存媒体节点", () => api.bindChannelMediaServer(channel.value!.id, mediaServerId));
  if (!succeeded) return;
  channel.value = {
    ...channel.value,
    config: { ...channel.value.config, media_server_id: mediaServerId },
  };
  voiceForm.mediaServerId = mediaServerId;
  play.value = null;
}

async function copyPlayAddress(url: string) {
  try {
    await navigator.clipboard.writeText(url);
    ui.toast("播放地址已复制");
  } catch {
    ui.toast("复制失败，请检查浏览器剪贴板权限");
  }
}

async function refreshSnapshot() {
  if (!channel.value || actionLoading.value) return;
  actionLoading.value = "刷新快照";
  try {
    const { data } = await api.snapshot(channel.value.id);
    snapshotUrl.value = data?.link || api.snapshotImage(channel.value.id, Date.now());
    snapshotSessionId.value = data?.session_id || "";
    snapshotState.value = null;
    if (snapshotSessionId.value) await loadSnapshotState();
    ui.toast(`快照已刷新${data?.method ? ` · ${data.method}` : ""}`);
  } catch (cause) {
    ui.toast(errorMessage(cause, "快照刷新失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function loadSnapshotState() {
  if (!channel.value || !snapshotSessionId.value) return;
  try {
    snapshotState.value = (await api.snapshotState(channel.value.id, snapshotSessionId.value)).data || null;
  } catch (cause) {
    ui.toast(errorMessage(cause, "抓拍状态查询失败"));
  }
}

async function runGBQuery() {
  if (!device.value || !channel.value || actionLoading.value) return;
  if (!queryAvailable.value) {
    ui.toast(queryValidationMessage.value || "当前协议能力不支持该查询");
    return;
  }
  actionLoading.value = "国标查询";
  try {
    const body: Record<string, unknown> = {
      action: gbQueryForm.action,
      target_id: channel.value.channel_id || channel.value.id,
      timeout: 8,
    };
    if (gbQueryForm.action === "config_download") body.config_type = gbQueryForm.configType;
    if (gbQueryForm.action === "cruise_track") body.number = gbQueryForm.number;
    if (["catalog", "record_info"].includes(gbQueryForm.action)) {
      const start = queryDateValue(gbQueryForm.startAt);
      const end = queryDateValue(gbQueryForm.endAt);
      if (start > 0) body.start = start;
      if (end > 0) body.end = end;
    }
    if (gbQueryForm.action === "record_info") {
      if (gbQueryForm.filePath.trim()) body.file_path = gbQueryForm.filePath.trim();
      if (gbQueryForm.address.trim()) body.address = gbQueryForm.address.trim();
      if (gbQueryForm.secrecy !== "") body.secrecy = Number(gbQueryForm.secrecy);
      if (gbQueryForm.recordType !== "") body.type = gbQueryForm.recordType;
      if (gbQueryForm.recorderId.trim()) body.recorder_id = gbQueryForm.recorderId.trim();
      if (gbQueryForm.indistinctQuery !== "" && recordIndistinctQuerySupported.value) body.indistinct_query = Number(gbQueryForm.indistinctQuery);
      if (gbQueryForm.streamNumber !== "" && recordStreamNumberSupported.value) body.stream_number = Number(gbQueryForm.streamNumber);
      if (gbQueryForm.alarmMethod.trim()) body.alarm_method = gbQueryForm.alarmMethod.trim();
      if (gbQueryForm.alarmType.trim() && alarmTypeQuerySupported.value) body.alarm_type = gbQueryForm.alarmType.trim();
    }
    if (gbQueryForm.action === "alarm") {
      body.start_alarm_priority = gbQueryForm.startAlarmPriority.trim();
      body.end_alarm_priority = gbQueryForm.endAlarmPriority.trim();
      body.alarm_method = gbQueryForm.alarmMethod.trim();
      body.alarm_type = alarmTypeQuerySupported.value ? gbQueryForm.alarmType.trim() : "";
      body.start_alarm_time = gbQueryForm.startAlarmTime.trim();
      body.end_alarm_time = gbQueryForm.endAlarmTime.trim();
    }
    gbQueryResult.value = (await api.gbQuery(device.value.id, body)).data || null;
    ui.toast("国标设备查询完成");
  } catch (cause) {
    ui.toast(errorMessage(cause, "国标设备查询失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function runGBControl() {
  if (!device.value || !channel.value || actionLoading.value) return;
  actionLoading.value = "国标控制";
  try {
    const body: Record<string, unknown> = {
      action: gbControlForm.action,
      target_id: channel.value.channel_id || channel.value.id,
      timeout: 8,
    };
    if (gbControlForm.action === "ptz_cmd") {
      body.ptz_cmd = gbControlForm.ptzCmd;
      body.ptz_speed = gbControlForm.ptzSpeed;
      body.ptz_preset = gbControlForm.ptzPreset;
      body.ptz_group = gbControlForm.ptzGroup;
      body.ptz_aux = gbControlForm.ptzAux;
      body.ptz_value = gbControlForm.ptzValue;
    }
    if (["record_start", "record_stop"].includes(gbControlForm.action)) {
      body.stream_number = gbControlForm.streamNumber;
    }
    if (gbControlForm.action === "alarm_reset") {
      if (gbControlForm.alarmMethod.trim()) body.alarm_method = gbControlForm.alarmMethod;
      if (gbControlForm.alarmType.trim()) body.alarm_type = gbControlForm.alarmType;
    }
    const area = {
      length: gbControlForm.frameLength,
      width: gbControlForm.frameWidth,
      mid_point_x: gbControlForm.frameMidPointX,
      mid_point_y: gbControlForm.frameMidPointY,
      length_x: gbControlForm.frameLengthX,
      length_y: gbControlForm.frameLengthY,
    };
    if (["drag_zoom_in", "drag_zoom_out"].includes(gbControlForm.action)) body.drag_zoom = area;
    if (gbControlForm.action === "home_position") {
      body.home_position = gbControlForm.homeEnabled === 0
        ? { enabled: 0 }
        : { enabled: 1, reset_time: gbControlForm.homeResetTime, preset_index: gbControlForm.homePresetIndex };
    }
    if (gbControlForm.action === "format_sdcard") body.sdcard_id = gbControlForm.sdcardId;
    if (gbControlForm.action === "ptz_precise") {
      body.ptz_precise = { pan: gbControlForm.pan, tilt: gbControlForm.tilt, zoom: gbControlForm.zoom };
    }
    if (gbControlForm.action === "target_track") {
      body.target_track = {
        mode: gbControlForm.targetTrackMode,
        ...(gbControlForm.targetTrackDeviceId2.trim() ? { device_id2: gbControlForm.targetTrackDeviceId2.trim() } : {}),
        ...(gbControlForm.targetTrackMode === "Manual" ? { target_area: area } : {}),
      };
    }
    gbControlResult.value = (await api.gbControl(device.value.id, body)).data || null;
    ui.toast("国标设备控制已下发");
  } catch (cause) {
    ui.toast(errorMessage(cause, "国标设备控制失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function startUpgrade() {
  if (!channel.value || actionLoading.value) return;
  if (!upgradeForm.firmware.trim() || !upgradeForm.fileUrl.trim() || !upgradeForm.manufacturer.trim()) {
    ui.toast("请填写固件版本、下载地址和厂商");
    return;
  }
  actionLoading.value = "设备升级";
  try {
    const { data } = await api.upgradeDevice(channel.value.id, {
      firmware: upgradeForm.firmware,
      file_url: upgradeForm.fileUrl,
      manufacturer: upgradeForm.manufacturer,
      session_id: upgradeForm.sessionId,
      timeout: 10,
    });
    upgradeSessionId.value = data?.session_id || "";
    upgradeState.value = upgradeSessionId.value ? (await api.upgradeDeviceState(channel.value.id, upgradeSessionId.value)).data : null;
    ui.toast("升级命令已被设备受理；请继续查询最终状态");
  } catch (cause) {
    ui.toast(errorMessage(cause, "设备升级命令失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function loadUpgradeState() {
  if (!channel.value || !upgradeSessionId.value || actionLoading.value) return;
  actionLoading.value = "升级状态";
  try {
    upgradeState.value = (await api.upgradeDeviceState(channel.value.id, upgradeSessionId.value)).data || null;
  } catch (cause) {
    ui.toast(errorMessage(cause, "升级状态查询失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function toggleAI() {
  if (!channel.value) return;
  const next = !aiEnabled.value;
  const succeeded = await runAction(next ? "启用 AI" : "停用 AI", () =>
    next ? api.enableAI(channel.value!.id) : api.disableAI(channel.value!.id)
  );
  if (succeeded) aiEnabled.value = next;
}

async function saveRecordMode() {
  if (!channel.value) return;
  await runAction("保存录像模式", () => api.recordMode(channel.value!.id, recordMode.value));
}

async function addZone() {
  if (!channel.value) return;
  zoneError.value = "";
  if (!zoneForm.name.trim()) {
    zoneError.value = "请输入区域名称";
    return;
  }
  const rawCoordinates = zoneForm.coordinates.split(",").map((item) => item.trim());
  const coordinates = rawCoordinates.map(Number);
  if (coordinates.length < 6 || coordinates.length % 2 || coordinates.some((value) => !Number.isFinite(value))) {
    zoneError.value = "区域坐标需要至少 3 个点，格式为 x1,y1,x2,y2…";
    return;
  }
  const succeeded = await runAction("新增检测区域", () =>
    api.addZone(channel.value!.id, {
      name: zoneForm.name.trim(),
      coordinates,
      color: zoneForm.color,
      labels: zoneForm.labels.split(",").map((item) => item.trim()).filter(Boolean),
    })
  );
  if (succeeded) {
    zoneForm.name = "";
    await loadZones(channel.value.id);
  }
}

async function queryRemoteRecords() {
  if (!channel.value || actionLoading.value) return;
  const end = Math.floor(Date.now() / 1000);
  resourceLoading.remote = true;
  resourceErrors.remote = "";
  actionLoading.value = "查询设备录像";
  try {
    remoteRecords.value = (await api.queryDeviceRecords(channel.value.id, {
      start_at: end - 86400,
      end_at: end,
      timeout: 10,
    })).data || {};
    ui.toast("设备端录像目录查询完成");
  } catch (cause) {
    resourceErrors.remote = errorMessage(cause, "设备录像查询失败");
  } finally {
    resourceLoading.remote = false;
    actionLoading.value = "";
  }
}

function historyUnix(value: string) {
  const time = new Date(value).getTime();
  return Number.isFinite(time) ? Math.floor(time / 1000) : 0;
}

async function startHistorySession() {
  if (!channel.value || actionLoading.value) return;
  const startAt = historyUnix(historyForm.startAt);
  const endAt = historyUnix(historyForm.endAt);
  if (!startAt || !endAt || startAt >= endAt) {
    ui.toast("历史会话时间范围无效，请确认开始时间早于结束时间");
    return;
  }
  if (historyForm.mode === "download" && historyForm.transport === "direct_tcp" && !supportsDirectDownload.value) {
    ui.toast("当前设备档案不支持 2014 附录 O 裸 TCP 下载");
    return;
  }
  if (!historyDownloadSpeedValid.value) {
    ui.toast("下载倍速必须是 1–16 的整数");
    return;
  }
  const downloadSpeed = hasGBCapability("download_speed") ? Number(historyForm.downloadSpeed) : 1;
  actionLoading.value = "启动历史会话";
  try {
    const { data } = await api.historyStart(channel.value.id, {
      mode: historyForm.mode,
      start_at: startAt,
      end_at: endAt,
      transport: historyForm.mode === "download" ? historyForm.transport : "rtp",
      download_speed: historyForm.mode === "download" ? downloadSpeed : 0,
      record_type: historyForm.recordType,
    });
    historyActive.value = true;
    historyState.value = null;
    if (data?.download) applyHistoryState({ ...data.download, transport: historyForm.transport });
    ui.toast(historyForm.mode === "download" ? "下载会话已启动" : "历史回放会话已启动");
  } catch (cause) {
    ui.toast(errorMessage(cause, "历史会话启动失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function refreshHistoryState() {
  if (!channel.value || historyForm.mode !== "download" || actionLoading.value) return;
  actionLoading.value = "刷新下载状态";
  try {
    const response = historyForm.transport === "direct_tcp" && historyState.value?.session_id
      ? await api.directDownloadState(historyState.value.session_id)
      : await api.historyStatus(channel.value.id);
    applyHistoryState(response.data || null);
  } catch (cause) {
    ui.toast(errorMessage(cause, "下载状态查询失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function stopHistorySession() {
  if (!channel.value || actionLoading.value) return;
  actionLoading.value = historyForm.transport === "direct_tcp" ? "取消下载" : "停止历史会话";
  try {
    if (historyForm.mode === "download" && historyForm.transport === "direct_tcp" && historyState.value?.session_id) {
      await api.cancelDirectDownload(historyState.value.session_id);
    } else {
      await api.historyStop(channel.value.id, { mode: historyForm.mode });
    }
    historyActive.value = false;
    if (historyState.value) {
      historyState.value = {
        ...historyState.value,
        status: historyForm.transport === "direct_tcp" ? "cancelled" : "stopped",
        updated_at: new Date().toISOString(),
      };
    }
    ui.toast(historyForm.mode === "download" ? "下载会话已停止" : "历史回放会话已停止");
  } catch (cause) {
    ui.toast(errorMessage(cause, "历史会话停止失败"));
  } finally {
    actionLoading.value = "";
  }
}

async function controlHistorySession() {
  if (!channel.value || !historyActive.value || actionLoading.value) return;
  if (!historyControlValid.value) {
    ui.toast(historyForm.controlAction === "speed" ? "播放倍率不能为 0，绝对值范围应为 0.25–16" : "历史控制时间无效");
    return;
  }
  actionLoading.value = "控制历史回放";
  try {
    await api.historyControl(channel.value.id, {
      mode: historyForm.mode,
      action: historyForm.controlAction,
      ...(historyForm.controlAction === "speed" ? { scale: historyForm.scale } : {}),
      ...(supportsPositionedSpeed.value && historyForm.speedAt ? { seek_at: historyUnix(historyForm.speedAt) } : {}),
      ...(historyForm.controlAction === "seek" ? { seek_at: historyUnix(historyForm.seekAt) } : {}),
    });
    ui.toast("历史回放控制已下发");
  } catch (cause) {
    ui.toast(errorMessage(cause, "历史回放控制失败"));
  } finally {
    actionLoading.value = "";
  }
}

watch(() => historyForm.mode, (mode) => {
  if (mode === "playback") historyForm.transport = "rtp";
});

watch(() => route.params.id, load, { immediate: true });
watch([supportsVoiceTalk, supportsStandardVoiceTalk, supportsVoiceBroadcast], ([talk, standard, broadcast]) => {
  if (voiceForm.mode === "talk_standard" && !standard) voiceForm.mode = talk ? "talk" : "broadcast";
  else if (voiceForm.mode === "talk" && !talk && broadcast) voiceForm.mode = "broadcast";
  else if (voiceForm.mode === "broadcast" && !broadcast && talk) voiceForm.mode = standard ? "talk_standard" : "talk";
});
</script>

<template>
  <main class="page-content channel-detail-page" :aria-busy="loading">
    <section v-if="loadError && !channel" class="channel-core-error card" role="alert">
      <ShieldAlert />
      <div>
        <h1>通道数据暂不可用</h1>
        <p>{{ loadError }}</p>
      </div>
      <div class="head-actions">
        <button class="btn" type="button" @click="goBack"><ArrowLeft />{{ backLabel }}</button>
        <button class="btn btn-primary" type="button" @click="load"><RefreshCw />重试</button>
      </div>
    </section>

    <div v-else-if="loading && !channel" class="card empty-state" role="status" aria-live="polite">
      <LoaderCircle class="mx-auto mb-3 animate-spin" />正在加载通道详情…
    </div>

    <template v-if="channel">
      <section class="device-command-header channel-command-header">
        <div class="device-command-identity">
          <button class="device-command-back" type="button" :aria-label="backLabel" :title="backLabel" @click="goBack">
            <ArrowLeft />
          </button>
          <span class="device-command-icon"><Radio /></span>
          <div class="min-w-0">
            <div class="device-command-title">
              <h1>{{ channel.name || "未命名通道" }}</h1>
              <span class="status" :class="channel.is_online ? 'online' : 'offline'">{{ channel.is_online ? "在线" : "离线" }}</span>
              <span class="status" :class="channel.is_playing ? 'online' : ''">{{ channel.is_playing ? "LIVE" : "空闲" }}</span>
              <span class="protocol-tag blue">{{ protocol }}</span>
              <span v-if="isGb" class="protocol-tag blue" :title="`有效版本：${gbVersionLabel}`">{{ gbVersionLabel }}</span>
              <span v-if="isGb" class="protocol-tag"><Server />{{ boundMediaServerId }}</span>
            </div>
            <div class="device-command-meta">
              <span class="mono">{{ channel.channel_id || channel.id }}</span>
              <RouterLink v-if="channel.did" :to="`/devices/${encodeURIComponent(channel.did)}`">
                <Camera />{{ device?.name || channel.device_id || "所属设备" }}
              </RouterLink>
              <span v-else><Camera />{{ device?.name || channel.device_id || "未关联设备" }}</span>
              <small v-if="resourceErrors.device">所属设备信息未加载</small>
            </div>
          </div>
        </div>
        <div v-if="streamConfigRoute" class="head-actions"><RouterLink class="btn" :to="streamConfigRoute">编辑流配置</RouterLink></div>
      </section>

      <div class="channel-detail-tabs" role="tablist" aria-label="通道详情功能">
        <button
          v-for="(item, index) in detailTabs"
          :id="`channel-tab-${item.id}`"
          :key="item.id"
          type="button"
          role="tab"
          :class="{ active: activeTab === item.id }"
          :aria-selected="activeTab === item.id"
          :aria-controls="`channel-panel-${item.id}`"
          :tabindex="activeTab === item.id ? 0 : -1"
          @click="selectTab(item.id)"
          @keydown="onTabKeydown($event, index)"
        >
          <span class="channel-tab-icon"><component :is="item.icon" /></span>
          <span><strong>{{ item.label }}</strong><small>{{ item.description }}</small></span>
        </button>
      </div>

      <section v-if="activeTab === 'live'" id="channel-panel-live" class="channel-detail-section" role="tabpanel" aria-labelledby="channel-tab-live" tabindex="0">
        <div class="detail-section-head">
          <div><h2>实时值守</h2><p>在同一工作区完成预览、云台控制、快照和语音对讲。</p></div>
        </div>
        <div class="channel-live-grid">
          <article class="card video-workspace">
            <div class="video-tile active !border-0 !aspect-video">
              <StreamPlayer :result="play" :poster="snapshotUrl" :autoplay="Boolean(play)" :protocol-priority="liveProtocolPriority" @error="(message) => ui.toast(message)" />
              <span class="video-meta"><span class="status" :class="channel.is_online ? 'online' : 'offline'">{{ channel.is_online ? "在线" : "离线" }}</span><span v-if="recordMode !== 'none'" class="rec">REC</span></span>
            </div>
            <div class="video-foot channel-video-foot">
              <div class="stream-context">
                <strong>{{ playAddresses.length ? "实时流已建立" : channel.is_online ? "尚未开始预览" : "通道离线，暂时无法预览" }}</strong>
                <span>{{ playAddresses.length ? `${playAddresses.length} 个播放地址可用于联调测试` : "开始预览后将显示可用协议地址" }}</span>
              </div>
              <div class="stream-actions">
                <button class="stream-link stream-primary" type="button" :disabled="!channel.is_online || Boolean(actionLoading)" @click="startPlay"><LoaderCircle v-if="actionLoading === '开始预览'" class="animate-spin" /><Video v-else />{{ play ? "重新预览" : "开始预览" }}</button>
                <button class="stream-link" type="button" :disabled="!canUseRealtime" @click="refreshSnapshot"><Aperture />抓拍</button>
              </div>
            </div>
          </article>

          <section class="card channel-stream-addresses" aria-labelledby="channel-stream-address-title">
            <div class="card-head">
              <div><h3 id="channel-stream-address-title" class="card-title">播放地址</h3><p class="card-sub">完整地址可复制到 VLC、FFplay 或接口测试工具</p></div>
              <span class="protocol-tag blue">{{ playAddresses.length }} 个</span>
            </div>
            <div v-if="playAddresses.length" class="channel-stream-address-list">
              <div v-for="item in playAddresses" :key="item.url" class="channel-stream-address-row">
                <span class="protocol-tag">{{ item.label }}</span>
                <code>{{ item.url }}</code>
                <div class="channel-stream-address-actions">
                  <a class="device-row-action" :href="item.url" target="_blank" rel="noreferrer" :aria-label="`打开 ${item.label} 地址`" title="打开测试"><ExternalLink /></a>
                  <button class="device-row-action" type="button" :aria-label="`复制 ${item.label} 地址`" title="复制地址" @click="copyPlayAddress(item.url)"><Copy /></button>
                </div>
              </div>
            </div>
            <div v-else class="empty-state compact">开始预览后显示后端返回的完整播放地址。</div>
          </section>

          <aside class="control-stack channel-control-stack">
            <article class="card control-panel">
              <div class="card-head"><div><h3 class="card-title">云台控制</h3><p class="card-sub">方向控制与能力探测</p></div><span class="status" :class="supportsPtz ? 'online' : ''">{{ supportsPtz ? "可用" : "不可用" }}</span></div>
              <div class="ptz" :aria-disabled="Boolean(ptzDisabledReason)">
                <button class="up" type="button" aria-label="云台向上" :disabled="Boolean(ptzDisabledReason) || Boolean(actionLoading)" @click="runAction('云台向上', () => api.ptz(channel!.id, { action: 'up', speed: 30, timeout: 1 }))"><ChevronUp /></button>
                <button class="right" type="button" aria-label="云台向右" :disabled="Boolean(ptzDisabledReason) || Boolean(actionLoading)" @click="runAction('云台向右', () => api.ptz(channel!.id, { action: 'right', speed: 30, timeout: 1 }))"><ChevronUp /></button>
                <button class="down" type="button" aria-label="云台向下" :disabled="Boolean(ptzDisabledReason) || Boolean(actionLoading)" @click="runAction('云台向下', () => api.ptz(channel!.id, { action: 'down', speed: 30, timeout: 1 }))"><ChevronUp /></button>
                <button class="left" type="button" aria-label="云台向左" :disabled="Boolean(ptzDisabledReason) || Boolean(actionLoading)" @click="runAction('云台向左', () => api.ptz(channel!.id, { action: 'left', speed: 30, timeout: 1 }))"><ChevronUp /></button>
                <button class="center" type="button" aria-label="停止云台" :disabled="Boolean(ptzDisabledReason) || Boolean(actionLoading)" @click="runAction('停止云台', () => api.ptz(channel!.id, { action: 'stop', speed: 30, timeout: 1 }))"><Square /></button>
              </div>
              <p class="capability-reason">{{ ptzDisabledReason || (channel.ptz_verified ? "能力已通过实际命令验证" : "能力来自设备静态声明") }}</p>
              <button class="btn w-full" type="button" :disabled="!channel.is_online || Boolean(actionLoading)" @click="runAction('PTZ 能力探测', () => api.ptzProbe(channel!.id, { action: 'stop', speed: 30, timeout: 5 }))"><RefreshCw />重新探测</button>
            </article>

            <article class="card control-panel">
              <div class="card-head"><div><h3 class="card-title">快捷操作</h3><p class="card-sub">保持当前画面持续可见</p></div><Mic /></div>
              <div class="form-grid">
                <label class="form-group"><span class="form-label">语音模式</span><select v-model="voiceForm.mode" class="input plain w-full"><option value="talk_standard" :disabled="!supportsStandardVoiceTalk">标准对讲（2016/2022）</option><option value="talk" :disabled="!supportsVoiceTalk">兼容对讲（2011+ 单 INVITE）</option><option value="broadcast" :disabled="!supportsVoiceBroadcast">语音广播（2014+）</option></select></label>
                <label class="form-group"><span class="form-label">音频源节点</span><select v-model="voiceForm.mediaServerId" class="select w-full" :disabled="resourceLoading.media"><option v-if="!mediaServers.some((item) => item.id === voiceForm.mediaServerId)" :value="voiceForm.mediaServerId">{{ voiceForm.mediaServerId }} · 当前绑定</option><option v-for="item in mediaServers" :key="item.id" :value="item.id">{{ item.id }} · {{ mediaServerStatusLabel(item.status) }}</option></select></label>
                <label class="form-group"><span class="form-label">G.711 音频源流</span><input v-model.trim="voiceForm.sourceStream" class="input plain w-full" placeholder="例如 voice-microphone-1" /></label>
              </div>
              <div class="channel-quick-actions">
                <button class="btn" type="button" :disabled="!canUseRealtime" @click="refreshSnapshot"><Aperture />刷新快照</button>
                <button class="btn" type="button" :disabled="Boolean(voiceDisabledReason) || Boolean(actionLoading)" @click="runAction(voiceForm.mode === 'broadcast' ? '开始广播' : '开始对讲', () => api.voiceStart(channel!.id, { mode: voiceForm.mode, media_server_id: voiceForm.mediaServerId, source_vhost: voiceForm.sourceVhost, source_app: voiceForm.sourceApp, source_stream: voiceForm.sourceStream }))"><Volume2 />{{ voiceForm.mode === "broadcast" ? "开始广播" : "开始对讲" }}</button>
                <button class="btn" type="button" :disabled="Boolean(voiceStopDisabledReason) || Boolean(actionLoading)" @click="runAction(voiceForm.mode === 'broadcast' ? '停止广播' : '停止对讲', () => api.voiceStop(channel!.id, { mode: voiceForm.mode }))"><VolumeX />{{ voiceForm.mode === "broadcast" ? "停止广播" : "停止对讲" }}</button>
              </div>
              <p class="capability-reason">{{ voiceDisabledReason || (voiceForm.mode === "talk_standard" ? `标准对讲会组合实时点播上行与 ${voiceForm.mediaServerId} 节点的语音广播下行` : voiceForm.mode === "talk" ? `兼容模式从 ${voiceForm.mediaServerId} 节点发送厂商单个双向 INVITE` : `音频源将由 ${voiceForm.mediaServerId} 节点转为国标 RTP 发送`) }}</p>
            </article>
          </aside>
        </div>
      </section>

      <section v-else-if="activeTab === 'ai'" id="channel-panel-ai" class="channel-detail-section" role="tabpanel" aria-labelledby="channel-tab-ai" tabindex="0">
        <div class="detail-section-head"><div><h2>智能分析</h2><p>管理通道 AI 状态与实际生效的检测区域。</p></div></div>
        <div class="channel-ai-grid">
          <article class="card form-section channel-ai-summary">
            <div class="card-head"><div><h3 class="card-title">AI 分析状态</h3><p class="card-sub">启用前请确认已配置检测区域</p></div><span class="status" :class="aiEnabled ? 'online' : ''">{{ aiEnabled ? "运行中" : "未启用" }}</span></div>
            <div class="channel-ai-state"><Cpu /><div><strong>{{ zones[0]?.name || "尚未配置检测区域" }}</strong><span>{{ zones[0] ? `${zones[0].labels?.join('、') || '默认标签'} · ${zones[0].coordinates.length / 2} 个点` : "新增区域后，第一个区域将作为当前生效区域" }}</span></div></div>
            <button class="btn btn-primary w-full" type="button" :disabled="Boolean(actionLoading) || (!aiEnabled && !zones.length)" @click="toggleAI">{{ aiEnabled ? "停用 AI" : "启用 AI" }}</button>
          </article>

          <article class="card form-section">
            <div class="card-head"><div><h3 class="card-title">检测区域</h3><p class="card-sub">当前 {{ zones.length }} 个，后端运行逻辑读取第一个区域</p></div><span class="protocol-tag amber">单区域生效</span></div>
            <div v-if="resourceErrors.zones" class="inline-resource-error" role="alert"><span>{{ resourceErrors.zones }}</span><button class="btn btn-sm" type="button" @click="loadZones(channel.id)">重试</button></div>
            <div v-else-if="resourceLoading.zones" class="empty-state compact" role="status"><LoaderCircle class="animate-spin" />正在读取检测区域…</div>
            <div v-else class="zone-list">
              <div v-for="(zone, index) in zones" :key="`${zone.name}-${index}`" class="read-only zone-row"><span class="zone-swatch" :style="{ background: zone.color || '#38bdf8' }" /><div><strong>{{ zone.name }}</strong><small>{{ zone.labels?.join("、") || "默认标签" }} · {{ zone.coordinates.length / 2 }} 个点</small></div><span class="protocol-tag" :class="index === 0 ? 'blue' : ''">{{ index === 0 ? "当前生效" : "备用" }}</span></div>
              <div v-if="!zones.length" class="empty-state compact">尚未配置检测区域</div>
            </div>
            <form class="channel-zone-form" @submit.prevent="addZone">
              <div class="form-grid">
                <label class="form-group"><span class="form-label">区域名称</span><input v-model="zoneForm.name" class="input plain w-full" :aria-invalid="Boolean(zoneError)" aria-describedby="zone-error" /></label>
                <label class="form-group"><span class="form-label">检测标签</span><input v-model="zoneForm.labels" class="input plain w-full" /></label>
                <label class="form-group full"><span class="form-label">归一化坐标（逗号分隔）</span><textarea v-model="zoneForm.coordinates" class="textarea mono" :aria-invalid="Boolean(zoneError)" aria-describedby="zone-error" /></label>
              </div>
              <p v-if="zoneError" id="zone-error" class="field-error" role="alert">{{ zoneError }}</p>
              <div class="settings-savebar"><span>API 当前支持查询与新增，不提供编辑或删除。</span><button class="btn btn-primary" type="submit" :disabled="Boolean(actionLoading)"><Save />新增区域</button></div>
            </form>
          </article>
        </div>
      </section>

      <section v-else-if="activeTab === 'recordings'" id="channel-panel-recordings" class="channel-detail-section" role="tabpanel" aria-labelledby="channel-tab-recordings" tabindex="0">
        <div class="detail-section-head"><div><h2>录像与回看</h2><p>集中管理平台录像策略、录像片段和 GB28181 设备端录像。</p></div><RouterLink class="btn btn-primary" :to="`/recordings?channel=${encodeURIComponent(channel.id)}`"><Film />进入录像中心</RouterLink></div>
        <div class="channel-recording-summary">
          <div><small>当前策略</small><strong>{{ recordMode === "always" ? "持续录像" : "不录制" }}</strong></div>
          <div><small>平台片段</small><strong>{{ resourceErrors.recordings ? "—" : recordingTotal }}</strong></div>
          <div><small>最近录像</small><strong>{{ resourceErrors.recordings ? "读取失败" : formatDate(recordings[0]?.started_at, "暂无") }}</strong></div>
        </div>
        <div class="channel-recording-grid">
          <article class="card form-section">
            <div class="card-head"><div><h3 class="card-title">平台录像策略</h3><p class="card-sub">控制当前通道的媒体录制行为</p></div><Film /></div>
            <label class="form-group"><span class="form-label">录像模式</span><select v-model="recordMode" class="select w-full"><option value="always">持续录像（always）</option><option value="none">不录制（none）</option><option value="ai" disabled>AI 触发（暂不可用）</option></select></label>
            <div class="warning-box mt-4"><ShieldAlert /><span>AI 模式当前与持续录像行为相同，因此暂不允许切换。</span></div>
            <button class="btn btn-primary mt-4" type="button" :disabled="Boolean(actionLoading)" @click="saveRecordMode"><Save />保存策略</button>
          </article>

          <article class="card card-pad">
            <div class="card-head"><div><h3 class="card-title">平台录像</h3><p class="card-sub">最近录制的通道片段</p></div><Film /></div>
            <div v-if="resourceErrors.recordings" class="inline-resource-error" role="alert"><span>{{ resourceErrors.recordings }}</span><button class="btn btn-sm" type="button" @click="loadRecordings(channel.id)">重试</button></div>
            <div v-else-if="resourceLoading.recordings" class="empty-state compact" role="status"><LoaderCircle class="animate-spin" />正在读取录像…</div>
            <div v-else-if="recordings.length" class="channel-recording-list"><div v-for="item in recordings.slice(0, 5)" :key="item.id"><span><strong>{{ formatDate(item.started_at) }}</strong><small>{{ item.app || channel.app || "—" }} / {{ item.stream || channel.stream || channel.id }}</small></span><a class="device-row-action" :href="api.recordingDownloadUrl(item.id)" download aria-label="下载录像"><ExternalLink /></a></div></div>
            <div v-else class="empty-state compact">{{ recordMode === "none" ? "当前未开启平台录像" : "已开启录像，但暂时没有片段" }}</div>
          </article>

          <article class="card card-pad">
            <div class="card-head"><div><h3 class="card-title">设备端录像</h3><p class="card-sub">查询最近 24 小时的 GB28181 设备录像目录</p></div><Camera /></div>
            <button class="btn" type="button" :disabled="!isGb || !channel.is_online || Boolean(actionLoading)" @click="queryRemoteRecords"><LoaderCircle v-if="resourceLoading.remote" class="animate-spin" /><RefreshCw v-else />查询设备录像</button>
            <p v-if="!isGb" class="capability-reason">设备端录像查询仅支持 GB28181 通道。</p>
            <p v-else-if="!channel.is_online" class="capability-reason">通道离线，暂时无法查询设备录像。</p>
            <div v-if="resourceErrors.remote" class="inline-resource-error mt-4" role="alert">{{ resourceErrors.remote }}</div>
            <details v-else-if="remoteRecords" class="detail-raw-card mt-4"><summary>查看设备技术响应</summary><pre>{{ JSON.stringify(remoteRecords, null, 2) }}</pre></details>
            <div v-else class="read-only mt-4">尚未查询。查询完成后可核验设备端录像目录；当前后端没有全量下载任务列表接口。</div>
          </article>

          <article class="card form-section history-session-card">
            <div class="card-head"><div><h3 class="card-title">设备历史会话</h3><p class="card-sub">四版本回放、RTP 下载与 2014 附录 O 裸 TCP 下载</p></div><Gauge /></div>
            <div v-if="resourceErrors.history" class="warning-box"><ShieldAlert /><span>{{ resourceErrors.history }}</span></div>
            <div class="form-grid history-session-fields">
              <label class="form-group"><span class="form-label">会话类型</span><select v-model="historyForm.mode" class="select w-full" :disabled="historyActive"><option value="playback">历史回放</option><option value="download">录像下载</option></select></label>
              <label v-if="historyForm.mode === 'download'" class="form-group"><span class="form-label">传输方式</span><select v-model="historyForm.transport" class="select w-full" :disabled="historyActive"><option value="rtp">RTP 下载（四版本）</option><option value="direct_tcp" :disabled="!supportsDirectDownload">裸 TCP（仅 2014 附录 O）</option></select></label>
              <label class="form-group"><span class="form-label">开始时间</span><input v-model="historyForm.startAt" class="input plain w-full" type="datetime-local" :disabled="historyActive" /></label>
              <label class="form-group"><span class="form-label">结束时间</span><input v-model="historyForm.endAt" class="input plain w-full" type="datetime-local" :disabled="historyActive" /></label>
              <label class="form-group"><span class="form-label">录像类型</span><select v-model.number="historyForm.recordType" class="select w-full" :disabled="historyActive"><option :value="3">定时录像</option><option :value="0">全部录像</option><option :value="1">手动录像</option><option :value="2">报警录像</option></select></label>
              <label v-if="historyForm.mode === 'download'" class="form-group"><span class="form-label">下载倍速</span><input v-model.number="historyForm.downloadSpeed" class="input plain w-full" type="number" min="1" max="16" step="1" :disabled="historyActive || !hasGBCapability('download_speed')" /><small :class="{ 'field-error': !historyDownloadSpeedValid }">{{ !historyDownloadSpeedValid ? "请输入 1–16 的整数倍速" : hasGBCapability("download_speed") ? "2014+ 支持整数倍速" : "当前档案固定 1 倍速" }}</small></label>
            </div>
            <p class="capability-reason">{{ historyForm.transport === "direct_tcp" ? supportsDirectDownload ? "2014 附录 O 已由设备能力档案开放；仍受服务端白名单与并发限制" : "当前设备未声明 direct_tcp_download 能力" : `RTP 历史会话将在 ${boundMediaServerId} 节点收流；适用于四个版本，仍需设备互通验证` }}</p>
            <div class="history-session-actions">
              <button class="btn btn-primary" type="button" :disabled="!isGb || !channel.is_online || historyActive || !historyStartAllowed || Boolean(actionLoading)" @click="startHistorySession"><LoaderCircle v-if="actionLoading === '启动历史会话'" class="animate-spin" /><Play v-else />启动会话</button>
              <button class="btn" type="button" :disabled="!historyActive || Boolean(actionLoading)" @click="stopHistorySession"><Square />{{ historyForm.mode === "download" && historyForm.transport === "direct_tcp" ? "取消下载" : "停止会话" }}</button>
              <button v-if="historyForm.mode === 'download'" class="btn" type="button" :disabled="!historyState || Boolean(actionLoading)" @click="refreshHistoryState"><RefreshCw />刷新进度</button>
            </div>

            <div v-if="historyForm.mode === 'playback' && historyActive" class="history-control-panel">
              <label class="form-group"><span class="form-label">回放控制</span><select v-model="historyForm.controlAction" class="select w-full"><option value="play">继续</option><option value="pause">暂停</option><option value="speed">倍速</option><option value="seek">跳转</option></select></label>
              <label v-if="historyForm.controlAction === 'speed'" class="form-group"><span class="form-label">播放倍率</span><input v-model.number="historyForm.scale" class="input plain w-full" type="number" min="-16" max="16" step="0.25" /><small :class="{ 'field-error': !historyScaleValid }">{{ historyScaleValid ? "负值表示倒放，不能为 0" : "绝对值范围应为 0.25–16，且不能为 0" }}</small></label>
              <label v-if="supportsPositionedSpeed" class="form-group"><span class="form-label">{{ effectiveGbVersion === "1.0" ? "倍速起点（2011，可选）" : "倒放起点（2022，可选）" }}</span><input v-model="historyForm.speedAt" class="input plain w-full" type="datetime-local" /><small>{{ effectiveGbVersion === "1.0" ? "留空时从当前位置变速；填写后按 2011 生成 Scale 与 npt Range。" : "留空时从当前位置倒放；填写后从指定位置倒放至录像起点。" }}</small></label>
              <label v-if="historyForm.controlAction === 'seek'" class="form-group"><span class="form-label">跳转时间</span><input v-model="historyForm.seekAt" class="input plain w-full" type="datetime-local" /></label>
              <button class="btn" type="button" :disabled="Boolean(actionLoading) || !historyControlValid" @click="controlHistorySession"><Pause v-if="historyForm.controlAction === 'pause'" /><Play v-else />下发控制</button>
            </div>

            <section v-if="historyState" class="history-progress" aria-live="polite">
              <div class="history-progress-head"><span><strong>{{ historyStatusLabel }}</strong><small class="mono">{{ historyState.session_id }}</small></span><span class="status" :class="historyStatusClass">{{ historyProgressKnown ? `${historyProgress.toFixed(1)}%` : historyStatusLabel }}</span></div>
              <div class="history-progress-track" role="progressbar" :aria-valuenow="historyProgressKnown ? historyProgress : undefined" :aria-valuetext="historyProgressKnown ? `${historyProgress.toFixed(1)}%` : historyStatusLabel" aria-valuemin="0" aria-valuemax="100"><span :style="{ transform: `scaleX(${historyProgress / 100})` }" /></div>
              <dl><div><dt>已接收</dt><dd>{{ formatBytes(historyState.received) }}</dd></div><div><dt>文件大小</dt><dd>{{ historyState.file_size_known ? formatBytes(historyState.file_size) : "未知" }}</dd></div><div><dt>实时速度</dt><dd>{{ historyState.bytes_speed ? `${formatBytes(historyState.bytes_speed)}/s` : "—" }}</dd></div><div><dt>更新时间</dt><dd>{{ formatDate(historyState.updated_at) }}</dd></div></dl>
              <p v-if="historyState.output" class="history-output mono">输出：{{ historyState.output }}</p>
              <p v-if="historyState.error" class="field-error" role="alert">{{ historyState.error }}</p>
            </section>
          </article>
        </div>
      </section>

      <section v-else-if="activeTab === 'gb'" id="channel-panel-gb" class="channel-detail-section" role="tabpanel" aria-labelledby="channel-tab-gb" tabindex="0">
        <div class="detail-section-head"><div><h2>国标扩展业务</h2><p>按设备实际协议档案执行查询、控制、2022 抓拍和升级任务。</p></div><span class="protocol-tag blue">{{ effectiveGbVersion || "版本未知" }}</span></div>
        <div v-if="!isGb" class="warning-box"><ShieldAlert /><span>当前通道不是 GB28181 通道，以下操作不可用。</span></div>
        <div class="channel-recording-grid">
          <article class="card form-section">
            <div class="card-head"><div><h3 class="card-title">设备查询</h3><p class="card-sub">标准查询按版本和设备能力门禁；厂商兼容项单独标识</p></div><Search /></div>
            <label class="form-group"><span class="form-label">查询类型</span><select v-model="gbQueryForm.action" class="select w-full" :disabled="!isGb"><option value="device_status">设备状态（四版本）</option><option value="device_info">设备信息（四版本）</option><option value="catalog">设备目录（四版本）</option><option value="record_info">录像目录（四版本）</option><option value="alarm">报警筛选请求（厂商兼容）</option><option value="config_download" :disabled="!hasGBCapability('config_query')">配置查询（2014+）</option><option value="preset_query" :disabled="!hasGBCapability('preset_query')">预置位（2014+）</option><option value="mobile_position" :disabled="!hasGBCapability('mobile_position')">移动位置（2016+）</option><option value="home_position_query" :disabled="!hasGBCapability('home_position_query')">看守位（2022）</option><option value="cruise_track_list" :disabled="!hasGBCapability('cruise_track_query')">巡航轨迹列表（2022）</option><option value="cruise_track" :disabled="!hasGBCapability('cruise_track_query')">巡航轨迹（2022）</option><option value="ptz_position" :disabled="!hasGBCapability('ptz_position')">PTZ 精准位置（2022）</option><option value="sdcard_status" :disabled="!hasGBCapability('sdcard')">存储卡状态（2022）</option></select></label>
            <label v-if="gbQueryForm.action === 'config_download'" class="form-group"><span class="form-label">配置类型</span><select v-model="gbQueryForm.configType" class="select w-full"><option value="basic_param">BasicParam</option><option value="video_param_opt">VideoParamOpt</option><option value="svac_encode_config">SVACEncodeConfig</option><option value="snapshot" :disabled="!hasGBCapability('snapshot')">SnapShot（2022）</option></select></label>
            <label v-if="gbQueryForm.action === 'cruise_track'" class="form-group"><span class="form-label">巡航编号</span><input v-model.number="gbQueryForm.number" class="input plain w-full" type="number" min="0" max="1" /></label>
            <div v-if="['catalog', 'record_info'].includes(gbQueryForm.action)" class="form-grid query-time-fields">
              <label class="form-group"><span class="form-label">起始时间</span><input v-model="gbQueryForm.startAt" class="input plain w-full" type="datetime-local" /><small>{{ gbQueryForm.action === "record_info" && ["2.0", "3.0"].includes(effectiveGbVersion) ? "2016/2022 必填。" : "留空为不限定起始时间。" }}</small></label>
              <label class="form-group"><span class="form-label">结束时间</span><input v-model="gbQueryForm.endAt" class="input plain w-full" type="datetime-local" /><small>{{ gbQueryForm.action === "record_info" && ["2.0", "3.0"].includes(effectiveGbVersion) ? "2016/2022 必填。" : "留空为不限定结束时间。" }}</small></label>
            </div>
            <div v-if="gbQueryForm.action === 'record_info'" class="form-grid record-query-fields">
              <label class="form-group"><span class="form-label">文件路径</span><input v-model="gbQueryForm.filePath" class="input plain w-full" placeholder="可选，如 /record/front-gate.ps" /></label>
              <label class="form-group"><span class="form-label">录像地址</span><input v-model="gbQueryForm.address" class="input plain w-full" placeholder="可选，支持不完全查询" /></label>
              <label class="form-group"><span class="form-label">涉密属性</span><select v-model="gbQueryForm.secrecy" class="select w-full"><option value="">不限定</option><option value="0">不涉密</option><option value="1">涉密</option></select></label>
              <label class="form-group"><span class="form-label">录像类型</span><select v-model="gbQueryForm.recordType" class="select w-full"><option value="">默认</option><option value="time">time</option><option value="alarm">alarm</option><option value="manual">manual</option><option value="all">all</option></select></label>
              <label class="form-group"><span class="form-label">录像触发者 ID</span><input v-model="gbQueryForm.recorderId" class="input plain w-full mono" placeholder="可选" /></label>
              <label class="form-group"><span class="form-label">模糊查询</span><select v-model="gbQueryForm.indistinctQuery" class="select w-full" :disabled="!recordIndistinctQuerySupported"><option value="">不指定</option><option value="0">0 · 按 To URI 位置</option><option value="1">1 · 中心与前端同时检索</option></select><small>{{ recordIndistinctQuerySupported ? "GB/T 28181-2014 起支持。" : "2011 不包含 IndistinctQuery。" }}</small></label>
              <label class="form-group"><span class="form-label">2022 码流编号</span><input v-model="gbQueryForm.streamNumber" class="input plain w-full" type="number" min="0" :disabled="!recordStreamNumberSupported" placeholder="0 为主码流" /><small>{{ recordStreamNumberSupported ? "0 为主码流，1/2… 为子码流。" : "仅 GB/T 28181-2022 支持。" }}</small></label>
              <label class="form-group"><span class="form-label">报警方式过滤</span><input v-model="gbQueryForm.alarmMethod" class="input plain w-full" placeholder="可选，例如 2/5" /></label>
              <label class="form-group"><span class="form-label">报警类型过滤</span><input v-model="gbQueryForm.alarmType" class="input plain w-full" :disabled="!alarmTypeQuerySupported" :placeholder="alarmTypeQuerySupported ? '仅报警录像可填' : '仅 2016 / 2022 支持'" /></label>
            </div>
            <div v-if="gbQueryForm.action === 'alarm'" class="form-grid alarm-query-fields">
              <p class="form-help full">兼容部分设备对 MESSAGE/Query/Alarm 的实现。标准报警筛选与持续接收请在设备详情使用 9.11 报警订阅。</p>
              <label class="form-group"><span class="form-label">起始报警级别</span><input v-model="gbQueryForm.startAlarmPriority" class="input plain w-full" type="number" min="0" max="4" inputmode="numeric" placeholder="0–4，留空为全部" /><small>0 表示不限定起始级别。</small></label>
              <label class="form-group"><span class="form-label">结束报警级别</span><input v-model="gbQueryForm.endAlarmPriority" class="input plain w-full" type="number" min="0" max="4" inputmode="numeric" placeholder="0–4，留空为全部" /><small>0 表示不限定结束级别。</small></label>
              <label class="form-group"><span class="form-label">报警方式</span><input v-model="gbQueryForm.alarmMethod" class="input plain w-full" placeholder="例如 2/5（2011/2014 可写 25）" /><small>按标准方式编码筛选，留空为全部。</small></label>
              <label class="form-group"><span class="form-label">报警类型</span><input v-model="gbQueryForm.alarmType" class="input plain w-full" :disabled="!alarmTypeQuerySupported" :placeholder="alarmTypeQuerySupported ? '按报警方式填写类型' : '仅 2016 / 2022 支持'" /><small>{{ alarmTypeQuerySupported ? "GB/T 28181-2016/2022 可用。" : "当前版本不会发送 AlarmType。" }}</small></label>
              <label class="form-group"><span class="form-label">起始报警时间</span><input v-model="gbQueryForm.startAlarmTime" class="input plain w-full" type="datetime-local" /><small>留空为不限定起始时间。</small></label>
              <label class="form-group"><span class="form-label">结束报警时间</span><input v-model="gbQueryForm.endAlarmTime" class="input plain w-full" type="datetime-local" /><small>留空为不限定结束时间。</small></label>
            </div>
            <p v-if="queryValidationMessage" class="field-error" role="alert">{{ queryValidationMessage }}</p>
            <p class="capability-reason">{{ queryValidationMessage ? "请修正查询条件后再试" : queryAvailable ? "当前协议能力允许该查询" : `设备未声明 ${queryUnavailableCapability} 能力` }}</p>
            <button class="btn btn-primary" type="button" :disabled="!isGb || !channel.is_online || !queryAvailable || Boolean(actionLoading)" @click="runGBQuery"><LoaderCircle v-if="actionLoading === '国标查询'" class="animate-spin" /><Search v-else />执行查询</button>
            <details v-if="gbQueryResult" class="detail-raw-card mt-4"><summary>查看查询响应</summary><pre>{{ JSON.stringify(gbQueryResult, null, 2) }}</pre></details>
          </article>

          <article class="card form-section">
            <div class="card-head"><div><h3 class="card-title">设备控制</h3><p class="card-sub">A.2.3 扩展控制按能力开放</p></div><Radio /></div>
            <label class="form-group"><span class="form-label">控制类型</span><select v-model="gbControlForm.action" class="select w-full" :disabled="!isGb"><option value="ptz_cmd">云台控制（四版本）</option><option value="teleboot">远程启动（四版本）</option><option value="record_start">设备录像开始（四版本）</option><option value="record_stop">设备录像停止（四版本）</option><option value="guard_set">布防（四版本）</option><option value="guard_reset">撤防（四版本）</option><option value="alarm_reset">报警复位（四版本）</option><option value="drag_zoom_in" :disabled="!hasGBCapability('drag_zoom_control')">拉框放大（2014+）</option><option value="drag_zoom_out" :disabled="!hasGBCapability('drag_zoom_control')">拉框缩小（2014+）</option><option value="iframe_send" :disabled="!hasGBCapability('iframe_control')">强制关键帧（2016+）</option><option value="home_position" :disabled="!hasGBCapability('home_position')">看守位（2016+）</option><option value="ptz_precise" :disabled="!hasGBCapability('ptz_position')">PTZ 精准控制（2022）</option><option value="format_sdcard" :disabled="!hasGBCapability('sdcard')">格式化存储卡（2022）</option><option value="target_track" :disabled="!hasGBCapability('target_track')">目标跟踪（2022）</option></select></label>
            <div v-if="gbControlForm.action === 'ptz_cmd'" class="form-grid"><label class="form-group"><span class="form-label">云台动作</span><select v-model="gbControlForm.ptzCmd" class="select w-full"><option value="stop">停止</option><option value="left">左转</option><option value="right">右转</option><option value="up">上转</option><option value="down">下转</option><option value="left_up">左上</option><option value="left_down">左下</option><option value="right_up">右上</option><option value="right_down">右下</option><option value="zoom_in">放大</option><option value="zoom_out">缩小</option><option value="focus_near">近聚焦</option><option value="focus_far">远聚焦</option><option value="iris_open">光圈开</option><option value="iris_close">光圈关</option><option value="preset_set">设置预置位</option><option value="preset_call">调用预置位</option><option value="preset_delete">删除预置位</option><option value="cruise_start">启动巡航</option><option value="scan_start">启动扫描</option><option value="aux_on">辅助开</option><option value="aux_off">辅助关</option></select></label><label class="form-group"><span class="form-label">速度（0–255）</span><input v-model.number="gbControlForm.ptzSpeed" class="input plain w-full" type="number" min="0" max="255" /></label><label class="form-group"><span class="form-label">预置位</span><input v-model.number="gbControlForm.ptzPreset" class="input plain w-full" type="number" min="0" max="255" /></label><label class="form-group"><span class="form-label">巡航/扫描组</span><input v-model.number="gbControlForm.ptzGroup" class="input plain w-full" type="number" min="0" max="255" /></label><label class="form-group"><span class="form-label">辅助编号</span><input v-model.number="gbControlForm.ptzAux" class="input plain w-full" type="number" min="0" max="255" /></label><label class="form-group"><span class="form-label">控制值</span><input v-model.number="gbControlForm.ptzValue" class="input plain w-full" type="number" min="0" max="65535" /></label></div>
            <label v-if="['record_start', 'record_stop'].includes(gbControlForm.action)" class="form-group"><span class="form-label">码流编号</span><input v-model.number="gbControlForm.streamNumber" class="input plain w-full" type="number" min="0" /><small>0 为缺省主码流；非零编号仅 2022 支持。</small></label>
            <div v-if="gbControlForm.action === 'alarm_reset'" class="form-grid"><label class="form-group"><span class="form-label">报警方式（可选）</span><input v-model="gbControlForm.alarmMethod" class="input plain w-full" placeholder="例如 2 或 2/5" /></label><label class="form-group"><span class="form-label">报警类型（可选）</span><input v-model="gbControlForm.alarmType" class="input plain w-full" placeholder="仅单一方式 2/5/6 可用" /></label></div>
            <div v-if="['drag_zoom_in', 'drag_zoom_out'].includes(gbControlForm.action) || (gbControlForm.action === 'target_track' && gbControlForm.targetTrackMode === 'Manual')" class="form-grid"><label class="form-group"><span class="form-label">画面宽</span><input v-model.number="gbControlForm.frameLength" class="input plain w-full" type="number" min="1" /></label><label class="form-group"><span class="form-label">画面高</span><input v-model.number="gbControlForm.frameWidth" class="input plain w-full" type="number" min="1" /></label><label class="form-group"><span class="form-label">中心 X</span><input v-model.number="gbControlForm.frameMidPointX" class="input plain w-full" type="number" min="0" /></label><label class="form-group"><span class="form-label">中心 Y</span><input v-model.number="gbControlForm.frameMidPointY" class="input plain w-full" type="number" min="0" /></label><label class="form-group"><span class="form-label">框宽</span><input v-model.number="gbControlForm.frameLengthX" class="input plain w-full" type="number" min="1" /></label><label class="form-group"><span class="form-label">框高</span><input v-model.number="gbControlForm.frameLengthY" class="input plain w-full" type="number" min="1" /></label></div>
            <div v-if="gbControlForm.action === 'home_position'" class="form-grid"><label class="form-group"><span class="form-label">启用状态</span><select v-model.number="gbControlForm.homeEnabled" class="select w-full"><option :value="1">启用</option><option :value="0">停用</option></select></label><label v-if="gbControlForm.homeEnabled === 1" class="form-group"><span class="form-label">空闲回位时间</span><input v-model.number="gbControlForm.homeResetTime" class="input plain w-full" type="number" /></label><label v-if="gbControlForm.homeEnabled === 1" class="form-group"><span class="form-label">预置位（0–255）</span><input v-model.number="gbControlForm.homePresetIndex" class="input plain w-full" type="number" min="0" max="255" /></label></div>
            <label v-if="gbControlForm.action === 'format_sdcard'" class="form-group"><span class="form-label">存储卡 ID</span><input v-model.number="gbControlForm.sdcardId" class="input plain w-full" type="number" min="0" /><small>0 表示格式化全部存储卡。</small></label>
            <div v-if="gbControlForm.action === 'ptz_precise'" class="form-grid"><label class="form-group"><span class="form-label">Pan</span><input v-model.number="gbControlForm.pan" class="input plain w-full" type="number" step="0.1" /></label><label class="form-group"><span class="form-label">Tilt</span><input v-model.number="gbControlForm.tilt" class="input plain w-full" type="number" step="0.1" /></label><label class="form-group"><span class="form-label">Zoom</span><input v-model.number="gbControlForm.zoom" class="input plain w-full" type="number" step="0.1" /></label></div>
            <div v-if="gbControlForm.action === 'target_track'" class="form-grid"><label class="form-group"><span class="form-label">跟踪模式</span><select v-model="gbControlForm.targetTrackMode" class="select w-full"><option value="Auto">自动</option><option value="Manual">手动选框</option><option value="Stop">停止</option></select></label><label class="form-group"><span class="form-label">全景通道编码（可选）</span><input v-model="gbControlForm.targetTrackDeviceId2" class="input plain w-full mono" /></label></div>
            <p class="capability-reason">{{ controlAvailable ? "当前协议能力允许该控制" : `设备未声明 ${controlCapability} 能力` }}</p>
            <button class="btn btn-primary" type="button" :disabled="!isGb || !channel.is_online || !controlAvailable || Boolean(actionLoading)" @click="runGBControl"><LoaderCircle v-if="actionLoading === '国标控制'" class="animate-spin" /><Radio v-else />下发控制</button>
            <details v-if="gbControlResult" class="detail-raw-card mt-4"><summary>查看控制响应</summary><pre>{{ JSON.stringify(gbControlResult, null, 2) }}</pre></details>
          </article>

          <article class="card form-section">
            <div class="card-head"><div><h3 class="card-title">2022 抓拍任务</h3><p class="card-sub">区分设备上传与实时流回退结果</p></div><Aperture /></div>
            <p class="capability-reason">{{ supportsDeviceSnapshot ? "设备档案支持 9.14 图像抓拍" : "当前档案不支持设备上传；刷新快照可能使用实时流回退" }}</p>
            <button class="btn" type="button" :disabled="!isGb || !channel.is_online || Boolean(actionLoading)" @click="refreshSnapshot"><Aperture />发起抓拍</button>
            <div v-if="snapshotSessionId" class="read-only mt-4"><strong class="mono">{{ snapshotSessionId }}</strong><small>设备抓拍会话</small><button class="btn btn-sm" type="button" @click="loadSnapshotState">刷新状态</button></div>
            <details v-if="snapshotState" class="detail-raw-card mt-4"><summary>查看抓拍状态</summary><pre>{{ JSON.stringify(snapshotState, null, 2) }}</pre></details>
          </article>

          <article class="card form-section">
            <div class="card-head"><div><h3 class="card-title">2022 设备升级</h3><p class="card-sub">accepted 仅表示受理，需查询最终通知</p></div><FileUp /></div>
            <div class="form-grid"><label class="form-group"><span class="form-label">固件版本</span><input v-model="upgradeForm.firmware" class="input plain w-full" :disabled="!supportsUpgrade" /></label><label class="form-group"><span class="form-label">厂商</span><input v-model="upgradeForm.manufacturer" class="input plain w-full" :disabled="!supportsUpgrade" /></label><label class="form-group full"><span class="form-label">固件下载地址</span><input v-model="upgradeForm.fileUrl" class="input plain w-full" :disabled="!supportsUpgrade" /></label><label class="form-group full"><span class="form-label">会话 ID（可选）</span><input v-model.trim="upgradeForm.sessionId" class="input plain w-full mono" :disabled="!supportsUpgrade" /></label></div>
            <p class="capability-reason">{{ supportsUpgrade ? "当前设备档案支持升级流程" : "仅 GB/T 28181-2022 且声明 upgrade 能力的设备可用" }}</p>
            <div class="channel-quick-actions"><button class="btn btn-primary" type="button" :disabled="!supportsUpgrade || !channel.is_online || Boolean(actionLoading)" @click="startUpgrade"><LoaderCircle v-if="actionLoading === '设备升级'" class="animate-spin" /><FileUp v-else />发起升级</button><button class="btn" type="button" :disabled="!upgradeSessionId || Boolean(actionLoading)" @click="loadUpgradeState"><RefreshCw />刷新状态</button></div>
            <div v-if="upgradeSessionId" class="read-only mt-4"><strong class="mono">{{ upgradeSessionId }}</strong><small>升级会话</small></div>
            <details v-if="upgradeState" class="detail-raw-card mt-4"><summary>查看升级状态</summary><pre>{{ JSON.stringify(upgradeState, null, 2) }}</pre></details>
          </article>
        </div>
      </section>

      <section v-else id="channel-panel-technical" class="channel-detail-section" role="tabpanel" aria-labelledby="channel-tab-technical" tabindex="0">
        <div class="detail-section-head"><div><h2>通道档案</h2><p>用于运维核验的协议、归属和媒体流标识。</p></div></div>
        <div class="channel-technical-layout">
          <article class="card card-pad"><dl class="channel-technical-grid"><div><dt>协议</dt><dd>{{ protocol }}</dd></div><div v-if="isGb"><dt>有效版本</dt><dd><span class="protocol-tag blue">{{ gbVersionLabel }}</span></dd></div><div><dt>通道编号</dt><dd class="mono">{{ channel.channel_id || channel.id }}</dd></div><div><dt>所属设备</dt><dd>{{ device?.name || channel.device_id || "—" }}</dd></div><div><dt>设备 ID</dt><dd class="mono">{{ channel.did || channel.device_id || "—" }}</dd></div><div><dt>应用 / 流</dt><dd class="mono">{{ channel.app || "—" }} / {{ channel.stream || "—" }}</dd></div><div><dt>媒体节点</dt><dd><span class="status" :class="boundMediaServerState.tone">{{ boundMediaServerId }} · {{ boundMediaServerState.label }}</span></dd></div><div><dt>更新时间</dt><dd>{{ formatDate(channel.updated_at) }}</dd></div></dl><details v-if="playAddresses.length" class="detail-raw-card mt-4"><summary><Link2 />查看全部播放地址（{{ playAddresses.length }}）</summary><div class="technical-link-list"><a v-for="item in playAddresses" :key="item.url" :href="item.url" target="_blank" rel="noreferrer"><strong>{{ item.label }}</strong><span class="mono">{{ item.url }}</span></a></div></details></article>

          <article v-if="isGb" class="card form-section media-binding-card">
            <div class="card-head"><div><h3 class="card-title">媒体节点路由</h3><p class="card-sub">直播、回放、下载、快照、语音与级联媒体共用此绑定</p></div><Server /></div>
            <div v-if="resourceErrors.media" class="inline-resource-error" role="alert"><span>{{ resourceErrors.media }}</span><button class="btn btn-sm" type="button" @click="loadMediaServers()">重试</button></div>
            <label v-else class="form-group"><span class="form-label">接收与处理节点</span><select v-model="selectedMediaServerId" class="select w-full" :disabled="Boolean(mediaBindingDisabledReason)" aria-describedby="media-binding-help"><option v-if="!mediaServers.some((item) => item.id === selectedMediaServerId)" :value="selectedMediaServerId">{{ selectedMediaServerId }} · 当前绑定</option><option v-for="item in mediaServers" :key="item.id" :value="item.id">{{ item.id }} · {{ mediaServerStatusLabel(item.status) }} · {{ item.ip || "地址未配置" }}</option></select></label>
            <div class="media-binding-route" aria-live="polite"><span><small>当前节点</small><strong class="mono">{{ boundMediaServerId }}</strong></span><span aria-hidden="true">→</span><span><small>保存后</small><strong class="mono">{{ selectedMediaServerId }}</strong></span></div>
            <p id="media-binding-help" class="capability-reason">{{ mediaBindingDisabledReason || (mediaBindingChanged ? "保存后仅影响新建媒体会话；当前播放地址将清空并需重新预览" : "当前绑定已生效，无需保存") }}</p>
            <button class="btn btn-primary" type="button" :aria-disabled="Boolean(mediaBindingDisabledReason) || !mediaBindingChanged || Boolean(actionLoading)" aria-describedby="media-binding-help" @click="saveMediaServerBinding"><LoaderCircle v-if="actionLoading === '保存媒体节点'" class="animate-spin" /><Save v-else />保存节点绑定</button>
          </article>
        </div>
      </section>
    </template>
  </main>
</template>
