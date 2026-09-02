<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, reactive, ref } from "vue";
import { RouterLink, useRoute } from "vue-router";
import {
  Copy,
  History,
  Info,
  KeyRound,
  LoaderCircle,
  Network,
  Plus,
  RadioTower,
  RefreshCcw,
  Save,
  ShieldAlert,
  ShieldCheck,
  SquareStack,
  Trash2,
} from "@lucide/vue";
import { api, errorMessage } from "../services/api";
import type {
  CascadePlatformStatus,
  SipAlarmReceiver,
  SipAnnexGSystem,
  SipConfig,
  SipPeerSecretStatus,
  SipSecretStatus,
  SipUpstream,
} from "../types/api";
import { useUiStore } from "../stores/ui";
import { formatDate } from "../utils/format";

type EditableUpstream = SipUpstream & {
  keepalive_seconds: number;
  password_input: string;
  password_configured: boolean;
  clear_password: boolean;
  signal_digest_seed_configured: boolean;
  clear_signal_digest_seed: boolean;
  shared_channels_input: string;
  channel_id_map_input: string;
  media_allowed_cidrs_input: string;
  monitor_user_identity: NonNullable<SipUpstream["monitor_user_identity"]>;
  trusted_gateway_ids_input: string;
  allowed_user_ids_input: string;
  allowed_organizations_input: string;
  allowed_categories_input: string;
  allowed_ranks_input: string;
};

type EditableAlarmReceiver = SipAlarmReceiver & {
  source_ids_input: string;
};

type EditableAnnexGSystem = SipAnnexGSystem & {
  password_input: string;
  password_configured: boolean;
  clear_password: boolean;
  signal_digest_seed_configured: boolean;
  clear_signal_digest_seed: boolean;
  source_cidrs_input: string;
};

const ui = useUiStore();
const route = useRoute();
const loading = ref(false);
const saving = ref(false);
const loadError = ref("");
const statusLoading = ref(false);
const statusError = ref("");
const cascadeStatuses = ref<CascadePlatformStatus[]>([]);
const sipSecrets = ref<SipSecretStatus>({});
const alarmReceivers = ref<EditableAlarmReceiver[]>([]);
const upstreams = ref<EditableUpstream[]>([]);
const annexGSystems = ref<EditableAnnexGSystem[]>([]);
const passwordInput = ref("");
const currentPassword = ref("");
const signalDigestSeedInput = ref("");
const clearSignalDigestSeed = ref(false);
const deviceCertificatesInput = ref("");
const directDeviceAllowlistInput = ref("");
const directAllowedCIDRsInput = ref("");
const form = reactive({
  host: "",
  port: 5060,
  id: "",
  domain: "",
  enable_tls: false,
  tls_port: 5061,
  tls_cert: "",
  tls_key: "",
  tls_client_ca: "",
  tls_require_client_cert: false,
  register_redirect: "",
  strict_source_check: true,
  require_message_auth: false,
  ptz_weak_confirm: false,
  device_history: { max_records: 1000, max_days: 30 },
});
const registerCertificateAuth = reactive({
  enabled: false,
  required: false,
  platform_cert: "",
  platform_key: "",
  device_ca: "",
  crl: "",
});
const signalDigest = reactive({
  enabled: false,
  required: false,
  algorithm: "MD5",
  encoding: "base64",
  accept_legacy_hex: true,
  window_seconds: 600,
});
const directDownload = reactive({
  enabled: false,
  cascade_relay_enabled: false,
  storage_dir: "./configs/downloads/gb28181",
  retain_days: 7,
  offer_port: 9,
  relay_listen_ip: "0.0.0.0",
  relay_advertise_ip: "",
  relay_port_start: 30200,
  relay_port_end: 30300,
  max_file_size_mb: 10240,
  global_concurrency: 4,
  device_concurrency: 1,
  dial_timeout_seconds: 5,
  first_byte_timeout_seconds: 15,
  idle_timeout_seconds: 30,
  total_timeout_seconds: 7200,
  allow_address_mismatch: false,
});
const annexG = reactive({
  enabled: false,
  max_send_records: 100,
  inbound_rate: 50,
  inbound_burst: 100,
  pending_ttl_seconds: 86400,
  max_pending: 4096,
});
const annexGConfigSnippet = computed(() => {
  const stringValue = (value: string) => JSON.stringify(value || "");
  const stringList = (values: string[]) => `[${values.map(stringValue).join(", ")}]`;
  const lines = [
    "[Sip.AnnexG]",
    `Enabled = ${annexG.enabled}`,
    `MaxSendRecords = ${annexG.max_send_records}`,
    `InboundRate = ${annexG.inbound_rate}`,
    `InboundBurst = ${annexG.inbound_burst}`,
    `PendingTTL = ${stringValue(`${annexG.pending_ttl_seconds}s`)}`,
    `MaxPending = ${annexG.max_pending}`,
  ];
  annexGSystems.value.forEach((item) => {
    lines.push(
      "",
      "[[Sip.AnnexG.Systems]]",
      `ID = ${stringValue(item.id)}`,
      `Role = ${stringValue(item.role)}`,
      `Version = ${stringValue(item.version)}`,
      `Password = ${stringValue(item.password_configured ? "replace-with-current-secret" : "replace-with-secret")}`,
      `Realm = ${stringValue(item.realm || item.id.slice(0, 10))}`,
      `Address = ${stringValue(item.address)}`,
      `Transport = ${stringValue(item.transport || "tls")}`,
      `SourceCIDRs = ${stringList(parseListInput(item.source_cidrs_input))}`,
      `AllowInsecureTransport = ${Boolean(item.allow_insecure_transport)}`,
    );
    if (item.signal_digest_seed_configured) lines.push(`SignalDigestSeed = ${stringValue("replace-with-current-seed")}`);
    if (item.tls_ca) lines.push(`TLSCA = ${stringValue(item.tls_ca)}`);
    if (item.tls_server_name) lines.push(`TLSServerName = ${stringValue(item.tls_server_name)}`);
    if (item.tls_cert) lines.push(`TLSCert = ${stringValue(item.tls_cert)}`);
    if (item.tls_key) lines.push(`TLSKey = ${stringValue(item.tls_key)}`);
  });
  return lines.join("\n");
});
const tlsConfigValid = computed(
  () =>
    !form.enable_tls ||
    (Boolean(form.tls_cert.trim()) &&
      Boolean(form.tls_key.trim()) &&
      (!form.tls_require_client_cert || Boolean(form.tls_client_ca.trim())))
);
const certificateConfigValid = computed(() => {
  if (!registerCertificateAuth.enabled && !registerCertificateAuth.required) return true;
  try {
    return Boolean(
      registerCertificateAuth.platform_cert.trim() &&
      registerCertificateAuth.platform_key.trim() &&
      parseKeyValueLines(deviceCertificatesInput.value).length &&
      (!registerCertificateAuth.crl.trim() || registerCertificateAuth.device_ca.trim())
    );
  } catch {
    return false;
  }
});
function isIPv4Literal(value: string) {
  const parts = value.split(".");
  return parts.length === 4 && parts.every((part) => /^(0|[1-9]\d{0,2})$/.test(part) && Number(part) <= 255);
}

function isIPLiteral(value: string) {
  const normalized = value.trim();
  if (isIPv4Literal(normalized)) return true;
  if (!normalized.includes(":") || !/^[\da-f:.]+$/i.test(normalized)) return false;
  try {
    return new URL(`http://[${normalized}]/`).hostname.startsWith("[");
  } catch {
    return false;
  }
}

function isUnspecifiedIP(value: string) {
  const normalized = value.trim().toLowerCase();
  return normalized === "0.0.0.0" || normalized === "::" || normalized === "0:0:0:0:0:0:0:0";
}

function isMulticastIP(value: string) {
  const normalized = value.trim().toLowerCase();
  if (isIPv4Literal(normalized)) {
    const first = Number(normalized.split(".")[0]);
    return first >= 224 && first <= 239;
  }
  return normalized.startsWith("ff");
}

function isCIDRLiteral(value: string) {
  const [address, prefix, ...rest] = value.trim().split("/");
  if (rest.length || !address || !/^\d+$/.test(prefix || "") || !isIPLiteral(address)) return false;
  const prefixLength = Number(prefix);
  return prefixLength >= 0 && prefixLength <= (isIPv4Literal(address) ? 32 : 128);
}

const directCIDRError = computed(() => {
  if (!directDownload.allow_address_mismatch) return "";
  const cidrs = parseListInput(directAllowedCIDRsInput.value);
  if (!cidrs.length) return "允许地址不一致时，至少填写一个设备媒体地址 CIDR。";
  if (new Set(cidrs).size !== cidrs.length) return "设备媒体地址 CIDR 不能重复。";
  const invalid = cidrs.find((value) => !isCIDRLiteral(value));
  return invalid ? `“${invalid}”不是有效 CIDR。` : "";
});

const directDownloadConfigValid = computed(() => {
  if (!directDownload.enabled && !directDownload.cascade_relay_enabled) return true;
  const devices = parseListInput(directDeviceAllowlistInput.value);
  const phaseTimeouts = [
    directDownload.dial_timeout_seconds,
    directDownload.first_byte_timeout_seconds,
    directDownload.idle_timeout_seconds,
  ];
  return Boolean(
    (!directDownload.enabled || (directDownload.storage_dir.trim() && directDownload.retain_days > 0)) &&
    (!directDownload.cascade_relay_enabled || (
      isIPLiteral(directDownload.relay_listen_ip) &&
      !isMulticastIP(directDownload.relay_listen_ip) &&
      (!directDownload.relay_advertise_ip.trim() || (
        isIPLiteral(directDownload.relay_advertise_ip) &&
        !isUnspecifiedIP(directDownload.relay_advertise_ip) &&
        !isMulticastIP(directDownload.relay_advertise_ip)
      )) &&
      directDownload.relay_port_start >= 1 &&
      directDownload.relay_port_start <= 65535 &&
      directDownload.relay_port_end >= directDownload.relay_port_start &&
      directDownload.relay_port_end <= 65535
    )) &&
    devices.length &&
    devices.every((value) => /^\d{20}$/.test(value)) &&
    new Set(devices).size === devices.length &&
    directDownload.offer_port >= 1 && directDownload.offer_port <= 65535 &&
    directDownload.max_file_size_mb > 0 &&
    directDownload.global_concurrency > 0 &&
    directDownload.device_concurrency > 0 &&
    directDownload.device_concurrency <= directDownload.global_concurrency &&
    phaseTimeouts.every((value) => value > 0 && directDownload.total_timeout_seconds >= value) &&
    !directCIDRError.value
  );
});
let statusTimer: number | undefined;

function durationSeconds(value?: number) {
  const duration = Number(value || 0);
  if (!duration) return 60;
  return duration > 3_600 ? Math.max(1, Math.round(duration / 1_000_000_000)) : duration;
}

function durationInputSeconds(value: number | string | undefined, fallback: number) {
  if (typeof value === "string") {
    const match = value.trim().match(/^([\d.]+)(ns|us|µs|ms|s|m|h)$/);
    if (!match) return fallback;
    const multipliers: Record<string, number> = { ns: 1e-9, us: 1e-6, "µs": 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };
    return Number(match[1]) * multipliers[match[2]];
  }
  if (!Number.isFinite(Number(value))) return fallback;
  const number = Number(value);
  return number > 86_400 ? number / 1_000_000_000 : number;
}

function durationNanoseconds(seconds: number) {
  return Math.round(Number(seconds || 0) * 1_000_000_000);
}

function listInput(values?: string[]) {
  return (values || []).join("\n");
}

function parseListInput(value: string) {
  return value.split(/[\s,]+/).map((item) => item.trim()).filter(Boolean);
}

function parseKeyValueLines(value: string) {
  return value.split(/\r?\n/).map((line) => line.trim()).filter(Boolean).map((line) => {
    const separator = line.indexOf("=");
    if (separator < 1) throw new Error(`证书映射“${line}”缺少 = 分隔符`);
    const key = line.slice(0, separator).trim();
    const path = line.slice(separator + 1).trim();
    if (!/^\d{20}$/.test(key) || !path) throw new Error(`证书映射“${line}”必须为 20 位设备 ID=证书路径`);
    return [key, path] as const;
  });
}

function mappingInput(values?: Record<string, string>) {
  return Object.entries(values || {}).map(([key, value]) => `${key}=${value}`).join("\n");
}

function editableAlarmReceiver(item: Partial<SipAlarmReceiver> = {}): EditableAlarmReceiver {
  return {
    name: item.name || "",
    enabled: item.enabled ?? false,
    device_id: item.device_id || "",
    source_ids: item.source_ids || [],
    source_ids_input: listInput(item.source_ids),
  };
}

function alarmReceiverPayload(item: EditableAlarmReceiver): SipAlarmReceiver {
  return {
    name: item.name.trim(),
    enabled: Boolean(item.enabled),
    device_id: item.device_id.trim(),
    source_ids: parseListInput(item.source_ids_input),
  };
}

function addAlarmReceiver() {
  alarmReceivers.value.push(editableAlarmReceiver({ name: `receiver-${alarmReceivers.value.length + 1}` }));
}

function removeAlarmReceiver(index: number) {
  alarmReceivers.value.splice(index, 1);
}

function editableAnnexGSystem(
  item: Partial<SipAnnexGSystem> = {},
  secretStatus: SipPeerSecretStatus = {},
): EditableAnnexGSystem {
  return {
    id: item.id || "",
    role: item.role || "emergency_command_system",
    version: item.version || "1.0",
    password: "",
    password_input: "",
    password_configured: Boolean(secretStatus.password_configured),
    clear_password: false,
    signal_digest_seed: "",
    signal_digest_seed_configured: Boolean(secretStatus.signal_digest_seed_configured),
    clear_signal_digest_seed: false,
    realm: item.realm || "",
    address: item.address || "",
    transport: item.transport || "tls",
    source_cidrs: item.source_cidrs || [],
    source_cidrs_input: listInput(item.source_cidrs),
    allow_insecure_transport: item.allow_insecure_transport ?? false,
    tls_ca: item.tls_ca || "",
    tls_server_name: item.tls_server_name || "",
    tls_cert: item.tls_cert || "",
    tls_key: item.tls_key || "",
  };
}

async function copyAnnexGConfig() {
  try {
    await navigator.clipboard.writeText(annexGConfigSnippet.value);
    ui.toast("附录 G 配置片段已复制；请先替换密钥占位符");
  } catch {
    ui.toast("复制失败，请检查浏览器剪贴板权限");
  }
}

function editableUpstream(
  item: Partial<SipUpstream> = {},
  secretStatus: SipPeerSecretStatus = {},
): EditableUpstream {
  return {
    name: item.name || "",
    enabled: item.enabled ?? true,
    server_id: item.server_id || "",
    domain: item.domain || "",
    host: item.host || "",
    port: item.port ?? 0,
    transport: item.transport || "udp",
    tls_ca: item.tls_ca || "",
    tls_cert: item.tls_cert || "",
    tls_key: item.tls_key || "",
    tls_server_name: item.tls_server_name || "",
    local_id: item.local_id || "",
    local_domain: item.local_domain || "",
    local_host: item.local_host || "",
    local_port: item.local_port || 0,
    password: "",
    password_input: "",
    password_configured: Boolean(secretStatus.password_configured),
    clear_password: false,
    register_certificate_auth: item.register_certificate_auth ? { ...item.register_certificate_auth } : undefined,
    signal_digest_seed: item.signal_digest_seed || "",
    signal_digest_seed_configured: Boolean(secretStatus.signal_digest_seed_configured),
    clear_signal_digest_seed: false,
    monitor_user_identity: {
      enabled: item.monitor_user_identity?.enabled ?? false,
      required: item.monitor_user_identity?.required ?? false,
      local_gateway_id: item.monitor_user_identity?.local_gateway_id || "",
      remote_gateway_id: item.monitor_user_identity?.remote_gateway_id || "",
      local_user_id: item.monitor_user_identity?.local_user_id || "",
      local_organization: item.monitor_user_identity?.local_organization || "",
      local_category: item.monitor_user_identity?.local_category || "",
      local_rank: item.monitor_user_identity?.local_rank || "",
      trusted_gateway_ids: item.monitor_user_identity?.trusted_gateway_ids || [],
      allowed_user_ids: item.monitor_user_identity?.allowed_user_ids || [],
      allowed_organizations: item.monitor_user_identity?.allowed_organizations || [],
      allowed_categories: item.monitor_user_identity?.allowed_categories || [],
      allowed_ranks: item.monitor_user_identity?.allowed_ranks || [],
      max_hops: item.monitor_user_identity?.max_hops || 8,
    },
    trusted_gateway_ids_input: listInput(item.monitor_user_identity?.trusted_gateway_ids),
    allowed_user_ids_input: listInput(item.monitor_user_identity?.allowed_user_ids),
    allowed_organizations_input: listInput(item.monitor_user_identity?.allowed_organizations),
    allowed_categories_input: listInput(item.monitor_user_identity?.allowed_categories),
    allowed_ranks_input: listInput(item.monitor_user_identity?.allowed_ranks),
    version: item.version || "1.0",
    expires: item.expires || 3600,
    keepalive_interval: item.keepalive_interval || 0,
    keepalive_seconds: durationSeconds(item.keepalive_interval),
    alarm_dispatch_enabled: item.alarm_dispatch_enabled ?? false,
    shared_channels: item.shared_channels || [],
    channel_id_map: item.channel_id_map || {},
    shared_channels_input: (item.shared_channels || []).join("\n"),
    channel_id_map_input: Object.keys(item.channel_id_map || {}).length
      ? JSON.stringify(item.channel_id_map, null, 2)
      : "",
    media_allowed_cidrs: item.media_allowed_cidrs || [],
    media_allowed_cidrs_input: (item.media_allowed_cidrs || []).join("\n"),
  };
}

function upstreamPayload(item: EditableUpstream): SipUpstream {
  const sharedChannels = item.shared_channels_input
    .split(/[\s,]+/)
    .map((value) => value.trim())
    .filter(Boolean);
  let channelIDMap: Record<string, string> = {};
  if (item.channel_id_map_input.trim()) {
    const parsed = JSON.parse(item.channel_id_map_input) as unknown;
    if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") {
      throw new Error(`${item.name || "上级平台"}的编码映射必须是 JSON 对象`);
    }
    channelIDMap = Object.fromEntries(
      Object.entries(parsed as Record<string, unknown>).map(([key, value]) => [key.trim(), String(value).trim()])
    );
  }
  return {
    name: item.name.trim(),
    enabled: item.enabled,
    server_id: item.server_id.trim(),
    domain: item.domain?.trim(),
    host: item.host.trim(),
    port: item.port,
    transport: item.transport || "udp",
    tls_ca: item.tls_ca?.trim(),
    tls_cert: item.tls_cert?.trim(),
    tls_key: item.tls_key?.trim(),
    tls_server_name: item.tls_server_name?.trim(),
    local_id: item.local_id?.trim(),
    local_domain: item.local_domain?.trim(),
    local_host: item.local_host?.trim(),
    local_port: item.local_port || undefined,
    password: item.clear_password || item.password_input === "" ? undefined : item.password_input,
    register_certificate_auth: item.register_certificate_auth ? { ...item.register_certificate_auth } : undefined,
    signal_digest_seed: item.clear_signal_digest_seed || item.signal_digest_seed === ""
      ? undefined
      : item.signal_digest_seed,
    monitor_user_identity: {
      enabled: Boolean(item.monitor_user_identity.enabled),
      required: Boolean(item.monitor_user_identity.required),
      local_gateway_id: item.monitor_user_identity.local_gateway_id?.trim(),
      remote_gateway_id: item.monitor_user_identity.remote_gateway_id?.trim(),
      local_user_id: item.monitor_user_identity.local_user_id?.trim(),
      local_organization: item.monitor_user_identity.local_organization?.trim(),
      local_category: item.monitor_user_identity.local_category?.trim(),
      local_rank: item.monitor_user_identity.local_rank?.trim(),
      trusted_gateway_ids: parseListInput(item.trusted_gateway_ids_input),
      allowed_user_ids: parseListInput(item.allowed_user_ids_input),
      allowed_organizations: parseListInput(item.allowed_organizations_input),
      allowed_categories: parseListInput(item.allowed_categories_input),
      allowed_ranks: parseListInput(item.allowed_ranks_input),
      max_hops: item.monitor_user_identity.max_hops || 8,
    },
    version: item.version,
    expires: item.expires,
    keepalive_interval: Math.round(item.keepalive_seconds * 1_000_000_000),
    alarm_dispatch_enabled: Boolean(item.alarm_dispatch_enabled),
    shared_channels: sharedChannels,
    channel_id_map: channelIDMap,
    media_allowed_cidrs: item.media_allowed_cidrs_input
      .split(/[\s,]+/)
      .map((value) => value.trim())
      .filter(Boolean),
  };
}

function addUpstream() {
  upstreams.value.push(editableUpstream({
    name: `upstream-${upstreams.value.length + 1}`,
    local_id: form.id,
    local_domain: form.domain,
    local_host: form.host,
  }));
}

function removeUpstream(index: number) {
  upstreams.value.splice(index, 1);
}

function statusFor(name: string) {
  return cascadeStatuses.value.find((item) => item.name === name);
}

async function loadCascadeStatuses() {
  statusLoading.value = true;
  statusError.value = "";
  try {
    const { data } = await api.cascadeStatuses();
    cascadeStatuses.value = data.items || [];
  } catch (cause) {
    statusError.value = errorMessage(cause, "级联状态加载失败");
  } finally {
    statusLoading.value = false;
  }
}

async function focusRouteSection() {
  if (!route.hash) return;
  await nextTick();
  requestAnimationFrame(() => {
    const target = document.getElementById(route.hash.slice(1));
    if (!target) return;
    if (!target.hasAttribute("tabindex")) target.setAttribute("tabindex", "-1");
    target.scrollIntoView({ block: "start" });
    target.focus({ preventScroll: true });
  });
}

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const { data } = await api.configInfo();
    const sip = data.sip || {};
    sipSecrets.value = data.sip_secrets || {};
    Object.assign(form, {
      host: sip.host || "",
      port: sip.port || 5060,
      id: sip.id || "",
      domain: sip.domain || "",
      enable_tls: Boolean(sip.enable_tls),
      tls_port: sip.tls_port || 5061,
      tls_cert: sip.tls_cert || "",
      tls_key: sip.tls_key || "",
      tls_client_ca: sip.tls_client_ca || "",
      tls_require_client_cert: Boolean(sip.tls_require_client_cert),
      register_redirect: sip.register_redirect || "",
      strict_source_check: sip.strict_source_check ?? true,
      require_message_auth: Boolean(sip.require_message_auth),
      ptz_weak_confirm: Boolean(sip.ptz_weak_confirm),
      device_history: {
        max_records: sip.device_history?.max_records ?? 1000,
        max_days: sip.device_history?.max_days ?? 30,
      },
    });
    Object.assign(registerCertificateAuth, {
      enabled: Boolean(sip.register_certificate_auth?.enabled),
      required: Boolean(sip.register_certificate_auth?.required),
      platform_cert: sip.register_certificate_auth?.platform_cert || "",
      platform_key: sip.register_certificate_auth?.platform_key || "",
      device_ca: sip.register_certificate_auth?.device_ca || "",
      crl: sip.register_certificate_auth?.crl || "",
    });
    deviceCertificatesInput.value = mappingInput(sip.register_certificate_auth?.device_certificates);
    Object.assign(signalDigest, {
      enabled: Boolean(sip.signal_digest?.enabled),
      required: Boolean(sip.signal_digest?.required),
      algorithm: sip.signal_digest?.algorithm || "MD5",
      encoding: sip.signal_digest?.encoding || "base64",
      accept_legacy_hex: sip.signal_digest?.accept_legacy_hex ?? true,
      window_seconds: durationInputSeconds(sip.signal_digest?.window, 600),
    });
    signalDigestSeedInput.value = "";
    clearSignalDigestSeed.value = false;
    Object.assign(directDownload, {
      enabled: Boolean(sip.direct_tcp_download?.enabled),
      cascade_relay_enabled: Boolean(sip.direct_tcp_download?.cascade_relay_enabled),
      storage_dir: sip.direct_tcp_download?.storage_dir || "./configs/downloads/gb28181",
      retain_days: sip.direct_tcp_download?.retain_days ?? 7,
      offer_port: sip.direct_tcp_download?.offer_port ?? 9,
      relay_listen_ip: sip.direct_tcp_download?.relay_listen_ip || "0.0.0.0",
      relay_advertise_ip: sip.direct_tcp_download?.relay_advertise_ip || "",
      relay_port_start: sip.direct_tcp_download?.relay_port_start ?? 30200,
      relay_port_end: sip.direct_tcp_download?.relay_port_end ?? 30300,
      max_file_size_mb: Math.max(1, Math.round(Number(sip.direct_tcp_download?.max_file_size || 10 * 1024 ** 3) / 1024 ** 2)),
      global_concurrency: sip.direct_tcp_download?.global_concurrency ?? 4,
      device_concurrency: sip.direct_tcp_download?.device_concurrency ?? 1,
      dial_timeout_seconds: durationInputSeconds(sip.direct_tcp_download?.dial_timeout, 5),
      first_byte_timeout_seconds: durationInputSeconds(sip.direct_tcp_download?.first_byte_timeout, 15),
      idle_timeout_seconds: durationInputSeconds(sip.direct_tcp_download?.idle_timeout, 30),
      total_timeout_seconds: durationInputSeconds(sip.direct_tcp_download?.total_timeout, 7200),
      allow_address_mismatch: Boolean(sip.direct_tcp_download?.allow_address_mismatch),
    });
    directDeviceAllowlistInput.value = listInput(sip.direct_tcp_download?.device_allowlist);
    directAllowedCIDRsInput.value = listInput(sip.direct_tcp_download?.allowed_address_cidrs);
    Object.assign(annexG, {
      enabled: Boolean(sip.annex_g?.enabled),
      max_send_records: sip.annex_g?.max_send_records ?? 100,
      inbound_rate: sip.annex_g?.inbound_rate ?? 50,
      inbound_burst: sip.annex_g?.inbound_burst ?? 100,
      pending_ttl_seconds: durationInputSeconds(sip.annex_g?.pending_ttl, 86400),
      max_pending: sip.annex_g?.max_pending ?? 4096,
    });
    annexGSystems.value = (sip.annex_g?.systems || []).map((item) =>
      editableAnnexGSystem(item, sipSecrets.value.annex_g_systems?.[item.id]),
    );
    currentPassword.value = sip.password || "";
    alarmReceivers.value = (sip.alarm_receivers || []).map(editableAlarmReceiver);
    upstreams.value = (sip.upstreams || []).map((item) =>
      editableUpstream(item, sipSecrets.value.upstreams?.[item.name]),
    );
    passwordInput.value = "";
    await loadCascadeStatuses();
  } catch (cause) {
    loadError.value = errorMessage(cause, "SIP 配置加载失败");
  } finally {
    loading.value = false;
    await focusRouteSection();
  }
}

async function save() {
  if (loadError.value) {
    ui.toast("SIP 配置尚未加载成功，请先重试加载再保存");
    return;
  }
  if (!tlsConfigValid.value) {
    ui.toast("启用 SIP-TLS 前请填写证书与私钥路径");
    return;
  }
  if (!certificateConfigValid.value) {
    ui.toast("证书注册认证需要平台证书、私钥和至少一条有效设备证书映射");
    return;
  }
  if (!directDownloadConfigValid.value) {
    ui.toast("请补齐 2014 裸 TCP 下载白名单、资源限制和地址安全参数");
    return;
  }
  saving.value = true;
  try {
    const body: SipConfig = {
      ...form,
      password: passwordInput.value || currentPassword.value,
      register_certificate_auth: {
        ...registerCertificateAuth,
        device_certificates: Object.fromEntries(parseKeyValueLines(deviceCertificatesInput.value)),
      },
      signal_digest: {
        enabled: signalDigest.enabled,
        required: signalDigest.required,
        seed: clearSignalDigestSeed.value || !signalDigestSeedInput.value ? undefined : signalDigestSeedInput.value,
        algorithm: signalDigest.algorithm,
        encoding: signalDigest.encoding,
        accept_legacy_hex: signalDigest.accept_legacy_hex,
        window: durationNanoseconds(signalDigest.window_seconds),
      },
      direct_tcp_download: {
        enabled: directDownload.enabled,
        cascade_relay_enabled: directDownload.cascade_relay_enabled,
        device_allowlist: parseListInput(directDeviceAllowlistInput.value),
        storage_dir: directDownload.storage_dir.trim(),
        retain_days: directDownload.retain_days,
        offer_port: directDownload.offer_port,
        relay_listen_ip: directDownload.relay_listen_ip.trim(),
        relay_advertise_ip: directDownload.relay_advertise_ip.trim(),
        relay_port_start: directDownload.relay_port_start,
        relay_port_end: directDownload.relay_port_end,
        max_file_size: Math.round(directDownload.max_file_size_mb * 1024 ** 2),
        global_concurrency: directDownload.global_concurrency,
        device_concurrency: directDownload.device_concurrency,
        dial_timeout: durationNanoseconds(directDownload.dial_timeout_seconds),
        first_byte_timeout: durationNanoseconds(directDownload.first_byte_timeout_seconds),
        idle_timeout: durationNanoseconds(directDownload.idle_timeout_seconds),
        total_timeout: durationNanoseconds(directDownload.total_timeout_seconds),
        allow_address_mismatch: directDownload.allow_address_mismatch,
        allowed_address_cidrs: parseListInput(directAllowedCIDRsInput.value),
      },
      alarm_receivers: alarmReceivers.value.map(alarmReceiverPayload),
      upstreams: upstreams.value.map(upstreamPayload),
      secret_clears: {
        signal_digest_seed: clearSignalDigestSeed.value,
        upstream_passwords: upstreams.value.filter((item) => item.clear_password).map((item) => item.name),
        upstream_signal_digest_seeds: upstreams.value
          .filter((item) => item.clear_signal_digest_seed)
          .map((item) => item.name),
      },
    };
    await api.updateSip(body);
    if (passwordInput.value) currentPassword.value = passwordInput.value;
    upstreams.value.forEach((item) => {
      if (item.clear_password) item.password_configured = false;
      else if (item.password_input !== "") item.password_configured = true;
      if (item.clear_signal_digest_seed) item.signal_digest_seed_configured = false;
      else if (item.signal_digest_seed !== "") item.signal_digest_seed_configured = true;
      item.password_input = "";
      item.signal_digest_seed = "";
      item.clear_password = false;
      item.clear_signal_digest_seed = false;
    });
    passwordInput.value = "";
    signalDigestSeedInput.value = "";
    clearSignalDigestSeed.value = false;
    annexGSystems.value.forEach((item) => {
      if (item.clear_password) item.password_configured = false;
      else if (item.password_input) item.password_configured = true;
      if (item.clear_signal_digest_seed) item.signal_digest_seed_configured = false;
      else if (item.signal_digest_seed) item.signal_digest_seed_configured = true;
      item.password_input = "";
      item.signal_digest_seed = "";
      item.clear_password = false;
      item.clear_signal_digest_seed = false;
    });
    ui.toast("SIP 配置已保存；可热更新项已生效");
    await loadCascadeStatuses();
  } catch (cause) {
    ui.toast(errorMessage(cause, "SIP 配置保存失败"));
  } finally {
    saving.value = false;
  }
}

onMounted(() => {
  void load();
  statusTimer = window.setInterval(() => void loadCascadeStatuses(), 10_000);
});

onUnmounted(() => {
  if (statusTimer !== undefined) window.clearInterval(statusTimer);
});
</script>

<template>
  <main class="page-content">
    <header class="page-head">
      <div>
        <h1 class="page-title">SIP 设置</h1>
        <p class="page-desc">维护 GB28181 注册入口、安全策略与历史规则；监听、证书上下文和附录 G 变更需修改配置文件并重启，其余支持项在线生效。</p>
      </div>
      <div class="head-actions">
        <span class="status" :class="loadError ? 'offline' : 'online'">{{ form.port || "—" }} / TCP+UDP</span>
        <button class="btn" :disabled="loading" @click="load">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新
        </button>
      </div>
    </header>

    <div v-if="loadError" class="warning-box mb-4" role="alert">
      <ShieldAlert /><span>{{ loadError }}</span><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>

    <section class="settings-layout">
      <nav class="card settings-nav">
        <a class="active" href="#base"><RadioTower />基础配置</a>
        <a href="#security"><ShieldCheck />信令安全</a>
        <a href="#history"><History />历史保留</a>
        <a href="#direct-download"><SquareStack />2014 下载</a>
        <a href="#annex-g"><KeyRound />附录 G</a>
        <a href="#alarm-receivers"><ShieldAlert />本域接警终端</a>
        <a href="#upstreams"><Network />上级平台</a>
      </nav>

      <form class="card form-section" @submit.prevent="save">
        <div class="card-head">
          <div><h2 class="card-title">SIP 服务</h2><p class="card-sub">修改服务 ID、域或密码前请同步设备侧配置</p></div>
          <RadioTower />
        </div>
        <div class="warning-box mb-4">
          <Info /><span>后端当前会返回完整配置。管理端不显示密码原值；留空时沿用当前值。</span>
        </div>

        <div id="base" class="form-grid">
          <label class="form-group"><span class="form-label">对外宣告地址</span><input v-model.trim="form.host" class="input plain w-full mono" placeholder="为空时自动探测" /></label>
          <label class="form-group"><span class="form-label">监听端口</span><input v-model.number="form.port" class="input plain w-full" type="number" min="1" max="65535" required /></label>
          <label class="form-group"><span class="form-label">传输协议</span><input class="input plain w-full" value="TCP + UDP" disabled /></label>
          <label class="form-group full"><span class="form-label">SIP 服务 ID</span><input v-model="form.id" class="input plain w-full mono" minlength="18" maxlength="20" required /></label>
          <label class="form-group"><span class="form-label">域</span><input v-model="form.domain" class="input plain w-full mono" required /></label>
          <label class="form-group"><span class="form-label">新注册密码</span><input v-model="passwordInput" class="input plain w-full" type="password" autocomplete="new-password" placeholder="留空保留当前密码" /></label>
        </div>

        <h3 id="security" class="section-title mt-6">安全与兼容</h3>
        <label class="toggle-row"><span><strong>启用 SIP-TLS</strong><small>需要同时配置证书与私钥</small></span><span class="switch"><input v-model="form.enable_tls" type="checkbox" /><span class="slider" /></span></label>
        <div v-if="form.enable_tls" class="form-grid mt-4">
          <label class="form-group"><span class="form-label">TLS 端口</span><input v-model.number="form.tls_port" class="input plain w-full" type="number" min="1" max="65535" /></label>
          <label class="form-group"><span class="form-label">证书路径</span><input v-model.trim="form.tls_cert" class="input plain w-full mono" required :aria-invalid="!tlsConfigValid" /></label>
          <label class="form-group"><span class="form-label">私钥路径</span><input v-model.trim="form.tls_key" class="input plain w-full mono" required :aria-invalid="!tlsConfigValid" /></label>
          <label class="form-group"><span class="form-label">客户端 CA 路径</span><input v-model.trim="form.tls_client_ca" class="input plain w-full mono" placeholder="配置后校验客户端提交的证书" :required="form.tls_require_client_cert" :aria-invalid="!tlsConfigValid" /></label>
          <label class="toggle-row full"><span><strong>强制客户端证书</strong><small>拒绝未提交或证书不受客户端 CA 信任的 TLS 连接</small></span><span class="switch"><input v-model="form.tls_require_client_cert" type="checkbox" /><span class="slider" /></span></label>
          <p v-if="!tlsConfigValid" class="field-error full" role="alert">启用 SIP-TLS 时必须配置证书与私钥；强制客户端证书时还必须配置客户端 CA。</p>
        </div>
        <label class="toggle-row"><span><strong>严格源地址校验</strong><small>校验设备上报源 IP 与注册地址一致</small></span><span class="switch"><input v-model="form.strict_source_check" type="checkbox" /><span class="slider" /></span></label>
        <label class="toggle-row"><span><strong>MESSAGE / NOTIFY 鉴权</strong><small>要求设备消息执行 Digest 鉴权</small></span><span class="switch"><input v-model="form.require_message_auth" type="checkbox" /><span class="slider" /></span></label>
        <label class="toggle-row"><span><strong>级联有应答控制弱确认</strong><small>兼容已确认 SIP、但遗漏 DeviceControl 业务应答的下级设备</small></span><span class="switch"><input v-model="form.ptz_weak_confirm" type="checkbox" /><span class="slider" /></span></label>

        <details class="config-panel mt-4" open>
          <summary><span><ShieldCheck /><strong>Date + Note 信令摘要</strong></span><small>四版本 · 默认关闭</small></summary>
          <div class="config-panel-body">
            <div class="version-note"><Info /><span>这是 GB/T 28181 信令数字摘要，不等同于 MESSAGE / NOTIFY 的 RFC Digest 鉴权。启用前需与每个设备和上级统一算法、编码与 seed。</span></div>
            <label class="toggle-row"><span><strong>发送信令摘要</strong><small>为出站请求与响应添加 Date 和 Note</small></span><span class="switch"><input v-model="signalDigest.enabled" type="checkbox" /><span class="slider" /></span></label>
            <label class="toggle-row"><span><strong>强制入站验签</strong><small>同时启用出站签名，并拒绝缺失或校验失败的摘要</small></span><span class="switch"><input v-model="signalDigest.required" type="checkbox" /><span class="slider" /></span></label>
            <div class="form-grid mt-4">
              <label class="form-group"><span class="form-label">摘要算法</span><select v-model="signalDigest.algorithm" class="input plain w-full"><option>MD5</option><option>SHA-1</option><option>SHA-256</option><option>SM3</option></select></label>
              <label class="form-group"><span class="form-label">nonce 编码</span><select v-model="signalDigest.encoding" class="input plain w-full"><option value="base64">Base64</option><option value="hex">Hex</option></select></label>
              <label class="form-group"><span class="form-label">允许时间偏差（秒）</span><input v-model.number="signalDigest.window_seconds" class="input plain w-full" type="number" min="1" max="86400" required /></label>
              <label class="form-group">
                <span class="form-label">全局新 seed</span>
                <input v-model="signalDigestSeedInput" class="input plain w-full mono" type="password" autocomplete="new-password" :disabled="clearSignalDigestSeed" :placeholder="sipSecrets.signal_digest_seed_configured ? '已配置；留空保留当前 seed' : '尚未配置；可回退使用 SIP 注册密码'" />
              </label>
              <label class="toggle-row"><span><strong>兼容十六进制 nonce</strong><small>Base64 模式下接受旧厂商 hex 写法</small></span><span class="switch"><input v-model="signalDigest.accept_legacy_hex" type="checkbox" /><span class="slider" /></span></label>
              <label class="toggle-row"><span><strong>清除全局 seed</strong><small>{{ sipSecrets.signal_digest_seed_configured ? "保存后显式清除" : "当前未配置 seed" }}</small></span><span class="switch"><input v-model="clearSignalDigestSeed" type="checkbox" :disabled="!sipSecrets.signal_digest_seed_configured" /><span class="slider" /></span></label>
            </div>
          </div>
        </details>

        <details class="config-panel mt-4">
          <summary><span><KeyRound /><strong>Capability / Asymmetric 证书注册</strong></span><small>2011 / 2014 / 2016</small></summary>
          <div class="config-panel-body">
            <div class="warning-box mb-4"><ShieldAlert /><span>该能力会改变 SIP 监听安全上下文。现有后端要求修改配置文件并重启；本区用于完整呈现和校验当前启动配置，尝试在线变更会返回明确错误。</span></div>
            <label class="toggle-row"><span><strong>接受数字证书注册</strong><small>接受 Capability / Asymmetric REGISTER 认证</small></span><span class="switch"><input v-model="registerCertificateAuth.enabled" type="checkbox" /><span class="slider" /></span></label>
            <label class="toggle-row"><span><strong>强制证书注册</strong><small>拒绝回退到普通 Digest 注册；设置后隐式启用</small></span><span class="switch"><input v-model="registerCertificateAuth.required" type="checkbox" /><span class="slider" /></span></label>
            <div class="form-grid mt-4">
              <label class="form-group"><span class="form-label">平台 X.509 证书</span><input v-model.trim="registerCertificateAuth.platform_cert" class="input plain w-full mono" :required="registerCertificateAuth.enabled || registerCertificateAuth.required" /></label>
              <label class="form-group"><span class="form-label">平台 RSA 私钥</span><input v-model.trim="registerCertificateAuth.platform_key" class="input plain w-full mono" :required="registerCertificateAuth.enabled || registerCertificateAuth.required" /></label>
              <label class="form-group"><span class="form-label">设备 CA</span><input v-model.trim="registerCertificateAuth.device_ca" class="input plain w-full mono" placeholder="留空时固定信任设备证书映射" /></label>
              <label class="form-group"><span class="form-label">证书撤销列表 CRL</span><input v-model.trim="registerCertificateAuth.crl" class="input plain w-full mono" :required="Boolean(registerCertificateAuth.crl && !registerCertificateAuth.device_ca)" /></label>
              <label class="form-group full"><span class="form-label">设备证书映射</span><textarea v-model="deviceCertificatesInput" class="input plain w-full cascade-textarea mono" :required="registerCertificateAuth.enabled || registerCertificateAuth.required" placeholder="每行一条：20 位设备 ID=/absolute/path/device.pem" /><span class="form-help">设备 ID 与证书必须一一绑定；启用时至少配置一条。</span></label>
              <p v-if="!certificateConfigValid" class="field-error full" role="alert">请补齐平台证书、私钥和有效设备证书映射；使用 CRL 时还必须配置设备 CA。</p>
            </div>
          </div>
        </details>

        <details class="config-panel mt-4">
          <summary><span><RefreshCcw /><strong>2022 REGISTER 重定向</strong></span><small>仅 2022</small></summary>
          <div class="config-panel-body">
            <label class="form-group"><span class="form-label">重定向目标 SIP URI</span><input v-model.trim="form.register_redirect" class="input plain w-full mono" placeholder="留空不重定向，例如 sip:34020000002000000001@10.0.0.8:5060" /><span class="form-help">配置后，2022 设备 REGISTER 会收到目标地址；旧版档案不使用该能力。</span></label>
          </div>
        </details>

        <h3 id="history" class="section-title mt-6">心跳与注册历史</h3>
        <div class="form-grid">
          <label class="form-group">
            <span class="form-label">每台设备最多保留</span>
            <input v-model.number="form.device_history.max_records" class="input plain w-full" type="number" min="0" max="100000" required />
            <span class="form-help">条；0 表示不限制条数。</span>
          </label>
          <label class="form-group">
            <span class="form-label">最多保留天数</span>
            <input v-model.number="form.device_history.max_days" class="input plain w-full" type="number" min="0" max="3650" required />
            <span class="form-help">天；0 表示不限制天数。</span>
          </label>
        </div>
        <div class="read-only mt-4">条数和天数限制同时生效。每次写入新记录时，后端会自动清理超出任一限制的旧记录。</div>

        <div id="direct-download" class="section-heading mt-6">
          <div><h3 class="section-title">2014 附录 O 裸 TCP 文件下载</h3><p class="form-help">设备直连落盘与上级平台级联可独立启用；两条链路共享设备白名单、地址范围、文件大小和并发限制。</p></div>
          <span class="protocol-tag amber">默认关闭</span>
        </div>
        <label class="toggle-row"><span><strong>启用设备直连落盘</strong><small>Owl 连接白名单设备的 TCP 服务并将原始 PS 文件保存到本地</small></span><span class="switch"><input v-model="directDownload.enabled" type="checkbox" /><span class="slider" /></span></label>
        <label class="toggle-row"><span><strong>启用上级平台裸 TCP 级联</strong><small>Owl 向上级宣告中继地址，并把下级设备原始 PS 流式转发给上级；不落盘</small></span><span class="switch"><input v-model="directDownload.cascade_relay_enabled" type="checkbox" /><span class="slider" /></span></label>
        <div v-if="directDownload.enabled || directDownload.cascade_relay_enabled" class="config-panel-body inset-panel mt-4">
          <div class="warning-box mb-4"><ShieldAlert /><span>启用前应确认设备地址、端口暴露和网络隔离。非 2014 设备继续使用 RTP 下载链路；真实设备与上级平台互通仍需上线验收。</span></div>
          <div class="form-grid">
            <div class="form-subheading full"><strong>共享安全与资源边界</strong><span>两条裸 TCP 链路共用并发额度和地址策略</span></div>
            <label class="form-group full"><span class="form-label">允许设备 ID</span><textarea v-model="directDeviceAllowlistInput" class="input plain w-full cascade-textarea mono" required placeholder="每行一个 20 位设备国标编码" /></label>
            <label class="form-group"><span class="form-label">SDP 占位端口</span><input v-model.number="directDownload.offer_port" class="input plain w-full" type="number" min="1" max="65535" required /></label>
            <label class="form-group"><span class="form-label">单文件上限（MB）</span><input v-model.number="directDownload.max_file_size_mb" class="input plain w-full" type="number" min="1" required /></label>
            <label class="form-group"><span class="form-label">全局并发</span><input v-model.number="directDownload.global_concurrency" class="input plain w-full" type="number" min="1" max="128" required /></label>
            <label class="form-group"><span class="form-label">单设备并发</span><input v-model.number="directDownload.device_concurrency" class="input plain w-full" type="number" min="1" max="32" required /></label>
            <label class="form-group"><span class="form-label">连接超时（秒）</span><input v-model.number="directDownload.dial_timeout_seconds" class="input plain w-full" type="number" min="1" required /></label>
            <label class="form-group"><span class="form-label">首字节超时（秒）</span><input v-model.number="directDownload.first_byte_timeout_seconds" class="input plain w-full" type="number" min="1" required /></label>
            <label class="form-group"><span class="form-label">空闲超时（秒）</span><input v-model.number="directDownload.idle_timeout_seconds" class="input plain w-full" type="number" min="1" required /></label>
            <label class="form-group"><span class="form-label">总超时（秒）</span><input v-model.number="directDownload.total_timeout_seconds" class="input plain w-full" type="number" min="1" required /></label>
            <label class="toggle-row full"><span><strong>允许 SDP 地址与注册地址不一致</strong><small>仅在设备媒体与信令分离部署时开启，并同时配置允许网段</small></span><span class="switch"><input v-model="directDownload.allow_address_mismatch" type="checkbox" /><span class="slider" /></span></label>
            <label v-if="directDownload.allow_address_mismatch" class="form-group full"><span class="form-label">允许设备媒体地址 CIDR</span><textarea v-model="directAllowedCIDRsInput" class="input plain w-full cascade-textarea mono" required placeholder="每行一个 CIDR，例如 192.0.2.0/24" :aria-invalid="Boolean(directCIDRError)" aria-describedby="direct-cidr-help direct-cidr-error direct-download-error" /><span id="direct-cidr-help" class="form-help">仅约束下级设备 200 OK SDP 中的 TCP 服务地址；不能授权上级平台连接中继。</span><span v-if="directCIDRError" id="direct-cidr-error" class="field-error" role="alert">{{ directCIDRError }}</span></label>

            <template v-if="directDownload.enabled">
              <div class="form-subheading full"><strong>设备直连落盘</strong><span>仅影响 Owl 本地文件保存和清理</span></div>
              <label class="form-group full"><span class="form-label">文件存储目录</span><input v-model.trim="directDownload.storage_dir" class="input plain w-full mono" required /></label>
              <label class="form-group"><span class="form-label">文件保留天数</span><input v-model.number="directDownload.retain_days" class="input plain w-full" type="number" min="1" max="3650" required /></label>
            </template>

            <template v-if="directDownload.cascade_relay_enabled">
              <div class="form-subheading full"><strong>上级平台级联中继</strong><span>监听地址用于本机绑定，宣告地址必须能被上级平台访问</span></div>
              <div class="version-note full"><Info /><span>Owl 内置附录 O 的 TCP 下载客户端，因此不需要客户端侧消息 1/2/5；本入口承接规范消息 3，向下级媒体发送方发起或接收带 SDP 的 Download INVITE。</span></div>
              <p class="form-help relay-policy-note">上级连接来源由对应平台的信令注册地址及“媒体允许网段（media_allowed_cidrs）”授权，与设备媒体地址 CIDR 分开控制。</p>
              <label class="form-group"><span class="form-label">中继监听 IP</span><input v-model.trim="directDownload.relay_listen_ip" class="input plain w-full mono" required placeholder="0.0.0.0" /><span class="form-help">可使用 0.0.0.0 或 :: 监听全部网卡。</span></label>
              <label class="form-group"><span class="form-label">上级宣告 IP</span><input v-model.trim="directDownload.relay_advertise_ip" class="input plain w-full mono" placeholder="留空使用媒体服务器 SDP IP" /><span class="form-help">写入给上级平台的 200 OK SDP，不能使用未指定地址。</span></label>
              <label class="form-group"><span class="form-label">中继端口起点</span><input v-model.number="directDownload.relay_port_start" class="input plain w-full" type="number" min="1" max="65535" required /></label>
              <label class="form-group"><span class="form-label">中继端口终点</span><input v-model.number="directDownload.relay_port_end" class="input plain w-full" type="number" min="1" max="65535" required /></label>
            </template>

            <p v-if="!directDownloadConfigValid" id="direct-download-error" class="field-error full" role="alert">请配置唯一的 20 位设备编码和有效资源限制；总超时不得短于阶段超时。级联中继还要求有效监听地址和递增端口范围，允许地址不一致时必须配置不重复的有效 CIDR。</p>
          </div>
        </div>

        <div id="annex-g" class="section-heading mt-6">
          <div><h3 class="section-title">附录 G 外部系统</h3><p class="form-help">管理 2011/2014/2016 综合接处警、卡口和城市信息系统的静态身份、来源范围、Digest 与 TLS 信任。</p></div>
          <span class="protocol-tag amber">只读启动配置</span>
        </div>
        <div class="version-note mt-4"><ShieldAlert /><span>2022 已删除附录 G。本区仅展示当前进程的启动配置，不参与页面底部的在线保存。请编辑 <code>./configs/config.toml</code> 中的 <code>[Sip.AnnexG]</code>，核对密钥后重启 Owl；该接入默认只允许 SIP-TLS，真实外部系统的安全、性能与业务互通仍是上线阶段门。</span></div>
        <fieldset class="static-config" disabled>
          <legend class="sr-only">当前附录 G 启动配置（只读）</legend>
        <label class="toggle-row"><span><strong>启用附录 G 接入</strong><small>当前启动值；开启七类 MESSAGE 业务、存储、主动交换与审计链路</small></span><span class="switch"><input v-model="annexG.enabled" type="checkbox" /><span class="slider" /></span></label>
        <div class="form-grid mt-4">
          <label class="form-group"><span class="form-label">单次最多记录数</span><input v-model.number="annexG.max_send_records" class="input plain w-full" type="number" min="0" max="10000" required /></label>
          <label class="form-group"><span class="form-label">每系统入向速率（次/秒）</span><input v-model.number="annexG.inbound_rate" class="input plain w-full" type="number" min="0" max="10000" required /></label>
          <label class="form-group"><span class="form-label">每系统突发量</span><input v-model.number="annexG.inbound_burst" class="input plain w-full" type="number" min="0" max="10000" required /></label>
          <label class="form-group"><span class="form-label">最大在途关联</span><input v-model.number="annexG.max_pending" class="input plain w-full" type="number" min="0" max="10000" required /></label>
          <label class="form-group"><span class="form-label">在途保留时间（秒）</span><input v-model.number="annexG.pending_ttl_seconds" class="input plain w-full" type="number" min="60" max="604800" required /></label>
        </div>
        <div v-if="!annexGSystems.length" class="read-only mt-4">尚未配置外部系统。启用附录 G 前至少添加一个系统档案。</div>
        <fieldset v-for="(item, index) in annexGSystems" :key="`${item.id}-${index}`" class="cascade-card annex-system-card mt-4">
          <legend class="sr-only">附录 G 外部系统 {{ index + 1 }}</legend>
          <div class="cascade-card-head">
            <div class="system-identity"><span class="details-icon"><KeyRound /></span><span><strong>{{ item.id || `外部系统 ${index + 1}` }}</strong><small>{{ item.role === "emergency_command_system" ? "综合接处警" : item.role === "tollgate_system" ? "卡口系统" : "城市信息系统" }} · {{ item.version }}</small></span></div>
          </div>
          <div class="form-grid mt-4">
            <label class="form-group"><span class="form-label">外部系统 ID</span><input v-model.trim="item.id" class="input plain w-full mono" pattern="[0-9]{20}" maxlength="20" :required="annexG.enabled" /></label>
            <label class="form-group"><span class="form-label">系统角色</span><select v-model="item.role" class="input plain w-full"><option value="emergency_command_system">综合接处警系统</option><option value="tollgate_system">卡口系统</option><option value="city_information_system">城市信息系统</option></select></label>
            <label class="form-group"><span class="form-label">协议档案</span><select v-model="item.version" class="input plain w-full"><option value="1.0">2011（1.0）</option><option value="1.1">2014（1.1）</option><option value="2.0">2016（2.0）</option></select></label>
            <label class="form-group"><span class="form-label">Digest Realm</span><input v-model.trim="item.realm" class="input plain w-full mono" pattern="[0-9]{10}" maxlength="10" placeholder="默认取系统 ID 前 10 位" /></label>
            <label class="form-group"><span class="form-label">主动请求地址</span><input v-model.trim="item.address" class="input plain w-full mono" :required="annexG.enabled" placeholder="host:port" /></label>
            <label class="form-group"><span class="form-label">信令传输</span><select v-model="item.transport" class="input plain w-full"><option value="tls">TLS（推荐）</option><option value="tcp">TCP</option><option value="udp">UDP</option></select></label>
            <label class="form-group full"><span class="form-label">允许来源 IP / CIDR</span><textarea v-model="item.source_cidrs_input" class="input plain w-full cascade-textarea mono" :required="annexG.enabled" placeholder="每行一个可信来源 IP 或 CIDR" /></label>
            <label class="form-group"><span class="form-label">新 Digest 密码</span><input v-model="item.password_input" class="input plain w-full" type="password" autocomplete="new-password" :disabled="item.clear_password" :required="annexG.enabled && !item.password_configured && !item.clear_password" :placeholder="item.password_configured ? '已配置；留空保留当前密码' : '新系统必须配置'" /></label>
            <label class="toggle-row"><span><strong>清除 Digest 密码</strong><small>{{ item.password_configured ? "保存后显式清除" : "当前未配置" }}</small></span><span class="switch"><input v-model="item.clear_password" type="checkbox" :disabled="!item.password_configured" /><span class="slider" /></span></label>
            <label class="form-group full"><span class="form-label">Date + Note 独立 seed</span><input v-model="item.signal_digest_seed" class="input plain w-full mono" type="password" autocomplete="new-password" :disabled="item.clear_signal_digest_seed" :placeholder="item.signal_digest_seed_configured ? '已配置；留空保留当前 seed' : '留空回退 Digest 密码或全局 seed'" /></label>
            <label class="toggle-row full"><span><strong>清除独立 seed</strong><small>{{ item.signal_digest_seed_configured ? "保存后回退到密码或全局 seed" : "当前未配置" }}</small></span><span class="switch"><input v-model="item.clear_signal_digest_seed" type="checkbox" :disabled="!item.signal_digest_seed_configured" /><span class="slider" /></span></label>
            <label class="toggle-row full"><span><strong>允许明文传输</strong><small>选择 UDP/TCP 时必须显式开启；来源 IP 不能替代密码学身份</small></span><span class="switch"><input v-model="item.allow_insecure_transport" type="checkbox" /><span class="slider" /></span></label>
            <template v-if="item.transport === 'tls'">
              <label class="form-group"><span class="form-label">TLS CA 文件</span><input v-model.trim="item.tls_ca" class="input plain w-full mono" placeholder="留空使用系统 CA" /></label>
              <label class="form-group"><span class="form-label">TLS 服务端名称</span><input v-model.trim="item.tls_server_name" class="input plain w-full mono" placeholder="默认使用 address 主机" /></label>
              <label class="form-group"><span class="form-label">TLS 客户端证书</span><input v-model.trim="item.tls_cert" class="input plain w-full mono" placeholder="可选双向 TLS" /></label>
              <label class="form-group"><span class="form-label">TLS 客户端私钥</span><input v-model.trim="item.tls_key" class="input plain w-full mono" :required="Boolean(item.tls_cert)" /></label>
            </template>
          </div>
        </fieldset>
        </fieldset>
        <div class="config-export mt-4">
          <div><strong>配置文件片段</strong><small>密钥不会从接口返回，复制后必须替换 <code>replace-with-*</code> 占位符。</small></div>
          <button class="btn btn-sm" type="button" @click="copyAnnexGConfig"><Copy />复制 TOML 片段</button>
          <pre class="config-snippet mono" tabindex="0">{{ annexGConfigSnippet }}</pre>
        </div>
        <div class="audit-link-row mt-4"><span>业务记录、当前布控和历史审计在“国标能力”页面统一查询。</span><RouterLink class="btn btn-sm" to="/gb28181-capabilities">查看附录 G 审计</RouterLink></div>

        <div id="alarm-receivers" class="section-heading mt-6">
          <div>
            <h3 class="section-title">本域接警 SIP 终端</h3>
            <p class="form-help">按 GB/T 28181 9.4 将报警 MESSAGE 分发给已向本平台注册且在线的接警客户端；每个目标默认关闭并须显式授权报警源。</p>
          </div>
          <button class="btn btn-sm" type="button" @click="addAlarmReceiver"><Plus />添加接警终端</button>
        </div>

        <div v-if="!alarmReceivers.length" class="read-only mt-4">尚未配置本域接警终端。未配置时不会向普通注册设备分发报警。</div>
        <fieldset v-for="(item, index) in alarmReceivers" :key="`${item.device_id}-${index}`" class="cascade-card mt-4">
          <legend class="sr-only">接警终端 {{ index + 1 }}</legend>
          <div class="cascade-card-head">
            <label class="toggle-row cascade-enabled">
              <span><strong>{{ item.name || `接警终端 ${index + 1}` }}</strong><small>{{ item.device_id || "尚未填写终端编码" }}</small></span>
              <span class="switch"><input v-model="item.enabled" type="checkbox" /><span class="slider" /></span>
            </label>
            <button class="btn btn-sm btn-danger" type="button" :aria-label="`删除接警终端 ${item.name}`" @click="removeAlarmReceiver(index)"><Trash2 />删除</button>
          </div>
          <div class="form-grid mt-4">
            <label class="form-group"><span class="form-label">配置名称</span><input v-model.trim="item.name" class="input plain w-full" :required="item.enabled" /></label>
            <label class="form-group"><span class="form-label">接警终端 DeviceID</span><input v-model.trim="item.device_id" class="input plain w-full mono" pattern="[0-9]{20}" maxlength="20" :required="item.enabled" /></label>
            <label class="form-group full">
              <span class="form-label">允许接收的报警源 ID</span>
              <textarea v-model="item.source_ids_input" class="input plain w-full cascade-textarea mono" :required="item.enabled" placeholder="每行一个 10 位中心编码或 20 位设备/通道编码；空列表不接收任何报警" />
              <span class="form-help">仅精确匹配列表中的报警 XML DeviceID；目标离线、未注册或未授权时不会分发。</span>
            </label>
          </div>
        </fieldset>

        <div id="upstreams" class="section-heading mt-6">
          <div>
            <h3 class="section-title">上级平台级联</h3>
            <p class="form-help">本平台作为下级，通过 UDP、TCP 或经证书校验的 TLS 向上级平台注册、续期并发送 Keepalive。</p>
          </div>
          <button class="btn btn-sm" type="button" @click="addUpstream"><Plus />添加上级平台</button>
        </div>

        <div v-if="!upstreams.length" class="read-only mt-4">尚未配置上级平台。添加后可选择 2011、2014、2016 或 2022 档案。</div>
        <fieldset v-for="(item, index) in upstreams" :key="`${item.name}-${index}`" class="cascade-card mt-4">
          <legend class="sr-only">上级平台 {{ index + 1 }}</legend>
          <div class="cascade-card-head">
            <label class="toggle-row cascade-enabled">
              <span><strong>{{ item.name || `上级平台 ${index + 1}` }}</strong><small>{{ item.server_id || "尚未填写平台编码" }}</small></span>
              <span class="switch"><input v-model="item.enabled" type="checkbox" /><span class="slider" /></span>
            </label>
            <span
              v-if="statusFor(item.name)"
              class="status"
              :class="statusFor(item.name)?.registered ? 'online' : 'offline'"
            >{{ statusFor(item.name)?.state }}</span>
            <button class="btn btn-sm btn-danger" type="button" :aria-label="`删除上级平台 ${item.name}`" @click="removeUpstream(index)"><Trash2 />删除</button>
          </div>

          <div class="form-grid mt-4">
            <label class="form-group"><span class="form-label">配置名称</span><input v-model.trim="item.name" class="input plain w-full" :required="item.enabled" /></label>
            <label class="form-group"><span class="form-label">协议档案</span><select v-model="item.version" class="input plain w-full"><option value="1.0">2011（1.0）</option><option value="1.1">2014（1.1）</option><option value="2.0">2016（2.0）</option><option value="3.0">2022（3.0）</option></select></label>
            <label class="form-group full"><span class="form-label">上级平台 ID</span><input v-model.trim="item.server_id" class="input plain w-full mono" pattern="[0-9]{20}" maxlength="20" :required="item.enabled" /></label>
            <label class="form-group"><span class="form-label">上级地址</span><input v-model.trim="item.host" class="input plain w-full mono" :required="item.enabled" /></label>
            <label class="form-group"><span class="form-label">上级端口</span><input v-model.number="item.port" class="input plain w-full" type="number" min="0" max="65535" placeholder="0 表示 UDP/TCP 5060、TLS 5061" :required="item.enabled" /></label>
            <label class="form-group"><span class="form-label">信令传输</span><select v-model="item.transport" class="input plain w-full"><option value="udp">UDP</option><option value="tcp">TCP</option><option value="tls">TLS</option></select></label>
            <label class="form-group"><span class="form-label">上级 SIP 域</span><input v-model.trim="item.domain" class="input plain w-full mono" placeholder="默认取上级 ID 前 10 位" /></label>
            <template v-if="item.transport === 'tls'">
              <label class="form-group"><span class="form-label">TLS CA 文件</span><input v-model.trim="item.tls_ca" class="input plain w-full mono" placeholder="留空使用系统 CA" /></label>
              <label class="form-group"><span class="form-label">TLS 服务端名称</span><input v-model.trim="item.tls_server_name" class="input plain w-full mono" placeholder="默认使用上级地址" /></label>
              <label class="form-group"><span class="form-label">TLS 客户端证书</span><input v-model.trim="item.tls_cert" class="input plain w-full mono" placeholder="双向认证时填写" /></label>
              <label class="form-group"><span class="form-label">TLS 客户端私钥</span><input v-model.trim="item.tls_key" class="input plain w-full mono" placeholder="与客户端证书同时填写" /></label>
            </template>
            <label class="form-group"><span class="form-label">本平台 ID</span><input v-model.trim="item.local_id" class="input plain w-full mono" pattern="[0-9]{20}" maxlength="20" placeholder="默认使用 SIP 服务 ID" /></label>
            <label class="form-group"><span class="form-label">本平台 SIP 域</span><input v-model.trim="item.local_domain" class="input plain w-full mono" placeholder="默认使用本平台域" /></label>
            <label class="form-group"><span class="form-label">Contact 地址</span><input v-model.trim="item.local_host" class="input plain w-full mono" placeholder="默认使用对外宣告地址" /></label>
            <label class="form-group"><span class="form-label">Contact 端口</span><input v-model.number="item.local_port" class="input plain w-full" type="number" min="0" max="65535" placeholder="0 表示使用对应监听端口" /></label>
            <label class="form-group">
              <span class="form-label">新注册密码</span>
              <input v-model="item.password_input" class="input plain w-full" type="password" autocomplete="new-password" :disabled="item.clear_password" :placeholder="item.password_configured ? '已配置；留空保留当前密码' : '尚未配置'" />
            </label>
            <label class="toggle-row">
              <span><strong>清除注册密码</strong><small>{{ item.password_configured ? "保存后显式清除现有密码" : "当前未配置密码" }}</small></span>
              <span class="switch"><input v-model="item.clear_password" type="checkbox" :disabled="!item.password_configured" /><span class="slider" /></span>
            </label>
            <label class="form-group"><span class="form-label">注册有效期（秒）</span><input v-model.number="item.expires" class="input plain w-full" type="number" min="60" max="86400" :required="item.enabled" /></label>
            <label class="form-group"><span class="form-label">心跳间隔（秒）</span><input v-model.number="item.keepalive_seconds" class="input plain w-full" type="number" min="5" max="3600" :required="item.enabled" /></label>
            <label class="form-group full">
              <span class="form-label">Date + Note 独立 seed</span>
              <input v-model="item.signal_digest_seed" class="input plain w-full mono" type="password" autocomplete="new-password" :disabled="item.clear_signal_digest_seed" :placeholder="item.signal_digest_seed_configured ? '已配置；留空保留当前 seed' : '留空依次使用上级密码和全局 seed'" />
              <span class="form-help">按原始字节参与摘要；首尾空格不会被自动删除。读取配置时只返回“已配置”状态，不返回原值。</span>
            </label>
            <label class="toggle-row full">
              <span><strong>清除独立 seed</strong><small>{{ item.signal_digest_seed_configured ? "保存后回退到上级密码或全局 seed" : "当前未配置独立 seed" }}</small></span>
              <span class="switch"><input v-model="item.clear_signal_digest_seed" type="checkbox" :disabled="!item.signal_digest_seed_configured" /><span class="slider" /></span>
            </label>
            <label class="toggle-row full">
              <span><strong>Monitor-User-Identity 跨域身份</strong><small>生成、逐级追加并验证安全路由网关和用户属性；默认关闭</small></span>
              <span class="switch"><input v-model="item.monitor_user_identity.enabled" type="checkbox" /><span class="slider" /></span>
            </label>
            <template v-if="item.monitor_user_identity.enabled || item.monitor_user_identity.required">
              <label class="toggle-row full">
                <span><strong>强制身份与属性授权</strong><small>拒绝缺失/非法身份头；必须至少填写一项允许用户或属性</small></span>
                <span class="switch"><input v-model="item.monitor_user_identity.required" type="checkbox" /><span class="slider" /></span>
              </label>
              <label class="form-group"><span class="form-label">本地安全路由网关 ID</span><input v-model.trim="item.monitor_user_identity.local_gateway_id" class="input plain w-full mono" pattern="[0-9]{10}211[0-9]{7}" maxlength="20" required /></label>
              <label class="form-group"><span class="form-label">直接上级安全路由网关 ID</span><input v-model.trim="item.monitor_user_identity.remote_gateway_id" class="input plain w-full mono" pattern="[0-9]{10}211[0-9]{7}" maxlength="20" required /></label>
              <label class="form-group"><span class="form-label">本域用户 ID</span><input v-model.trim="item.monitor_user_identity.local_user_id" class="input plain w-full mono" pattern="[0-9]{20}" maxlength="20" required /></label>
              <label class="form-group"><span class="form-label">最大网关跳数</span><input v-model.number="item.monitor_user_identity.max_hops" class="input plain w-full" type="number" min="2" max="32" required /></label>
              <label class="form-group"><span class="form-label">本域用户机构</span><input v-model.trim="item.monitor_user_identity.local_organization" class="input plain w-full" required /><span class="form-help">属性值不得包含连字符。</span></label>
              <label class="form-group"><span class="form-label">本域用户类别</span><input v-model.trim="item.monitor_user_identity.local_category" class="input plain w-full" required /></label>
              <label class="form-group"><span class="form-label">本域用户职级</span><input v-model.trim="item.monitor_user_identity.local_rank" class="input plain w-full" required /></label>
              <label class="form-group full"><span class="form-label">可信中间网关 ID</span><textarea v-model="item.trusted_gateway_ids_input" class="input plain w-full cascade-textarea mono" placeholder="每行一个类型码 211 的网关 ID；不含直接上级和本地网关" /></label>
              <label class="form-group full"><span class="form-label">允许用户 ID</span><textarea v-model="item.allowed_user_ids_input" class="input plain w-full cascade-textarea mono" placeholder="每行一个类型码 300-499 的用户 ID；空表示不按用户 ID 限制" /></label>
              <label class="form-group"><span class="form-label">允许机构</span><textarea v-model="item.allowed_organizations_input" class="input plain w-full cascade-textarea" placeholder="每行一个机构属性" /></label>
              <label class="form-group"><span class="form-label">允许用户类别</span><textarea v-model="item.allowed_categories_input" class="input plain w-full cascade-textarea" placeholder="每行一个类别属性" /></label>
              <label class="form-group full"><span class="form-label">允许用户职级</span><textarea v-model="item.allowed_ranks_input" class="input plain w-full cascade-textarea" placeholder="每行一个职级属性" /></label>
            </template>
            <label class="toggle-row full">
              <span><strong>9.4 报警 MESSAGE 分发</strong><small>将显式共享通道的报警转发给该上级并等待业务应答；默认关闭</small></span>
              <span class="switch"><input v-model="item.alarm_dispatch_enabled" type="checkbox" /><span class="slider" /></span>
            </label>
            <label class="form-group full">
              <span class="form-label">共享通道国标 ID</span>
              <textarea v-model="item.shared_channels_input" class="input plain w-full cascade-textarea mono" placeholder="每行一个 20 位通道编码；空列表表示不向该上级共享通道" />
            </label>
            <label class="form-group full">
              <span class="form-label">上级可见编码映射（JSON）</span>
              <textarea v-model="item.channel_id_map_input" class="input plain w-full cascade-textarea mono" placeholder="JSON 对象：本地通道编码映射为上级可见编码" />
              <span class="form-help">键必须已列入共享通道；映射后的编码必须唯一且为 20 位数字。</span>
            </label>
            <label class="form-group full">
              <span class="form-label">附加媒体地址白名单</span>
              <textarea v-model="item.media_allowed_cidrs_input" class="input plain w-full cascade-textarea mono" placeholder="每行一个媒体 IP 或 CIDR；上级信令 IP 默认允许" />
              <span class="form-help">用于上级平台信令与媒体分离部署，未列入白名单的 SDP 目标地址会被拒绝。</span>
            </label>
          </div>

          <div v-if="statusFor(item.name)" class="cascade-runtime mt-4">
            <span>地址：<strong>{{ statusFor(item.name)?.address }}</strong></span>
            <span>版本：<strong>{{ statusFor(item.name)?.configured_version }} → {{ statusFor(item.name)?.negotiated_version || "待协商" }}</strong></span>
            <span>注册：<strong>{{ formatDate(statusFor(item.name)?.last_register_at) }}</strong></span>
            <span>心跳：<strong>{{ formatDate(statusFor(item.name)?.last_keepalive_at) }}</strong></span>
            <span>过期：<strong>{{ formatDate(statusFor(item.name)?.expires_at) }}</strong></span>
          </div>
          <div v-if="statusFor(item.name)?.last_error" class="warning-box mt-4" role="alert"><ShieldAlert /><span>{{ statusFor(item.name)?.last_error }}</span></div>
        </fieldset>

        <div v-if="statusError" class="warning-box mt-4" role="alert"><ShieldAlert /><span>{{ statusError }}</span><button class="btn btn-sm ml-auto" type="button" @click="loadCascadeStatuses">重试</button></div>
        <div v-else-if="statusLoading" class="read-only mt-4"><LoaderCircle class="animate-spin" />正在刷新级联状态…</div>

        <div class="settings-savebar">
          <span>保存会热更新摘要安全策略、下载、接警终端和上级平台；监听、证书上下文与附录 G 需修改配置文件并重启</span>
          <button class="btn btn-primary" :disabled="saving || loading || Boolean(loadError) || !tlsConfigValid || !certificateConfigValid || !directDownloadConfigValid">
            <LoaderCircle v-if="saving" class="animate-spin" /><Save v-else />{{ saving ? "正在保存…" : "保存配置" }}
          </button>
        </div>
      </form>
    </section>
  </main>
</template>

<style scoped>
.section-heading,
.cascade-card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 1rem;
}

.section-heading .section-title {
  margin: 0;
}

.cascade-card {
  min-width: 0;
  margin-inline: 0;
  padding: 1rem;
  border: 1px solid var(--line);
  border-radius: var(--radius);
}

.cascade-enabled {
  flex: 1;
  margin: 0;
  padding: 0;
  border: 0;
}

.cascade-runtime {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 0.5rem 1rem;
  color: var(--muted);
  font-size: 0.82rem;
}

.cascade-runtime strong {
  color: var(--ink);
  font-weight: 600;
}

.cascade-textarea {
  min-height: 92px;
  resize: vertical;
}

.form-grid > * { min-width: 0; }
.form-grid > .full { grid-column: 1 / -1; }
.form-grid .input, .form-grid .select { width: 100%; min-width: 0; }

.config-panel {
  border: 1px solid var(--line);
  border-radius: var(--radius);
  background: #fbfcfe;
}

.config-panel > summary {
  min-height: 50px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 1rem;
  padding: 0 14px;
  color: var(--ink);
  cursor: pointer;
  list-style: none;
}

.config-panel > summary::-webkit-details-marker { display: none; }
.config-panel > summary span { display: inline-flex; align-items: center; gap: 8px; }
.config-panel > summary svg { width: 17px; color: var(--blue); }
.config-panel > summary small { color: var(--muted); }
.config-panel[open] > summary { border-bottom: 1px solid var(--line); }
.config-panel-body { padding: 14px; }
.inset-panel { border: 1px solid var(--line); border-radius: var(--radius); background: #fbfcfe; }
.form-subheading { grid-column: 1 / -1; display: flex; align-items: baseline; justify-content: space-between; gap: 12px; padding-top: 4px; color: var(--ink); border-top: 1px solid var(--line); }
.form-subheading:first-child { padding-top: 0; border-top: 0; }
.form-subheading strong { font-size: 12px; }
.form-subheading span { color: var(--muted); font-size: 11px; }
.relay-policy-note { grid-column: 1 / -1; margin: -5px 0 0; }
.version-note { display: flex; align-items: flex-start; gap: 9px; padding: 10px 12px; color: #5a6472; background: #f2f6fb; border: 1px solid #dce6f1; border-radius: 8px; font-size: 12px; line-height: 1.55; }
.version-note svg { flex: 0 0 auto; width: 16px; margin-top: 1px; color: var(--blue); }
.system-identity { min-width: 0; display: flex; align-items: center; gap: 10px; }
.system-identity > span:last-child, .system-identity strong, .system-identity small { display: block; }
.system-identity small { margin-top: 2px; color: var(--muted); }
.annex-system-card { background: #fff; }
.static-config {
  min-width: 0;
  margin: 0;
  padding: 0;
  border: 0;
}
.static-config:disabled { opacity: 1; }
.static-config :disabled { cursor: not-allowed; }
.config-export {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 10px 16px;
  padding: 12px;
  border: 1px solid var(--line);
  border-radius: 8px;
  background: #f7f9fb;
}
.config-export strong,
.config-export small { display: block; }
.config-export small { margin-top: 3px; color: var(--muted); font-size: 11px; }
.config-snippet {
  grid-column: 1 / -1;
  max-height: 260px;
  margin: 0;
  padding: 12px;
  overflow: auto;
  color: var(--ink);
  background: #fff;
  border: 1px solid var(--line);
  border-radius: 6px;
  font-size: 11px;
  line-height: 1.55;
  white-space: pre;
}
.audit-link-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 10px 12px; color: var(--muted); background: #f7f9fb; border: 1px solid var(--line); border-radius: 8px; font-size: 12px; }
#annex-g { scroll-margin-top: 80px; }
#annex-g:focus { outline: 3px solid rgba(23, 104, 212, .18); outline-offset: 5px; border-radius: 4px; }

.sr-only {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}

@media (max-width: 720px) {
  .section-heading,
  .cascade-card-head {
    align-items: stretch;
    flex-direction: column;
  }

  .audit-link-row { align-items: stretch; flex-direction: column; }
  .config-export { grid-template-columns: 1fr; }
  .config-export .btn { width: 100%; }
  .form-subheading { align-items: flex-start; flex-direction: column; gap: 2px; }
}
</style>
