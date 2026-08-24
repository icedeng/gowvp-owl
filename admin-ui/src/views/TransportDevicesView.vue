<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { ChevronLeft, ChevronRight, RadioTower, RefreshCcw, Search, ShieldAlert, Truck } from "@lucide/vue";
import { api, collectPages, errorMessage } from "../services/api";
import type { ApiDevice } from "../types/api";

const loading = ref(false);
const loadError = ref("");
const query = ref("");
const rows = ref<ApiDevice[]>([]);
const currentPage = ref(1);
const PAGE_SIZE = 30;
const TRANSPORT_TYPES = ["JT1078", "JT808", "TRANSPORT", "JT/T 1078", "JT/T 808"];

function isTransportDevice(device: ApiDevice) {
  return /JT\/?T?\s*(808|1078)|JT808|JT1078|TRANSPORT/i.test(String(device.type || ""));
}

const transportRows = computed(() => rows.value);
const filtered = computed(() =>
  transportRows.value.filter((device) =>
    `${device.name || ""}${device.device_id || ""}${device.id}${device.address || ""}`
      .toLowerCase()
      .includes(query.value.toLowerCase())
  )
);
const pageCount = computed(() => Math.max(1, Math.ceil(filtered.value.length / PAGE_SIZE)));
const pagedRows = computed(() =>
  filtered.value.slice((currentPage.value - 1) * PAGE_SIZE, currentPage.value * PAGE_SIZE)
);

async function loadTransportDevices() {
  const probe = await api.devices({ type: TRANSPORT_TYPES[0], page: 1, size: 1000 });
  const probeItems = probe.data?.items || [];
  if (probeItems.some((item) => !isTransportDevice(item))) {
    const legacy = await collectPages(api.devices);
    return legacy.items.filter(isTransportDevice);
  }
  const results = await Promise.all(
    TRANSPORT_TYPES.map((type) => collectPages(api.devices, { type }))
  );
  return [
    ...new Map(
      results.flatMap((result) => result.items)
        .filter(isTransportDevice)
        .map((item) => [item.id, item])
    ).values(),
  ];
}

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    rows.value = await loadTransportDevices();
    currentPage.value = 1;
  } catch (cause) {
    loadError.value = errorMessage(cause, "部标设备列表加载失败");
  } finally {
    loading.value = false;
  }
}

onMounted(load);
watch(query, () => { currentPage.value = 1; });
</script>

<template>
  <main class="page-content device-page">
    <header class="page-head">
      <div>
        <h1 class="page-title">部标设备</h1>
        <p class="page-desc">集中管理 JT/T 808、JT/T 1078 终端及车辆视频资源。</p>
      </div>
      <button class="btn" :disabled="loading" @click="load">
        <RefreshCcw :class="{ 'animate-spin': loading }" />刷新
      </button>
    </header>

    <div v-if="loadError" class="warning-box mb-4" role="alert">
      <ShieldAlert /><span>{{ loadError }}</span><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>

    <section class="device-summary transport-summary" aria-label="部标设备统计">
      <div><span>设备</span><strong>{{ transportRows.length }}</strong></div>
      <div><span>在线</span><strong>{{ transportRows.filter((item) => item.is_online).length }}</strong></div>
      <div><span>离线</span><strong>{{ transportRows.filter((item) => !item.is_online).length }}</strong></div>
      <div><span>通道</span><strong>{{ transportRows.reduce((sum, item) => sum + Number(item.channels || 0), 0) }}</strong></div>
    </section>

    <section class="card table-card">
      <div class="toolbar">
        <label class="field"><Search /><input v-model="query" class="input" aria-label="搜索部标设备" placeholder="搜索名称、终端编号或地址" /></label>
        <span class="toolbar-spacer" /><span class="section-note" aria-live="polite">本页 {{ pagedRows.length }} / 共 {{ filtered.length }} 台</span>
      </div>
      <div v-if="loading" class="empty-state"><RefreshCcw class="mx-auto mb-3 animate-spin" />正在加载部标设备…</div>
      <div v-else-if="!filtered.length" class="empty-state empty-action transport-empty">
        <span class="empty-state-icon"><Truck /></span>
        <strong>{{ transportRows.length ? "没有符合搜索条件的部标设备" : "当前后端尚未接入部标设备" }}</strong>
        <span>{{ transportRows.length ? "清空搜索条件后可恢复全部设备。" : "当前后端尚未开放 JT/T 808、JT/T 1078 设备接入；接口开放后，设备会自动显示在此页面。" }}</span>
        <div class="capability-strip" aria-label="计划支持的部标能力">
          <span><RadioTower />JT/T 808 信令</span><span><RadioTower />JT/T 1078 音视频</span><span><RadioTower />车辆与通道目录</span>
        </div>
      </div>
      <div v-else class="table-wrap">
        <table class="data-table device-data-table">
          <thead><tr><th>名称</th><th>终端编号</th><th>地址</th><th>协议</th><th>通道数</th><th>状态</th><th>操作</th></tr></thead>
          <tbody><tr v-for="device in pagedRows" :key="device.id"><td>{{ device.name || "未命名设备" }}</td><td class="mono">{{ device.device_id || device.id }}</td><td class="mono">{{ device.address || [device.ip, device.port].filter(Boolean).join(":") || "—" }}</td><td>{{ device.type }}</td><td>{{ device.channels || 0 }} 路</td><td><span class="status" :class="device.is_online ? 'online' : 'offline'">{{ device.is_online ? "在线" : "离线" }}</span></td><td><RouterLink class="btn btn-sm" :to="`/transport-devices/${encodeURIComponent(device.id)}`">详情</RouterLink></td></tr></tbody>
        </table>
      </div>
      <div v-if="filtered.length" class="pagination">
        <span>本页 {{ pagedRows.length }} 条 · 共 {{ filtered.length }} 条部标设备</span>
        <div v-if="pageCount > 1" class="pagination-actions" aria-label="部标设备分页">
          <button type="button" class="page-btn" :disabled="currentPage === 1" aria-label="上一页" @click="currentPage -= 1"><ChevronLeft /></button>
          <span>第 {{ currentPage }} / {{ pageCount }} 页</span>
          <button type="button" class="page-btn" :disabled="currentPage === pageCount" aria-label="下一页" @click="currentPage += 1"><ChevronRight /></button>
        </div>
      </div>
    </section>
  </main>
</template>
