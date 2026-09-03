<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref } from "vue";
import { RouterLink } from "vue-router";
import {
  Activity,
  ChevronLeft,
  ChevronRight,
  CircleCheck,
  CircleDashed,
  FileSearch,
  LoaderCircle,
  Network,
  RefreshCcw,
  Search,
  Settings2,
  ShieldAlert,
  ShieldCheck,
} from "@lucide/vue";
import { api, countDevicesByType, errorMessage } from "../services/api";
import type {
  AnnexGAlarmAudit,
  AnnexGDefenceAudit,
  AnnexGDefenceState,
  CascadePlatformStatus,
  GbMetrics,
  SipConfig,
} from "../types/api";
import { formatDate } from "../utils/format";

type MatrixState = "ready" | "verify" | "limited" | "na";
type AuditTab = "alarms" | "defences" | "history";
type SummaryState = "idle" | "loading" | "ready" | "error";
type SummaryKey = "devices" | "media" | "config" | "metrics" | "cascade";

interface MatrixCell {
  state: MatrixState;
  label: string;
  detail?: string;
}

interface CapabilityRow {
  name: string;
  scope: string;
  versions: Record<"2011" | "2014" | "2016" | "2022", MatrixCell>;
}

const versions = ["2011", "2014", "2016", "2022"] as const;
const ready = (label = "代码完成", detail = "有自动化证据"): MatrixCell => ({ state: "ready", label, detail });
const verify = (label = "待互通", detail = "需真实设备或平台验收"): MatrixCell => ({ state: "verify", label, detail });
const limited = (label: string, detail: string): MatrixCell => ({ state: "limited", label, detail });
const na = (detail = "该版本未定义"): MatrixCell => ({ state: "na", label: "不适用", detail });

const capabilityRows: CapabilityRow[] = [
  {
    name: "注册、心跳与设备目录",
    scope: "REGISTER、Keepalive、DeviceInfo、Catalog 及在线生命周期",
    versions: { 2011: ready(), 2014: ready(), 2016: ready(), 2022: ready() },
  },
  {
    name: "基础查询、PTZ 与报警",
    scope: "设备信息、状态、录像检索、云台控制和报警处理",
    versions: { 2011: ready(), 2014: ready(), 2016: ready(), 2022: ready() },
  },
  {
    name: "配置与目录扩展",
    scope: "配置查询/写入、目录扩展字段和多响应消息",
    versions: { 2011: na(), 2014: ready(), 2016: ready(), 2022: ready() },
  },
  {
    name: "移动位置与看守位",
    scope: "MobilePosition 订阅；2016 起支持看守位控制，2022 增加查询",
    versions: { 2011: na(), 2014: na(), 2016: ready(), 2022: ready() },
  },
  {
    name: "直播、回放与 RTP 下载",
    scope: "UDP/TCP 协商、会话收敛、SSRC 与来源地址隔离",
    versions: { 2011: verify(), 2014: verify(), 2016: verify(), 2022: verify() },
  },
  {
    name: "语音广播与对讲",
    scope: "标准双流程及保留的厂商兼容 Talk 链路",
    versions: {
      2011: verify("兼容对讲已实现", "UDP 双向音频与厂商差异待互通"),
      2014: verify("广播与兼容对讲已实现", "标准广播与厂商差异待互通"),
      2016: verify("标准链路已实现", "音频方向与回声控制待互通"),
      2022: verify("标准链路已实现", "音频方向与回声控制待互通"),
    },
  },
  {
    name: "上下级平台级联",
    scope: "注册、目录、查询、控制、媒体、报警与订阅的版本门禁",
    versions: { 2011: verify(), 2014: verify(), 2016: verify(), 2022: verify() },
  },
  {
    name: "Date + Note 信令摘要",
    scope: "MD5、SHA-1、SHA-256、SM3；默认关闭并按对端隔离 seed",
    versions: { 2011: verify(), 2014: verify(), 2016: verify(), 2022: verify() },
  },
  {
    name: "Capability/Asymmetric 证书注册",
    scope: "设备与级联双向认证、CA/CRL、强制模式与防重放",
    versions: { 2011: verify(), 2014: verify(), 2016: verify(), 2022: na("2022 正文不再定义该流程") },
  },
  {
    name: "2014 附录 O 裸 TCP 下载",
    scope: "设备直连落盘与上级平台流式中继；共享白名单、大小、并发、超时和地址安全边界",
    versions: { 2011: na(), 2014: verify("管理已接入", "内置下载客户端与级联中继已有本地自动化证据；真实设备和上级平台互通待验收"), 2016: na(), 2022: na() },
  },
  {
    name: "附录 G 外部系统",
    scope: "接处警、卡口、城市信息系统的身份门禁、交换、存储与审计",
    versions: { 2011: verify(), 2014: verify(), 2016: verify(), 2022: na("2022 已删除附录 G") },
  },
  {
    name: "2022 扩展命令",
    scope: "A.4、升级、抓拍、目标跟踪、主动上传通知与注册重定向",
    versions: { 2011: na(), 2014: na(), 2016: na(), 2022: ready() },
  },
];

const loading = ref(false);
const loadError = ref("");
const deviceCount = ref<number | null>(null);
const mediaNodeCount = ref<number | null>(null);
const cascadeCount = ref<number | null>(null);
const cascadeStatuses = ref<CascadePlatformStatus[]>([]);
const sip = ref<SipConfig>({});
const metrics = ref<GbMetrics>({});
const summaryStates = reactive<Record<SummaryKey, SummaryState>>({
  devices: "idle",
  media: "idle",
  config: "idle",
  metrics: "idle",
  cascade: "idle",
});
const summaryRevision = ref(0);

const auditTab = ref<AuditTab>("alarms");
const auditLoading = ref(false);
const auditError = ref("");
const auditPage = ref(1);
const auditTotal = ref(0);
const alarmRows = ref<AnnexGAlarmAudit[]>([]);
const defenceRows = ref<AnnexGDefenceState[]>([]);
const defenceAuditRows = ref<AnnexGDefenceAudit[]>([]);
const filters = reactive({ kind: "mp", car_plate: "", tollgate_id: "" });
const pageSize = 20;
let auditLoadSequence = 0;

const annexEnabled = computed(() => Boolean(sip.value.annex_g?.enabled));
const annexStatus = computed(() => {
  if (summaryStates.config === "loading" || summaryStates.config === "idle") return "loading";
  if (summaryStates.config === "error") return "unknown";
  return annexEnabled.value ? "enabled" : "disabled";
});
const configuredVersions = computed(() => {
  const values = new Set<string>((sip.value.upstreams || []).map((item) => item.version));
  return ["1.0", "1.1", "2.0", "3.0"].filter((item) => values.has(item)).length;
});
const auditPageCount = computed(() => Math.max(1, Math.ceil(auditTotal.value / pageSize)));
const currentRows = computed(() => auditTab.value === "alarms"
  ? alarmRows.value
  : auditTab.value === "defences" ? defenceRows.value : defenceAuditRows.value);

function summaryValue(state: SummaryState, value: number | null) {
  if (state === "loading" || state === "idle") return "…";
  if (state === "error") return "—";
  return value ?? 0;
}

function summaryNote(state: SummaryState, ready: string) {
  if (state === "loading" || state === "idle") return "正在读取";
  if (state === "error") return "暂不可用";
  return ready;
}

function stateIcon(state: MatrixState) {
  return state === "ready" ? CircleCheck : state === "na" ? CircleDashed : ShieldAlert;
}

function kindLabel(kind?: string) {
  return ({ mp: "管理平台", ecs: "应急指挥", tgs: "卡口系统" } as Record<string, string>)[kind || ""] || kind || "—";
}

function cascadeStateLabel(item: { registered?: boolean; state?: string }) {
  if (item.registered) return "已注册";
  const state = String(item.state || "").trim().toLowerCase();
  return ({
    disabled: "已停用",
    error: "异常",
    failed: "注册失败",
    connecting: "连接中",
    registering: "注册中",
  } as Record<string, string>)[state] || (state ? item.state : "未注册");
}

function cascadeStateClass(item: { registered?: boolean; state?: string }) {
  if (item.registered) return "online";
  const state = String(item.state || "").trim().toLowerCase();
  return ["error", "failed"].includes(state) ? "offline" : "pending";
}

async function loadSummary() {
  loading.value = true;
  loadError.value = "";
  (Object.keys(summaryStates) as SummaryKey[]).forEach((key) => { summaryStates[key] = "loading"; });
  const [devices, media, config, gb, cascade] = await Promise.allSettled([
    countDevicesByType("GB28181"),
    api.mediaServers({ page: 1, size: 1 }),
    api.configInfo(),
    api.gbMetrics(),
    api.cascadeStatuses(),
  ]);
  summaryStates.devices = devices.status === "fulfilled" ? "ready" : "error";
  summaryStates.media = media.status === "fulfilled" ? "ready" : "error";
  summaryStates.config = config.status === "fulfilled" ? "ready" : "error";
  summaryStates.metrics = gb.status === "fulfilled" ? "ready" : "error";
  summaryStates.cascade = cascade.status === "fulfilled" ? "ready" : "error";
  deviceCount.value = devices.status === "fulfilled" ? devices.value : null;
  mediaNodeCount.value = media.status === "fulfilled" ? Number(media.value.data?.total ?? media.value.data?.items?.length ?? 0) : null;
  sip.value = config.status === "fulfilled" ? config.value.data.sip || {} : {};
  metrics.value = gb.status === "fulfilled" ? gb.value.data || {} : {};
  cascadeStatuses.value = cascade.status === "fulfilled" ? cascade.value.data.items || [] : [];
  cascadeCount.value = cascade.status === "fulfilled" ? cascadeStatuses.value.filter((item) => item.registered).length : null;
  const failures = [devices, media, config, gb, cascade].filter((item) => item.status === "rejected");
  if (failures.length) loadError.value = `运行摘要有 ${failures.length} 项暂不可用；能力矩阵仍可查阅。`;
  summaryRevision.value += 1;
  loading.value = false;
}

async function loadAudits(page = auditPage.value) {
  const sequence = ++auditLoadSequence;
  const tab = auditTab.value;
  if (annexStatus.value === "disabled") {
    auditLoading.value = false;
    auditError.value = "";
    auditTotal.value = 0;
    auditPage.value = 1;
    alarmRows.value = [];
    defenceRows.value = [];
    defenceAuditRows.value = [];
    return;
  }
  auditLoading.value = true;
  auditError.value = "";
  if (tab === "alarms") alarmRows.value = [];
  else if (tab === "defences") defenceRows.value = [];
  else defenceAuditRows.value = [];
  try {
    const common = {
      page,
      page_size: pageSize,
      car_plate: filters.car_plate || undefined,
      tollgate_id: filters.tollgate_id || undefined,
    };
    if (tab === "alarms") {
      const { data } = await api.annexGAlarms({ ...common, kind: filters.kind });
      if (sequence !== auditLoadSequence || tab !== auditTab.value) return;
      alarmRows.value = data.items || [];
      auditTotal.value = Number(data.total || 0);
    } else if (tab === "defences") {
      const { data } = await api.annexGDefences(common);
      if (sequence !== auditLoadSequence || tab !== auditTab.value) return;
      defenceRows.value = data.items || [];
      auditTotal.value = Number(data.total || 0);
    } else {
      const { data } = await api.annexGDefenceAudits(common);
      if (sequence !== auditLoadSequence || tab !== auditTab.value) return;
      defenceAuditRows.value = data.items || [];
      auditTotal.value = Number(data.total || 0);
    }
    auditPage.value = page;
  } catch (cause) {
    if (sequence !== auditLoadSequence || tab !== auditTab.value) return;
    auditError.value = errorMessage(cause, "附录 G 审计加载失败");
    auditTotal.value = 0;
    auditPage.value = 1;
  } finally {
    if (sequence === auditLoadSequence) auditLoading.value = false;
  }
}

function changeAuditTab(tab: AuditTab) {
  auditTab.value = tab;
  auditPage.value = 1;
  void loadAudits(1);
}

async function moveAuditTab(event: KeyboardEvent) {
  const order: AuditTab[] = ["alarms", "defences", "history"];
  const current = order.indexOf(auditTab.value);
  let target = current;
  if (event.key === "ArrowRight") target = (current + 1) % order.length;
  else if (event.key === "ArrowLeft") target = (current - 1 + order.length) % order.length;
  else if (event.key === "Home") target = 0;
  else if (event.key === "End") target = order.length - 1;
  else return;
  event.preventDefault();
  changeAuditTab(order[target]);
  await nextTick();
  document.getElementById(`audit-tab-${order[target]}`)?.focus();
}

function searchAudits() {
  auditPage.value = 1;
  void loadAudits(1);
}

async function refresh() {
  await loadSummary();
  await loadAudits();
}

onMounted(refresh);
</script>

<template>
  <main class="page-content gb-capability-page">
    <header class="page-head">
      <div>
        <h1 class="page-title">国标能力</h1>
        <p class="page-desc">集中核对 GB/T 28181 四版本实现边界、当前运行配置和附录 G 业务审计。</p>
      </div>
      <div class="head-actions">
        <RouterLink class="btn" to="/sip-settings"><Settings2 />配置 SIP</RouterLink>
        <button class="btn btn-primary" :disabled="loading || auditLoading" @click="refresh">
          <RefreshCcw :class="{ 'animate-spin': loading || auditLoading }" />刷新状态
        </button>
      </div>
    </header>

    <div v-if="loadError" class="warning-box mb-4" role="status"><ShieldAlert /><span>{{ loadError }}</span></div>

    <section :key="summaryRevision" class="runtime-strip" :class="{ 'is-updated': summaryRevision > 0 }" aria-label="国标运行摘要" :aria-busy="loading" aria-live="polite">
      <div><span>国标设备</span><strong>{{ summaryValue(summaryStates.devices, deviceCount) }}</strong><small>{{ summaryNote(summaryStates.devices, "当前设备档案") }}</small></div>
      <div><span>媒体节点</span><strong>{{ summaryValue(summaryStates.media, mediaNodeCount) }}</strong><small>{{ summaryNote(summaryStates.media, "收流基础设施") }}</small></div>
      <div><span>已注册上级</span><strong>{{ summaryValue(summaryStates.cascade, cascadeCount) }}</strong><small>{{ summaryNote(summaryStates.cascade, `${configuredVersions} 个版本档案已配置`) }}</small></div>
      <div><span>附录 G</span><strong>{{ annexStatus === "enabled" ? "已启用" : annexStatus === "disabled" ? "未启用" : annexStatus === "unknown" ? "暂不可用" : "读取中" }}</strong><small>{{ summaryStates.metrics === "ready" ? `${metrics.annex_g_pending ?? 0} 条在途关联` : summaryNote(summaryStates.metrics, "0 条在途关联") }}</small></div>
    </section>

    <section class="card capability-workbench">
      <div class="card-head">
        <div><h2 class="card-title">四版本实现矩阵</h2><p class="card-sub">实现状态来自项目审计；“待互通”表示代码与自动化完成，但不能替代真实设备、上级或网络验收。</p></div>
        <ShieldCheck />
      </div>
      <div class="matrix-legend" aria-label="状态图例">
        <span class="legend-ready"><CircleCheck />代码完成</span>
        <span class="legend-verify"><ShieldAlert />待真实互通</span>
        <span class="legend-limited"><ShieldAlert />兼容或受限</span>
        <span class="legend-na"><CircleDashed />版本不适用</span>
      </div>
      <p class="matrix-scroll-hint">左右滑动查看 2011、2014、2016、2022 四个版本</p>
      <div class="capability-table-wrap" tabindex="0" aria-label="四版本实现矩阵，可横向滚动">
        <table class="capability-table">
          <thead><tr><th>能力域</th><th v-for="version in versions" :key="version">{{ version }}</th></tr></thead>
          <tbody>
            <tr v-for="row in capabilityRows" :key="row.name">
              <th scope="row"><strong>{{ row.name }}</strong><small>{{ row.scope }}</small></th>
              <td v-for="version in versions" :key="version">
                <span class="matrix-state" :class="row.versions[version].state">
                  <component :is="stateIcon(row.versions[version].state)" />
                  <span><b>{{ row.versions[version].label }}</b><small>{{ row.versions[version].detail }}</small></span>
                </span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <footer class="matrix-foot">
        <span><Activity />发布门禁仍包括真实 RTP/RTCP 抓包、三级级联、证书体系和厂商互通。</span>
        <RouterLink to="/diagnostics">打开协议诊断</RouterLink>
      </footer>
    </section>

    <section class="card cascade-workbench">
      <div class="card-head">
        <div><h2 class="card-title">上级平台级联状态</h2><p class="card-sub">查看四版本 SIP 级联的协商版本、注册生命周期和最近一次错误。</p></div>
        <Network />
      </div>
      <div v-if="summaryStates.cascade === 'loading' || summaryStates.cascade === 'idle'" class="cascade-empty" aria-live="polite"><LoaderCircle class="animate-spin" /><span>正在读取上级平台注册状态…</span></div>
      <div v-else-if="summaryStates.cascade === 'error'" class="cascade-empty" role="status"><ShieldAlert /><span><strong>级联状态暂不可用</strong><small>配置或状态接口读取失败，请刷新后重试。</small></span><button class="btn btn-sm" type="button" @click="loadSummary">重试</button></div>
      <div v-else-if="!cascadeStatuses.length" class="cascade-empty"><Network /><span><strong>尚未启用上级平台</strong><small>在 SIP 设置中启用上级平台后，这里会显示版本协商和注册状态。</small></span><RouterLink class="btn btn-sm" to="/sip-settings#upstreams">配置级联</RouterLink></div>
      <div v-else class="table-wrap cascade-table-wrap">
        <table class="data-table cascade-table">
          <thead><tr><th>上级平台</th><th>地址</th><th>协议版本</th><th>注册状态</th><th>最近注册</th><th>有效期</th><th>最近错误</th></tr></thead>
          <tbody>
            <tr v-for="item in cascadeStatuses" :key="`${item.name}-${item.server_id}`">
              <td data-label="上级平台"><span class="stacked-value"><strong>{{ item.name || item.server_id || "未命名平台" }}</strong><small class="mono">{{ item.server_id || "—" }}</small></span></td>
              <td data-label="地址" class="mono">{{ item.address || "—" }}</td>
              <td data-label="协议版本"><span class="protocol-tag blue">{{ item.negotiated_version || item.configured_version || "—" }}</span><small v-if="item.negotiated_version && item.configured_version !== item.negotiated_version" class="version-downgrade">协商低于配置</small></td>
              <td data-label="注册状态"><span class="status" :class="cascadeStateClass(item)">{{ cascadeStateLabel(item) }}</span></td>
              <td data-label="最近注册">{{ formatDate(item.last_register_at) }}</td>
              <td data-label="有效期">{{ formatDate(item.expires_at) }}</td>
              <td data-label="最近错误"><span class="cascade-error">{{ item.last_error || "—" }}</span></td>
            </tr>
          </tbody>
        </table>
      </div>
      <footer class="cascade-foot"><span><Activity />级联业务仍需真实上级平台和三级链路抓包验收。</span><span class="cascade-links"><RouterLink to="/gb28181-cascade">打开级联平台工作台</RouterLink><RouterLink to="/sip-settings#upstreams">配置 SIP 级联</RouterLink></span></footer>
    </section>

    <section class="card audit-workbench">
      <div class="card-head">
        <div><h2 class="card-title">附录 G 业务审计</h2><p class="card-sub">2011/2014/2016 外部系统的报警记录、当前布控和不可变布撤控历史。</p></div>
        <FileSearch />
      </div>
      <div v-if="annexStatus === 'disabled'" class="audit-disabled"><ShieldAlert /><span><strong>附录 G 当前未启用</strong><small>审计存储随附录 G 运行时启用；请完成静态配置并重启服务后查询。</small></span><RouterLink class="btn btn-sm" to="/sip-settings#annex-g">前往配置</RouterLink></div>
      <div v-else-if="annexStatus === 'unknown'" class="audit-disabled" role="status"><ShieldAlert /><span><strong>附录 G 配置暂不可用</strong><small>配置接口读取失败，以下审计查询仍可尝试；请稍后刷新确认运行状态。</small></span><button class="btn btn-sm" type="button" @click="refresh">重试</button></div>
      <template v-if="annexStatus !== 'disabled'">
        <div class="audit-toolbar">
          <div class="segmented" role="tablist" aria-label="附录 G 审计类型" @keydown="moveAuditTab">
            <button id="audit-tab-alarms" :class="{ active: auditTab === 'alarms' }" role="tab" :tabindex="auditTab === 'alarms' ? 0 : -1" :aria-selected="auditTab === 'alarms'" aria-controls="audit-panel" @click="changeAuditTab('alarms')">报警记录</button>
            <button id="audit-tab-defences" :class="{ active: auditTab === 'defences' }" role="tab" :tabindex="auditTab === 'defences' ? 0 : -1" :aria-selected="auditTab === 'defences'" aria-controls="audit-panel" @click="changeAuditTab('defences')">当前布控</button>
            <button id="audit-tab-history" :class="{ active: auditTab === 'history' }" role="tab" :tabindex="auditTab === 'history' ? 0 : -1" :aria-selected="auditTab === 'history'" aria-controls="audit-panel" @click="changeAuditTab('history')">布撤控历史</button>
          </div>
          <form class="audit-filters" @submit.prevent="searchAudits">
            <label v-if="auditTab === 'alarms'"><span class="sr-only">报警来源</span><select v-model="filters.kind" class="input plain"><option value="mp">管理平台</option><option value="ecs">应急指挥系统</option><option value="tgs">卡口系统</option></select></label>
            <label><span class="sr-only">车牌号码</span><input v-model.trim="filters.car_plate" class="input plain" placeholder="车牌号码" /></label>
            <label><span class="sr-only">卡口编码</span><input v-model.trim="filters.tollgate_id" class="input plain mono" placeholder="卡口编码" /></label>
            <button class="btn" :disabled="auditLoading"><Search />查询</button>
          </form>
        </div>

        <div v-if="auditError" class="warning-box audit-warning" role="alert"><ShieldAlert /><span>{{ auditError }}</span><button class="btn btn-sm ml-auto" @click="loadAudits()">重试</button></div>
        <div id="audit-panel" class="table-wrap audit-table-wrap" role="tabpanel" :aria-labelledby="`audit-tab-${auditTab}`" :aria-busy="auditLoading">
          <table v-if="auditTab === 'alarms'" class="data-table audit-table">
            <thead><tr><th>来源</th><th>报警时间</th><th>设备 / 卡口</th><th>类型 / 优先级</th><th>位置 / 车牌</th><th>报警编号</th></tr></thead>
            <tbody><tr v-for="item in alarmRows" :key="item.id"><td><span class="protocol-tag blue">{{ kindLabel(item.kind) }}</span></td><td>{{ formatDate(item.alarm_time) }}</td><td class="mono">{{ item.device_id || item.tollgate_id || "—" }}</td><td>{{ [item.alarm_class, item.alarm_priority].filter(Boolean).join(" / ") || "—" }}</td><td>{{ item.alarm_address || item.car_plate || "—" }}</td><td class="mono">{{ item.alarm_no || "—" }}</td></tr></tbody>
          </table>
          <table v-else class="data-table audit-table">
            <thead><tr><th>卡口编码</th><th>车牌号码</th><th>号牌类型</th><th>布控类型</th><th>状态</th><th>{{ auditTab === "defences" ? "更新时间" : "审计时间" }}</th></tr></thead>
            <tbody><tr v-for="item in currentRows" :key="item.id"><td class="mono">{{ 'tollgate_id' in item ? item.tollgate_id || "—" : "—" }}</td><td>{{ 'car_plate' in item ? item.car_plate || "—" : "—" }}</td><td>{{ 'plate_type' in item ? item.plate_type || "—" : "—" }}</td><td>{{ 'defence_type' in item ? item.defence_type || "—" : "—" }}</td><td><span class="status" :class="'active' in item && item.active ? 'online' : 'offline'">{{ 'active' in item && item.active ? "布控中" : "已撤控" }}</span></td><td>{{ formatDate(auditTab === "defences" && 'updated_at' in item ? item.updated_at : 'created_at' in item ? item.created_at : undefined) }}</td></tr></tbody>
          </table>
          <div v-if="auditLoading" class="audit-empty"><LoaderCircle class="animate-spin" /><span>正在读取附录 G 审计…</span></div>
          <div v-else-if="!auditError && !currentRows.length" class="audit-empty"><FileSearch /><span><strong>没有匹配记录</strong><small>调整筛选条件，或等待外部系统产生业务数据。</small></span></div>
        </div>
        <footer class="audit-pagination">
          <span>共 {{ auditTotal }} 条 · 第 {{ auditPage }} / {{ auditPageCount }} 页</span>
          <div><button class="page-btn" :disabled="auditPage <= 1 || auditLoading" aria-label="上一页" @click="loadAudits(auditPage - 1)"><ChevronLeft /></button><button class="page-btn" :disabled="auditPage >= auditPageCount || auditLoading" aria-label="下一页" @click="loadAudits(auditPage + 1)"><ChevronRight /></button></div>
        </footer>
      </template>
    </section>
  </main>
</template>

<style scoped>
.gb-capability-page { max-width: 1680px; }
.runtime-strip { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); margin-bottom: 14px; background: #fff; border: 1px solid var(--line); border-radius: var(--radius); overflow: hidden; }
.runtime-strip.is-updated { animation: runtime-confirm .48s cubic-bezier(.16, 1, .3, 1); }
.runtime-strip > div { min-width: 0; display: grid; grid-template-columns: 1fr auto; gap: 3px 12px; padding: 15px 18px; border-right: 1px solid var(--line); }
.runtime-strip > div:last-child { border-right: 0; }
.runtime-strip span, .runtime-strip small { color: var(--muted); font-size: 12px; }
.runtime-strip strong { color: var(--ink); font: 700 20px "Barlow Condensed", sans-serif; font-variant-numeric: tabular-nums; }
.runtime-strip small { grid-column: 1 / -1; }
.capability-workbench, .cascade-workbench, .audit-workbench { padding: 18px; }
.audit-workbench { margin-top: 14px; }
.cascade-workbench { margin-top: 14px; }
.matrix-legend { display: flex; flex-wrap: wrap; gap: 8px 18px; margin: 12px 0; color: var(--muted); font-size: 12px; }
.matrix-legend span { display: inline-flex; align-items: center; gap: 6px; }
.matrix-legend svg { width: 14px; height: 14px; }
.legend-ready svg { color: #15805b; }.legend-verify svg { color: #b66b00; }.legend-limited svg { color: #a33f32; }.legend-na svg { color: #7f8b9b; }
.matrix-scroll-hint { display: none; margin: 0 0 8px; color: var(--muted); font-size: 11px; }
.capability-table-wrap { overflow-x: auto; border: 1px solid var(--line); border-radius: 9px; }
.capability-table { width: 100%; min-width: 980px; border-collapse: collapse; table-layout: fixed; }
.capability-table th, .capability-table td { padding: 12px; border-right: 1px solid var(--line); border-bottom: 1px solid var(--line); text-align: left; vertical-align: top; }
.capability-table th:last-child, .capability-table td:last-child { border-right: 0; }
.capability-table tbody tr:last-child > * { border-bottom: 0; }
.capability-table thead th { color: #3f5066; background: #f7f9fb; font-size: 12px; }
.capability-table thead th:first-child { width: 30%; }
.capability-table tbody th strong, .capability-table tbody th small { display: block; }
.capability-table tbody th strong { margin-bottom: 4px; color: var(--ink); font-size: 13px; }
.capability-table tbody th small { color: var(--muted); font-size: 11px; font-weight: 400; line-height: 1.55; }
.matrix-state { display: flex; gap: 7px; color: var(--muted); }
.matrix-state > svg { flex: 0 0 auto; width: 15px; height: 15px; margin-top: 1px; }
.matrix-state span, .matrix-state b, .matrix-state small { display: block; }
.matrix-state b { color: var(--ink); font-size: 12px; }
.matrix-state small { margin-top: 3px; font-size: 10px; line-height: 1.45; }
.matrix-state.ready > svg { color: #15805b; }.matrix-state.verify > svg { color: #b66b00; }.matrix-state.limited > svg { color: #a33f32; }.matrix-state.na > svg { color: #8793a2; }
.matrix-foot, .audit-pagination { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 12px; color: var(--muted); font-size: 12px; }
.matrix-foot span { display: inline-flex; align-items: center; gap: 7px; }.matrix-foot svg { width: 15px; }.matrix-foot a { color: var(--blue); font-weight: 700; }
.cascade-empty { min-height: 132px; display: flex; align-items: center; justify-content: center; gap: 10px; color: var(--muted); }
.cascade-empty > svg { width: 20px; flex: 0 0 auto; }.cascade-empty span { display: block; }.cascade-empty strong, .cascade-empty small { display: block; }.cascade-empty small { margin-top: 3px; }
.cascade-table-wrap { border: 1px solid var(--line); border-radius: 9px; }.cascade-table { min-width: 980px; }
.cascade-table td, .cascade-table th { vertical-align: top; }.cascade-table .stacked-value strong, .cascade-table .stacked-value small { display: block; }.cascade-table .stacked-value small { margin-top: 3px; color: var(--muted); }
.version-downgrade { display: block; margin-top: 4px; color: var(--amber); font-size: 10px; }.cascade-error { display: block; max-width: 280px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--muted); }
.cascade-foot { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 12px; color: var(--muted); font-size: 12px; }.cascade-foot span { display: inline-flex; align-items: center; gap: 7px; }.cascade-foot svg { width: 15px; }.cascade-foot a { color: var(--blue); font-weight: 700; }.cascade-links { display: inline-flex; align-items: center; gap: 14px; }
.audit-disabled { display: flex; align-items: center; gap: 10px; padding: 11px 12px; margin: 12px 0; color: #795416; background: #fff8e9; border: 1px solid #edd9ad; border-radius: 9px; }
.audit-disabled > svg { width: 18px; }.audit-disabled span { flex: 1; }.audit-disabled strong, .audit-disabled small { display: block; }.audit-disabled small { margin-top: 2px; color: #88692f; }
.audit-toolbar { display: flex; align-items: center; justify-content: space-between; gap: 14px; margin: 14px 0 10px; }
.segmented { display: inline-flex; padding: 3px; background: #eef2f6; border-radius: 8px; }
.segmented button { min-height: 34px; padding: 0 11px; color: var(--muted); background: transparent; border: 0; border-radius: 6px; font-weight: 600; cursor: pointer; }
.segmented button.active { color: var(--blue); background: #fff; box-shadow: 0 3px 9px rgba(28, 51, 79, .09); }
.audit-filters { display: flex; gap: 8px; }.audit-filters .input { min-height: 38px; }.audit-filters input { width: 150px; }
.audit-warning { margin-bottom: 10px; }.audit-table-wrap { position: relative; min-height: 170px; border: 1px solid var(--line); border-radius: 9px; }.audit-table { min-width: 860px; }
.audit-empty { min-height: 160px; display: flex; align-items: center; justify-content: center; gap: 10px; color: var(--muted); }
.audit-empty svg { width: 20px; }.audit-empty strong, .audit-empty small { display: block; }.audit-empty small { margin-top: 3px; }
.audit-pagination > div { display: flex; gap: 6px; }
.sr-only { position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; border: 0; }
@keyframes runtime-confirm { from { border-color: rgba(23, 104, 212, .42); box-shadow: 0 0 0 3px rgba(23, 104, 212, .08); } to { border-color: var(--line); box-shadow: 0 0 0 0 rgba(23, 104, 212, 0); } }
@media (max-width: 980px) { .runtime-strip { grid-template-columns: repeat(2, minmax(0, 1fr)); }.runtime-strip > div:nth-child(2) { border-right: 0; }.runtime-strip > div:nth-child(-n+2) { border-bottom: 1px solid var(--line); }.audit-toolbar { align-items: stretch; flex-direction: column; }.audit-filters { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); }.audit-filters label:first-child, .audit-filters .btn { grid-column: auto; }.audit-filters input { width: 100%; } }
@media (max-width: 600px) { .runtime-strip { grid-template-columns: repeat(2, minmax(0, 1fr)); }.runtime-strip > div { padding: 13px; }.capability-workbench, .cascade-workbench, .audit-workbench { padding: 14px; }.matrix-scroll-hint { display: block; }.capability-table thead th:first-child { width: 220px; }.capability-table tr > :first-child { position: sticky; left: 0; z-index: 1; background: #fff; box-shadow: 1px 0 0 var(--line); }.capability-table thead th:first-child { z-index: 2; background: #f7f9fb; }.matrix-foot, .cascade-foot, .audit-pagination, .audit-disabled { align-items: stretch; flex-direction: column; }.audit-pagination .page-btn { width: 44px; height: 44px; }.audit-filters { grid-template-columns: 1fr; }.segmented { width: 100%; overflow-x: auto; }.segmented button { flex: 1 0 auto; min-height: 44px; } }
@media (prefers-reduced-motion: reduce) { .runtime-strip.is-updated { animation: none; } }
</style>
