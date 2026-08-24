<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import {
  History,
  Info,
  LoaderCircle,
  RadioTower,
  RefreshCcw,
  Save,
  ShieldAlert,
  ShieldCheck,
} from "@lucide/vue";
import { api, errorMessage } from "../services/api";
import type { SipConfig } from "../types/api";
import { useUiStore } from "../stores/ui";

const ui = useUiStore();
const loading = ref(false);
const saving = ref(false);
const loadError = ref("");
const passwordInput = ref("");
const currentPassword = ref("");
const directConfig = ref<Record<string, unknown>>({});
const form = reactive({
  port: 5060,
  id: "",
  domain: "",
  enable_tls: false,
  tls_port: 5061,
  tls_cert: "",
  tls_key: "",
  strict_source_check: true,
  require_message_auth: false,
  ptz_weak_confirm: false,
  device_history: { max_records: 1000, max_days: 30 },
});
const tlsConfigValid = computed(
  () =>
    !form.enable_tls ||
    (Boolean(form.tls_cert.trim()) && Boolean(form.tls_key.trim()))
);

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const { data } = await api.configInfo();
    const sip = data.sip || {};
    Object.assign(form, {
      port: sip.port || 5060,
      id: sip.id || "",
      domain: sip.domain || "",
      enable_tls: Boolean(sip.enable_tls),
      tls_port: sip.tls_port || 5061,
      tls_cert: sip.tls_cert || "",
      tls_key: sip.tls_key || "",
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
    passwordInput.value = "";
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
    };
    await api.updateSip(body);
    if (passwordInput.value) currentPassword.value = passwordInput.value;
    passwordInput.value = "";
    ui.toast("SIP 配置已保存并热更新");
  } catch (cause) {
    ui.toast(errorMessage(cause, "SIP 配置保存失败"));
  } finally {
    saving.value = false;
  }
}

onMounted(load);
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
          <label class="form-group full"><span class="form-label">私钥路径</span><input v-model.trim="form.tls_key" class="input plain w-full mono" required :aria-invalid="!tlsConfigValid" /></label>
          <p v-if="!tlsConfigValid" class="field-error full" role="alert">启用 SIP-TLS 时必须同时配置证书和私钥路径。</p>
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

        <div class="settings-savebar">
          <span>保存会立即更新当前环境的 SIP 服务及历史保留配置</span>
          <button class="btn btn-primary" :disabled="saving || loading || !tlsConfigValid">
            <LoaderCircle v-if="saving" class="animate-spin" /><Save v-else />{{ saving ? "正在保存…" : "保存配置" }}
          </button>
        </div>
      </form>
    </section>
  </main>
</template>
