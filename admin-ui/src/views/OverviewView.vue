<script setup lang="ts">
import { computed, defineAsyncComponent, onMounted, ref } from "vue";
import {
  Activity,
  ArrowRight,
  Bot,
  Camera,
  CircleAlert,
  Film,
  HardDrive,
  LoaderCircle,
  RadioTower,
  RefreshCcw,
  Server,
  ShieldAlert,
  Video,
} from "@lucide/vue";
import { api, errorMessage, typeLabel } from "../services/api";
import type {
  ApiChannel,
  ApiDevice,
  ApiEvent,
  ApiMetrics,
  HealthInfo,
  MediaServer,
  ResourceStats,
} from "../types/api";
import { formatBytes, formatDate, formatUptime } from "../utils/format";

const TrendChart = defineAsyncComponent(
  () => import("../components/TrendChart.vue")
);
const loading = ref(false);
const loadError = ref("");
const devices = ref<ApiDevice[]>([]);
const channels = ref<ApiChannel[]>([]);
const events = ref<ApiEvent[]>([]);
const mediaServers = ref<MediaServer[]>([]);
const health = ref<HealthInfo>({});
const metrics = ref<ApiMetrics>({});
const stats = ref<ResourceStats>({});

const onlineDevices = computed(
  () => devices.value.filter((item) => item.is_online).length
);
const onlineChannels = computed(
  () => channels.value.filter((item) => item.is_online).length
);
const activeStreams = computed(
  () => channels.value.filter((item) => item.is_playing).length
);
const activeMedia = computed(
  () => mediaServers.value.filter((item) => item.status).length
);
const coreConnected = computed(
  () => Boolean(health.value.version) && !loadError.value
);
const coreHeadline = computed(() => {
  if (loading.value && !health.value.version) return "正在核对核心链路";
  if (!coreConnected.value) return "核心链路状态未知";
  if (mediaServers.value.length && !activeMedia.value) return "媒体链路需要关注";
  return "核心链路运行正常";
});
const cpu = computed(
  () =>
    stats.value.cpu?.[Math.max(0, (stats.value.cpu?.length || 1) - 1)]?.used ||
    0
);
const memory = computed(
  () =>
    stats.value.mem?.[Math.max(0, (stats.value.mem?.length || 1) - 1)]?.used ||
    0
);
const disk = computed(() => stats.value.disk?.[0]);
const diskPercent = computed(() =>
  disk.value?.total
    ? (Number(disk.value.used || 0) / disk.value.total) * 100
    : 0
);
const protocolSlots = computed(() =>
  ["GB28181", "ONVIF", "RTMP", "RTSP"].map((protocol) => {
    const items = channels.value.filter(
      (item) =>
        typeLabel(
          item.type,
          item.did || item.device_id || item.channel_id || item.id
        ) === protocol
    );
    const online = items.filter((item) => item.is_online).length;
    return {
      protocol,
      value: `${online} / ${items.length}`,
      note: items.length ? `${items.length - online} 路离线` : "暂无接入",
      state: items.length && online < items.length ? "warn" : "",
    };
  })
);
const channelNames = computed(() =>
  Object.fromEntries(
    channels.value.map((item) => [
      item.id,
      item.name || item.channel_id || item.id,
    ])
  )
);
const watchItems = computed(() => {
  const items: {
    level: string;
    warn: boolean;
    name: string;
    detail: string;
    reason: string;
    time: string;
    to: string;
  }[] = [];
  devices.value
    .filter((item) => !item.is_online)
    .slice(0, 2)
    .forEach((item) =>
      items.push({
        level: "P1",
        warn: false,
        name: item.name || item.device_id || item.id,
        detail: `离线 · 影响 ${item.channels || 0} 路通道`,
        reason: "注册或心跳中断",
        time: formatDate(item.keepalive_at, "暂无"),
        to: `/devices/${encodeURIComponent(item.id)}`,
      })
    );
  events.value
    .slice(0, 2)
    .forEach((item) =>
      items.push({
        level: "P2",
        warn: true,
        name: `${item.label || "未分类事件"} · ${
          channelNames.value[item.cid || ""] || item.cid || "未知通道"
        }`,
        detail: `${Math.round(Number(item.score || 0) * 100)}% · ${
          item.model || "设备上报"
        }`,
        reason: "最新检测事件",
        time: formatDate(item.started_at || item.created_at),
        to: `/events?event=${item.id}`,
      })
    );
  if (diskPercent.value >= 80)
    items.push({
      level: "P2",
      warn: true,
      name: "录像磁盘",
      detail: `已使用 ${diskPercent.value.toFixed(1)}%`,
      reason: "接近清理阈值",
      time: "当前",
      to: "/system-status",
    });
  return items.slice(0, 4);
});
const trend = computed(() => {
  const now = new Date();
  const buckets = Array.from({ length: 12 }, (_, index) => ({
    start: now.getTime() - (11 - index) * 2 * 3600000,
    value: 0,
  }));
  events.value.forEach((item) => {
    const time = new Date(item.started_at || item.created_at || 0).getTime();
    const bucket = buckets.findIndex(
      (entry, index) =>
        time >= entry.start &&
        (index === buckets.length - 1 || time < buckets[index + 1].start)
    );
    if (bucket >= 0) buckets[bucket].value += 1;
  });
  return {
    values: buckets.map((item) => item.value),
    labels: buckets.map((item) =>
      new Date(item.start).getHours().toString().padStart(2, "0")
    ),
  };
});

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const now = Date.now();
    const [
      deviceResponse,
      channelResponse,
      eventResponse,
      mediaResponse,
      healthResponse,
      metricsResponse,
      statsResponse,
    ] = await Promise.all([
      api.devices({ page: 1, size: 99999 }),
      api.channels({ page: 1, size: 99999 }),
      api.events({
        page: 1,
        size: 1000,
        start_ms: now - 24 * 3600000,
        end_ms: now,
      }),
      api.mediaServers({ page: 1, size: 100 }),
      api.health(),
      api.metrics(),
      api.stats(),
    ]);
    devices.value = deviceResponse.data.items || [];
    channels.value = channelResponse.data.items || [];
    events.value = eventResponse.data.items || [];
    mediaServers.value = mediaResponse.data.items || [];
    health.value = healthResponse.data;
    metrics.value = metricsResponse.data;
    stats.value = statsResponse.data;
  } catch (cause) {
    loadError.value = errorMessage(cause, "运行态势加载失败");
  } finally {
    loading.value = false;
  }
}

onMounted(load);
</script>

<template>
  <main class="page-content overview-page">
    <header class="page-head">
      <div>
        <h1 class="page-title">运行总览</h1>
        <p class="page-desc">
          聚合当前环境的设备、通道、媒体节点、事件与系统资源。
        </p>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新态势</button
        ><RouterLink class="btn btn-primary" to="/live"
          ><Video />进入实时监控</RouterLink
        >
      </div>
    </header>
    <div v-if="loadError" class="warning-box mb-4">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load">重试</button>
    </div>
    <section class="signal-strip" aria-label="平台核心运行态势">
      <div class="signal-intro">
        <div class="signal-kicker">
          <i :class="{ warn: !coreConnected }" />平台态势
        </div>
        <h2>{{ coreHeadline }}</h2>
        <p>
          {{ activeMedia }} / {{ mediaServers.length }} 个媒体节点在线 ·
          服务运行 {{ formatUptime(health.start_at) }}
        </p>
      </div>
      <div
        class="signal-cell"
        :class="{ warn: devices.length && onlineDevices < devices.length }"
      >
        <small>在线设备</small
        ><strong>{{ onlineDevices }} / {{ devices.length }}</strong
        ><span
          ><i />{{
            devices.length
              ? ((onlineDevices / devices.length) * 100).toFixed(1)
              : 0
          }}% 在线</span
        >
      </div>
      <div
        class="signal-cell"
        :class="{ warn: channels.length && onlineChannels < channels.length }"
      >
        <small>在线通道</small
        ><strong>{{ onlineChannels }} / {{ channels.length }}</strong
        ><span
          ><i />{{
            channels.length
              ? ((onlineChannels / channels.length) * 100).toFixed(1)
              : 0
          }}% 可用</span
        >
      </div>
      <div class="signal-cell" :class="{ warn: events.length }">
        <small>24 小时事件</small><strong>{{ events.length }}</strong
        ><span
          ><i />最近一条
          {{
            events[0]
              ? formatDate(events[0].started_at || events[0].created_at)
              : "暂无"
          }}</span
        >
      </div>
      <div class="signal-cell">
        <small>活跃媒体流</small><strong>{{ activeStreams }}</strong
        ><span><i />当前播放中</span>
      </div>
    </section>
    <section class="overview-actions" aria-label="快捷入口">
      <div class="overview-actions-intro">
        <strong>快捷入口</strong>
        <span>沿信号链路进入高频任务</span>
      </div>
      <RouterLink class="overview-action" to="/devices">
        <span class="overview-action-icon"><Camera /></span>
        <span><strong>设备管理</strong><small>接入与能力探测</small></span>
        <ArrowRight />
      </RouterLink>
      <RouterLink class="overview-action" to="/recordings">
        <span class="overview-action-icon"><Film /></span>
        <span><strong>录像中心</strong><small>检索、回放与下载</small></span>
        <ArrowRight />
      </RouterLink>
      <RouterLink class="overview-action" to="/diagnostics">
        <span class="overview-action-icon"><RadioTower /></span>
        <span><strong>协议诊断</strong><small>版本与能力检查</small></span>
        <ArrowRight />
      </RouterLink>
      <RouterLink class="overview-action" to="/system-status">
        <span class="overview-action-icon"><Server /></span>
        <span><strong>系统状态</strong><small>资源与服务指标</small></span>
        <ArrowRight />
      </RouterLink>
    </section>
    <section class="overview-grid overview-protocol">
      <article class="card card-pad protocol-panel">
        <div class="card-head">
          <div>
            <h2 class="card-title">协议接入矩阵</h2>
            <p class="card-sub">统一核验各协议在线率与离线规模</p>
          </div>
          <RouterLink class="btn btn-sm" to="/devices"
            >查看国标设备<ArrowRight
          /></RouterLink>
        </div>
        <div class="protocol-board">
          <RouterLink
            v-for="item in protocolSlots"
            :key="item.protocol"
            class="protocol-lane"
            :to="
              item.protocol === 'RTMP'
                ? '/push-streams'
                : item.protocol === 'RTSP'
                ? '/pull-streams'
                : item.protocol === 'GB28181'
                ? '/devices'
                : '/live'
            "
            ><span class="protocol-lane-name"
              ><i class="slot-led" :class="item.state" />
              <span class="protocol-tag blue">{{ item.protocol }}</span></span
            ><strong>{{ item.value }}</strong
            ><small>{{ item.note }}</small><ArrowRight /></RouterLink
          >
        </div>
      </article>
      <article class="card card-pad media-service-card">
        <div class="card-head">
          <div>
            <h2 class="card-title">媒体与服务</h2>
            <p class="card-sub">
              {{ mediaServers[0]?.id || "暂无节点" }} ·
              {{ mediaServers[0]?.type || "—" }}
            </p>
          </div>
          <span class="status" :class="activeMedia ? 'online' : 'offline'">{{
            activeMedia ? "在线" : "离线"
          }}</span>
        </div>
        <div class="media-service-summary">
          <span class="details-icon"><Server /></span>
          <div>
            <strong>{{ activeMedia }} / {{ mediaServers.length }}</strong>
            <small>媒体节点在线</small>
          </div>
        </div>
        <dl class="service-stat-grid">
          <div>
            <dt>服务版本</dt>
            <dd>{{ health.version || "—" }}</dd>
          </div>
          <div>
            <dt>Git 提交</dt>
            <dd class="mono">{{ health.git_hash || "—" }}</dd>
          </div>
          <div>
            <dt>API 累计响应</dt>
            <dd>{{ metrics.total_responses || 0 }}</dd>
          </div>
          <div>
            <dt>运行内存</dt>
            <dd>{{ formatBytes(metrics.sys_alloc) }}</dd>
          </div>
        </dl>
        <RouterLink class="btn btn-sm mt-4 w-full" to="/media-servers"
          >查看媒体节点<ArrowRight
        /></RouterLink>
      </article>
    </section>
    <section class="metric-line overview-metrics" aria-label="系统资源摘要">
      <div class="metric-item">
        <div class="metric-label"><span>CPU 使用率</span><Activity /></div>
        <div class="metric-value">{{ cpu.toFixed(1) }}%</div>
        <div class="metric-foot">实时采样</div>
      </div>
      <div class="metric-item">
        <div class="metric-label"><span>内存使用率</span><Server /></div>
        <div class="metric-value">{{ memory.toFixed(1) }}%</div>
        <div class="metric-foot">系统内存</div>
      </div>
      <div class="metric-item">
        <div class="metric-label"><span>磁盘使用率</span><HardDrive /></div>
        <div class="metric-value">{{ diskPercent.toFixed(1) }}%</div>
        <div class="metric-foot">
          {{ formatBytes(disk?.used) }} / {{ formatBytes(disk?.total) }}
        </div>
      </div>
      <div class="metric-item">
        <div class="metric-label">
          <span>实时 API 请求</span><CircleAlert />
        </div>
        <div class="metric-value">{{ metrics.real_time_requests || 0 }}</div>
        <div class="metric-foot">累计 {{ metrics.total_requests || 0 }} 次</div>
      </div>
    </section>
    <section class="overview-grid overview-watch">
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">需要关注</h2>
            <p class="card-sub">离线设备、最新事件与资源告警</p>
          </div>
          <span class="status" :class="watchItems.length ? 'warning' : 'online'"
            >{{ watchItems.length }} 项</span
          >
        </div>
        <div class="watch-list">
          <RouterLink
            v-for="item in watchItems"
            :key="`${item.name}-${item.time}`"
            class="watch-row"
            :to="item.to"
            ><span class="watch-level" :class="{ warn: item.warn }">{{
              item.level
            }}</span
            ><span
              ><strong>{{ item.name }}</strong
              ><small>{{ item.detail }}</small></span
            ><span
              ><strong>{{ item.reason }}</strong
              ><small>实时状态</small></span
            ><time class="mono">{{ item.time }}</time
            ><ArrowRight class="h-4 w-4 text-slate-400"
          /></RouterLink>
          <div v-if="loading" class="empty-state">
            <LoaderCircle class="mx-auto mb-2 animate-spin" />正在聚合运行状态…
          </div>
          <div v-else-if="!watchItems.length" class="empty-state empty-action">
            <ShieldAlert />
            <strong>当前没有需要优先关注的项目</strong>
            <span>设备、媒体节点和系统资源暂未发现异常。</span>
          </div>
        </div>
      </article>
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">最新事件</h2>
            <p class="card-sub">AI 检测与国标报警统一展示</p>
          </div>
          <RouterLink class="btn btn-sm" to="/events"
            >全部<ArrowRight
          /></RouterLink>
        </div>
        <div class="activity-list">
          <RouterLink
            v-for="event in events.slice(0, 3)"
            :key="event.id"
            class="activity-item"
            :to="`/events?event=${event.id}`"
            ><span class="activity-icon"><Bot /></span
            ><span class="activity-main"
              ><strong
                >{{ event.label || "未分类事件" }} ·
                {{
                  channelNames[event.cid || ""] || event.cid || "未知通道"
                }}</strong
              ><span
                >{{ Math.round(Number(event.score || 0) * 100) }}% ·
                {{ event.model || "设备上报" }}</span
              ></span
            ><span class="activity-time">{{
              formatDate(event.started_at || event.created_at)
            }}</span></RouterLink
          >
          <div
            v-if="!loading && !events.length"
            class="empty-state empty-action compact"
          >
            <Bot />
            <strong>过去 24 小时暂无事件</strong>
            <span>新事件会在这里出现，并同步进入事件中心。</span>
          </div>
        </div>
      </article>
    </section>
    <section class="overview-bottom">
      <article class="card card-pad overview-trend-card">
        <div class="card-head">
          <div>
            <h2 class="card-title">过去 24 小时事件趋势</h2>
            <p class="card-sub">按 2 小时聚合当前已加载事件</p>
          </div>
          <span class="status info">{{ events.length }} 次</span>
        </div>
        <TrendChart
          :values="trend.values"
          :labels="trend.labels"
          aria-label="过去 24 小时事件趋势图"
        />
      </article>
    </section>
  </main>
</template>
