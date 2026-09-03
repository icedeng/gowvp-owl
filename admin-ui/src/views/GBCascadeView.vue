<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { RouterLink } from "vue-router";
import {
  Activity,
  AlertTriangle,
  ArrowRight,
  CheckCircle2,
  CircleDashed,
  LoaderCircle,
  Network,
  RefreshCcw,
  Settings2,
  ShieldCheck,
} from "@lucide/vue";
import { api, errorMessage } from "../services/api";
import type { CascadePlatformStatus } from "../types/api";
import { formatDate } from "../utils/format";

type StatusFilter = "all" | "registered" | "pending" | "error";

const loading = ref(false);
const loadError = ref("");
const hasLoaded = ref(false);
const platforms = ref<CascadePlatformStatus[]>([]);
const statusFilter = ref<StatusFilter>("all");
const versionFilter = ref("all");

const versions = ["1.0", "1.1", "2.0", "3.0"] as const;
const errorStates = new Set(["error", "failed", "expired", "stopped"]);
const versionRanks: Record<string, number> = { "1.0": 1, "1.1": 2, "2.0": 3, "3.0": 4 };

function normalizeVersion(value?: string) {
  const normalized = String(value || "").trim().toLowerCase();
  return ({
    "2011": "1.0",
    "2014": "1.1",
    "2011-supplement-2014": "1.1",
    "2016": "2.0",
    "2022": "3.0",
  } as Record<string, string>)[normalized] || normalized;
}

function versionLabel(value?: string) {
  const normalized = normalizeVersion(value);
  return ({
    "1.0": "2011（1.0）",
    "1.1": "2014（1.1）",
    "2.0": "2016（2.0）",
    "3.0": "2022（3.0）",
  } as Record<string, string>)[normalized] || value || "版本未知";
}

function versionRank(value?: string) {
  return versionRanks[normalizeVersion(value)] || 0;
}

function isVersionDowngrade(item: CascadePlatformStatus) {
  if (!item.negotiated_version || !item.configured_version) return false;
  const negotiated = versionRank(item.negotiated_version);
  const configured = versionRank(item.configured_version);
  return negotiated > 0 && configured > 0 && negotiated < configured;
}

function statusLabel(item: CascadePlatformStatus) {
  if (item.registered) return "已注册";
  const state = String(item.state || "").trim().toLowerCase();
  return ({
    starting: "正在启动",
    retrying: "等待重试",
    stopping: "正在停止",
    stopped: "已停止",
    expired: "注册已过期",
    error: "异常",
    failed: "注册失败",
    connecting: "连接中",
    registering: "注册中",
  } as Record<string, string>)[state] || (state ? item.state : "未注册");
}

function statusTone(item: CascadePlatformStatus) {
  if (item.registered) return "online";
  const state = String(item.state || "").toLowerCase();
  return errorStates.has(state) || state === "stopped" ? "offline" : "pending";
}

function statusIcon(item: CascadePlatformStatus) {
  if (item.registered) return CheckCircle2;
  const state = String(item.state || "").toLowerCase();
  return errorStates.has(state) || state === "stopped" ? AlertTriangle : CircleDashed;
}

const filteredPlatforms = computed(() => platforms.value.filter((item) => {
  const matchesStatus = statusFilter.value === "all"
    || (statusFilter.value === "registered" && item.registered)
    || (statusFilter.value === "error" && errorStates.has(String(item.state || "").toLowerCase()))
    || (statusFilter.value === "pending" && !item.registered && !errorStates.has(String(item.state || "").toLowerCase()))
  const negotiated = normalizeVersion(item.negotiated_version || item.configured_version);
  return matchesStatus && (versionFilter.value === "all" || negotiated === versionFilter.value);
}));

const summary = computed(() => ({
  total: platforms.value.length,
  registered: platforms.value.filter((item) => item.registered).length,
  attention: platforms.value.filter((item) => !item.registered).length,
  versions: new Set(platforms.value.map((item) => normalizeVersion(item.negotiated_version || item.configured_version)).filter(Boolean)).size,
}));

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const { data } = await api.cascadeStatuses();
    platforms.value = data.items || [];
    hasLoaded.value = true;
  } catch (cause) {
    loadError.value = errorMessage(cause, "级联平台状态加载失败");
  } finally {
    loading.value = false;
  }
}

onMounted(load);
</script>

<template>
  <main class="page-content cascade-page">
    <header class="page-head">
      <div>
        <h1 class="page-title">级联平台</h1>
        <p class="page-desc">集中查看上下级平台的四版本协商、注册生命周期和媒体级联运行状态（待真实验收）。</p>
      </div>
      <div class="head-actions">
        <RouterLink class="btn" to="/sip-settings#upstreams"><Settings2 />配置上级平台</RouterLink>
        <button class="btn btn-primary" type="button" :disabled="loading" @click="load"><RefreshCcw :class="{ 'animate-spin': loading }" />刷新状态</button>
      </div>
    </header>

    <div v-if="loadError && hasLoaded" class="warning-box mb-4" role="alert"><AlertTriangle /><span>{{ loadError }}；当前继续显示最近一次成功结果。</span><button class="btn btn-sm ml-auto" type="button" @click="load">重试</button></div>

    <section class="cascade-summary" aria-label="级联运行摘要" :aria-busy="loading">
      <div><span>启用平台</span><strong>{{ hasLoaded ? summary.total : loading ? "…" : "—" }}</strong><small>{{ hasLoaded ? "运行中的上级档案" : loading ? "正在读取状态" : "状态暂不可用" }}</small></div>
      <div><span>注册成功</span><strong>{{ hasLoaded ? summary.registered : loading ? "…" : "—" }}</strong><small>{{ hasLoaded ? summary.total ? `${Math.round(summary.registered / summary.total * 100)}% 正常` : "等待启用" : loading ? "正在读取状态" : "状态暂不可用" }}</small></div>
      <div><span>需要关注</span><strong>{{ hasLoaded ? summary.attention : loading ? "…" : "—" }}</strong><small>{{ hasLoaded ? summary.attention ? "检查网络或凭据" : "暂无异常平台" : loading ? "正在读取状态" : "状态暂不可用" }}</small></div>
      <div><span>协商版本</span><strong>{{ hasLoaded ? summary.versions : loading ? "…" : "—" }}</strong><small>{{ hasLoaded ? "1.0 / 1.1 / 2.0 / 3.0" : loading ? "正在读取状态" : "状态暂不可用" }}</small></div>
    </section>

    <section class="card cascade-panel">
      <div class="card-head">
        <div><h2 class="card-title">平台注册矩阵</h2><p class="card-sub">状态来自运行时级联 worker；协商版本低于配置时会明确标记。</p></div>
        <Network />
      </div>
      <div class="cascade-toolbar">
        <div class="segmented" role="group" aria-label="按注册状态筛选">
          <button type="button" :class="{ active: statusFilter === 'all' }" :aria-pressed="statusFilter === 'all'" @click="statusFilter = 'all'">全部 <span>{{ summary.total }}</span></button>
          <button type="button" :class="{ active: statusFilter === 'registered' }" :aria-pressed="statusFilter === 'registered'" @click="statusFilter = 'registered'">已注册 <span>{{ summary.registered }}</span></button>
          <button type="button" :class="{ active: statusFilter === 'pending' }" :aria-pressed="statusFilter === 'pending'" @click="statusFilter = 'pending'">待处理</button>
          <button type="button" :class="{ active: statusFilter === 'error' }" :aria-pressed="statusFilter === 'error'" @click="statusFilter = 'error'">异常</button>
        </div>
        <select v-model="versionFilter" class="select" aria-label="按协议版本筛选">
          <option value="all">全部版本</option>
          <option v-for="version in versions" :key="version" :value="version">{{ versionLabel(version) }}</option>
        </select>
      </div>

      <div v-if="loading && !hasLoaded" class="cascade-empty" aria-live="polite"><LoaderCircle class="animate-spin" /><span>正在读取上级平台注册状态…</span></div>
      <div v-else-if="loadError && !hasLoaded" class="cascade-empty" role="alert"><AlertTriangle /><span><strong>级联状态暂不可用</strong><small>{{ loadError }}</small></span><button class="btn btn-sm" type="button" @click="load">重试</button></div>
      <div v-else-if="!platforms.length" class="cascade-empty"><Network /><span><strong>尚未启用上级平台</strong><small>在 SIP 设置中启用上级平台后，这里会显示注册、版本和最近错误。</small></span><RouterLink class="btn btn-sm" to="/sip-settings#upstreams">去配置</RouterLink></div>
      <div v-else-if="!filteredPlatforms.length" class="cascade-empty"><CircleDashed /><span><strong>没有匹配的平台</strong><small>调整状态或版本筛选后重试。</small></span></div>
      <div v-else class="cascade-platform-list">
        <article v-for="item in filteredPlatforms" :key="`${item.name}-${item.server_id}`" class="cascade-platform-row">
          <div class="cascade-platform-main">
            <span class="cascade-platform-icon" :class="statusTone(item)"><component :is="statusIcon(item)" /></span>
            <div><strong>{{ item.name || item.server_id || "未命名平台" }}</strong><small class="mono">{{ item.server_id || "—" }}</small></div>
          </div>
          <dl class="cascade-platform-facts">
            <div><dt>地址</dt><dd class="mono">{{ item.address || "—" }}</dd></div>
            <div><dt>协议版本</dt><dd><span class="protocol-tag blue">{{ versionLabel(item.negotiated_version || item.configured_version) }}</span><small v-if="isVersionDowngrade(item)" class="version-warning">低于配置</small></dd></div>
            <div><dt>注册状态</dt><dd><span class="status" :class="statusTone(item)">{{ statusLabel(item) }}</span></dd></div>
            <div><dt>有效期</dt><dd>{{ formatDate(item.expires_at) }}</dd></div>
          </dl>
          <div class="cascade-platform-last"><span>最近注册 {{ formatDate(item.last_register_at) }}</span><span v-if="item.last_error" class="cascade-error" :title="item.last_error" :aria-label="`最近错误：${item.last_error}`">{{ item.last_error }}</span><span v-else>最近心跳 {{ formatDate(item.last_keepalive_at) }}</span></div>
        </article>
      </div>
      <footer class="cascade-panel-foot"><span><Activity />级联媒体、目录和订阅仍需真实上级平台及三级链路验收。</span><RouterLink to="/gb28181-capabilities">查看四版本能力矩阵 <ArrowRight /></RouterLink></footer>
    </section>

    <section class="cascade-next-steps" aria-label="级联运维入口">
      <article class="card"><ShieldCheck /><div><h2>先确认版本，再看业务</h2><p>使用能力矩阵核对 2011、2014、2016、2022 的门禁；本页只展示运行时协商结果。</p></div><RouterLink class="btn" to="/gb28181-capabilities">打开能力矩阵</RouterLink></article>
      <article class="card"><Settings2 /><div><h2>需要修改级联参数？</h2><p>上级平台地址、传输方式、摘要种子和共享通道均在 SIP 设置中维护。</p></div><RouterLink class="btn" to="/sip-settings#upstreams">打开 SIP 设置</RouterLink></article>
    </section>
  </main>
</template>

<style scoped>
.cascade-page { max-width: 1480px; }
.cascade-summary { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); margin-bottom: 14px; background: #fff; border: 1px solid var(--line); border-radius: var(--radius); overflow: hidden; }
.cascade-summary > div { display: grid; grid-template-columns: 1fr auto; gap: 3px 12px; padding: 15px 18px; border-right: 1px solid var(--line); }
.cascade-summary > div:last-child { border-right: 0; }
.cascade-summary span, .cascade-summary small { color: var(--muted); font-size: 12px; }
.cascade-summary strong { color: var(--ink); font: 700 22px "Barlow Condensed", sans-serif; font-variant-numeric: tabular-nums; }
.cascade-summary small { grid-column: 1 / -1; }
.cascade-panel { padding: 18px; }
.cascade-toolbar { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin: 14px 0; }
.segmented { display: inline-flex; padding: 3px; background: #eef2f6; border-radius: 8px; }
.segmented button { min-height: 34px; padding: 0 11px; color: var(--muted); background: transparent; border: 0; border-radius: 6px; font-weight: 600; cursor: pointer; }
.segmented button.active { color: var(--blue); background: #fff; box-shadow: 0 3px 9px rgba(28, 51, 79, .09); }
.segmented button span { margin-left: 4px; color: inherit; font: 700 11px "Barlow Condensed", sans-serif; }
.cascade-empty { min-height: 220px; display: flex; align-items: center; justify-content: center; gap: 10px; color: var(--muted); }
.cascade-empty > svg { width: 20px; flex: 0 0 auto; }.cascade-empty span { display: block; }.cascade-empty strong, .cascade-empty small { display: block; }.cascade-empty small { margin-top: 3px; }
.cascade-platform-list { display: grid; gap: 8px; }
.cascade-platform-row { display: grid; grid-template-columns: minmax(220px, 1.2fr) minmax(450px, 2fr) minmax(180px, .9fr); align-items: center; gap: 18px; padding: 14px; background: #f8fafc; border: 1px solid var(--line); border-radius: 9px; }
.cascade-platform-main { display: flex; align-items: center; gap: 10px; min-width: 0; }.cascade-platform-main strong, .cascade-platform-main small { display: block; }.cascade-platform-main strong { color: var(--ink-strong); }.cascade-platform-main small { margin-top: 3px; color: var(--muted); }
.cascade-platform-icon { display: grid; place-items: center; width: 34px; height: 34px; border-radius: 8px; background: var(--panel-3); }.cascade-platform-icon svg { width: 17px; }.cascade-platform-icon.online { color: var(--green); background: var(--green-soft); }.cascade-platform-icon.pending { color: var(--amber); background: var(--amber-soft); }.cascade-platform-icon.offline { color: var(--red); background: var(--red-soft); }
.cascade-platform-facts { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 10px; margin: 0; }.cascade-platform-facts dt { color: var(--muted); font-size: 10px; }.cascade-platform-facts dd { margin: 4px 0 0; color: var(--ink); font-size: 12px; }.cascade-platform-facts dd small { display: block; margin-top: 3px; }.version-warning { color: #8b5100; }.cascade-platform-last { display: grid; gap: 4px; min-width: 0; color: var(--muted); font-size: 11px; }.cascade-error { overflow-wrap: anywhere; color: #b4232b; white-space: normal; }
.cascade-page :deep(.status.online) { color: #126b49; }.cascade-page :deep(.status.pending) { color: #8b5100; }.cascade-page :deep(.status.offline) { color: #b4232b; }
.cascade-platform-icon.online { color: #126b49; }.cascade-platform-icon.pending { color: #8b5100; }.cascade-platform-icon.offline { color: #b4232b; }
.cascade-panel-foot { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 13px; color: var(--muted); font-size: 12px; }.cascade-panel-foot span { display: inline-flex; align-items: center; gap: 7px; }.cascade-panel-foot svg { width: 15px; }.cascade-panel-foot a { display: inline-flex; align-items: center; gap: 4px; color: var(--blue); font-weight: 700; }.cascade-panel-foot a svg { width: 14px; }
.cascade-next-steps { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; margin-top: 14px; }.cascade-next-steps article { display: grid; grid-template-columns: auto 1fr auto; align-items: center; gap: 12px; padding: 15px 17px; }.cascade-next-steps article > svg { width: 20px; color: var(--blue); }.cascade-next-steps h2 { margin: 0; color: var(--ink-strong); font-size: 14px; }.cascade-next-steps p { margin: 4px 0 0; color: var(--muted); font-size: 12px; line-height: 1.5; }
@media (max-width: 1100px) { .cascade-platform-row { grid-template-columns: 1fr; gap: 12px; }.cascade-platform-last { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 760px) { .cascade-summary { grid-template-columns: repeat(2, minmax(0, 1fr)); }.cascade-summary > div:nth-child(2) { border-right: 0; }.cascade-summary > div:nth-child(-n+2) { border-bottom: 1px solid var(--line); }.cascade-panel { padding: 14px; }.cascade-toolbar, .cascade-panel-foot { align-items: stretch; flex-direction: column; }.segmented { width: 100%; overflow-x: auto; }.segmented button { flex: 1 0 auto; min-height: 44px; }.cascade-platform-facts { grid-template-columns: repeat(2, minmax(0, 1fr)); }.cascade-next-steps { grid-template-columns: 1fr; }.cascade-next-steps article { grid-template-columns: auto 1fr; }.cascade-next-steps article .btn { grid-column: 1 / -1; width: 100%; } }
</style>
