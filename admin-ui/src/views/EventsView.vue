<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useRoute } from "vue-router";
import {
  CalendarClock,
  ImageOff,
  LoaderCircle,
  Play,
  RefreshCcw,
  Search,
  ShieldAlert,
  Siren,
  Video,
  X,
} from "@lucide/vue";
import { api, errorMessage } from "../services/api";
import type { ApiEvent } from "../types/api";
import { formatDate } from "../utils/format";
import RemoteChannelSelect from "../components/RemoteChannelSelect.vue";

const route = useRoute();
const query = ref("");
const source = ref("全部来源");
const period = ref("today");
const channelFilter = ref("all");
const loading = ref(false);
const loadingMore = ref(false);
const loadError = ref("");
const events = ref<ApiEvent[]>([]);
const channelNames = ref<Record<string, string>>({});
const total = ref(0);
const page = ref(1);
const eventImageErrors = ref(new Set<number>());
const autoRefresh = ref(false);
const PAGE_SIZE = 48;
let timer: number | undefined;
let loadSequence = 0;
const channelNamePending = new Set<string>();

const focusedId = computed(() => Number(route.query.event || 0));
const filtered = computed(() =>
  events.value
    .filter((item) => {
      const channel = channelNames.value[item.cid || ""] || item.cid || "";
      const text = `${item.label || ""}${channel}${item.model || ""}${
        item.did || ""
      }`
        .toLowerCase()
        .includes(query.value.toLowerCase());
      const alarm = isAlarm(item);
      const matchSource =
        source.value === "全部来源" ||
        (source.value === "设备报警" ? alarm : !alarm);
      return text && matchSource;
    })
    .sort(
      (a, b) =>
        Number(b.id === focusedId.value) - Number(a.id === focusedId.value)
    )
);
const hasFilters = computed(() =>
  Boolean(
    query.value ||
      source.value !== "全部来源" ||
      period.value !== "today" ||
      channelFilter.value !== "all"
  )
);
const canLoadMore = computed(() => events.value.length < total.value);

function resetFilters() {
  query.value = "";
  source.value = "全部来源";
  period.value = "today";
  channelFilter.value = "all";
  load();
}

function isAlarm(item: ApiEvent) {
  return /alarm|报警/i.test(`${item.model || ""}${item.label || ""}`);
}

function rangeParams() {
  const end = Date.now();
  const date = new Date();
  date.setHours(0, 0, 0, 0);
  const start =
    period.value === "30d"
      ? end - 30 * 86400000
      : period.value === "7d"
      ? end - 7 * 86400000
      : date.getTime();
  return { start_ms: start, end_ms: end };
}

async function load(silent = false, append = false) {
  const sequence = ++loadSequence;
  const requestedPage = append ? page.value + 1 : 1;
  if (append) loadingMore.value = true;
  else if (!silent) loading.value = true;
  loadError.value = "";
  try {
    const eventResponse = await api.events({
      page: requestedPage,
      size: PAGE_SIZE,
      cid: channelFilter.value === "all" ? undefined : channelFilter.value,
      ...rangeParams(),
    });
    if (sequence !== loadSequence) return;
    const nextEvents = eventResponse.data?.items || [];
    events.value = append
      ? [...new Map([...events.value, ...nextEvents].map((item) => [item.id, item])).values()]
      : nextEvents;
    if (!append) eventImageErrors.value = new Set();
    page.value = requestedPage;
    total.value = eventResponse.data?.total ?? events.value.length;
    if (
      !append &&
      focusedId.value &&
      !events.value.some((item) => item.id === focusedId.value)
    ) {
      try {
        const focused = (await api.event(focusedId.value)).data;
        if (sequence === loadSequence) events.value = [focused, ...events.value];
      } catch {
        // 聚焦事件可能已过期或无权访问，保留当前列表。
      }
    }
    void resolveChannelNames(events.value);
  } catch (cause) {
    if (sequence === loadSequence) loadError.value = errorMessage(cause, "事件列表加载失败");
  } finally {
    if (sequence === loadSequence) {
      loading.value = false;
      loadingMore.value = false;
    }
  }
}

async function resolveChannelNames(items: ApiEvent[]) {
  const ids = [
    ...new Set(
      items
        .map((item) => item.cid)
        .filter(
          (id): id is string =>
            Boolean(id) &&
            !channelNames.value[id!] &&
            !channelNamePending.has(id!)
        )
    ),
  ];
  if (!ids.length) return;
  ids.forEach((id) => channelNamePending.add(id));
  const results = await Promise.allSettled(ids.map((id) => api.channel(id)));
  const resolved: Record<string, string> = {};
  results.forEach((result, index) => {
    if (result.status !== "fulfilled") return;
    const item = result.value.data;
    resolved[ids[index]] = item.name || item.channel_id || item.id;
  });
  channelNames.value = { ...channelNames.value, ...resolved };
  ids.forEach((id) => channelNamePending.delete(id));
}

function loadMore() {
  if (!loading.value && !loadingMore.value && canLoadMore.value)
    void load(false, true);
}

function toggleAutoRefresh() {
  autoRefresh.value = !autoRefresh.value;
  window.clearInterval(timer);
  if (autoRefresh.value) timer = window.setInterval(() => load(true), 30000);
}

function imageUrl(item: ApiEvent) {
  return item.image_path ? api.eventImage(item.image_path) : "";
}

function markEventImageError(id: number) {
  eventImageErrors.value = new Set(eventImageErrors.value).add(id);
}

onMounted(load);
onBeforeUnmount(() => window.clearInterval(timer));
</script>

<template>
  <main class="page-content">
    <header class="page-head">
      <div>
        <h1 class="page-title">事件中心</h1>
        <p class="page-desc">
          统一查询 AI 检测与 GB28181 设备报警，关联实时画面和平台录像。
        </p>
      </div>
      <div class="head-actions">
        <button
          class="btn"
          :class="{ 'btn-primary': autoRefresh }"
          @click="toggleAutoRefresh"
        >
          <CalendarClock />{{
            autoRefresh ? "自动刷新中" : "自动刷新 30s"
          }}</button
        ><button class="btn" :disabled="loading" @click="load()">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新</button
        ><RouterLink class="btn btn-primary" to="/live"
          ><Video />实时核验</RouterLink
        >
      </div>
    </header>
    <div v-if="loadError" class="warning-box mb-4" role="alert">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load()">重试</button>
    </div>
    <section class="card card-pad mb-4">
      <div class="toolbar mb-0">
        <label class="field"
          ><Search /><input
            v-model="query"
            class="input"
            aria-label="搜索事件"
            placeholder="搜索标签、通道、模型或设备" /></label
        ><select v-model="source" class="select" aria-label="按事件来源筛选">
          <option>全部来源</option>
          <option>AI 检测</option>
          <option>设备报警</option></select
        ><RemoteChannelSelect
          v-model="channelFilter"
          aria-label="按通道筛选"
          @change="load()"
        />
        <select
          v-model="period"
          class="select"
          aria-label="按时间范围筛选"
          @change="load()"
        >
          <option value="today">今天</option>
          <option value="7d">最近 7 天</option>
          <option value="30d">最近 30 天</option></select
        ><button
          v-if="hasFilters"
          class="btn btn-sm filter-reset"
          type="button"
          @click="resetFilters"
        >
          <X />清除筛选</button
        ><span class="toolbar-spacer" /><span
          class="section-note"
          aria-live="polite"
          >显示 {{ filtered.length }} / 已加载 {{ events.length }} 条<span
            v-if="total > events.length"
            >（共 {{ total }} 条）</span
          ></span
        >
      </div>
    </section>
    <div class="warning-box mb-4">
      <Siren /><span
        >当前后端事件没有确认、忽略或派发状态；界面只提供查询、详情与关联跳转。</span
      >
    </div>
    <section class="event-grid" :aria-busy="loading || loadingMore">
      <article
        v-for="event in filtered"
        :key="event.id"
        class="card event-card"
        :class="{ 'focus-ring': event.id === focusedId }"
      >
        <div
          class="event-visual"
          :class="[
            isAlarm(event) ? 'alarm' : 'person',
            { 'has-image': Boolean(imageUrl(event)) && !eventImageErrors.has(event.id) },
          ]"
        >
          <img
            v-if="imageUrl(event) && !eventImageErrors.has(event.id)"
            class="event-image"
            :src="imageUrl(event)"
            :alt="`${event.label || '未分类'}事件抓拍`"
            loading="lazy"
            @error="markEventImageError(event.id)"
          />
          <div v-else class="event-image-empty">
            <ImageOff /><span>{{ event.image_path ? "抓拍加载失败" : "无事件抓拍" }}</span>
          </div>
          <span class="event-time">{{
            formatDate(event.started_at || event.created_at)
          }}</span
          ><span class="confidence">{{
            isAlarm(event)
              ? "设备报警"
              : `${Math.round(Number(event.score || 0) * 100)}%`
          }}</span>
        </div>
        <div class="event-body">
          <div class="flex items-start justify-between gap-3">
            <div>
              <h3>{{ event.label || "未分类事件" }}</h3>
              <p class="mt-1 text-xs text-slate-500">事件 #{{ event.id }}</p>
            </div>
            <span
              class="protocol-tag"
              :class="isAlarm(event) ? 'amber' : 'blue'"
              >{{ isAlarm(event) ? "GB ALARM" : "AI" }}</span
            >
          </div>
          <div class="event-meta">
            <span>{{
              channelNames[event.cid || ""] || event.cid || "未知通道"
            }}</span
            ><span>{{
              event.model || (isAlarm(event) ? "设备上报" : "未知模型")
            }}</span>
          </div>
          <div class="event-actions">
            <RouterLink
              class="btn btn-sm flex-1"
              :to="`/live?channel=${encodeURIComponent(event.cid || '')}`"
              ><Play />实时画面</RouterLink
            ><RouterLink
              class="btn btn-sm flex-1"
              :to="`/recordings?event=${event.id}&channel=${encodeURIComponent(
                event.cid || ''
              )}`"
              >关联录像</RouterLink
            >
          </div>
        </div>
      </article>
      <div v-if="loading" class="card empty-state col-span-full">
        <LoaderCircle class="mx-auto mb-3 h-7 w-7 animate-spin" />正在加载事件…
      </div>
      <div
        v-else-if="!filtered.length"
        class="card empty-state empty-action col-span-full"
      >
        <Siren /><strong>{{
          events.length ? "当前筛选条件下没有事件" : "所选时间范围内暂无事件"
        }}</strong
        ><span>{{
          events.length
            ? "清除筛选后可恢复已加载事件。"
            : "新事件到达后会自动进入事件中心。"
        }}</span
        ><button v-if="hasFilters" class="btn" @click="resetFilters">
          清除筛选
        </button>
      </div>
    </section>
    <div v-if="events.length" class="pagination mt-4">
      <span>已加载 {{ events.length }} / {{ total }} 条事件</span>
      <button
        v-if="canLoadMore"
        class="btn btn-sm"
        type="button"
        :disabled="loadingMore"
        @click="loadMore"
      >
        <LoaderCircle v-if="loadingMore" class="animate-spin" />{{ loadingMore ? "正在加载…" : "加载更多事件" }}
      </button>
      <span v-else class="section-note">当前结果已全部加载</span>
    </div>
  </main>
</template>
