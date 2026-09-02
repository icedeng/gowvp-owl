<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { RouterLink, useRoute } from "vue-router";
import {
  Activity,
  CheckCircle2,
  LoaderCircle,
  Radio,
  RefreshCcw,
  Search,
  ShieldAlert,
  ShieldCheck,
  XCircle,
} from "@lucide/vue";
import { api, errorMessage, typeLabel } from "../services/api";
import type { ApiDevice, GbMetrics } from "../types/api";
import { formatDate } from "../utils/format";
import { useUiStore } from "../stores/ui";

const route = useRoute();
const ui = useUiStore();
const devices = ref<ApiDevice[]>([]);
const selectedId = ref("");
const deviceQuery = ref("");
const devicePage = ref(1);
const deviceTotal = ref(0);
const loadedDeviceCount = ref(0);
const loadingMoreDevices = ref(false);
const loading = ref(false);
const running = ref(false);
const loadError = ref("");
const metrics = ref<GbMetrics>({});
const metricsAvailable = ref(false);
const lastResult = ref("尚未在本次会话执行探测");
const DEVICE_PAGE_SIZE = 50;
let deviceSearchTimer: number | undefined;
let deviceLoadSequence = 0;
const selected = computed(
  () =>
    devices.value.find((item) => item.id === selectedId.value) ||
    devices.value[0]
);
const capabilityDefinitions = [
  ["BasicParam 查询", "2014+", "config_query"],
  ["BasicParam 写入", "2014+", "config_write"],
  ["目录扩展字段", "2014+", "catalog_extension"],
  ["目录订阅", "2011+", "directory_notify"],
  ["多响应消息", "2014+", "multi_response"],
  ["媒体结束通知", "2011+", "media_status"],
  ["语音广播", "2014+", "voice_broadcast"],
  ["语音对讲", "2011+（标准双流程为 2016+）", "voice_intercom"],
  ["RTP over TCP", "2016+", "rtp_over_tcp"],
  ["直接 TCP 下载", "仅 2014", "direct_tcp_download"],
  ["下载倍速", "2014+", "download_speed"],
  ["强制关键帧", "2016+", "iframe_control"],
  ["拉框缩放", "2014+", "drag_zoom_control"],
  ["预置位查询", "2014+", "preset_query"],
  ["移动位置订阅", "2016+", "mobile_position"],
  ["PTZ 精准位置", "仅 2022", "ptz_position"],
  ["看守位控制", "2016+", "home_position"],
  ["看守位查询", "仅 2022", "home_position_query"],
  ["巡航轨迹查询", "仅 2022", "cruise_track_query"],
  ["存储卡管理", "仅 2022", "sdcard"],
  ["H.265 视频", "仅 2022", "h265"],
  ["AAC 音频", "仅 2022", "aac"],
  ["设备抓拍", "仅 2022", "snapshot"],
  ["设备升级", "仅 2022", "upgrade"],
  ["目标跟踪", "仅 2022", "target_track"],
] as const;
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
const protocolProfile = computed(() => {
  const value = selected.value?.ext?.gb_effective_version || selected.value?.ext?.gb_version || "";
  switch (String(value).trim()) {
    case "1.0": case "2011": return "1.0";
    case "1.1": case "2014": case "2011-supplement-2014": return "1.1";
    case "2.0": case "2016": return "2.0";
    case "3.0": case "2022": return "3.0";
    default: return "";
  }
});
const declaredCapabilities = computed<string[] | undefined>(() => {
  const value = selected.value?.ext?.gb_version_capabilities;
  return Array.isArray(value) ? value : undefined;
});
const capabilities = computed(() =>
  new Set(declaredCapabilities.value || fallbackCapabilitiesByVersion[protocolProfile.value] || [])
);
const disabledCapabilities = computed(
  () => new Set(selected.value?.ext?.gb_disabled_capabilities || [])
);
const matrix = computed(() =>
  capabilityDefinitions.map(([name, version, key]) => ({
    name,
    version,
    state: disabledCapabilities.value.has(key)
      ? "disabled"
      : capabilities.value.has(key) ? "supported" : "unavailable",
    label: disabledCapabilities.value.has(key)
      ? "已禁用"
      : capabilities.value.has(key)
        ? declaredCapabilities.value ? "已声明" : "按版本支持"
        : declaredCapabilities.value ? "未声明" : "不支持",
  }))
);
const registerRate = computed(() =>
  metrics.value.register_requests
    ? (Number(metrics.value.register_success || 0) /
        metrics.value.register_requests) *
      100
    : 0
);
const mediaRate = computed(() =>
  metrics.value.media_requests
    ? (Number(metrics.value.media_success || 0) /
        metrics.value.media_requests) *
      100
    : 0
);
const canLoadMoreDevices = computed(
  () => loadedDeviceCount.value < deviceTotal.value
);

function isGbDevice(item: ApiDevice) {
  return typeLabel(item.type, item.device_id || item.id) === "GB28181";
}

async function loadDevicePage(reset = true) {
  const sequence = ++deviceLoadSequence;
  const requestedPage = reset ? 1 : devicePage.value + 1;
  if (!reset) loadingMoreDevices.value = true;
  try {
    const { data } = await api.devices({
      type: "GB28181",
      key: deviceQuery.value.trim() || undefined,
      page: requestedPage,
      size: DEVICE_PAGE_SIZE,
    });
    if (sequence !== deviceLoadSequence) return;
    const batch = (data?.items || []).filter(isGbDevice);
    devices.value = reset
      ? batch
      : [...new Map([...devices.value, ...batch].map((item) => [item.id, item])).values()];
    devicePage.value = requestedPage;
    loadedDeviceCount.value = reset
      ? (data?.items || []).length
      : loadedDeviceCount.value + (data?.items || []).length;
    deviceTotal.value = Number(data?.total ?? loadedDeviceCount.value);
    if (!devices.value.some((item) => item.id === selectedId.value)) {
      selectedId.value = devices.value[0]?.id || "";
    }
  } catch (cause) {
    loadError.value = errorMessage(cause, "诊断设备加载失败");
  } finally {
    if (sequence === deviceLoadSequence) loadingMoreDevices.value = false;
  }
}

async function load() {
  loading.value = true;
  loadError.value = "";
  metrics.value = {};
  metricsAvailable.value = false;
  try {
    const [deviceResult, metricsResult] = await Promise.allSettled([
      api.devices({ type: "GB28181", page: 1, size: DEVICE_PAGE_SIZE }),
      api.gbMetrics(),
    ]);
    if (deviceResult.status === "rejected") throw deviceResult.reason;
    const initialItems = deviceResult.value.data?.items || [];
    devices.value = initialItems.filter(isGbDevice);
    devicePage.value = 1;
    loadedDeviceCount.value = initialItems.length;
    deviceTotal.value = Number(deviceResult.value.data?.total ?? initialItems.length);
    const routeId = String(route.query.device || "");
    if (routeId && !devices.value.some((item) => item.id === routeId)) {
      try {
        const { data } = await api.device(routeId);
        if (isGbDevice(data)) devices.value = [data, ...devices.value];
      } catch {
        // 路由指定设备可能已删除，回退到首个可用设备。
      }
    }
    selectedId.value = devices.value.some((item) => item.id === routeId)
      ? routeId
      : devices.value[0]?.id || "";
    if (metricsResult.status === "fulfilled") {
      metrics.value = metricsResult.value.data;
      metricsAvailable.value = true;
    } else {
      loadError.value = `设备档案已加载，运行指标暂不可用：${errorMessage(
        metricsResult.reason,
        "当前服务未开放 GB28181 运行指标接口"
      )}`;
    }
  } catch (cause) {
    loadError.value = errorMessage(cause, "诊断数据加载失败");
  } finally {
    loading.value = false;
  }
}

async function run() {
  if (!selected.value) return;
  running.value = true;
  try {
    await api.optionsProbe(selected.value.id);
    const { data } = await api.devicePtzProbe(selected.value.id, {
      action: "stop",
      speed: 30,
      timeout: 5,
    });
    lastResult.value = `OPTIONS 成功；PTZ ${String(
      (data as { success_count?: number }).success_count ?? "—"
    )} 路通过`;
    ui.toast("OPTIONS 与 PTZ 能力探测已完成");
    const { data: refreshed } = await api.device(selected.value.id);
    const index = devices.value.findIndex((item) => item.id === refreshed.id);
    if (index >= 0) devices.value[index] = refreshed;
  } catch (cause) {
    lastResult.value = errorMessage(cause, "探测失败");
    ui.toast(lastResult.value);
  } finally {
    running.value = false;
  }
}

watch(deviceQuery, () => {
  window.clearTimeout(deviceSearchTimer);
  deviceSearchTimer = window.setTimeout(() => {
    loadError.value = "";
    void loadDevicePage(true);
  }, 350);
});
onMounted(load);
onBeforeUnmount(() => window.clearTimeout(deviceSearchTimer));
</script>

<template>
  <main class="page-content">
    <header class="page-head">
      <div>
        <h1 class="page-title">协议诊断</h1>
        <p class="page-desc">
          集中检查 GB 版本、能力矩阵、运行指标、最近不支持命令与探测结果。
        </p>
      </div>
      <div class="head-actions">
        <RouterLink class="btn" to="/gb28181-capabilities"><ShieldCheck />四版本能力</RouterLink>
        <button
            class="btn btn-primary"
            :disabled="running || !selected || !selected.is_online"
            :title="selected && !selected.is_online ? '设备离线时无法执行能力探测' : undefined"
        @click="run"
      >
        <RefreshCcw :class="{ 'animate-spin': running }" />{{
          running ? "正在探测…" : "执行能力探测"
        }}
      </button>
      </div>
    </header>
    <div v-if="loadError" class="warning-box mb-4" role="alert">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <section class="card card-pad mb-4">
      <div class="toolbar mb-0">
        <label class="field"
          ><Search /><input
            v-model="deviceQuery"
            class="input"
            aria-label="搜索诊断设备"
            placeholder="搜索设备名称或编号"
        /></label>
        <select
            v-model="selectedId"
            class="select"
            aria-label="选择诊断设备"
          >
            <option v-for="item in devices" :key="item.id" :value="item.id">
              {{ item.name || item.device_id || item.id }}
            </option>
          </select>
        <button
          v-if="canLoadMoreDevices"
          type="button"
          class="btn btn-sm"
          :disabled="loadingMoreDevices"
          @click="loadDevicePage(false)"
        >
          <LoaderCircle v-if="loadingMoreDevices" class="animate-spin" />
          {{ loadingMoreDevices ? "加载中…" : "更多设备" }}
        </button>
        <span class="protocol-tag blue"
          >GB/T 28181-{{
            selected?.ext?.gb_effective_version ||
            selected?.ext?.gb_version ||
            "未知"
          }}</span
        ><span
          class="status"
          :class="selected?.is_online ? 'online' : 'offline'"
          >{{ selected?.is_online ? "设备在线" : "设备离线" }}</span
        ><span class="toolbar-spacer" /><span class="section-note" aria-live="polite">{{
          lastResult
        }}</span>
      </div>
      <div v-if="loading" class="empty-state">
        <LoaderCircle class="mx-auto mb-2 animate-spin" />正在加载设备档案…
      </div>
      <div v-else-if="!devices.length" class="empty-state">
        当前环境没有 GB28181 设备。
      </div>
    </section>
    <section class="grid four-col mb-4">
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">注册成功率</h2>
            <p class="card-sub">SIP 进程累计指标</p>
          </div>
          <Activity />
        </div>
        <div class="metric-value">{{ metricsAvailable ? `${registerRate.toFixed(1)}%` : "—" }}</div>
        <p class="section-note mt-2">
          {{ metricsAvailable ? `${metrics.register_success || 0} / ${metrics.register_requests || 0} 次` : "指标暂不可用" }}
        </p>
      </article>
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">媒体请求成功率</h2>
            <p class="card-sub">实时媒体请求</p>
          </div>
          <Radio />
        </div>
        <div class="metric-value">{{ metricsAvailable ? `${mediaRate.toFixed(1)}%` : "—" }}</div>
        <p class="section-note mt-2">
          {{ metricsAvailable ? `${metrics.media_success || 0} / ${metrics.media_requests || 0} 次` : "指标暂不可用" }}
        </p>
      </article>
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">直连下载</h2>
            <p class="card-sub">进程累计任务</p>
          </div>
          <ShieldCheck />
        </div>
        <div class="metric-value">{{ metricsAvailable ? metrics.direct_tcp_started || 0 : "—" }}</div>
        <p class="section-note mt-2">
          {{ metricsAvailable ? `${metrics.direct_tcp_completed || 0} 完成 · ${metrics.direct_tcp_failed || 0} 失败` : "指标暂不可用" }}
        </p>
      </article>
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">附录 G 接入</h2>
            <p class="card-sub">进程累计请求与当前在途</p>
          </div>
          <ShieldCheck />
        </div>
        <div class="metric-value">{{ metricsAvailable ? metrics.annex_g_inbound_accepted || 0 : "—" }}</div>
        <p class="section-note mt-2">
          {{ metricsAvailable ? `${metrics.annex_g_inbound_requests || 0} 请求 · ${metrics.annex_g_inbound_rejected || 0} 拒绝 · ${metrics.annex_g_inbound_rate_limited || 0} 限流 · ${metrics.annex_g_business_failures || 0} 业务失败 · ${metrics.annex_g_pending || 0} 在途` : "指标暂不可用" }}
        </p>
      </article>
    </section>
    <section class="card card-pad verification-gates mb-4">
      <div class="card-head"><div><h2 class="card-title">生产验收阶段门</h2><p class="card-sub">自动化证据之外，以下项目必须在目标环境留存真实结果。</p></div><ShieldAlert /></div>
      <div class="gate-grid">
        <div><span class="protocol-tag amber">媒体</span><strong>RTP / RTCP 抓包</strong><small>四版本 UDP、2016/2022 TCP、SSRC 与来源地址隔离。</small><RouterLink to="/media-servers">查看媒体边界</RouterLink></div>
        <div><span class="protocol-tag amber">级联</span><strong>真实上下级与三级路径</strong><small>注册、订阅、查询、控制、媒体与报警全链路。</small><RouterLink to="/sip-settings#upstreams">查看上级配置</RouterLink></div>
        <div><span class="protocol-tag amber">安全</span><strong>证书与信令摘要</strong><small>真实 CA/CRL、Date + Note 算法、编码与独立 seed。</small><RouterLink to="/sip-settings#security">查看安全配置</RouterLink></div>
        <div><span class="protocol-tag amber">外部系统</span><strong>附录 G 互通与性能</strong><small>接处警、卡口、城市信息系统以及限流和审计。</small><RouterLink to="/gb28181-capabilities">查看业务审计</RouterLink></div>
      </div>
    </section>
    <section class="grid equal-col diagnostics-comparison">
      <article class="card table-card">
        <div class="card-head">
          <div>
            <h2 class="card-title">版本能力矩阵</h2>
            <p class="card-sub">
              来源：{{ declaredCapabilities ? selected?.ext?.gb_version_source || "设备档案" : "按有效版本档案回退" }}
            </p>
          </div>
          <ShieldCheck />
        </div>
        <div class="table-wrap">
          <table class="data-table">
            <thead>
              <tr>
                <th>能力</th>
                <th>最低版本</th>
                <th>协商结果</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="row in matrix" :key="row.name">
                <td>{{ row.name }}</td>
                <td>{{ row.version }}</td>
                <td>
                  <span
                    class="status"
                    :class="row.state === 'supported' ? 'online' : row.state === 'disabled' ? 'warning' : 'offline'"
                    ><CheckCircle2 v-if="row.state === 'supported'" /><ShieldAlert v-else-if="row.state === 'disabled'" /><XCircle v-else />{{
                      row.label
                    }}</span
                  >
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </article>
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">当前设备诊断</h2>
            <p class="card-sub">协议档案与最近不支持命令</p>
          </div>
          <ShieldAlert />
        </div>
        <dl class="definition-grid !grid-cols-1">
          <div>
            <dt>设备</dt>
            <dd>{{ selected?.name || selected?.device_id || "—" }}</dd>
          </div>
          <div>
            <dt>有效版本</dt>
            <dd>
              {{
                selected?.ext?.gb_effective_version ||
                selected?.ext?.gb_version ||
                "—"
              }}
            </dd>
          </div>
          <div>
            <dt>声明版本</dt>
            <dd>{{ selected?.ext?.gb_declared_version || "—" }}</dd>
          </div>
          <div>
            <dt>手动覆盖</dt>
            <dd>{{ selected?.ext?.gb_manual_version || "未设置" }}</dd>
          </div>
          <div>
            <dt>版本更新时间</dt>
            <dd>{{ selected?.ext?.gb_version_updated_at ? formatDate(selected.ext.gb_version_updated_at * 1000) : "—" }}</dd>
          </div>
          <div>
            <dt>REGISTER 绑定</dt>
            <dd>
              <span
                class="status"
                :class="selected?.is_online ? 'online' : selected?.ext?.gb_registration_closed === false ? 'warning' : 'offline'"
              >{{ selected?.is_online ? "有效" : selected?.ext?.gb_registration_closed === true ? "已关闭" : selected?.ext?.gb_registration_closed === false ? "有效但离线" : "未知" }}</span>
            </dd>
          </div>
          <div>
            <dt>最后不支持命令</dt>
            <dd>
              {{ selected?.ext?.gb_last_unsupported_command || "暂无记录" }}
            </dd>
          </div>
          <div>
            <dt>记录时间</dt>
            <dd>
              {{
                selected?.ext?.gb_last_unsupported_updated_at
                  ? formatDate(
                      selected.ext.gb_last_unsupported_updated_at * 1000
                    )
                  : "—"
              }}
            </dd>
          </div>
        </dl>
      </article>
    </section>
  </main>
</template>

<style scoped>
.diagnostics-comparison { align-items: start; }
.gate-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 9px; }
.gate-grid > div { min-width: 0; padding: 11px; background: #f8fafc; border: 1px solid var(--line); border-radius: 8px; }
.gate-grid strong, .gate-grid small, .gate-grid a { display: block; }.gate-grid strong { margin-top: 9px; color: var(--ink); font-size: 12px; }.gate-grid small { min-height: 3.2em; margin-top: 3px; color: var(--muted); font-size: 10px; line-height: 1.55; }.gate-grid a { margin-top: 9px; color: var(--blue); font-size: 11px; font-weight: 700; }
@media (max-width: 920px) { .gate-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 560px) { .gate-grid { grid-template-columns: 1fr; }.gate-grid small { min-height: 0; } }
</style>
