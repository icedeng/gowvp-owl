<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  Activity,
  ArrowLeft,
  Camera,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Clock3,
  Download,
  FileCog,
  History,
  Info,
  ListVideo,
  LoaderCircle,
  Play,
  Radio,
  RefreshCcw,
  Search,
  Settings2,
  ShieldAlert,
  ShieldCheck,
  SlidersHorizontal,
  Trash2,
  UploadCloud,
  Wifi,
  Wrench,
} from "@lucide/vue";
import { api, countItems, errorMessage, typeLabel } from "../services/api";
import type { ApiChannel, ApiDevice, DeviceHistoryRecord, GBSubscriptionState } from "../types/api";
import { formatDate, relativeTime } from "../utils/format";
import { useUiStore } from "../stores/ui";
import ModalDialog from "../components/ModalDialog.vue";

const route = useRoute();
const router = useRouter();
const ui = useUiStore();
type DetailTab = "overview" | "channels" | "operations" | "diagnostics";
type HistoryKind = "heartbeat" | "register";
type SubscriptionEvent = "catalog" | "alarm" | "mobile_position" | "ptz_position";

interface SubscriptionReceipt {
  key: string;
  event: SubscriptionEvent;
  eventName: string;
  targetId: string;
  expires: number;
  confirmedAt?: string;
  expiresAt?: string;
  refreshAt?: string;
  status?: string;
  notifyCSeq?: number;
  persisted?: boolean;
  updatedAt?: string;
  nextAttemptAt?: string;
  lastError?: string;
  runtime: boolean;
  payload: Record<string, unknown>;
}

const tab = ref<DetailTab>("overview");
const loading = ref(false);
const loadError = ref("");
const actionLoading = ref("");
const device = ref<ApiDevice | null>(null);
const relatedChannels = ref<ApiChannel[]>([]);
const channelTotal = ref(0);
const onlineChannelTotal = ref(0);
const channelPage = ref(1);
const channelLoading = ref(false);
const channelError = ref("");
const diagnostics = ref<Record<string, unknown> | null>(null);
const a4 = ref<Record<string, unknown> | null>(null);
const heartbeatHistory = ref<DeviceHistoryRecord[]>([]);
const registerHistory = ref<DeviceHistoryRecord[]>([]);
const historyKind = ref<HistoryKind | null>(null);
const historyRows = ref<DeviceHistoryRecord[]>([]);
const historyLoading = ref(false);
const historyError = ref("");
const editOpen = ref(false);
const deleteOpen = ref(false);
const deleting = ref(false);
const CHANNEL_PAGE_SIZE = 25;
const gbCapabilityOptions = [
  ["config_query", "配置查询"],
  ["config_write", "配置写入"],
  ["catalog_extension", "目录扩展字段"],
  ["directory_notify", "目录通知"],
  ["multi_response", "多响应消息"],
  ["media_status", "媒体状态通知"],
  ["voice_broadcast", "语音广播"],
  ["voice_intercom", "语音对讲"],
  ["rtp_over_tcp", "RTP over TCP"],
  ["direct_tcp_download", "2014 TCP 下载"],
  ["download_speed", "下载倍速"],
  ["iframe_control", "强制关键帧"],
  ["drag_zoom_control", "拉框缩放"],
  ["preset_query", "预置位查询"],
  ["mobile_position", "移动位置"],
  ["ptz_position", "PTZ 精准位置"],
  ["home_position", "看守位"],
  ["home_position_query", "看守位查询"],
  ["cruise_track_query", "巡航轨迹查询"],
  ["sdcard", "存储卡管理"],
  ["h265", "H.265 视频"],
  ["aac", "AAC 音频"],
  ["snapshot", "抓拍"],
  ["upgrade", "升级"],
  ["target_track", "目标跟踪"],
] as const;
const editForm = reactive({
  name: "",
  device_id: "",
  username: "",
  password: "",
  ip: "",
  port: 0,
  stream_mode: 1,
  gb_version: "",
  gb_disabled_capabilities: [] as string[],
});
const basicForm = reactive({
  name: "",
  expiration: 3600,
  heartbeat_interval: 60,
  heartbeat_count: 3,
});
const subscriptionForm = reactive({
  event: "alarm" as SubscriptionEvent,
  targetId: "",
  expires: 3600,
  startAlarmPriority: "",
  endAlarmPriority: "",
  alarmMethod: "",
  alarmType: "",
  startAlarmTime: "",
  endAlarmTime: "",
  startTime: "",
  endTime: "",
  interval: 0,
});
const subscriptionReceipts = ref<SubscriptionReceipt[]>([]);
const subscriptionLoading = ref(false);
const subscriptionError = ref("");
const advancedConfigForm = reactive({
  section: "svac_encode_config",
  payload: '{\n  "inner_xml": "<AudioParam><AudioRecognitionFlag>0</AudioRecognitionFlag></AudioParam>"\n}',
});
const advancedConfigOptions = [
  { key: "video_param_config", label: "VideoParamConfig", versions: ["1.1"], payload: '{\n  "items": [\n    {\n      "stream_name": "Stream1",\n      "video_format": "H.264",\n      "resolution": "1920x1080",\n      "frame_rate": "25",\n      "bit_rate_type": "1",\n      "video_bit_rate": "4096"\n    }\n  ]\n}' },
  { key: "audio_param_config", label: "AudioParamConfig", versions: ["1.1"], payload: '{\n  "items": [\n    {\n      "stream_name": "Stream1",\n      "audio_format": "G.711",\n      "audio_bit_rate": "64",\n      "sampling_rate": "8"\n    }\n  ]\n}' },
  { key: "svac_encode_config", label: "SVACEncodeConfig", versions: ["1.1", "2.0", "3.0"], payload: '{\n  "inner_xml": "<AudioParam><AudioRecognitionFlag>0</AudioRecognitionFlag></AudioParam>"\n}' },
  { key: "svac_decode_config", label: "SVACDecodeConfig", versions: ["1.1", "2.0", "3.0"], payload: '{\n  "inner_xml": "<SVCParam><SVCSTMMode>0</SVCSTMMode></SVCParam>"\n}' },
  { key: "video_param_attribute", label: "VideoParamAttribute", versions: ["3.0"], payload: '{\n  "inner_xml": "<Item><StreamNumber>0</StreamNumber><VideoFormat>H.264</VideoFormat><Resolution>1920x1080</Resolution><FrameRate>25</FrameRate><BitRateType>1</BitRateType></Item>"\n}' },
  { key: "video_record_plan", label: "VideoRecordPlan", versions: ["3.0"], payload: '{\n  "inner_xml": "<RecordEnable>0</RecordEnable><RecordScheduleSumNum>0</RecordScheduleSumNum><StreamNumber>0</StreamNumber>"\n}' },
  { key: "video_alarm_record", label: "VideoAlarmRecord", versions: ["3.0"], payload: '{\n  "inner_xml": "<RecordEnable>0</RecordEnable><StreamNumber>0</StreamNumber>"\n}' },
  { key: "picture_mask", label: "PictureMask", versions: ["3.0"], payload: '{\n  "inner_xml": "<On>0</On><SumNum>0</SumNum>"\n}' },
  { key: "frame_mirror", label: "FrameMirror", versions: ["3.0"], payload: '{\n  "inner_xml": "0"\n}' },
  { key: "alarm_report", label: "AlarmReport", versions: ["3.0"], payload: '{\n  "inner_xml": "<MotionDetection>0</MotionDetection><FieldDetection>0</FieldDetection>"\n}' },
  { key: "osd_config", label: "OSDConfig", versions: ["3.0"], payload: '{\n  "inner_xml": "<Length>1920</Length><Width>1080</Width><TimeX>0</TimeX><TimeY>0</TimeY><SumNum>0</SumNum>"\n}' },
  { key: "snapshot_config", label: "SnapShotConfig", versions: ["3.0"], capability: "snapshot", payload: '{\n  "snap_num": 1,\n  "interval": 1,\n  "upload_url": "https://example.com/gb28181/snapshot",\n  "session_id": "snapshot-session-0000000000000001"\n}' },
] as const;
const isGb = computed(
  () =>
    typeLabel(
      device.value?.type,
      device.value?.device_id || device.value?.id
    ) === "GB28181"
);
const allTabs: Array<{ key: DetailTab; name: string; note: string; icon: typeof Activity }> = [
  { key: "overview", name: "运行概览", note: "状态、档案与最近活动", icon: Activity },
  { key: "channels", name: "通道资源", note: "预览与通道能力", icon: ListVideo },
  { key: "operations", name: "设备运维", note: "同步、订阅与参数下发", icon: Wrench },
  { key: "diagnostics", name: "协议诊断", note: "版本、探测与高级查询", icon: ShieldCheck },
];
const tabs = computed(() =>
  isGb.value
    ? allTabs
    : allTabs.filter((item) => item.key === "overview" || item.key === "channels")
);
const isTransportContext = computed(() => route.name === "transport-device-detail");
const listRoute = computed(() => isTransportContext.value ? "/transport-devices" : "/devices");
const listLabel = computed(() => isTransportContext.value ? "部标设备" : "国标设备");
const protocolLabel = computed(() =>
  typeLabel(device.value?.type, device.value?.device_id || device.value?.id)
);
const deviceAddress = computed(() =>
  device.value?.address ||
  [device.value?.ip, device.value?.port].filter(Boolean).join(":") ||
  "—"
);
const protocolVersion = computed(() =>
  device.value?.ext?.gb_effective_version || device.value?.ext?.gb_version || "—"
);
const registrationState = computed(() => {
  if (device.value?.is_online) {
    return {
      label: "信令链路正常",
      note: "注册绑定有效，心跳持续更新",
      badge: "有效",
      tone: "online",
    };
  }
  if (device.value?.ext?.gb_registration_closed === true) {
    return {
      label: "注册绑定已关闭",
      note: "设备已注销或 REGISTER 有效期已结束",
      badge: "已关闭",
      tone: "offline",
    };
  }
  if (device.value?.ext?.gb_registration_closed === false) {
    return {
      label: "设备状态离线",
      note: "REGISTER 绑定仍有效，请检查心跳或 DeviceStatus",
      badge: "绑定有效",
      tone: "warning",
    };
  }
  return {
    label: "设备当前离线",
    note: "历史档案未记录绑定状态，请检查注册、网络及鉴权配置",
    badge: "未知",
    tone: "offline",
  };
});
const protocolProfile = computed(() => {
  switch (String(protocolVersion.value).trim()) {
    case "1.0": case "2011": return "1.0";
    case "1.1": case "2014": case "2011-supplement-2014": return "1.1";
    case "2.0": case "2016": return "2.0";
    case "3.0": case "2022": return "3.0";
    default: return "";
  }
});
const protocolDisplay = computed(() =>
  isGb.value ? `GB28181 · ${protocolVersion.value}` : protocolLabel.value
);
const capabilityMinimumVersion: Record<string, string> = {
  config_query: "2014+",
  config_write: "2014+",
  catalog_extension: "2014+",
  directory_notify: "2011+",
  multi_response: "2014+",
  media_status: "2011+",
  voice_broadcast: "2014+",
  voice_intercom: "2011+（标准双流程为 2016+）",
  rtp_over_tcp: "2016+",
  direct_tcp_download: "仅 2014",
  download_speed: "2014+",
  iframe_control: "2016+",
  drag_zoom_control: "2014+",
  preset_query: "2014+",
  mobile_position: "2016+",
  ptz_position: "仅 2022",
  home_position: "2016+",
  home_position_query: "仅 2022",
  cruise_track_query: "仅 2022",
  sdcard: "仅 2022",
  h265: "仅 2022",
  aac: "仅 2022",
  snapshot: "仅 2022",
  upgrade: "仅 2022",
  target_track: "仅 2022",
};
const fallbackCapabilitiesByVersion: Record<string, ReadonlySet<string>> = {
  "1.0": new Set(["directory_notify", "media_status", "voice_intercom"]),
  "1.1": new Set([
    "config_query", "config_write", "catalog_extension", "directory_notify", "multi_response", "media_status",
    "voice_broadcast", "voice_intercom", "direct_tcp_download", "download_speed", "drag_zoom_control", "preset_query",
  ]),
  "2.0": new Set([
    "config_query", "config_write", "catalog_extension", "directory_notify", "multi_response", "media_status",
    "voice_broadcast", "voice_intercom", "rtp_over_tcp", "download_speed", "iframe_control",
    "drag_zoom_control", "preset_query", "mobile_position", "home_position",
  ]),
  "3.0": new Set([
    "config_query", "config_write", "catalog_extension", "directory_notify", "multi_response", "media_status",
    "voice_broadcast", "voice_intercom", "rtp_over_tcp", "download_speed", "iframe_control",
    "drag_zoom_control", "preset_query", "mobile_position", "ptz_position", "home_position",
    "home_position_query", "cruise_track_query", "sdcard", "h265", "aac", "snapshot", "upgrade", "target_track",
  ]),
};
const declaredCapabilities = computed<string[] | undefined>(() => {
  const value = device.value?.ext?.gb_version_capabilities;
  return Array.isArray(value) ? value : undefined;
});
const effectiveCapabilities = computed(() =>
  new Set(declaredCapabilities.value || fallbackCapabilitiesByVersion[protocolProfile.value] || [])
);
const capabilityProfile = computed(() => {
  const supported = effectiveCapabilities.value;
  const disabled = new Set(device.value?.ext?.gb_disabled_capabilities || []);
  return gbCapabilityOptions.map(([key, label]) => ({
    key,
    label,
    minimum: capabilityMinimumVersion[key] || "按档案",
    state: disabled.has(key) ? "disabled" : supported.has(key) ? "ready" : "unavailable",
    reason: disabled.has(key)
      ? "设备兼容策略已禁用"
      : supported.has(key)
        ? declaredCapabilities.value ? "当前版本档案已声明" : "按有效版本档案推断"
        : declaredCapabilities.value ? "当前版本未声明" : "当前版本不支持",
  }));
});
function capabilityAvailability(key?: string) {
  if (!key) return { available: true, reason: "协议基础能力" };
  const disabled = device.value?.ext?.gb_disabled_capabilities || [];
  if (disabled.includes(key)) return { available: false, reason: "设备兼容策略已显式禁用" };
  if (!effectiveCapabilities.value.has(key)) {
    return { available: false, reason: `${capabilityMinimumVersion[key] || "当前档案"} ${declaredCapabilities.value ? "未声明该能力" : "不支持"}` };
  }
  return {
    available: true,
    reason: `${capabilityMinimumVersion[key] || "当前档案"} · ${declaredCapabilities.value ? "当前档案可用" : "按版本档案兼容"}`,
  };
}
const basicConfigAvailability = computed(() => capabilityAvailability("config_write"));
const selectedAdvancedConfig = computed(() =>
  advancedConfigOptions.find((item) => item.key === advancedConfigForm.section) || advancedConfigOptions[0]
);
const advancedConfigActionName = computed(() => `下发 ${selectedAdvancedConfig.value.label}`);
const advancedConfigAvailability = computed(() => {
  const base = basicConfigAvailability.value;
  if (!base.available) return base;
  const option = selectedAdvancedConfig.value;
  if (!(option.versions as readonly string[]).includes(protocolProfile.value)) {
    return { available: false, reason: `${option.label} 不属于当前 ${protocolVersion.value} 协议档案` };
  }
  if ("capability" in option && option.capability) return capabilityAvailability(option.capability);
  return { available: true, reason: `${option.label} 可按当前版本下发` };
});
const a4Availability = computed(() => {
  const version = String(device.value?.ext?.gb_effective_version || device.value?.ext?.gb_version || "").toLowerCase();
  const available = version === "3.0" || version.includes("2022");
  return { available, reason: available ? "GB/T 28181-2022 档案可用" : "仅 GB/T 28181-2022 档案可用" };
});
const subscriptionItems = computed(() => {
  const items = [
    { name: "目录订阅", event: "catalog" as SubscriptionEvent, capability: "directory_notify" },
    { name: "报警订阅", event: "alarm" as SubscriptionEvent, capability: "" },
    { name: "位置订阅", event: "mobile_position" as SubscriptionEvent, capability: "mobile_position" },
    { name: "PTZ 精准位置订阅", event: "ptz_position" as SubscriptionEvent, capability: "ptz_position" },
  ];
  return items.map((item) => ({ ...item, ...capabilityAvailability(item.capability) }));
});
const selectedSubscription = computed(() =>
  subscriptionItems.value.find((item) => item.event === subscriptionForm.event) || subscriptionItems.value[1]
);
const alarmTypeSupported = computed(() =>
  protocolProfile.value === "2.0" || protocolProfile.value === "3.0"
);
const currentSubscriptionKey = computed(() =>
  subscriptionKeyFromPayload(buildSubscriptionBody())
);
const currentSubscriptionReceipt = computed(() =>
  subscriptionReceipts.value.find((item) => item.key === currentSubscriptionKey.value)
);
const streamMode = computed(() => {
  const mode = Number(device.value?.stream_mode);
  return ({ 0: "UDP", 1: "TCP 被动", 2: "TCP 主动" } as Record<number, string>)[mode] || device.value?.transport?.toUpperCase() || "—";
});
const channelPageCount = computed(() =>
  Math.max(1, Math.ceil(channelTotal.value / CHANNEL_PAGE_SIZE))
);
const recentActivity = computed(() => {
  const rows = [...heartbeatHistory.value, ...registerHistory.value];
  const kinds = new Set(rows.map((item) => item.kind));
  if (!kinds.has("heartbeat") && device.value?.keepalive_at) {
    rows.push({
      id: -1,
      device_id: device.value.device_id || device.value.id,
      kind: "heartbeat",
      recorded_at: device.value.keepalive_at,
      address: deviceAddress.value,
      status: device.value.is_online ? "在线" : "最近状态",
    });
  }
  if (!kinds.has("register") && device.value?.registered_at) {
    rows.push({
      id: -2,
      device_id: device.value.device_id || device.value.id,
      kind: "register",
      recorded_at: device.value.registered_at,
      address: deviceAddress.value,
      status: "已注册",
    });
  }
  return rows
    .sort((a, b) => new Date(b.recorded_at || 0).getTime() - new Date(a.recorded_at || 0).getTime())
    .slice(0, 6);
});
const historyTitle = computed(() =>
  historyKind.value === "heartbeat" ? "心跳记录" : "注册记录"
);

function latestHistorySnapshot(kind: HistoryKind): DeviceHistoryRecord[] {
  if (!device.value) return [];
  const recordedAt = kind === "heartbeat" ? device.value.keepalive_at : device.value.registered_at;
  if (!recordedAt) return [];
  return [{
    id: kind === "heartbeat" ? -1 : -2,
    device_id: device.value.device_id || device.value.id,
    kind,
    recorded_at: recordedAt,
    address: deviceAddress.value,
    status: kind === "heartbeat" ? (device.value.is_online ? "在线" : "最近状态") : "已注册",
  }];
}

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const id = String(route.params.id);
    const { data } = await api.device(id);
    device.value = data;
    basicForm.name = data.name || "";
    channelPage.value = 1;
    channelError.value = "";
    const [channelResponse, onlineChannelResponse, heartbeatResponse, registerResponse] = await Promise.allSettled([
      api.channels({ did: data.id, page: 1, size: CHANNEL_PAGE_SIZE }),
      countItems(api.channels, { did: data.id, is_online: true }),
      api.deviceHistory(data.id, "heartbeat", { page: 1, size: 6 }),
      api.deviceHistory(data.id, "register", { page: 1, size: 6 }),
    ]);
    if (channelResponse.status === "fulfilled") {
      relatedChannels.value = channelResponse.value.data?.items || [];
      channelTotal.value = Number(
        channelResponse.value.data?.total ?? relatedChannels.value.length
      );
    } else {
      relatedChannels.value = [];
      channelTotal.value = 0;
      channelError.value = errorMessage(
        channelResponse.reason,
        "设备通道加载失败"
      );
    }
    onlineChannelTotal.value =
      onlineChannelResponse.status === "fulfilled"
        ? onlineChannelResponse.value
        : relatedChannels.value.filter((item) => item.is_online === true).length;
    heartbeatHistory.value = heartbeatResponse.status === "fulfilled" ? heartbeatResponse.value.data?.items || [] : [];
    registerHistory.value = registerResponse.status === "fulfilled" ? registerResponse.value.data?.items || [] : [];
    if (isGb.value) {
      const [diagnosticsResponse] = await Promise.allSettled([
        api.gbDiagnostics(data.id),
        loadSubscriptionStates(data.id),
      ]);
      diagnostics.value = diagnosticsResponse.status === "fulfilled" ? diagnosticsResponse.value.data : null;
    } else {
      diagnostics.value = null;
      a4.value = null;
      subscriptionReceipts.value = [];
      subscriptionError.value = "";
      if (tab.value === "operations" || tab.value === "diagnostics") tab.value = "overview";
    }
  } catch (cause) {
    loadError.value = errorMessage(cause, "设备详情加载失败");
  } finally {
    loading.value = false;
  }
}

function subscriptionEventName(event: string) {
  return ({
    catalog: "目录订阅",
    alarm: "报警订阅",
    mobile_position: "位置订阅",
    ptz_position: "PTZ 精准位置订阅",
  } as Record<string, string>)[event] || event;
}

function subscriptionStatusLabel(status?: string) {
  return ({
    active: "有效",
    refreshing: "续订中",
    recovering: "待恢复",
    blocked: "已阻止重订",
    terminating: "取消中",
    expired: "已过期",
  } as Record<string, string>)[status || ""] || status || "本页已确认";
}

function subscriptionPayloadFromState(state: GBSubscriptionState): Record<string, unknown> {
  const body: Record<string, unknown> = {
    target_id: state.target_id,
    event: state.event,
    expires: Math.max(1, Number(state.expires) || 1),
  };
  if (state.event === "alarm") {
    Object.assign(body, {
      start_alarm_priority: state.start_alarm_priority || "",
      end_alarm_priority: state.end_alarm_priority || "",
      alarm_method: state.alarm_method || "",
      alarm_type: state.alarm_type || "",
      start_alarm_time: state.start_alarm_time || "",
      end_alarm_time: state.end_alarm_time || "",
    });
  } else if (state.event === "catalog") {
    Object.assign(body, { start_time: state.start_time || "", end_time: state.end_time || "" });
  } else if (state.event === "mobile_position") {
    body.interval = Number(state.interval) || 0;
  }
  return body;
}

function subscriptionReceiptFromState(state: GBSubscriptionState): SubscriptionReceipt | null {
  if (!["catalog", "alarm", "mobile_position", "ptz_position"].includes(state.event)) return null;
  const payload = subscriptionPayloadFromState(state);
  return {
    key: subscriptionKeyFromPayload(payload),
    event: state.event as SubscriptionEvent,
    eventName: subscriptionEventName(state.event),
    targetId: state.target_id,
    expires: Number(state.expires) || 0,
    expiresAt: state.expires_at && !state.expires_at.startsWith("0001-") ? state.expires_at : undefined,
    refreshAt: state.refresh_at,
    status: state.status,
    notifyCSeq: state.notify_cseq,
    persisted: state.persisted,
    updatedAt: state.updated_at,
    nextAttemptAt: state.next_attempt_at,
    lastError: state.last_error,
    runtime: true,
    payload,
  };
}

async function loadSubscriptionStates(id = device.value?.id) {
  if (!id) return;
  subscriptionLoading.value = true;
  subscriptionError.value = "";
  try {
    const { data } = await api.subscriptionStates(id);
    subscriptionReceipts.value = (data.items || [])
      .map(subscriptionReceiptFromState)
      .filter((item): item is SubscriptionReceipt => Boolean(item));
  } catch (cause) {
    subscriptionError.value = errorMessage(cause, "订阅运行态加载失败");
  } finally {
    subscriptionLoading.value = false;
  }
}

async function changeChannelPage(next: number) {
  if (!device.value) return;
  const target = Math.min(channelPageCount.value, Math.max(1, next));
  if (target === channelPage.value && !channelError.value) return;
  channelLoading.value = true;
  channelError.value = "";
  try {
    const response = await api.channels({
      did: device.value.id,
      page: target,
      size: CHANNEL_PAGE_SIZE,
    });
    relatedChannels.value = response.data?.items || [];
    channelTotal.value = Number(
      response.data?.total ?? relatedChannels.value.length
    );
    channelPage.value = target;
  } catch (cause) {
    channelError.value = errorMessage(cause, "设备通道加载失败");
  } finally {
    channelLoading.value = false;
  }
}

async function runAction(
  name: string,
  fn: () => Promise<unknown>,
  refresh = false
): Promise<boolean> {
  if (!device.value) return false;
  actionLoading.value = name;
  try {
    await fn();
    ui.toast(`${device.value.name || device.value.id} · ${name}成功`);
    if (refresh) await load();
    return true;
  } catch (cause) {
    ui.toast(errorMessage(cause, `${name}失败`));
    return false;
  } finally {
    actionLoading.value = "";
  }
}

function normalizeSubscriptionTime(value: string) {
  const normalized = value.trim();
  return /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(normalized)
    ? `${normalized}:00`
    : normalized;
}

function buildSubscriptionBody(): Record<string, unknown> {
  const body: Record<string, unknown> = {
    target_id: subscriptionForm.targetId.trim(),
    event: subscriptionForm.event,
    expires: Number(subscriptionForm.expires),
  };
  if (subscriptionForm.event === "alarm") {
    Object.assign(body, {
      start_alarm_priority: subscriptionForm.startAlarmPriority,
      end_alarm_priority: subscriptionForm.endAlarmPriority,
      alarm_method: subscriptionForm.alarmMethod.trim(),
      alarm_type: alarmTypeSupported.value ? subscriptionForm.alarmType.trim() : "",
      start_alarm_time: normalizeSubscriptionTime(subscriptionForm.startAlarmTime),
      end_alarm_time: normalizeSubscriptionTime(subscriptionForm.endAlarmTime),
    });
  } else if (subscriptionForm.event === "catalog") {
    Object.assign(body, {
      start_time: normalizeSubscriptionTime(subscriptionForm.startTime),
      end_time: normalizeSubscriptionTime(subscriptionForm.endTime),
    });
  } else if (subscriptionForm.event === "mobile_position") {
    body.interval = Number(subscriptionForm.interval);
  }
  return body;
}

function subscriptionKeyFromPayload(payload: Record<string, unknown>) {
  const identity = { ...payload };
  identity.target_id = String(identity.target_id || "").trim() || device.value?.device_id || device.value?.id || "";
  delete identity.expires;
  delete identity.cancel;
  return JSON.stringify(identity);
}

function validateSubscriptionForm() {
  if (!selectedSubscription.value.available) return selectedSubscription.value.reason;
  if (!Number.isInteger(subscriptionForm.expires) || subscriptionForm.expires <= 0) {
    return "订阅有效期必须是大于 0 的整数秒";
  }
  if (subscriptionForm.event === "mobile_position" &&
      (!Number.isInteger(subscriptionForm.interval) || subscriptionForm.interval < 0)) {
    return "位置上报间隔必须是大于等于 0 的整数秒";
  }
  const start = subscriptionForm.event === "alarm"
    ? normalizeSubscriptionTime(subscriptionForm.startAlarmTime)
    : normalizeSubscriptionTime(subscriptionForm.startTime);
  const end = subscriptionForm.event === "alarm"
    ? normalizeSubscriptionTime(subscriptionForm.endAlarmTime)
    : normalizeSubscriptionTime(subscriptionForm.endTime);
  if (start && end && end < start) return "开始时间不能晚于结束时间";
  return "";
}

async function submitSubscription() {
  if (!device.value) return;
  const invalid = validateSubscriptionForm();
  if (invalid) {
    ui.toast(invalid);
    return;
  }
  const body = buildSubscriptionBody();
  const renewing = Boolean(currentSubscriptionReceipt.value);
  const name = `${renewing ? "续订" : "创建"}${selectedSubscription.value.name}`;
  const succeeded = await runAction(name, () => api.subscribe(device.value!.id, body));
  if (!succeeded) return;
  const receipt: SubscriptionReceipt = {
    key: subscriptionKeyFromPayload(body),
    event: subscriptionForm.event,
    eventName: selectedSubscription.value.name,
    targetId: subscriptionForm.targetId.trim() || device.value.device_id || device.value.id,
    expires: Number(subscriptionForm.expires),
    confirmedAt: new Date().toISOString(),
    runtime: false,
    payload: { ...body },
  };
  const index = subscriptionReceipts.value.findIndex((item) => item.key === receipt.key);
  if (index >= 0) subscriptionReceipts.value.splice(index, 1, receipt);
  else subscriptionReceipts.value.unshift(receipt);
  await loadSubscriptionStates();
}

async function cancelSubscription(receipt: SubscriptionReceipt) {
  if (!device.value) return;
  const name = `取消${receipt.eventName}`;
  const succeeded = await runAction(name, () =>
    api.subscribe(device.value!.id, { ...receipt.payload, cancel: true })
  );
  if (succeeded) {
    await loadSubscriptionStates();
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
    gb_disabled_capabilities: [...(device.value.ext?.gb_disabled_capabilities || [])],
  });
  editOpen.value = true;
}

async function saveEdit() {
  if (!device.value) return;
  const body: Record<string, unknown> = { ...editForm };
  delete body.password;
  if (editForm.password) body.password = editForm.password;
  const saved = await runAction(
    "保存设备",
    () => api.editDevice(device.value!.id, body),
    true
  );
  if (saved) editOpen.value = false;
}

async function deleteDevice() {
  if (!device.value) return;
  deleting.value = true;
  const name = device.value.name || device.value.device_id || device.value.id;
  try {
    await api.deleteDevice(device.value.id);
    ui.toast(`设备 ${name} 已删除`);
    await router.push(listRoute.value);
  } catch (cause) {
    ui.toast(errorMessage(cause, "设备删除失败"));
  } finally {
    deleting.value = false;
  }
}

async function openHistory(kind: HistoryKind) {
  if (!device.value) return;
  historyKind.value = kind;
  const loadedRows = kind === "heartbeat" ? heartbeatHistory.value : registerHistory.value;
  historyRows.value = loadedRows.length ? loadedRows : latestHistorySnapshot(kind);
  historyError.value = "";
  historyLoading.value = true;
  try {
    const { data } = await api.deviceHistory(device.value.id, kind, { page: 1, size: 100 });
    historyRows.value = data?.items || [];
  } catch (cause) {
    historyError.value = "历史明细接口暂不可用，当前展示设备最近状态。";
  } finally {
    historyLoading.value = false;
  }
}

async function saveBasic() {
  if (!device.value || !basicConfigAvailability.value.available) return;
  await runAction("下发 BasicParam", () =>
    api.gbConfig(device.value!.id, {
      target_id: device.value?.device_id,
      timeout: 8,
      basic_param: basicForm,
    })
  );
}

function selectAdvancedConfig(section: string) {
  const option = advancedConfigOptions.find((item) => item.key === section);
  if (!option) return;
  advancedConfigForm.section = option.key;
  advancedConfigForm.payload = option.payload;
}

function changeAdvancedConfig(event: Event) {
  selectAdvancedConfig((event.target as HTMLSelectElement).value);
}

function restoreAdvancedConfigExample() {
  advancedConfigForm.payload = selectedAdvancedConfig.value.payload;
}

async function saveAdvancedConfig() {
  if (!device.value || !advancedConfigAvailability.value.available) return;
  let payload: unknown;
  try {
    payload = JSON.parse(advancedConfigForm.payload);
  } catch {
    ui.toast("配置 JSON 格式无效");
    return;
  }
  if (!payload || Array.isArray(payload) || typeof payload !== "object") {
    ui.toast("配置内容必须是 JSON 对象");
    return;
  }
  await runAction(advancedConfigActionName.value, () =>
    api.gbConfig(device.value!.id, {
      target_id: device.value?.device_id,
      timeout: 8,
      [advancedConfigForm.section]: payload,
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
  <main class="page-content device-detail-page">
    <div v-if="loadError" class="warning-box mb-4" role="alert">
      <ShieldAlert /><span>{{ loadError }}</span><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <div v-if="loading && !device" class="detail-skeleton" aria-live="polite">
      <span /><span /><span />
      <p><LoaderCircle class="animate-spin" />正在加载设备运行状态…</p>
    </div>

    <template v-if="device">
      <section class="device-command-header">
        <div class="device-command-identity">
          <RouterLink
            class="device-command-back"
            :to="listRoute"
            :aria-label="`返回${listLabel}列表`"
            :title="`返回${listLabel}`"
          ><ArrowLeft /></RouterLink>
          <span class="device-command-icon"><Camera /></span>
          <div>
            <div class="device-command-title">
              <h1>{{ device.name || "未命名设备" }}</h1>
              <span class="status" :class="registrationState.tone">{{ registrationState.badge }}</span>
              <span class="protocol-tag blue">{{ protocolDisplay }}</span>
            </div>
            <div class="device-command-meta">
              <span class="mono">{{ device.device_id || device.id }}</span>
              <span><Wifi />{{ deviceAddress }}</span>
              <span><Clock3 />最近心跳 {{ relativeTime(device.keepalive_at) }}</span>
            </div>
          </div>
        </div>
        <div class="head-actions">
          <button class="btn" @click="openEdit"><SlidersHorizontal />编辑档案</button>
          <button class="btn btn-danger" type="button" @click="deleteOpen = true"><Trash2 />删除设备</button>
        </div>
      </section>

      <section class="device-pulse" aria-label="设备运行摘要">
        <div class="device-pulse-lead" :class="{ offline: registrationState.tone === 'offline', warning: registrationState.tone === 'warning' }">
          <span class="pulse-led" />
          <div><strong>{{ registrationState.label }}</strong><small>{{ registrationState.note }}</small></div>
        </div>
        <div><small>通道在线</small><strong>{{ onlineChannelTotal }} / {{ channelTotal }}</strong><span>当前目录资源</span></div>
        <div><small>最近心跳</small><strong>{{ relativeTime(device.keepalive_at) }}</strong><span>{{ heartbeatHistory.length ? "历史已记录" : "等待新记录" }}</span></div>
        <div><small>流传输</small><strong>{{ streamMode }}</strong><span>{{ device.transport?.toUpperCase() || "SIP" }} 信令</span></div>
        <div><small>注册有效期</small><strong>{{ device.expires || "—" }}<em v-if="device.expires">s</em></strong><span>{{ formatDate(device.registered_at) }}</span></div>
      </section>

      <section class="device-workbench">
        <aside class="device-workbench-nav">
          <nav class="card detail-task-nav" aria-label="设备详情工作区">
            <button v-for="item in tabs" :key="item.key" :class="{ active: tab === item.key }" @click="tab = item.key">
              <component :is="item.icon" /><span><strong>{{ item.name }}</strong><small>{{ item.note }}</small></span><ChevronRight />
            </button>
          </nav>
          <article class="card detail-side-facts">
            <div><span>厂商</span><strong>{{ device.ext?.manufacturer || "—" }}</strong></div>
            <div><span>型号</span><strong>{{ device.ext?.model || "—" }}</strong></div>
            <div><span>接入地址</span><strong class="mono">{{ deviceAddress }}</strong></div>
          </article>
        </aside>

        <div class="device-workspace-content">
          <template v-if="tab === 'overview'">
            <section class="detail-section-head">
              <div><h2>运行概览</h2><p>核对设备注册档案、协议能力与最近信令活动。</p></div>
            </section>
            <section class="detail-overview-grid">
              <article class="card detail-profile-card">
                <div class="card-head"><div><h3 class="card-title">设备档案</h3><p class="card-sub">注册、网络与协议基础信息</p></div><FileCog /></div>
                <dl class="detail-definition-list">
                  <div><dt>设备编码</dt><dd class="mono">{{ device.device_id || device.id }}</dd></div>
                  <div><dt>厂商 / 型号</dt><dd>{{ [device.ext?.manufacturer, device.ext?.model].filter(Boolean).join(" / ") || "—" }}</dd></div>
                  <div><dt>注册地址</dt><dd class="mono">{{ deviceAddress }}</dd></div>
                  <div><dt>流传输模式</dt><dd>{{ streamMode }}</dd></div>
                  <div><dt>注册时间</dt><dd>{{ formatDate(device.registered_at) }}</dd></div>
                  <div><dt>{{ isGb ? "协议版本" : "接入协议" }}</dt><dd>{{ isGb ? protocolVersion : protocolLabel }} <small v-if="isGb">{{ device.ext?.gb_version_source || "自动识别" }}</small></dd></div>
                </dl>
              </article>
              <article class="card detail-capability-card">
                <div class="card-head"><div><h3 class="card-title">能力就绪度</h3><p class="card-sub">按版本声明与探测结果汇总</p></div><ShieldCheck /></div>
                <div class="detail-capability-list">
                  <div class="ready"><Play /><span><strong>实时播放</strong><small>统一播放链路可用</small></span><b>可用</b></div>
                  <div :class="device.ptz_capable ? 'ready' : 'pending'"><Activity /><span><strong>PTZ 控制</strong><small>{{ device.ptz_verified ? "设备通道已验证" : device.ptz_capable ? "设备声明支持" : "尚未完成探测" }}</small></span><b>{{ device.ptz_capable ? "支持" : "待探测" }}</b></div>
                  <div class="ready"><Camera /><span><strong>快照与录像</strong><small>进入具体通道执行</small></span><b>可用</b></div>
                  <div v-if="isGb" :class="effectiveCapabilities.size ? 'ready' : 'pending'"><Settings2 /><span><strong>版本扩展</strong><small>{{ declaredCapabilities ? `${declaredCapabilities.length} 项能力声明` : `${effectiveCapabilities.size} 项按版本推断` }}</small></span><b>{{ protocolVersion }}</b></div>
                  <div v-else class="ready"><Settings2 /><span><strong>协议接入</strong><small>当前设备按 {{ protocolLabel }} 模型管理</small></span><b>已识别</b></div>
                </div>
              </article>
              <article class="card detail-activity-card">
                <div class="card-head detail-activity-head">
                  <div><h3 class="card-title">最近信令活动</h3><p class="card-sub">设备心跳与注册事件</p></div>
                  <div class="detail-history-actions">
                    <button type="button" @click="openHistory('heartbeat')"><Activity />心跳记录</button>
                    <button type="button" @click="openHistory('register')"><Radio />注册记录</button>
                  </div>
                </div>
                <div v-if="recentActivity.length" class="device-activity-list">
                  <div v-for="record in recentActivity" :key="`${record.kind}-${record.id}`">
                    <span class="activity-kind" :class="record.kind"><Activity v-if="record.kind === 'heartbeat'" /><Radio v-else /></span>
                    <span><strong>{{ record.kind === "heartbeat" ? "心跳上报" : "设备注册" }}</strong><small class="mono">{{ record.address || "来源地址未记录" }}</small></span>
                    <time>{{ relativeTime(record.recorded_at) }}</time>
                  </div>
                </div>
                <div v-else class="compact-empty"><History /><span><strong>暂无信令时间</strong><small>设备产生心跳或注册事件后会显示在这里。</small></span></div>
              </article>
            </section>
          </template>

          <template v-else-if="tab === 'channels'">
            <section class="detail-section-head">
              <div><h2>通道资源</h2><p>查看通道目录、在线状态并进入通道详情。</p></div>
              <button v-if="isGb" class="btn" :disabled="actionLoading === '同步通道'" @click="runAction('同步通道', () => api.catalog(device!.id), true)">
                <LoaderCircle v-if="actionLoading === '同步通道'" class="animate-spin" /><RefreshCcw v-else />同步通道
              </button>
            </section>
            <section class="card detail-channel-list">
              <div v-if="channelError" class="inline-resource-error" role="alert">
                <span>{{ channelError }}</span>
                <button type="button" class="btn btn-sm" @click="changeChannelPage(channelPage)">重试</button>
              </div>
              <div v-if="channelLoading" class="compact-empty" aria-live="polite">
                <LoaderCircle class="animate-spin" /><span><strong>正在加载通道</strong><small>请稍候…</small></span>
              </div>
              <div v-for="channel in relatedChannels" :key="channel.id" class="detail-channel-row">
                <span class="channel-status-icon" :class="{ offline: !channel.is_online }"><Camera /></span>
                <span class="channel-main"><strong>{{ channel.name || "未命名通道" }}</strong><small class="mono">{{ channel.channel_id || channel.id }}</small></span>
                <span><small>状态</small><b class="status" :class="channel.is_online ? 'online' : 'offline'">{{ channel.is_online ? "在线" : "离线" }}</b></span>
                <span><small>PTZ</small><b>{{ channel.ptz_verified ? "已验证" : channel.ptz_capable ? "声明支持" : "不支持" }}</b></span>
                <span><small>AI</small><b>{{ channel.ext?.enabled_ai ? "已启用" : "未启用" }}</b></span>
                <div class="device-row-actions channel-row-actions">
                  <RouterLink class="device-row-detail" :to="`/channels/${encodeURIComponent(channel.id)}`">详情<ChevronRight /></RouterLink>
                </div>
              </div>
              <div v-if="!channelLoading && !channelError && !relatedChannels.length" class="compact-empty"><ListVideo /><span><strong>暂无通道资源</strong><small>{{ isGb ? "执行同步通道，从设备获取最新通道。" : "当前设备尚未上报可管理的视频通道。" }}</small></span></div>
              <div v-if="channelTotal" class="pagination">
                <span>共 {{ channelTotal }} 路通道</span>
                <div v-if="channelPageCount > 1" class="pagination-actions" aria-label="设备通道分页">
                  <button type="button" class="page-btn" :disabled="channelPage === 1 || channelLoading" aria-label="上一页通道" @click="changeChannelPage(channelPage - 1)"><ChevronLeft /></button>
                  <span>第 {{ channelPage }} / {{ channelPageCount }} 页</span>
                  <button type="button" class="page-btn" :disabled="channelPage === channelPageCount || channelLoading" aria-label="下一页通道" @click="changeChannelPage(channelPage + 1)"><ChevronRight /></button>
                </div>
              </div>
            </section>
          </template>

          <template v-else-if="tab === 'operations'">
            <section class="detail-section-head"><div><h2>设备运维</h2><p>执行设备级同步、校时、订阅和基础参数下发。</p></div></section>
            <section class="detail-operations-grid">
              <article class="card detail-operation-card">
                <div class="card-head"><div><h3 class="card-title">目录同步与兼容校时</h3><p class="card-sub">同步设备目录；主动校时仅用于厂商扩展</p></div><RefreshCcw /></div>
                <div class="operation-row"><span><strong>目录同步</strong><small>增量保存通道并保留业务配置</small></span><button class="btn btn-sm" :disabled="actionLoading === '目录同步'" @click="runAction('目录同步', () => api.catalog(device!.id), true)">执行</button></div>
                <div class="operation-row"><span><strong>厂商扩展校时</strong><small>标准校时由 REGISTER 200 OK Date 或 NTP 完成</small></span><button class="btn btn-sm" :disabled="actionLoading === '厂商扩展校时'" @click="runAction('厂商扩展校时', () => api.timeSync(device!.id))">执行</button></div>
              </article>
              <article class="card detail-basic-config detail-subscription-card">
                <div class="card-head">
                  <div><h3 class="card-title">事件订阅管理</h3><p class="card-sub">创建、续订或取消设备及通道事件订阅</p></div>
                  <Radio />
                </div>
                <div class="operation-notice" :class="{ warning: !selectedSubscription.available }">
                  <Info />{{ selectedSubscription.available ? `${selectedSubscription.name}可按当前协议档案使用；手工订阅需在到期前续订。` : selectedSubscription.reason }}
                </div>
                <form class="subscription-form" @submit.prevent="submitSubscription">
                  <div class="form-grid">
                    <label class="form-group"><span class="form-label">事件类型</span><select v-model="subscriptionForm.event" class="select w-full"><option v-for="item in subscriptionItems" :key="item.event" :value="item.event" :disabled="!item.available">{{ item.name }} · {{ item.available ? item.event : item.reason }}</option></select></label>
                    <label class="form-group"><span class="form-label">有效期（秒）</span><input v-model.number="subscriptionForm.expires" class="input plain w-full" type="number" min="1" step="1" required /></label>
                    <label class="form-group full"><span class="form-label">目标国标编码</span><input v-model="subscriptionForm.targetId" class="input plain w-full mono" list="subscription-targets" maxlength="20" :placeholder="`${device.device_id || device.id}（留空订阅设备本身）`" /><datalist id="subscription-targets"><option v-for="channel in relatedChannels" :key="channel.id" :value="channel.channel_id || channel.id">{{ channel.name || channel.channel_id || channel.id }}</option></datalist><small class="form-help">可填写设备下任意通道编码；候选项来自当前通道页，留空时使用设备编码。</small></label>
                  </div>

                  <div v-if="subscriptionForm.event === 'alarm'" class="form-grid subscription-filter-grid">
                    <label class="form-group"><span class="form-label">起始报警级别</span><select v-model="subscriptionForm.startAlarmPriority" class="select w-full"><option value="">不限制</option><option value="0">0 · 全部</option><option v-for="priority in 4" :key="priority" :value="String(priority)">{{ priority }}</option></select></label>
                    <label class="form-group"><span class="form-label">结束报警级别</span><select v-model="subscriptionForm.endAlarmPriority" class="select w-full"><option value="">不限制</option><option value="0">0 · 全部</option><option v-for="priority in 4" :key="priority" :value="String(priority)">{{ priority }}</option></select></label>
                    <label class="form-group"><span class="form-label">报警方式</span><input v-model="subscriptionForm.alarmMethod" class="input plain w-full mono" placeholder="例如 25 或 2/5；0 表示全部" /></label>
                    <label class="form-group"><span class="form-label">报警类型</span><input v-model="subscriptionForm.alarmType" class="input plain w-full mono" :disabled="!alarmTypeSupported" :placeholder="alarmTypeSupported ? '按报警方式填写类型' : '仅 2016 / 2022 支持'" /></label>
                    <label class="form-group"><span class="form-label">报警开始时间</span><input v-model="subscriptionForm.startAlarmTime" class="input plain w-full" type="datetime-local" step="1" /></label>
                    <label class="form-group"><span class="form-label">报警结束时间</span><input v-model="subscriptionForm.endAlarmTime" class="input plain w-full" type="datetime-local" step="1" /></label>
                  </div>
                  <div v-else-if="subscriptionForm.event === 'catalog'" class="form-grid subscription-filter-grid">
                    <label class="form-group"><span class="form-label">目录新增开始时间</span><input v-model="subscriptionForm.startTime" class="input plain w-full" type="datetime-local" step="1" /></label>
                    <label class="form-group"><span class="form-label">目录新增结束时间</span><input v-model="subscriptionForm.endTime" class="input plain w-full" type="datetime-local" step="1" /></label>
                  </div>
                  <div v-else-if="subscriptionForm.event === 'mobile_position'" class="form-grid subscription-filter-grid">
                    <label class="form-group"><span class="form-label">位置上报间隔（秒）</span><input v-model.number="subscriptionForm.interval" class="input plain w-full" type="number" min="0" step="1" /><small class="form-help">0 表示使用设备默认间隔。</small></label>
                  </div>

                  <div class="detail-form-actions">
                    <span>{{ currentSubscriptionReceipt?.status === "recovering" ? "该参数组合已持久化，等待设备在线后恢复" : currentSubscriptionReceipt?.status === "blocked" ? "设备已终止该订阅并禁止自动重订；可修改参数后重新创建，或取消持久意图" : currentSubscriptionReceipt?.runtime ? `该参数组合当前${subscriptionStatusLabel(currentSubscriptionReceipt.status)}，有效至 ${formatDate(currentSubscriptionReceipt.expiresAt)}` : currentSubscriptionReceipt ? `该参数组合于 ${formatDate(currentSubscriptionReceipt.confirmedAt)} 收到成功响应` : "当前参数组合尚未建立订阅" }}</span>
                    <button class="btn btn-primary" :disabled="!device.is_online || !selectedSubscription.available || Boolean(actionLoading)" :title="!selectedSubscription.available ? selectedSubscription.reason : !device.is_online ? '设备离线' : undefined"><LoaderCircle v-if="actionLoading.includes(selectedSubscription.name)" class="animate-spin" /><Radio v-else />{{ currentSubscriptionReceipt ? "续订" : "创建订阅" }}</button>
                  </div>
                </form>

                <section class="subscription-receipts" aria-live="polite" :aria-busy="subscriptionLoading">
                  <div class="subscription-receipts-head"><span><strong>服务端订阅运行态</strong><small>订阅意图会持久保存；服务重启或设备重新注册后建立新对话，并继续自动续订。</small></span><button type="button" class="btn btn-sm" :disabled="subscriptionLoading" @click="loadSubscriptionStates()"><LoaderCircle v-if="subscriptionLoading" class="animate-spin" /><RefreshCcw v-else />刷新</button></div>
                  <div v-if="subscriptionError" class="operation-notice warning"><ShieldAlert />{{ subscriptionError }}</div>
                  <div v-else-if="!subscriptionLoading && !subscriptionReceipts.length" class="compact-empty"><Radio /><span><strong>暂无活动订阅</strong><small>创建订阅并收到设备成功响应后，运行态会显示在这里。</small></span></div>
                  <div v-for="receipt in subscriptionReceipts" :key="receipt.key" class="operation-row">
                    <span><strong>{{ receipt.eventName }} · {{ receipt.targetId }} <span class="status" :class="receipt.status === 'active' ? 'online' : receipt.status === 'expired' || receipt.status === 'blocked' ? 'offline' : 'pending'">{{ subscriptionStatusLabel(receipt.status) }}</span></strong><small v-if="receipt.status === 'recovering'">持久化意图等待恢复<template v-if="receipt.nextAttemptAt"> · 下次尝试 {{ formatDate(receipt.nextAttemptAt) }}</template><template v-if="receipt.lastError"> · {{ receipt.lastError }}</template></small><small v-else-if="receipt.status === 'blocked'">设备终止原因 {{ receipt.lastError || '禁止自动重订' }}；修改参数并重新创建可解除阻止</small><small v-else>协商有效期 {{ receipt.expires }} 秒 · 有效至 {{ formatDate(receipt.expiresAt || receipt.confirmedAt) }}<template v-if="receipt.notifyCSeq"> · NOTIFY CSeq {{ receipt.notifyCSeq }}</template><template v-if="receipt.persisted"> · 已持久化</template></small></span>
                    <button type="button" class="btn btn-sm" :disabled="Boolean(actionLoading) || receipt.status === 'terminating' || receipt.status === 'expired' || (!device.is_online && receipt.status !== 'recovering' && receipt.status !== 'blocked')" @click="cancelSubscription(receipt)">取消订阅</button>
                  </div>
                </section>
              </article>
              <article class="card detail-basic-config">
                <div class="card-head"><div><h3 class="card-title">BasicParam 基础参数</h3><p class="card-sub">配置将通过 SIP 下发至在线设备</p></div><span class="protocol-tag blue">2014+</span></div>
                <div class="operation-notice"><Info />{{ basicConfigAvailability.available ? "当前档案支持配置写入；设备返回响应后才表示生效。" : basicConfigAvailability.reason }}</div>
                <form @submit.prevent="saveBasic">
                  <div class="form-grid">
                    <label class="form-group"><span class="form-label">设备名称</span><input v-model="basicForm.name" class="input plain w-full" :disabled="!basicConfigAvailability.available" required /></label>
                    <label class="form-group"><span class="form-label">注册过期时间（秒）</span><input v-model.number="basicForm.expiration" class="input plain w-full" type="number" min="3600" :disabled="!basicConfigAvailability.available" required /></label>
                    <label class="form-group"><span class="form-label">心跳间隔（秒）</span><input v-model.number="basicForm.heartbeat_interval" class="input plain w-full" type="number" min="1" :disabled="!basicConfigAvailability.available" required /></label>
                    <label class="form-group"><span class="form-label">心跳超时次数</span><input v-model.number="basicForm.heartbeat_count" class="input plain w-full" type="number" min="1" :disabled="!basicConfigAvailability.available" required /></label>
                  </div>
                  <div class="detail-form-actions"><span>{{ basicConfigAvailability.reason }}</span><button class="btn btn-primary" :disabled="!device.is_online || !basicConfigAvailability.available || actionLoading === '下发 BasicParam'" :title="!basicConfigAvailability.available ? basicConfigAvailability.reason : !device.is_online ? '设备离线' : undefined"><LoaderCircle v-if="actionLoading === '下发 BasicParam'" class="animate-spin" /><UploadCloud v-else />下发配置</button></div>
                </form>
              </article>
              <article class="card detail-basic-config detail-advanced-config">
                <div class="card-head">
                  <div><h3 class="card-title">DeviceConfig 高级配置</h3><p class="card-sub">覆盖当前协议档案允许的设备配置节点</p></div>
                  <span class="protocol-tag blue">{{ selectedAdvancedConfig.versions.join(" / ") }}</span>
                </div>
                <div class="operation-notice" :class="{ warning: !advancedConfigAvailability.available }"><Info />{{ advancedConfigAvailability.reason }}</div>
                <form @submit.prevent="saveAdvancedConfig">
                  <label class="form-group full">
                    <span class="form-label">配置类型</span>
                    <select class="select w-full" :value="advancedConfigForm.section" :disabled="actionLoading === advancedConfigActionName" @change="changeAdvancedConfig">
                      <option v-for="option in advancedConfigOptions" :key="option.key" :value="option.key">{{ option.label }} · {{ option.versions.join(" / ") }}</option>
                    </select>
                  </label>
                  <label class="form-group full">
                    <span class="form-label">配置内容（JSON）</span>
                    <textarea v-model="advancedConfigForm.payload" class="textarea mono detail-config-json" rows="11" spellcheck="false" :disabled="!advancedConfigAvailability.available || actionLoading === advancedConfigActionName" aria-describedby="advanced-config-help" />
                    <span id="advanced-config-help" class="form-help">示例可直接修改。<code>inner_xml</code> 只填写配置节点内部 XML，不要包含 {{ selectedAdvancedConfig.label }} 根节点；服务端会按当前版本严格校验。</span>
                  </label>
                  <div class="detail-form-actions">
                    <button type="button" class="btn" :disabled="actionLoading === advancedConfigActionName" @click="restoreAdvancedConfigExample">恢复示例</button>
                    <button class="btn btn-primary" :disabled="!device.is_online || !advancedConfigAvailability.available || Boolean(actionLoading)" :title="!advancedConfigAvailability.available ? advancedConfigAvailability.reason : !device.is_online ? '设备离线' : undefined">
                      <LoaderCircle v-if="actionLoading === advancedConfigActionName" class="animate-spin" /><UploadCloud v-else />下发高级配置
                    </button>
                  </div>
                </form>
              </article>
            </section>
          </template>

          <template v-else>
            <section class="detail-section-head"><div><h2>协议诊断</h2><p>核验版本能力，执行在线探测并查看协议扩展结果。</p></div><RouterLink class="btn" :to="`/diagnostics?device=${encodeURIComponent(device.id)}`">打开完整诊断</RouterLink></section>
            <section class="detail-diagnostics-grid">
              <article class="card detail-version-card">
                <div class="card-head"><div><h3 class="card-title">协议档案</h3><p class="card-sub">自动协商与手动覆盖结果</p></div><ShieldCheck /></div>
                <dl class="detail-definition-list compact"><div><dt>有效版本</dt><dd>{{ protocolVersion }}</dd></div><div><dt>声明版本</dt><dd>{{ device.ext?.gb_declared_version || "—" }}</dd></div><div><dt>版本来源</dt><dd>{{ device.ext?.gb_version_source || "—" }}</dd></div><div><dt>版本更新时间</dt><dd>{{ device.ext?.gb_version_updated_at ? formatDate(device.ext.gb_version_updated_at * 1000) : "—" }}</dd></div><div><dt>手动覆盖</dt><dd>{{ device.ext?.gb_manual_version || "未设置" }}</dd></div><div><dt>REGISTER 绑定</dt><dd><span class="status" :class="registrationState.tone">{{ registrationState.badge }}</span></dd></div></dl>
                <div class="detail-capability-matrix">
                  <div v-for="item in capabilityProfile" :key="item.key" :class="item.state">
                    <span><strong>{{ item.label }}</strong><small>{{ item.minimum }} · {{ item.reason }}</small></span>
                    <b>{{ item.state === "ready" ? "可用" : item.state === "disabled" ? "已禁用" : "不支持" }}</b>
                  </div>
                </div>
              </article>
              <article class="card detail-operation-card">
                <div class="card-head"><div><h3 class="card-title">能力探测</h3><p class="card-sub">仅在线设备可执行</p></div><Search /></div>
                <div class="operation-row"><span><strong>OPTIONS 探测</strong><small>刷新版本和能力协商</small></span><button class="btn btn-sm" :disabled="!device.is_online || actionLoading === 'OPTIONS 探测'" @click="runAction('OPTIONS 探测', () => api.optionsProbe(device!.id, { timeout: 5 }), true)">重测</button></div>
                <div class="operation-row"><span><strong>PTZ stop 探测</strong><small>核验通道控制能力</small></span><button class="btn btn-sm" :disabled="!device.is_online || actionLoading === 'PTZ 探测'" @click="runAction('PTZ 探测', () => api.devicePtzProbe(device!.id, { action: 'stop', speed: 30, timeout: 5 }), true)">重测</button></div>
                <div class="operation-row"><span><strong>A.4 扩展快照</strong><small>{{ a4Availability.reason }}</small></span><button class="btn btn-sm" :disabled="!a4Availability.available || actionLoading === '查询 A.4 快照'" :title="!a4Availability.available ? a4Availability.reason : undefined" @click="queryA4">查询</button></div>
              </article>
              <article class="card detail-raw-card">
                <details open><summary><span><Settings2 /><strong>诊断快照</strong></span><small>后端实时诊断结果</small></summary><pre>{{ diagnostics ? JSON.stringify(diagnostics, null, 2) : "当前后端未返回诊断快照。" }}</pre></details>
                <details v-if="a4" open><summary><span><Download /><strong>A.4 查询结果</strong></span><small>最近一次查询</small></summary><pre>{{ JSON.stringify(a4, null, 2) }}</pre></details>
              </article>
            </section>
          </template>
        </div>
      </section>
      <ModalDialog
        :open="Boolean(historyKind)"
        :title="`${device.name || device.device_id || '设备'} · ${historyTitle}`"
        description="按时间倒序展示后端持久化的最近 100 条记录。"
        @close="historyKind = null"
      >
        <div class="history-summary">
          <span>{{ historyKind === "heartbeat" ? "最近心跳" : "最近注册" }}</span>
          <strong>{{ relativeTime(historyKind === "heartbeat" ? device.keepalive_at : device.registered_at) }}</strong>
        </div>
        <div class="table-wrap history-table-wrap">
          <table class="data-table history-table">
            <thead><tr><th>序号</th><th>时间</th><th>间隔（秒）</th><th>来源地址</th><th>状态</th></tr></thead>
            <tbody>
              <tr v-for="(record, index) in historyRows" :key="record.id">
                <td>{{ index + 1 }}</td>
                <td>{{ formatDate(record.recorded_at) }}</td>
                <td>{{ record.interval_seconds || "—" }}</td>
                <td class="mono">{{ record.address || "—" }}</td>
                <td>{{ record.status || "—" }}</td>
              </tr>
            </tbody>
          </table>
          <div v-if="historyLoading" class="empty-state"><LoaderCircle class="mx-auto mb-2 animate-spin" />正在加载历史记录…</div>
          <div v-else-if="historyError && historyRows.length" class="history-inline-warning"><ShieldAlert /><span>{{ historyError }}</span><button class="btn btn-sm" @click="historyKind && openHistory(historyKind)">重试</button></div>
          <div v-else-if="historyError" class="empty-state empty-action"><ShieldAlert /><strong>历史记录加载失败</strong><span>{{ historyError }}</span><button class="btn" @click="historyKind && openHistory(historyKind)">重试</button></div>
          <div v-else-if="!historyRows.length" class="empty-state">当前仅有最近{{ historyKind === "heartbeat" ? "心跳" : "注册" }}时间，尚无已持久化的明细记录。</div>
        </div>
      </ModalDialog>
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
              将同时删除 {{ channelTotal }} 路关联通道；录像和事件历史不会在此同步删除。
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
            ><span class="form-label">{{ isGb ? "国标编码" : "终端编号" }}</span
            ><input
              v-model="editForm.device_id"
              class="input plain w-full mono"
              :disabled="isGb"
              :title="isGb ? '国标编码是协议身份，如需更换请删除后重新添加设备' : ''" /></label
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
            ><span class="select-control">
              <select
                v-model.number="editForm.stream_mode"
                class="select w-full"
              >
                <option :value="0">UDP</option>
                <option :value="1">TCP 被动模式</option>
                <option :value="2">TCP 主动模式</option>
              </select>
              <ChevronDown aria-hidden="true" />
            </span></label
          ><label v-if="isGb" class="form-group"
            ><span class="form-label">GB 版本覆盖</span
            ><span class="select-control">
              <select v-model="editForm.gb_version" class="select w-full">
                <option value="">自动协商</option>
                <option value="1.0">1.0 / 2011</option>
                <option value="1.1">1.1 / 2014</option>
                <option value="2.0">2.0 / 2016</option>
                <option value="3.0">3.0 / 2022</option>
              </select>
              <ChevronDown aria-hidden="true" />
            </span></label
          >
          <div v-if="isGb" class="form-group full">
            <span class="form-label">按设备禁用扩展能力</span>
            <div class="capability-tags">
              <label v-for="item in gbCapabilityOptions" :key="item[0]" class="protocol-tag">
                <input v-model="editForm.gb_disabled_capabilities" type="checkbox" :value="item[0]" />
                {{ item[1] }}
              </label>
            </div>
            <small class="section-note">仅用于设备声明了某版本、但固件未实现其中部分能力的兼容场景。</small>
          </div>
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

<style scoped>
.detail-capability-matrix {
  display: grid;
  gap: 7px;
  margin-top: 14px;
}

.detail-advanced-config form {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: 14px;
}

.detail-subscription-card {
  grid-column: 1 / -1;
}

.subscription-form {
  display: grid;
  gap: 15px;
}

.subscription-filter-grid {
  padding-top: 15px;
  border-top: 1px solid var(--line);
}

.subscription-form .form-help {
  display: block;
  margin-top: 6px;
  color: var(--muted);
  font-size: 10px;
  line-height: 1.5;
}

.subscription-receipts {
  margin-top: 17px;
  padding-top: 15px;
  border-top: 1px solid var(--line);
}

.subscription-receipts-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 8px;
}

.subscription-receipts-head strong,
.subscription-receipts-head small {
  display: block;
}

.subscription-receipts-head strong {
  font-size: 12px;
}

.subscription-receipts-head small {
  margin-top: 3px;
  color: var(--muted);
  font-size: 10px;
}

.detail-config-json {
  min-height: 220px;
  line-height: 1.55;
  tab-size: 2;
}

.detail-advanced-config .form-help {
  display: block;
  margin-top: 7px;
  color: var(--muted);
  font-size: 10px;
  line-height: 1.55;
}

.detail-advanced-config .form-help code {
  color: var(--ink);
}

.detail-advanced-config .operation-notice.warning,
.detail-subscription-card .operation-notice.warning {
  color: #7c4a03;
  background: #fff8e8;
  border-color: #f2d49b;
}

.detail-advanced-config .detail-form-actions {
  margin-top: 4px;
}

.detail-capability-matrix > div {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 9px 10px;
  background: #f7f9fb;
  border: 1px solid var(--line);
  border-radius: 8px;
}

.detail-capability-matrix span,
.detail-capability-matrix strong,
.detail-capability-matrix small {
  display: block;
}

.detail-capability-matrix strong { color: var(--ink); font-size: 12px; }
.detail-capability-matrix small { margin-top: 2px; color: var(--muted); font-size: 10px; }
.detail-capability-matrix b { flex: 0 0 auto; color: #69778a; font-size: 11px; }
.detail-capability-matrix .ready { background: #f0f8f4; border-color: #cbe5d8; }
.detail-capability-matrix .ready b { color: #167653; }
.detail-capability-matrix .disabled { background: #fff8e9; border-color: #edd9ad; }
.detail-capability-matrix .disabled b { color: #9a5b08; }
</style>
