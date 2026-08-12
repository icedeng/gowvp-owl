<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import {
  Activity,
  ArrowUpRight,
  Box,
  Braces,
  Cpu,
  Database,
  HardDrive,
  LoaderCircle,
  MemoryStick,
  Network,
  RefreshCcw,
  Server,
  ShieldAlert,
} from "@lucide/vue";
import { api, apiUrl, errorMessage } from "../services/api";
import type {
  ApiChannel,
  ApiMetrics,
  HealthInfo,
  MediaServer,
  ResourceStats,
} from "../types/api";
import {
  formatBytes,
  formatDate,
  formatUptime,
  relativeTime,
} from "../utils/format";

const loading = ref(false);
const loadError = ref("");
const health = ref<HealthInfo>({});
const metrics = ref<ApiMetrics>({});
const stats = ref<ResourceStats>({});
const media = ref<MediaServer[]>([]);
const channels = ref<ApiChannel[]>([]);
const latest = (items?: { used?: number; up?: number; down?: number }[]) =>
  items?.[Math.max(0, (items?.length || 1) - 1)] || {};
const cpu = computed(() => Number(latest(stats.value.cpu).used || 0));
const memory = computed(() => Number(latest(stats.value.mem).used || 0));
const network = computed(
  () =>
    (Number(latest(stats.value.net).up || 0) +
      Number(latest(stats.value.net).down || 0)) /
    1_000_000
);
const disk = computed(() => stats.value.disk?.[0]);
const diskPercent = computed(() =>
  disk.value?.total
    ? (Number(disk.value.used || 0) / disk.value.total) * 100
    : 0
);
const endpoints = computed(() =>
  (metrics.value.request_top10 || []).map((item) => ({
    name: item.Key || item.key || "未知接口",
    count: item.Value ?? item.value ?? 0,
  }))
);
const statusCodes = computed(() =>
  Object.fromEntries(
    (metrics.value.status_code_top10 || []).map((item) => [
      item.Key || item.key || "",
      item.Value ?? item.value ?? 0,
    ])
  )
);
const responseTotal = computed(() =>
  Math.max(1, Number(metrics.value.total_responses || 0))
);
const statusPercent = (prefix: string) =>
  (Object.entries(statusCodes.value)
    .filter(([key]) => key.startsWith(prefix))
    .reduce((sum, [, value]) => sum + Number(value), 0) /
    responseTotal.value) *
  100;
const aiTasks = computed(
  () => channels.value.filter((item) => item.ext?.enabled_ai).length
);

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const [
      healthResponse,
      metricResponse,
      statResponse,
      mediaResponse,
      channelResponse,
    ] = await Promise.all([
      api.health(),
      api.metrics(),
      api.stats(),
      api.mediaServers({ page: 1, size: 100 }),
      api.channels({ page: 1, size: 99999 }),
    ]);
    health.value = healthResponse.data;
    metrics.value = metricResponse.data;
    stats.value = statResponse.data;
    media.value = mediaResponse.data.items || [];
    channels.value = channelResponse.data.items || [];
  } catch (cause) {
    loadError.value = errorMessage(cause, "系统指标加载失败");
  } finally {
    loading.value = false;
  }
}

onMounted(load);
</script>

<template>
  <main class="page-content">
    <header class="page-head">
      <div>
        <h1 class="page-title">系统状态</h1>
        <p class="page-desc">
          查看服务构建信息、主机资源、API 指标、媒体节点与 AI 任务状态。
        </p>
      </div>
      <button class="btn" :disabled="loading" @click="load">
        <RefreshCcw :class="{ 'animate-spin': loading }" />刷新指标
      </button>
    </header>
    <div v-if="loadError" class="warning-box mb-4">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <section class="metric-line mb-4">
      <div class="metric-item">
        <div class="metric-label"><span>CPU</span><Cpu /></div>
        <div class="metric-value">{{ cpu.toFixed(1) }}%</div>
        <div class="metric-foot">实时系统采样</div>
      </div>
      <div class="metric-item">
        <div class="metric-label"><span>内存</span><MemoryStick /></div>
        <div class="metric-value">{{ memory.toFixed(1) }}%</div>
        <div class="metric-foot">
          运行时 {{ formatBytes(metrics.sys_alloc) }}
        </div>
      </div>
      <div class="metric-item">
        <div class="metric-label"><span>磁盘</span><HardDrive /></div>
        <div class="metric-value">{{ diskPercent.toFixed(1) }}%</div>
        <div class="metric-foot">
          {{ formatBytes(disk?.used) }} / {{ formatBytes(disk?.total) }}
        </div>
      </div>
      <div class="metric-item">
        <div class="metric-label"><span>网络</span><Network /></div>
        <div class="metric-value">{{ network.toFixed(1) }}</div>
        <div class="metric-foot">Mbps 总吞吐</div>
      </div>
    </section>
    <section class="grid two-col mb-4">
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">服务与构建</h2>
            <p class="card-sub">来自 /health</p>
          </div>
          <Box />
        </div>
        <dl class="definition-grid">
          <div>
            <dt>版本</dt>
            <dd>{{ health.version || "—" }}</dd>
          </div>
          <div>
            <dt>启动时间</dt>
            <dd>{{ formatDate(health.start_at) }}</dd>
          </div>
          <div>
            <dt>运行时间</dt>
            <dd>{{ formatUptime(health.start_at) }}</dd>
          </div>
          <div>
            <dt>Git 分支</dt>
            <dd class="mono">{{ health.git_branch || "—" }}</dd>
          </div>
          <div>
            <dt>Git Commit</dt>
            <dd class="mono">{{ health.git_hash || "—" }}</dd>
          </div>
          <div>
            <dt>API 启动时间</dt>
            <dd>{{ metrics.start_at || "—" }}</dd>
          </div>
        </dl>
      </article>
      <aside class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">依赖服务</h2>
            <p class="card-sub">核心链路状态</p>
          </div>
          <Server />
        </div>
        <div class="step-list">
          <div v-for="item in media" :key="item.id" class="step-item">
            <span class="step-index"><Server /></span
            ><span
              ><strong>{{ item.type || "媒体服务" }}</strong
              ><small
                >{{ item.id }} ·
                {{ relativeTime(item.last_keepalive_at) }}心跳</small
              ></span
            ><span class="status" :class="item.status ? 'online' : 'offline'">{{
              item.status ? "在线" : "离线"
            }}</span>
          </div>
          <div class="step-item">
            <span class="step-index"><Braces /></span
            ><span
              ><strong>AI 分析任务</strong><small>来自通道启用状态</small></span
            ><span class="status" :class="aiTasks ? 'online' : ''"
              >{{ aiTasks }} 个</span
            >
          </div>
          <div class="step-item">
            <span class="step-index"><Database /></span
            ><span
              ><strong>业务数据库</strong
              ><small>健康检查成功即视为服务可用</small></span
            ><span
              class="status"
              :class="health.version ? 'online' : 'offline'"
              >{{ health.version ? "正常" : "未知" }}</span
            >
          </div>
        </div>
      </aside>
    </section>
    <section class="grid equal-col">
      <article class="card table-card">
        <div class="card-head">
          <div>
            <h2 class="card-title">热门 API</h2>
            <p class="card-sub">进程启动以来的请求量</p>
          </div>
          <Activity />
        </div>
        <div class="table-wrap">
          <table class="data-table">
            <thead>
              <tr>
                <th>接口</th>
                <th>请求量</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="item in endpoints" :key="item.name">
                <td class="mono">{{ item.name }}</td>
                <td>{{ item.count }}</td>
              </tr>
            </tbody>
          </table>
          <div v-if="loading" class="empty-state">
            <LoaderCircle class="mx-auto mb-2 animate-spin" />正在加载指标…
          </div>
          <div v-else-if="!endpoints.length" class="empty-state">
            暂无 API 请求统计。
          </div>
        </div>
      </article>
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">运行时指标</h2>
            <p class="card-sub">API、GC 与协程</p>
          </div>
          <Activity />
        </div>
        <div class="health-list">
          <div class="health-row">
            <span>2xx 响应</span>
            <div class="progress">
              <i :style="{ width: `${Math.min(100, statusPercent('2'))}%` }" />
            </div>
            <span>{{ statusPercent("2").toFixed(1) }}%</span>
          </div>
          <div class="health-row">
            <span>4xx 响应</span>
            <div class="progress warn">
              <i :style="{ width: `${Math.min(100, statusPercent('4'))}%` }" />
            </div>
            <span>{{ statusPercent("4").toFixed(1) }}%</span>
          </div>
          <div class="health-row">
            <span>5xx 响应</span>
            <div class="progress danger">
              <i :style="{ width: `${Math.min(100, statusPercent('5'))}%` }" />
            </div>
            <span>{{ statusPercent("5").toFixed(1) }}%</span>
          </div>
          <div class="health-row">
            <span>协程</span>
            <span class="health-note">当前运行数量</span>
            <strong>{{ metrics.goroutines || 0 }}</strong>
          </div>
          <div class="health-row">
            <span>GC 次数</span>
            <span class="health-note">进程启动以来</span>
            <strong>{{ metrics.num_gc || 0 }}</strong>
          </div>
        </div>
        <a
          class="btn btn-sm mt-4"
          :href="apiUrl('/swagger/index.html')"
          target="_blank"
          rel="noreferrer"
          >打开 Swagger<ArrowUpRight
        /></a>
      </article>
    </section>
  </main>
</template>
