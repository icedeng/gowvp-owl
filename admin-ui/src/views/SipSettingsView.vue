<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from "vue";
import {
  History,
  Info,
  LoaderCircle,
  Network,
  Plus,
  RadioTower,
  RefreshCcw,
  Save,
  ShieldAlert,
  ShieldCheck,
  Trash2,
} from "@lucide/vue";
import { api, errorMessage } from "../services/api";
import type { CascadePlatformStatus, SipConfig, SipUpstream } from "../types/api";
import { useUiStore } from "../stores/ui";
import { formatDate } from "../utils/format";

type EditableUpstream = SipUpstream & {
  keepalive_seconds: number;
  password_input: string;
  shared_channels_input: string;
  channel_id_map_input: string;
  media_allowed_cidrs_input: string;
};

const ui = useUiStore();
const loading = ref(false);
const saving = ref(false);
const loadError = ref("");
const statusLoading = ref(false);
const statusError = ref("");
const cascadeStatuses = ref<CascadePlatformStatus[]>([]);
const upstreams = ref<EditableUpstream[]>([]);
const passwordInput = ref("");
const currentPassword = ref("");
const directConfig = ref<Record<string, unknown>>({});
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
  strict_source_check: true,
  require_message_auth: false,
  ptz_weak_confirm: false,
  device_history: { max_records: 1000, max_days: 30 },
});
const tlsConfigValid = computed(
  () =>
    !form.enable_tls ||
    (Boolean(form.tls_cert.trim()) &&
      Boolean(form.tls_key.trim()) &&
      (!form.tls_require_client_cert || Boolean(form.tls_client_ca.trim())))
);
let statusTimer: number | undefined;

function durationSeconds(value?: number) {
  const duration = Number(value || 0);
  if (!duration) return 60;
  return duration > 3_600 ? Math.max(1, Math.round(duration / 1_000_000_000)) : duration;
}

function editableUpstream(item: Partial<SipUpstream> = {}): EditableUpstream {
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
    password: item.password || "",
    password_input: "",
    version: item.version || "1.0",
    expires: item.expires || 3600,
    keepalive_interval: item.keepalive_interval || 0,
    keepalive_seconds: durationSeconds(item.keepalive_interval),
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
    password: item.password_input || item.password || "",
    version: item.version,
    expires: item.expires,
    keepalive_interval: Math.round(item.keepalive_seconds * 1_000_000_000),
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

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const { data } = await api.configInfo();
    const sip = data.sip || {};
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
      strict_source_check: sip.strict_source_check ?? true,
      require_message_auth: Boolean(sip.require_message_auth),
      ptz_weak_confirm: Boolean(sip.ptz_weak_confirm),
      device_history: {
        max_records: sip.device_history?.max_records ?? 1000,
        max_days: sip.device_history?.max_days ?? 30,
      },
    });
    currentPassword.value = sip.password || "";
    directConfig.value = sip.direct_tcp_download || {};
    upstreams.value = (sip.upstreams || []).map(editableUpstream);
    passwordInput.value = "";
    await loadCascadeStatuses();
  } catch (cause) {
    loadError.value = errorMessage(cause, "SIP 配置加载失败");
  } finally {
    loading.value = false;
  }
}

async function save() {
  if (!tlsConfigValid.value) {
    ui.toast("启用 SIP-TLS 前请填写证书与私钥路径");
    return;
  }
  saving.value = true;
  try {
    const body: SipConfig = {
      ...form,
      password: passwordInput.value || currentPassword.value,
      direct_tcp_download: directConfig.value,
      upstreams: upstreams.value.map(upstreamPayload),
    };
    await api.updateSip(body);
    if (passwordInput.value) currentPassword.value = passwordInput.value;
    upstreams.value.forEach((item) => {
      if (item.password_input) item.password = item.password_input;
      item.password_input = "";
    });
    passwordInput.value = "";
    ui.toast("SIP 配置已保存并热更新");
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
        <p class="page-desc">维护 GB28181 设备注册入口、安全策略与历史保留规则，保存后由后端热更新。</p>
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
        <a href="#security"><ShieldCheck />安全与兼容</a>
        <a href="#history"><History />历史保留</a>
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
        <label class="toggle-row"><span><strong>PTZ 弱确认模式</strong><small>兼容不返回 DeviceControl 应答的设备</small></span><span class="switch"><input v-model="form.ptz_weak_confirm" type="checkbox" /><span class="slider" /></span></label>

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
            <label class="form-group"><span class="form-label">新注册密码</span><input v-model="item.password_input" class="input plain w-full" type="password" autocomplete="new-password" placeholder="留空保留当前密码" /></label>
            <label class="form-group"><span class="form-label">注册有效期（秒）</span><input v-model.number="item.expires" class="input plain w-full" type="number" min="60" max="86400" :required="item.enabled" /></label>
            <label class="form-group"><span class="form-label">心跳间隔（秒）</span><input v-model.number="item.keepalive_seconds" class="input plain w-full" type="number" min="5" max="3600" :required="item.enabled" /></label>
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
          <span>保存会立即热更新 SIP 服务、历史保留和上级平台注册</span>
          <button class="btn btn-primary" :disabled="saving || loading || !tlsConfigValid">
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
}
</style>
