<script setup lang="ts">
import {
  computed,
  onBeforeUnmount,
  onMounted,
  reactive,
  ref,
  watch,
} from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  AppWindow,
  ArrowLeft,
  Bell,
  BookOpen,
  Camera,
  ChevronLeft,
  ChevronRight,
  FileText,
  KeyRound,
  ListFilter,
  LoaderCircle,
  Plus,
  RefreshCcw,
  Search,
  Settings2,
  ShieldAlert,
  ShieldCheck,
  SlidersHorizontal,
  Webhook,
  X,
} from "@lucide/vue";
import ModalDialog from "../components/ModalDialog.vue";
import { useUiStore } from "../stores/ui";
import { formatDate } from "../utils/format";
import {
  defaultOpenApp,
  openRequest,
  type OpenApp,
  type OpenDevice,
  type OpenPage,
  type OpenRow,
} from "../services/open-platform";

const route = useRoute();
const router = useRouter();
const ui = useUiStore();
const apps = ref<OpenApp[]>([]),
  total = ref(0),
  page = ref(1),
  query = ref("");
const error = ref(""),
  busy = ref(false),
  loading = ref(false),
  rowsLoading = ref(false);
const selected = ref<OpenApp | null>(null),
  rows = ref<OpenRow[]>([]),
  rowTotal = ref(0),
  rowPage = ref(1);
const scopes = ref<string[]>([]),
  actions = ref<string[]>([]),
  allowHTTP = ref(false);
const editOpen = ref(false),
  editing = ref(false),
  form = reactive(defaultOpenApp()),
  cidrsText = ref(""),
  expires = ref("");
const devices = ref<OpenDevice[]>([]),
  deviceTotal = ref(0),
  devicePage = ref(1),
  deviceQuery = ref(""),
  chosen = ref<string[]>([]),
  authorized = ref<OpenDevice[]>([]);
const secret = ref<{
    access_key?: string;
    key_id?: string;
    secret_key: string;
  } | null>(null),
  secretExpiry = ref(""),
  credentialOpen = ref(false);
const targetOpen = ref(false),
  targetID = ref(""),
  target = reactive({ url: "", cidrs: "", status: "pending", revision: 0 });
const auditAction = ref(""),
  auditStatus = ref(""),
  auditStart = ref(""),
  auditEnd = ref("");
const details = ref<OpenRow | null>(null);
let routeSequence = 0;
let rowsSequence = 0;
const scopeNames: Record<string, string> = {
  "device:read": "设备查询",
  "channel:read": "通道查询",
  "live:play": "实时播放",
  "recording:read": "录像查询",
  "recording:play": "录像播放",
  "recording:download": "录像下载",
  "event:read": "告警查询",
  "event:image": "告警图片",
  "event:video": "事件视频播放",
  "event:download": "事件视频下载",
  "event:process": "事件处理",
  "snapshot:capture": "抓拍",
  "ptz:control": "云台控制",
  "event:subscribe": "告警订阅",
};
const tabs = [
  {
    id: "overview",
    name: "应用概览",
    note: "基本信息与接入策略",
    icon: AppWindow,
  },
  {
    id: "credentials",
    name: "AK / SK",
    note: "应用凭据与密钥轮换",
    icon: KeyRound,
  },
  {
    id: "authorization",
    name: "接口与设备授权",
    note: "接口能力与设备范围",
    icon: ShieldCheck,
  },
  {
    id: "callback-targets",
    name: "回调目标",
    note: "回调地址与审核",
    icon: Webhook,
  },
  {
    id: "subscriptions",
    name: "告警订阅",
    note: "应用订阅与当前状态",
    icon: Bell,
  },
  {
    id: "deliveries",
    name: "投递记录",
    note: "告警结果与受控重投",
    icon: FileText,
  },
  {
    id: "snapshot-tasks",
    name: "抓拍任务",
    note: "任务状态与执行结果",
    icon: Camera,
  },
  {
    id: "ptz-commands",
    name: "云台记录",
    note: "控制记录与安全停止",
    icon: SlidersHorizontal,
  },
  {
    id: "audit-logs",
    name: "审计日志",
    note: "调用记录与错误排查",
    icon: FileText,
  },
];
const quotas = [
  { key: "qps", name: "每秒请求数", max: 1000 },
  { key: "burst", name: "突发请求数", max: 2000 },
  { key: "live_limit", name: "播放会话数", max: 100 },
  { key: "channel_limit", name: "每通道会话数", max: 100 },
  { key: "download_limit", name: "录像读取并发", max: 20 },
  { key: "snapshot_limit", name: "抓拍并发", max: 20 },
  { key: "subscription_limit", name: "订阅数量", max: 100 },
  { key: "delivery_limit", name: "投递并发 / 频率", max: 20 },
] as const;
const recordLabels: Record<string, string> = {
  id: "记录 ID",
  app_id: "应用 ID",
  access_key: "AK",
  credential_id: "凭据 ID",
  status: "状态",
  state: "执行状态",
  result: "结果",
  request_id: "请求 ID",
  created_at: "创建时间",
  updated_at: "更新时间",
  expires_at: "到期时间",
  revoked_at: "撤销时间",
  url: "回调 URL",
  cidrs: "批准的目标 CIDR",
  revision: "配置版本",
  channel_id: "通道 ID",
  action: "操作",
  event_id: "事件 ID",
  callback_target_id: "回调目标 ID",
  subscription_id: "订阅 ID",
  attempts: "尝试次数",
  next_attempt_at: "下次尝试时间",
  last_error: "最近错误",
  error: "错误说明",
  http_status: "HTTP 状态",
  source_ip: "来源 IP",
  method: "请求方法",
  path: "请求路径",
  duration_ms: "耗时（毫秒）",
  channel_ids: "通道范围",
  labels: "告警标签",
  stop_error: "停止错误",
  stopped_at: "停止时间",
  started_at: "开始时间",
  finished_at: "结束时间",
  captured_at: "抓拍时间",
  input: "云台控制参数",
  deadline: "动作截止时间",
  stop_attempts: "停止尝试次数",
  next_stop_at: "下次停止时间",
  attempt: "投递尝试次数",
  last_status: "最近响应状态",
  next_retry_at: "下次重试时间",
  key_configured: "已配置回调签名密钥",
  actor: "操作人",
  resource: "操作资源",
  ip: "来源 IP",
};
const isDetail = computed(() => route.name === "open-platform-detail");
const appID = computed(() => String(route.params.id || ""));
const tab = computed(() =>
  tabs.some((item) => item.id === route.query.tab)
    ? String(route.query.tab)
    : "overview"
);
const currentTab = computed(() => tabs.find((item) => item.id === tab.value)!);
const selectedIDs = computed(() => new Set(chosen.value));
const pageCount = computed(() => Math.max(1, Math.ceil(total.value / 20)));
const rowPageCount = computed(() =>
  Math.max(1, Math.ceil(rowTotal.value / 20))
);
const blocked = computed(() => busy.value || loading.value);
const listQuery = computed(() => ({
  ...(route.query.q ? { q: String(route.query.q) } : {}),
  ...(route.query.page ? { page: String(route.query.page) } : {}),
}));
const listLink = computed(() => ({
  name: "open-platform",
  query: listQuery.value,
}));
const recordHeadings = computed(
  () =>
    ({
      credentials: ["AK / 凭据", "状态", "创建时间"],
      "callback-targets": ["目标 ID", "审核状态", "回调地址"],
      subscriptions: ["订阅 ID", "状态", "回调目标"],
      deliveries: ["投递 ID", "投递状态", "事件 ID"],
      "snapshot-tasks": ["任务 ID", "任务状态", "通道 ID"],
      "ptz-commands": ["命令 ID", "执行状态", "动作 / 通道"],
      "audit-logs": ["请求 / 记录 ID", "结果", "操作"],
    }[tab.value] || ["记录 ID", "状态", "摘要"])
);

function effectiveStatus(row: {
  status?: unknown;
  state?: unknown;
  result?: unknown;
  expires_at?: unknown;
}) {
  if (
    row.expires_at &&
    new Date(String(row.expires_at)).getTime() <= Date.now() &&
    ["enabled", "active"].includes(String(row.status))
  )
    return "expired";
  return row.status || row.state || row.result;
}
const statusName = (value: unknown) =>
  ({
    enabled: "已启用",
    disabled: "已停用",
    archived: "已归档",
    active: "有效",
    revoked: "已撤销",
    pending: "待处理",
    approved: "已批准",
    paused: "已暂停",
    cancelled: "已取消",
    queued: "排队中",
    sending: "投递中",
    delivered: "投递成功",
    dead: "死信",
    failed: "失败",
    succeeded: "成功",
    running: "执行中",
    unknown: "结果待核对",
    confirmed: "已发送",
    completed: "已完成",
    stopped: "已停止",
    stop_failed: "停止失败",
    expired: "已到期",
  }[String(value)] || String(value || "—"));
function statusTone(value: unknown) {
  if (
    [
      "enabled",
      "active",
      "approved",
      "delivered",
      "succeeded",
      "completed",
      "stopped",
    ].includes(String(value))
  )
    return "online";
  if (["failed", "dead", "stop_failed"].includes(String(value)))
    return "danger";
  if (
    ["pending", "paused", "queued", "unknown", "expired"].includes(
      String(value)
    )
  )
    return "warning";
  if (["sending", "running", "confirmed"].includes(String(value)))
    return "info";
  return "";
}
function normalizeApp(app: OpenApp): OpenApp {
  return {
    ...app,
    scopes: app.scopes || [],
    device_ids: app.device_ids || [],
    allowed_actions: app.allowed_actions || [],
    cidrs: app.cidrs || [],
  };
}
function fail(cause: unknown) {
  error.value = cause instanceof Error ? cause.message : "操作失败，请重试";
}
async function run(fn: () => Promise<void>) {
  if (blocked.value) return;
  busy.value = true;
  error.value = "";
  try {
    await fn();
  } catch (cause) {
    fail(cause);
  } finally {
    busy.value = false;
  }
}
async function load() {
  const data = await openRequest<OpenPage<OpenApp>>("GET", "/apps", undefined, {
    page: page.value,
    size: 20,
    q: query.value,
  });
  apps.value = (data.items || []).map(normalizeApp);
  total.value = data.total;
}
async function loadRows() {
  if (!selected.value || tab.value === "overview") return;
  const sequence = ++rowsSequence;
  const id = selected.value.id,
    activeTab = tab.value;
  rows.value = [];
  authorized.value = [];
  rowTotal.value = 0;
  rowsLoading.value = true;
  try {
    if (activeTab === "authorization") {
      const data = await openRequest<{ items: OpenDevice[] }>(
        "GET",
        `/apps/${id}/authorized-devices`
      );
      if (
        sequence === rowsSequence &&
        id === appID.value &&
        activeTab === tab.value
      )
        authorized.value = data.items || [];
      return;
    }
    const path =
      activeTab === "audit-logs" ? "/audit-logs" : `/apps/${id}/${activeTab}`;
    const data = await openRequest<OpenPage<OpenRow>>("GET", path, undefined, {
      page: rowPage.value,
      size: 20,
      app_id: id,
      ...(activeTab === "audit-logs"
        ? {
            action: auditAction.value,
            status: auditStatus.value,
            start: auditStart.value
              ? new Date(`${auditStart.value}Z`).toISOString()
              : undefined,
            end: auditEnd.value
              ? new Date(`${auditEnd.value}Z`).toISOString()
              : undefined,
          }
        : {}),
    });
    if (
      sequence === rowsSequence &&
      id === appID.value &&
      activeTab === tab.value
    ) {
      rows.value = data.items || [];
      rowTotal.value = data.total;
    }
  } finally {
    if (sequence === rowsSequence) rowsLoading.value = false;
  }
}
async function refreshRoute() {
  const sequence = ++routeSequence;
  ++rowsSequence;
  loading.value = true;
  rowsLoading.value = false;
  error.value = "";
  details.value = null;
  secret.value = null;
  editOpen.value = credentialOpen.value = targetOpen.value = false;
  rowPage.value = 1;
  try {
    if (!scopes.value.length) await loadStatus();
    if (sequence !== routeSequence) return;
    if (isDetail.value) {
      if (selected.value?.id !== appID.value) selected.value = null;
      const app = normalizeApp(
        await openRequest<OpenApp>("GET", `/apps/${appID.value}`)
      );
      if (sequence !== routeSequence) return;
      selected.value = app;
      await loadRows();
    } else {
      selected.value = null;
      query.value = String(route.query.q || "");
      const requestedPage = Number(route.query.page);
      page.value =
        Number.isInteger(requestedPage) && requestedPage > 0
          ? requestedPage
          : 1;
      await load();
    }
  } catch (cause) {
    if (sequence === routeSequence) fail(cause);
  } finally {
    if (sequence === routeSequence) loading.value = false;
  }
}
function detailLink(app: OpenApp) {
  return {
    name: "open-platform-detail",
    params: { id: app.id },
    query: listQuery.value,
  };
}
function changeTab(value: string) {
  return router.replace({ query: { ...route.query, tab: value } });
}
function searchApps(nextPage = 1) {
  const nextQuery = {
    ...(query.value.trim() ? { q: query.value.trim() } : {}),
    ...(nextPage > 1 ? { page: String(nextPage) } : {}),
  };
  if (route.query.q === nextQuery.q && route.query.page === nextQuery.page)
    return refreshRoute();
  return router.replace({ name: "open-platform", query: nextQuery });
}
async function loadDevices() {
  const data = await openRequest<OpenPage<OpenDevice>>(
    "GET",
    "/devices",
    undefined,
    { q: deviceQuery.value, page: devicePage.value, size: 20 }
  );
  devices.value = data.items || [];
  deviceTotal.value = data.total;
}
function toggleDevice(id: string, check: boolean) {
  chosen.value = check
    ? [...new Set([...chosen.value, id])]
    : chosen.value.filter((value) => value !== id);
}
function toggleMode() {
  if (form.device_access_mode === "all") chosen.value = [];
}
async function edit(app?: OpenApp) {
  if (blocked.value) return;
  error.value = "";
  editing.value = Boolean(app);
  Object.assign(form, app ? JSON.parse(JSON.stringify(app)) : defaultOpenApp());
  chosen.value = [...(app?.device_ids || [])];
  cidrsText.value = form.cidrs.join("\n");
  expires.value = form.expires_at
    ? new Date(form.expires_at).toISOString().slice(0, 16)
    : "";
  devicePage.value = 1;
  deviceQuery.value = "";
  editOpen.value = true;
  await run(loadDevices);
}
async function save() {
  await run(async () => {
    const id = form.id;
    const body = Object.fromEntries(
      Object.keys(defaultOpenApp())
        .filter((key) => key !== "id" && key !== "status")
        .map((key) => [key, Reflect.get(form, key)])
    ) as Omit<OpenApp, "id" | "status">;
    body.device_ids =
      body.device_access_mode === "all" ? [] : [...chosen.value];
    body.cidrs = body.allow_any_ip
      ? []
      : cidrsText.value
          .split(/[\n,]+/)
          .map((value) => value.trim())
          .filter(Boolean);
    body.expires_at = expires.value
      ? new Date(`${expires.value}Z`).toISOString()
      : null;
    if (
      body.device_access_mode === "selected_devices" &&
      !body.device_ids.length
    )
      throw new Error("请至少勾选一台设备");
    const saved = normalizeApp(
      await openRequest<OpenApp>(
        editing.value ? "PATCH" : "POST",
        editing.value ? `/apps/${id}` : "/apps",
        body
      )
    );
    editOpen.value = false;
    if (isDetail.value && appID.value === saved.id) {
      selected.value = saved;
      await loadRows();
    } else {
      await router.push(detailLink(saved));
    }
    ui.toast("应用已保存");
  });
}
async function setStatus(app: OpenApp, action: string) {
  await run(async () => {
    await openRequest("POST", `/apps/${app.id}/${action}`, {});
    if (isDetail.value) {
      selected.value = normalizeApp(
        await openRequest<OpenApp>("GET", `/apps/${app.id}`)
      );
      await loadRows();
    } else {
      await load();
    }
    ui.toast("应用状态已更新");
  });
}
async function createCredential() {
  if (!selected.value) return;
  await run(async () => {
    const data = await openRequest<{
      credential: { access_key: string };
      secret_key: string;
    }>("POST", `/apps/${selected.value!.id}/credentials`, {
      expires_at: secretExpiry.value
        ? new Date(`${secretExpiry.value}Z`).toISOString()
        : null,
    });
    credentialOpen.value = false;
    secretExpiry.value = "";
    secret.value = {
      access_key: data.credential.access_key,
      secret_key: data.secret_key,
    };
    await loadRows();
  });
}
async function revoke(row: OpenRow) {
  await run(async () => {
    await openRequest(
      "POST",
      `/apps/${selected.value!.id}/credentials/${row.id}/revoke`,
      {}
    );
    await loadRows();
    ui.toast("凭据及关联会话已撤销");
  });
}
async function retry(row: OpenRow) {
  await run(async () => {
    await openRequest(
      "POST",
      `/apps/${selected.value!.id}/deliveries/${row.id}/retry`,
      {}
    );
    await loadRows();
    ui.toast("已提交受控重投");
  });
}
async function stop(row: OpenRow) {
  await run(async () => {
    await openRequest(
      "POST",
      `/apps/${selected.value!.id}/ptz-commands/${row.id}/stop`,
      {}
    );
    await loadRows();
    ui.toast("安全停止结果已更新");
  });
}
function editTarget(row?: OpenRow) {
  error.value = "";
  targetID.value = row?.id || "";
  Object.assign(target, {
    url: String(row?.url || ""),
    cidrs: ((row?.cidrs || []) as string[]).join("\n"),
    status: String(row?.status || "pending"),
    revision: row?.revision || 0,
  });
  targetOpen.value = true;
}
async function saveTarget() {
  await run(async () => {
    await openRequest(
      targetID.value ? "PATCH" : "POST",
      `/apps/${selected.value!.id}/callback-targets${
        targetID.value ? `/${targetID.value}` : ""
      }`,
      {
        ...target,
        cidrs: target.cidrs
          .split(/[\n,]+/)
          .map((value) => value.trim())
          .filter(Boolean),
      }
    );
    targetOpen.value = false;
    await loadRows();
    ui.toast("回调目标已保存，地址变化后需重新批准");
  });
}
function recordSummary(row: OpenRow) {
  if (tab.value === "credentials")
    return formatDate(String(row.created_at || ""));
  if (tab.value === "ptz-commands")
    return [
      (row.input as { action?: string } | undefined)?.action,
      row.channel_id,
    ]
      .filter(Boolean)
      .join(" · ");
  return (
    row.url ||
    row.action ||
    row.channel_id ||
    row.event_id ||
    row.callback_target_id ||
    "—"
  );
}
function present(value: unknown, key = "") {
  if (value == null || value === "") return "—";
  if (["status", "state", "result"].includes(key)) return statusName(value);
  if (key.endsWith("_at") && typeof value === "string")
    return formatDate(value);
  if (typeof value === "boolean") return value ? "是" : "否";
  return typeof value === "object"
    ? JSON.stringify(value, null, 2)
    : String(value);
}
watch(() => route.fullPath, refreshRoute);
async function loadStatus() {
  const status = await openRequest<{
    scopes: string[];
    default_actions: string[];
    allow_http: boolean;
  }>("GET", "/status");
  scopes.value = status.scopes || [];
  actions.value = [
    ...(status.default_actions || []),
    "preset_set",
    "preset_delete",
    "cruise_add",
    "cruise_del",
    "cruise_speed",
    "cruise_stay",
    "cruise_start",
    "scan_start",
    "scan_left",
    "scan_right",
    "scan_speed",
    "aux_on",
    "aux_off",
  ];
  allowHTTP.value = status.allow_http;
}
onMounted(refreshRoute);
onBeforeUnmount(() => {
  secret.value = null;
  ++routeSequence;
  ++rowsSequence;
});
</script>

<template>
  <main
    class="page-content open-platform-page"
    :class="{ 'device-detail-page': isDetail }"
  >
    <header v-if="!isDetail" class="page-head">
      <div>
        <h1 class="page-title">开放平台</h1>
        <p class="page-desc">
          管理三方应用的接入凭据、接口权限与设备访问范围。
        </p>
      </div>
      <div class="head-actions">
        <RouterLink class="btn" :to="{ name: 'open-platform-docs' }"
          ><BookOpen aria-hidden="true" />接入文档</RouterLink
        >
        <button class="btn" :disabled="blocked" @click="refreshRoute">
          <RefreshCcw
            :class="{ 'animate-spin': loading }"
            aria-hidden="true"
          />刷新
        </button>
        <button
          class="btn btn-primary"
          :disabled="blocked || !scopes.length"
          @click="edit()"
        >
          <Plus aria-hidden="true" />创建应用
        </button>
      </div>
    </header>

    <div
      v-if="error && !editOpen && !credentialOpen && !targetOpen && !secret"
      class="warning-box mb-4"
      role="alert"
    >
      <ShieldAlert aria-hidden="true" /><span>{{ error }}</span>
      <button
        class="btn btn-sm ml-auto"
        :disabled="blocked"
        @click="refreshRoute"
      >
        重新加载
      </button>
    </div>

    <section
      v-if="!isDetail"
      class="card table-card"
      aria-label="三方应用列表"
      :aria-busy="loading"
    >
      <form class="toolbar" @submit.prevent="searchApps()">
        <label class="field open-search"
          ><Search aria-hidden="true" /><input
            v-model="query"
            class="input"
            type="search"
            placeholder="搜索应用名称、接入单位"
            aria-label="搜索应用"
        /></label>
        <button class="btn btn-sm" :disabled="blocked">搜索</button>
        <button
          v-if="route.query.q"
          type="button"
          class="btn btn-sm btn-ghost"
          :disabled="blocked"
          @click="
            query = '';
            searchApps();
          "
        >
          <X aria-hidden="true" />清除筛选
        </button>
        <span class="toolbar-spacer" />
        <span class="section-note" aria-live="polite"
          >共 {{ total }} 个应用</span
        >
      </form>
      <div class="table-wrap">
        <table class="data-table open-data-table open-app-table">
          <thead>
            <tr>
              <th scope="col">应用 / 接入单位</th>
              <th scope="col">状态</th>
              <th scope="col">设备访问范围</th>
              <th scope="col">接口权限</th>
              <th scope="col">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="app in apps" :key="app.id">
              <td data-label="应用 / 接入单位">
                <RouterLink
                  :to="detailLink(app)"
                  class="row-title compact open-app-link"
                  ><span class="open-app-icon"
                    ><AppWindow aria-hidden="true" /></span
                  ><span
                    ><strong>{{ app.name }}</strong
                    ><small>{{
                      app.organization || "未填写接入单位"
                    }}</small></span
                  ></RouterLink
                >
              </td>
              <td data-label="状态">
                <span
                  class="status"
                  :class="statusTone(effectiveStatus(app))"
                  >{{ statusName(effectiveStatus(app)) }}</span
                >
              </td>
              <td data-label="设备访问范围">
                <span>{{
                  app.device_access_mode === "all"
                    ? "全部设备"
                    : `指定设备 ${app.device_ids.length} 台`
                }}</span
                ><small
                  v-if="app.device_access_mode === 'all'"
                  class="open-cell-note"
                  >包含今后新增设备</small
                >
              </td>
              <td data-label="接口权限">
                <span
                  :title="
                    app.scopes
                      .map((value) => scopeNames[value] || value)
                      .join('、')
                  "
                  >{{
                    app.scopes.length
                      ? `已授权 ${app.scopes.length} 项`
                      : "未授权"
                  }}</span
                >
              </td>
              <td data-label="操作">
                <div class="open-row-actions">
                  <RouterLink class="btn btn-sm btn-ghost" :to="detailLink(app)"
                    >详情<ChevronRight aria-hidden="true"
                  /></RouterLink>
                  <button
                    class="btn btn-sm"
                    :disabled="blocked || app.status === 'archived'"
                    @click="edit(app)"
                  >
                    编辑 / 授权
                  </button>
                  <button
                    class="btn btn-sm btn-ghost"
                    :disabled="blocked || app.status === 'archived'"
                    @click="
                      setStatus(
                        app,
                        app.status === 'enabled' ? 'disable' : 'enable'
                      )
                    "
                  >
                    {{ app.status === "enabled" ? "停用" : "启用" }}
                  </button>
                  <button
                    v-if="app.status === 'disabled'"
                    class="btn btn-sm btn-ghost"
                    :disabled="blocked"
                    @click="setStatus(app, 'archive')"
                  >
                    归档
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-if="loading && !apps.length" class="empty-state" role="status">
          <LoaderCircle
            class="mx-auto mb-3 h-6 w-6 animate-spin"
          />正在加载应用…
        </div>
        <div
          v-else-if="!apps.length && !error"
          class="empty-state empty-action"
        >
          <ListFilter v-if="route.query.q" aria-hidden="true" /><AppWindow
            v-else
            aria-hidden="true"
          />
          <strong>{{
            route.query.q ? "没有符合条件的应用" : "尚未创建三方应用"
          }}</strong>
          <span>{{
            route.query.q
              ? "清除筛选后查看全部应用。"
              : "创建应用后配置权限和设备范围，再生成 AK / SK。"
          }}</span>
          <button
            v-if="route.query.q"
            class="btn"
            @click="
              query = '';
              searchApps();
            "
          >
            清除筛选
          </button>
          <button
            v-else
            class="btn btn-primary"
            :disabled="blocked || !scopes.length"
            @click="edit()"
          >
            <Plus aria-hidden="true" />创建应用
          </button>
        </div>
      </div>
      <div class="pagination">
        <span>共 {{ total }} 条匹配记录</span>
        <div class="pagination-actions" aria-label="应用列表分页">
          <button
            class="page-btn"
            :disabled="blocked || page <= 1"
            aria-label="上一页应用"
            @click="searchApps(page - 1)"
          >
            <ChevronLeft />
          </button>
          <span>第 {{ page }} / {{ pageCount }} 页</span>
          <button
            class="page-btn"
            :disabled="blocked || page >= pageCount"
            aria-label="下一页应用"
            @click="searchApps(page + 1)"
          >
            <ChevronRight />
          </button>
        </div>
      </div>
    </section>

    <template v-else>
      <div v-if="loading && !selected" class="detail-skeleton" role="status">
        <span /><span /><span />
        <p><LoaderCircle class="animate-spin" />正在加载应用详情…</p>
      </div>
      <div v-else-if="!selected" class="card empty-state empty-action">
        <ShieldAlert aria-hidden="true" /><strong>暂时无法打开应用详情</strong
        ><span>请重新加载，或返回列表选择应用。</span
        ><RouterLink class="page-back-btn" :to="listLink" aria-label="返回应用列表" title="返回应用列表"
          ><ArrowLeft aria-hidden="true" /></RouterLink
        >
      </div>
      <template v-if="selected">
        <header class="device-command-header">
          <div class="device-command-identity">
            <RouterLink
              class="page-back-btn"
              :to="listLink"
              aria-label="返回应用列表"
              title="返回应用列表"
              ><ArrowLeft aria-hidden="true"
            /></RouterLink>
            <span class="device-command-icon"
              ><AppWindow aria-hidden="true"
            /></span>
            <div class="open-identity-text">
              <div class="device-command-title">
                <h1>{{ selected.name }}</h1>
                <span
                  class="status"
                  :class="statusTone(effectiveStatus(selected))"
                  >{{ statusName(effectiveStatus(selected)) }}</span
                >
              </div>
              <div class="device-command-meta">
                <span class="mono">{{ selected.id }}</span
                ><span>{{ selected.organization || "未填写接入单位" }}</span>
              </div>
            </div>
          </div>
          <div class="head-actions">
            <button class="btn" :disabled="blocked" @click="refreshRoute">
              <RefreshCcw
                :class="{ 'animate-spin': loading }"
                aria-hidden="true"
              />刷新
            </button>
            <button
              class="btn btn-primary"
              :disabled="blocked || selected.status === 'archived'"
              @click="edit(selected)"
            >
              <Settings2 aria-hidden="true" />编辑 / 授权
            </button>
          </div>
        </header>

        <section class="device-workbench open-workbench">
          <aside class="device-workbench-nav">
            <nav class="card detail-task-nav" aria-label="应用详情工作区">
              <button
                v-for="item in tabs"
                :key="item.id"
                :class="{ active: tab === item.id }"
                :aria-current="tab === item.id ? 'page' : undefined"
                :disabled="blocked || rowsLoading"
                @click="changeTab(item.id)"
              >
                <component :is="item.icon" aria-hidden="true" /><span
                  ><strong>{{ item.name }}</strong
                  ><small>{{ item.note }}</small></span
                ><ChevronRight aria-hidden="true" />
              </button>
            </nav>
            <section class="card detail-side-facts" aria-label="应用接入摘要">
              <div>
                <span>设备范围</span
                ><strong>{{
                  selected.device_access_mode === "all"
                    ? "全部设备（含新增）"
                    : `指定设备 ${selected.device_ids.length} 台`
                }}</strong>
              </div>
              <div>
                <span>接口权限</span
                ><strong>{{ selected.scopes.length }} 项已授权</strong>
              </div>
              <div>
                <span>有效期</span
                ><strong>{{
                  selected.expires_at
                    ? formatDate(selected.expires_at)
                    : "长期有效"
                }}</strong>
              </div>
            </section>
          </aside>
          <div
            class="device-workspace-content"
            :aria-busy="loading || rowsLoading"
          >
            <section class="detail-section-head">
              <div>
                <h2>{{ currentTab.name }}</h2>
                <p>{{ currentTab.note }}</p>
              </div>
              <div class="head-actions">
                <button
                  v-if="tab === 'credentials'"
                  class="btn btn-primary"
                  :disabled="
                    blocked || rowsLoading || selected.status === 'archived'
                  "
                  @click="
                    error = '';
                    secretExpiry = '';
                    credentialOpen = true;
                  "
                >
                  <Plus aria-hidden="true" />创建 AK / SK
                </button>
                <button
                  v-if="tab === 'callback-targets'"
                  class="btn btn-primary"
                  :disabled="
                    blocked || rowsLoading || selected.status === 'archived'
                  "
                  @click="editTarget()"
                >
                  <Plus aria-hidden="true" />添加回调目标
                </button>
                <RouterLink
                  v-if="tab === 'overview'"
                  class="btn"
                  :to="{ name: 'open-platform-docs' }"
                  ><BookOpen aria-hidden="true" />接入文档</RouterLink
                >
              </div>
            </section>

            <template v-if="tab === 'overview'">
              <section class="card open-section">
                <div class="card-head">
                  <div>
                    <h3 class="card-title">基本信息</h3>
                    <p class="card-sub">应用身份与业务负责人</p>
                  </div>
                  <AppWindow aria-hidden="true" />
                </div>
                <dl class="detail-definition-list">
                  <div>
                    <dt>应用名称</dt>
                    <dd>{{ selected.name }}</dd>
                  </div>
                  <div>
                    <dt>接入单位</dt>
                    <dd>{{ selected.organization || "—" }}</dd>
                  </div>
                  <div>
                    <dt>负责人</dt>
                    <dd>{{ selected.owner || "—" }}</dd>
                  </div>
                  <div>
                    <dt>联系方式</dt>
                    <dd>{{ selected.contact || "—" }}</dd>
                  </div>
                  <div class="open-wide">
                    <dt>用途</dt>
                    <dd class="open-purpose">
                      {{ selected.purpose || "未填写用途" }}
                    </dd>
                  </div>
                  <div>
                    <dt>到期时间</dt>
                    <dd>
                      {{
                        selected.expires_at
                          ? formatDate(selected.expires_at)
                          : "长期有效"
                      }}
                    </dd>
                  </div>
                  <div>
                    <dt>配置版本</dt>
                    <dd>{{ selected.revision }}</dd>
                  </div>
                </dl>
              </section>
              <section class="card open-section">
                <div class="card-head">
                  <div>
                    <h3 class="card-title">来源与配额</h3>
                    <p class="card-sub">应用调用的来源限制与资源额度</p>
                  </div>
                  <ShieldCheck aria-hidden="true" />
                </div>
                <dl class="detail-definition-list">
                  <div class="open-wide">
                    <dt>允许的来源 IP</dt>
                    <dd>
                      {{
                        selected.allow_any_ip
                          ? "任意来源 IP"
                          : selected.cidrs.join("、") || "未配置，拒绝所有来源"
                      }}
                    </dd>
                  </div>
                  <div v-for="quota in quotas" :key="quota.key">
                    <dt>{{ quota.name }}</dt>
                    <dd>{{ selected[quota.key] }}</dd>
                  </div>
                </dl>
              </section>
              <section class="card open-section open-state-section">
                <div>
                  <h3 class="card-title">应用状态</h3>
                  <p class="card-sub">
                    {{
                      selected.status === "archived"
                        ? "已归档应用保留历史记录。"
                        : "停用后无法发起新的业务调用。"
                    }}
                  </p>
                </div>
                <div class="open-row-actions">
                  <button
                    class="btn"
                    :disabled="blocked || selected.status === 'archived'"
                    @click="
                      setStatus(
                        selected,
                        selected.status === 'enabled' ? 'disable' : 'enable'
                      )
                    "
                  >
                    {{
                      selected.status === "enabled" ? "停用应用" : "启用应用"
                    }}
                  </button>
                  <button
                    v-if="selected.status === 'disabled'"
                    class="btn btn-ghost"
                    :disabled="blocked"
                    @click="setStatus(selected, 'archive')"
                  >
                    归档应用
                  </button>
                </div>
              </section>
            </template>

            <template v-else-if="tab === 'authorization'">
              <section class="card open-section">
                <div class="card-head">
                  <div>
                    <h3 class="card-title">接口权限</h3>
                    <p class="card-sub">
                      该应用的所有有效 AK / SK 继承以下权限。
                    </p>
                  </div>
                  <ShieldCheck aria-hidden="true" />
                </div>
                <ul v-if="selected.scopes.length" class="open-scope-list">
                  <li v-for="scope in selected.scopes" :key="scope">
                    <span>{{ scopeNames[scope] || scope }}</span
                    ><code>{{ scope }}</code>
                  </li>
                </ul>
                <p v-else class="section-note">
                  尚未授予接口权限，请通过“编辑 / 授权”配置。
                </p>
                <template v-if="selected.scopes.includes('ptz:control')"
                  ><h4 class="open-subheading">云台动作白名单</h4>
                  <p class="open-action-list mono">
                    {{
                      selected.allowed_actions.join(" · ") || "未授权任何动作"
                    }}
                  </p></template
                >
              </section>
              <section class="card table-card">
                <div class="card-head">
                  <div>
                    <h3 class="card-title">设备访问范围</h3>
                    <p class="card-sub">
                      {{
                        selected.device_access_mode === "all"
                          ? "全部设备：当前及今后新增设备均可访问，仍受接口权限限制。"
                          : "指定设备：通道继承设备权限，新增设备需重新授权。"
                      }}
                    </p>
                  </div>
                </div>
                <div class="table-wrap">
                  <table class="data-table open-data-table">
                    <thead>
                      <tr>
                        <th scope="col">设备</th>
                        <th scope="col">设备编号</th>
                        <th scope="col">协议</th>
                        <th scope="col">状态</th>
                      </tr>
                    </thead>
                    <tbody>
                      <tr v-for="device in authorized" :key="device.id">
                        <td data-label="设备">
                          <strong>{{ device.name || device.id }}</strong
                          ><small class="open-cell-note mono">{{
                            device.id
                          }}</small>
                        </td>
                        <td data-label="设备编号" class="mono">
                          {{ device.device_id || "—" }}
                        </td>
                        <td data-label="协议">
                          <span class="protocol-tag">{{
                            device.type || "—"
                          }}</span>
                        </td>
                        <td data-label="状态">
                          <span
                            class="status"
                            :class="
                              device.deleted
                                ? 'warning'
                                : device.is_online
                                ? 'online'
                                : ''
                            "
                            >{{
                              device.deleted
                                ? "已删除，授权失效"
                                : device.is_online
                                ? "在线"
                                : "离线"
                            }}</span
                          >
                        </td>
                      </tr>
                    </tbody>
                  </table>
                </div>
                <div v-if="rowsLoading" class="empty-state" role="status">
                  <LoaderCircle
                    class="mx-auto mb-3 h-6 w-6 animate-spin"
                  />正在加载授权设备…
                </div>
                <div
                  v-else-if="!authorized.length && !error"
                  class="empty-state"
                >
                  暂无授权设备
                </div>
              </section>
            </template>

            <template v-else>
              <p v-if="tab === 'credentials'" class="open-context-note">
                <ShieldCheck aria-hidden="true" /><span
                  >凭据继承应用的接口权限和设备范围。每个应用最多两组有效凭据，SK
                  仅创建时展示一次。</span
                >
              </p>
              <section class="card table-card">
                <form
                  v-if="tab === 'audit-logs'"
                  class="toolbar open-audit-filters"
                  @submit.prevent="
                    rowPage = 1;
                    run(loadRows);
                  "
                >
                  <label class="form-group"
                    ><span class="form-label">操作</span
                    ><input
                      v-model="auditAction"
                      class="input plain"
                      placeholder="如 credential.create / GET"
                  /></label>
                  <label class="form-group"
                    ><span class="form-label">HTTP 状态</span
                    ><input
                      v-model="auditStatus"
                      class="input plain"
                      placeholder="如 403"
                  /></label>
                  <label class="form-group"
                    ><span class="form-label">开始时间（UTC）</span
                    ><input
                      v-model="auditStart"
                      type="datetime-local"
                      class="input plain"
                  /></label>
                  <label class="form-group"
                    ><span class="form-label">结束时间（UTC）</span
                    ><input
                      v-model="auditEnd"
                      type="datetime-local"
                      class="input plain"
                  /></label>
                  <button class="btn btn-sm" :disabled="blocked || rowsLoading">
                    <Search aria-hidden="true" />筛选
                  </button>
                </form>
                <div class="table-wrap">
                  <table class="data-table open-data-table open-record-table">
                    <thead>
                      <tr>
                        <th
                          v-for="heading in recordHeadings"
                          :key="heading"
                          scope="col"
                        >
                          {{ heading }}
                        </th>
                        <th scope="col">操作</th>
                      </tr>
                    </thead>
                    <tbody>
                      <tr v-for="row in rows" :key="row.id">
                        <td
                          :data-label="recordHeadings[0]"
                          class="mono open-record-id"
                        >
                          {{ row.access_key || row.request_id || row.id }}
                        </td>
                        <td :data-label="recordHeadings[1]">
                          <span
                            class="status"
                            :class="statusTone(effectiveStatus(row))"
                            >{{ statusName(effectiveStatus(row)) }}</span
                          >
                        </td>
                        <td
                          :data-label="recordHeadings[2]"
                          class="open-record-summary"
                        >
                          {{ recordSummary(row) }}
                        </td>
                        <td data-label="操作">
                          <div class="open-row-actions">
                            <button
                              class="btn btn-sm btn-ghost"
                              :disabled="blocked"
                              @click="details = row"
                            >
                              详情<ChevronRight aria-hidden="true" />
                            </button>
                            <button
                              v-if="
                                tab === 'credentials' && row.status === 'active'
                              "
                              class="btn btn-sm"
                              :disabled="blocked || rowsLoading"
                              @click="revoke(row)"
                            >
                              撤销
                            </button>
                            <button
                              v-if="tab === 'callback-targets'"
                              class="btn btn-sm"
                              :disabled="
                                blocked ||
                                rowsLoading ||
                                selected.status === 'archived'
                              "
                              @click="editTarget(row)"
                            >
                              编辑 / 审核
                            </button>
                            <button
                              v-if="
                                tab === 'deliveries' &&
                                ['failed', 'dead'].includes(String(row.state))
                              "
                              class="btn btn-sm"
                              :disabled="blocked || rowsLoading"
                              @click="retry(row)"
                            >
                              受控重投
                            </button>
                            <button
                              v-if="
                                tab === 'ptz-commands' &&
                                [
                                  'pending',
                                  'confirmed',
                                  'unknown',
                                  'stop_failed',
                                ].includes(String(row.state))
                              "
                              class="btn btn-sm"
                              :disabled="blocked || rowsLoading"
                              @click="stop(row)"
                            >
                              安全停止
                            </button>
                          </div>
                        </td>
                      </tr>
                    </tbody>
                  </table>
                </div>
                <div v-if="rowsLoading" class="empty-state" role="status">
                  <LoaderCircle
                    class="mx-auto mb-3 h-6 w-6 animate-spin"
                  />正在加载{{ currentTab.name }}…
                </div>
                <div
                  v-else-if="!rows.length && !error"
                  class="empty-state empty-action"
                >
                  <component :is="currentTab.icon" aria-hidden="true" /><strong
                    >暂无{{ currentTab.name }}记录</strong
                  ><span>{{
                    tab === "credentials"
                      ? "创建凭据后，三方系统可使用 AK / SK 签名调用已授权接口。"
                      : tab === "callback-targets"
                      ? "添加回调地址并审核后，应用才能建立告警订阅。"
                      : "该应用的相关记录将显示在这里。"
                  }}</span>
                </div>
                <div class="pagination">
                  <span>共 {{ rowTotal }} 条记录</span>
                  <div
                    class="pagination-actions"
                    :aria-label="`${currentTab.name}分页`"
                  >
                    <button
                      class="page-btn"
                      :disabled="blocked || rowsLoading || rowPage <= 1"
                      aria-label="上一页记录"
                      @click="
                        rowPage--;
                        run(loadRows);
                      "
                    >
                      <ChevronLeft /></button
                    ><span>第 {{ rowPage }} / {{ rowPageCount }} 页</span
                    ><button
                      class="page-btn"
                      :disabled="
                        blocked || rowsLoading || rowPage >= rowPageCount
                      "
                      aria-label="下一页记录"
                      @click="
                        rowPage++;
                        run(loadRows);
                      "
                    >
                      <ChevronRight />
                    </button>
                  </div>
                </div>
              </section>
            </template>
          </div>
        </section>
      </template>
    </template>

    <ModalDialog
      :open="editOpen"
      :title="editing ? '编辑应用与授权' : '创建三方应用'"
      :description="
        editing
          ? '保存权限变更会撤销旧媒体会话、暂停订阅，并停止活动云台命令。'
          : '创建后配置权限与设备范围，再生成凭据并启用应用。'
      "
      @close="editOpen = false"
    >
      <form class="form-grid open-platform-form" @submit.prevent="save">
        <label class="form-group">
          <span class="form-label">应用名称 *</span>
          <input
            v-model="form.name"
            required
            maxlength="120"
            class="input plain w-full"
          />
        </label>
        <label class="form-group">
          <span class="form-label">接入单位</span>
          <input v-model="form.organization" class="input plain w-full" />
        </label>
        <label class="form-group">
          <span class="form-label">负责人</span>
          <input v-model="form.owner" class="input plain w-full" />
        </label>
        <label class="form-group">
          <span class="form-label">联系方式</span>
          <input v-model="form.contact" class="input plain w-full" />
        </label>
        <label class="form-group full">
          <span class="form-label">用途</span>
          <textarea v-model="form.purpose" maxlength="2000" class="textarea" />
        </label>
        <label class="form-group full">
          <span class="form-label">到期时间（UTC，留空为长期有效）</span>
          <input
            v-model="expires"
            type="datetime-local"
            class="input plain w-full"
          />
        </label>
        <fieldset class="full open-form-section">
          <legend>接口权限</legend>
          <p class="form-help open-form-help">
            应用的所有有效 AK / SK 自动继承已选权限，无需单独授权。
          </p>
          <div class="open-checks">
            <label v-for="scope in scopes" :key="scope">
              <input v-model="form.scopes" type="checkbox" :value="scope" />{{
                scopeNames[scope]
              }}
              <small>{{ scope }}</small>
            </label>
          </div>
        </fieldset>
        <fieldset class="full open-form-section">
          <legend>设备访问范围 *</legend>
          <div class="open-checks">
            <label>
              <input
                v-model="form.device_access_mode"
                type="radio"
                value="all"
                @change="toggleMode"
              />全部设备（含今后新增设备）</label
            >
            <label>
              <input
                v-model="form.device_access_mode"
                type="radio"
                value="selected_devices"
                @change="toggleMode"
              />指定设备</label
            >
          </div>
          <div v-if="form.device_access_mode === 'selected_devices'">
            <div class="toolbar my-4">
              <input
                v-model="deviceQuery"
                class="input plain"
                placeholder="设备名称 / 内部 ID / 国标编码"
                aria-label="搜索可授权设备"
              />
              <button
                type="button"
                class="btn btn-sm"
                :disabled="busy"
                @click="
                  devicePage = 1;
                  run(loadDevices);
                "
              >
                搜索
              </button>
              <button
                type="button"
                class="btn btn-sm"
                @click="devices.forEach((v) => toggleDevice(v.id, true))"
              >
                勾选当前页
              </button>
              <span>已选 {{ chosen.length }} 台</span>
            </div>
            <div class="open-device-picker">
              <label v-for="device in devices" :key="device.id">
                <input
                  type="checkbox"
                  :checked="selectedIDs.has(device.id)"
                  @change="
                    toggleDevice(
                      device.id,
                      ($event.target as HTMLInputElement).checked
                    )
                  "
                />
                <span
                  >{{ device.name || device.id
                  }}<small
                    >{{ device.id }} · {{ device.device_id }} ·
                    {{ device.type }} ·
                    {{ device.is_online ? "在线" : "离线" }}</small
                  >
                </span>
              </label>
              <p v-if="!devices.length" class="section-note">无匹配设备</p>
            </div>
            <div class="open-picker-pagination">
              <button
                type="button"
                class="btn btn-sm"
                :disabled="busy || devicePage <= 1"
                @click="
                  devicePage--;
                  run(loadDevices);
                "
              >
                上一页
              </button>
              <span>{{ devicePage }}</span>
              <button
                type="button"
                class="btn btn-sm"
                :disabled="busy || devicePage * 20 >= deviceTotal"
                @click="
                  devicePage++;
                  run(loadDevices);
                "
              >
                下一页
              </button>
            </div>
            <details v-if="chosen.length">
              <summary>查看 / 移除已选设备</summary>
              <div class="open-chosen">
                <button
                  v-for="id in chosen"
                  :key="id"
                  type="button"
                  class="btn btn-sm mono"
                  @click="toggleDevice(id, false)"
                >
                  {{ id }}<X aria-hidden="true" />
                </button>
              </div>
            </details>
          </div>
        </fieldset>
        <fieldset
          v-if="form.scopes.includes('ptz:control')"
          class="full open-form-section"
        >
          <legend>云台动作白名单</legend>
          <div class="open-checks">
            <label v-for="action in actions" :key="action">
              <input
                v-model="form.allowed_actions"
                type="checkbox"
                :value="action"
              />{{ action }}</label
            >
          </div>
          <p class="section-note">
            设备不支持的动作会被拒绝；连续动作最长 3 秒，停止失败时锁定通道。
          </p>
        </fieldset>
        <fieldset class="full open-form-section">
          <legend>来源策略</legend>
          <label class="open-check-label">
            <input
              v-model="form.allow_any_ip"
              type="checkbox"
            />明确允许任意来源 IP</label
          >
          <label v-if="!form.allow_any_ip" class="form-group mt-3">
            <span class="form-label"
              >允许的 IPv4 / IPv6 CIDR（每行一条，空白拒绝所有来源）</span
            >
            <textarea
              v-model="cidrsText"
              class="textarea"
              rows="3"
              placeholder="203.0.113.10/32"
            />
          </label>
        </fieldset>
        <fieldset class="full open-form-section">
          <legend>调用配额</legend>
          <div class="form-grid">
            <label v-for="quota in quotas" :key="quota.key" class="form-group">
              <span class="form-label">{{ quota.name }}</span>
              <input
                v-model.number="form[quota.key]"
                type="number"
                min="1"
                :max="quota.max"
                required
                class="input plain w-full"
              />
            </label>
          </div>
        </fieldset>
        <p v-if="error" class="warning-box full" role="alert">{{ error }}</p>
        <div class="modal-foot full open-form-footer">
          <button type="button" class="btn" @click="editOpen = false">
            取消
          </button>
          <button class="btn btn-primary" :disabled="busy">
            {{ busy ? "正在保存…" : "保存" }}
          </button>
        </div>
      </form>
    </ModalDialog>
    <ModalDialog
      :open="credentialOpen"
      title="创建 AK / SK"
      description="每个应用最多两组有效凭据。轮换时创建新组，接入方切换后撤销旧组。"
      @close="credentialOpen = false"
    >
      <form class="open-platform-form" @submit.prevent="createCredential">
        <label class="form-group">
          <span class="form-label">到期时间（UTC，可留空）</span>
          <input
            v-model="secretExpiry"
            type="datetime-local"
            class="input plain w-full"
          />
        </label>
        <p v-if="error" class="warning-box mt-3" role="alert">{{ error }}</p>
        <div class="modal-foot">
          <button class="btn btn-primary" :disabled="busy">
            创建并展示一次
          </button>
        </div>
      </form>
    </ModalDialog>
    <ModalDialog
      :open="Boolean(secret)"
      title="保存应用凭据"
      description="SK 仅此一次展示。关闭后无法取回；请只保存在接入系统服务端。"
      @close="secret = null"
    >
      <div v-if="secret" class="open-secret-box open-platform-form">
        <label class="form-group">
          <span class="form-label">AK / Key ID</span>
          <input
            :value="secret.access_key || secret.key_id"
            readonly
            class="input plain w-full mono"
            autocomplete="off"
          />
        </label>
        <label class="form-group">
          <span class="form-label">SK</span>
          <input
            :value="secret.secret_key"
            readonly
            class="input plain w-full mono"
            autocomplete="off"
          />
        </label>
        <p class="warning-box">
          请妥善保存。若响应丢失，请撤销未知凭据后重新创建。
        </p>
        <button class="btn btn-primary" @click="secret = null">
          已保存，关闭并清除
        </button>
      </div>
    </ModalDialog>
    <ModalDialog
      :open="targetOpen"
      title="回调目标审核"
      :description="`${
        allowHTTP ? '允许 HTTP / HTTPS' : '仅允许 HTTPS'
      }。内网目标须明确批准 CIDR；修改地址或 CIDR 后自动回到待审核。`"
      @close="targetOpen = false"
    >
      <form class="form-grid open-platform-form" @submit.prevent="saveTarget">
        <label class="form-group full">
          <span class="form-label">{{
            allowHTTP ? "HTTP / HTTPS 回调 URL" : "HTTPS 回调 URL"
          }}</span>
          <input
            v-model="target.url"
            type="url"
            required
            class="input plain w-full"
          />
        </label>
        <label class="form-group full">
          <span class="form-label">批准的目标 CIDR（空白仅允许公网地址）</span>
          <textarea v-model="target.cidrs" class="textarea" />
        </label>
        <label class="form-group full">
          <span class="form-label">审核状态</span>
          <select v-model="target.status" class="select w-full">
            <option value="pending">待审核</option>
            <option value="approved">批准</option>
            <option value="disabled">禁用</option>
          </select>
        </label>
        <p v-if="error" class="warning-box full" role="alert">{{ error }}</p>
        <div class="modal-foot full open-form-footer">
          <button class="btn btn-primary" :disabled="busy">保存审核结果</button>
        </div>
      </form>
    </ModalDialog>
    <ModalDialog
      :open="Boolean(details)"
      :title="`${currentTab.name}详情`"
      :description="String(details?.id || '')"
      @close="details = null"
    >
      <dl v-if="details" class="open-record-detail">
        <div v-for="(value, key) in details" :key="key">
          <dt>
            {{ recordLabels[key] || key
            }}<small v-if="recordLabels[key]">{{ key }}</small>
          </dt>
          <dd>
            <pre v-if="value && typeof value === 'object'">{{
              present(value, key)
            }}</pre>
            <span v-else>{{ present(value, key) }}</span>
          </dd>
        </div>
      </dl>
      <template #footer>
        <button class="btn" @click="details = null">关闭</button>
      </template>
    </ModalDialog>
  </main>
</template>

<style scoped>
.open-platform-page {
  min-width: 0;
}
.open-search .input {
  width: 300px;
  max-width: 100%;
}
.open-row-actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.open-app-table th:first-child {
  width: 26%;
}
.open-app-link {
  min-width: 0;
}
.open-app-link strong,
.open-identity-text h1 {
  overflow-wrap: anywhere;
}
.open-app-link:hover strong {
  color: var(--blue);
}
.open-app-icon {
  display: grid;
  place-items: center;
  flex: 0 0 auto;
  width: 36px;
  height: 36px;
  color: var(--blue);
  background: var(--blue-soft);
  border-radius: 8px;
}
.open-app-icon svg {
  width: 18px;
  height: 18px;
}
.open-cell-note {
  display: block;
  margin-top: 4px;
  color: var(--muted);
  font-size: 12px;
  overflow-wrap: anywhere;
}
.open-workbench {
  margin-top: 16px;
}
.open-identity-text {
  min-width: 0;
}
.open-identity-text .mono {
  overflow-wrap: anywhere;
}
.open-section {
  margin-bottom: 14px;
  padding: 14px;
}
.open-wide {
  grid-column: 1 / -1;
}
.open-purpose {
  white-space: pre-wrap;
  font-weight: 400 !important;
}
.open-state-section {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}
.open-scope-list {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 0 20px;
  margin: 0;
  padding: 0;
  list-style: none;
}
.open-scope-list li {
  min-width: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 11px 0;
  border-bottom: 1px solid var(--line);
  font-size: 13px;
}
.open-scope-list code {
  color: var(--muted);
  font-size: 12px;
  overflow-wrap: anywhere;
}
.open-subheading {
  margin: 20px 0 8px;
  font-size: 13px;
}
.open-action-list {
  margin: 0;
  color: var(--muted);
  font-size: 12px;
  line-height: 1.8;
  overflow-wrap: anywhere;
}
.open-context-note {
  display: flex;
  align-items: flex-start;
  gap: 9px;
  margin: 0 0 14px;
  padding: 12px 14px;
  color: var(--muted);
  background: var(--panel-2);
  border: 1px solid var(--line);
  border-radius: 8px;
  font-size: 12px;
  line-height: 1.65;
}
.open-context-note svg {
  flex: 0 0 auto;
  width: 16px;
  height: 16px;
  margin-top: 2px;
}
.open-record-table {
  table-layout: fixed;
}
.open-record-table th:first-child {
  width: 31%;
}
.open-record-table th:nth-child(2) {
  width: 17%;
}
.open-record-id,
.open-record-summary {
  overflow-wrap: anywhere;
}
.open-platform-page .detail-definition-list dt {
  font-size: 12px;
}
.open-platform-page .detail-definition-list dd {
  font-size: 13px;
}
.open-audit-filters {
  align-items: flex-end;
}
.open-audit-filters .form-group {
  flex: 1 1 180px;
  min-width: 0;
}
.open-audit-filters .input {
  width: 100%;
  min-width: 0;
}
.open-platform-form {
  min-width: 0;
}
.open-platform-form > .full {
  grid-column: 1 / -1;
}
.open-platform-form .form-group {
  min-width: 0;
}
.open-platform-form .input,
.open-platform-form .select {
  width: 100%;
  min-width: 0;
}
.open-form-section {
  min-width: 0;
  margin: 4px 0 0;
  padding: 16px 0 0;
  border: 0;
  border-top: 1px solid var(--line);
}
.open-form-section legend {
  padding: 0 12px 0 0;
  color: var(--ink-strong);
  font-size: 14px;
  font-weight: 700;
}
.open-form-help {
  margin: 0 0 12px;
  font-size: 12px;
}
.open-checks {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 8px 16px;
}
.open-checks label,
.open-check-label {
  min-width: 0;
  min-height: 38px;
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 6px 0;
  font-size: 13px;
  cursor: pointer;
  flex-wrap: wrap;
}
.open-checks input,
.open-check-label input,
.open-device-picker input {
  flex: 0 0 auto;
  width: 16px;
  height: 16px;
  margin-top: 2px;
  accent-color: var(--blue);
}
.open-checks small {
  width: 100%;
  margin-left: 24px;
  margin-top: -4px;
  color: var(--muted);
  font: 12px/1.5 SFMono-Regular, Consolas, monospace;
  overflow-wrap: anywhere;
}
.open-form-section .toolbar .input {
  width: 100%;
}
.open-device-picker {
  max-height: 250px;
  overflow: auto;
  border: 1px solid var(--line);
  border-radius: 8px;
}
.open-device-picker > label {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  min-height: 52px;
  padding: 10px 12px;
  border-bottom: 1px solid var(--line);
  font-size: 13px;
  cursor: pointer;
}
.open-device-picker > label:last-child {
  border-bottom: 0;
}
.open-device-picker > label:hover {
  background: var(--panel-2);
}
.open-device-picker span {
  min-width: 0;
  overflow-wrap: anywhere;
}
.open-device-picker small {
  display: block;
  margin-top: 4px;
  color: var(--muted);
  font-size: 12px;
}
.open-device-picker > p {
  padding: 12px;
}
.open-picker-pagination {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 10px;
  margin: 12px 0;
  color: var(--muted);
  font-size: 12px;
}
.open-chosen {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 10px;
}
.open-chosen button {
  max-width: 100%;
  height: auto;
  padding: 6px 10px;
  overflow-wrap: anywhere;
  text-align: left;
}
.open-chosen button svg {
  flex: 0 0 auto;
}
.open-platform-form details summary {
  min-height: 36px;
  color: var(--blue);
  font-size: 12px;
  cursor: pointer;
}
.open-form-footer {
  margin-top: 6px;
  background: var(--panel);
}
.open-secret-box {
  display: grid;
  gap: 16px;
}
.open-secret-box .warning-box {
  margin: 0;
}
.open-record-detail {
  margin: 0;
}
.open-record-detail > div {
  display: grid;
  grid-template-columns: minmax(110px, 1fr) minmax(0, 2.4fr);
  gap: 16px;
  padding: 12px 0;
  border-bottom: 1px solid var(--line);
}
.open-record-detail > div:last-child {
  border-bottom: 0;
}
.open-record-detail dt {
  color: var(--muted);
  font-size: 13px;
}
.open-record-detail dt small {
  display: block;
  margin-top: 3px;
  color: var(--muted);
  font: 12px/1.5 SFMono-Regular, Consolas, monospace;
  overflow-wrap: anywhere;
}
.open-record-detail dd {
  min-width: 0;
  margin: 0;
  overflow-wrap: anywhere;
  font-size: 13px;
  white-space: pre-wrap;
}
.open-record-detail pre {
  margin: 0;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  font-size: 12px;
}
@media (max-width: 1200px) {
  .open-workbench {
    grid-template-columns: 200px minmax(0, 1fr);
  }
  .open-scope-list {
    grid-template-columns: 1fr;
  }
  .open-record-table {
    table-layout: auto;
  }
}
@media (max-width: 900px) {
  .open-workbench {
    grid-template-columns: 1fr;
  }
  .open-workbench .device-workbench-nav {
    position: static;
  }
  .open-workbench .detail-task-nav {
    grid-template-columns: repeat(3, minmax(0, 1fr));
  }
  .open-workbench .detail-task-nav button {
    min-height: 58px;
  }
  .open-data-table {
    min-width: 0;
    table-layout: auto;
  }
  .open-data-table thead {
    display: none;
  }
  .open-data-table tbody {
    display: grid;
  }
  .open-data-table tr {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    padding: 12px 16px;
    gap: 12px;
    border-bottom: 1px solid var(--line);
  }
  .open-data-table tr:last-child {
    border-bottom: 0;
  }
  .open-data-table td {
    min-width: 0;
    display: block;
    padding: 0;
    border: 0;
    overflow-wrap: anywhere;
  }
  .open-data-table td::before {
    display: block;
    content: attr(data-label);
    margin-bottom: 6px;
    color: var(--muted);
    font: 12px/1.5 "Noto Sans SC", sans-serif;
  }
  .open-data-table td:first-child,
  .open-data-table td:last-child {
    grid-column: 1 / -1;
  }
  .open-app-link strong {
    font-size: 14px;
  }
}
@media (max-width: 640px) {
  .open-platform-page .head-actions {
    flex-wrap: wrap;
  }
  .open-workbench .detail-task-nav {
    grid-template-columns: repeat(3, minmax(0, 1fr));
  }
  .open-workbench .detail-task-nav strong {
    font-size: 12px;
  }
  .open-search {
    width: 100%;
  }
  .open-search .input {
    width: 100%;
  }
  .open-section {
    padding: 16px;
  }
  .open-checks {
    grid-template-columns: 1fr;
    gap: 4px;
  }
  .open-checks label,
  .open-check-label {
    min-height: 44px;
  }
  .open-picker-pagination {
    justify-content: space-between;
  }
  .open-record-detail > div {
    grid-template-columns: 1fr;
    gap: 6px;
  }
  .open-row-actions .btn {
    min-height: 44px;
  }
  .open-form-footer {
    position: static;
  }
}
</style>
