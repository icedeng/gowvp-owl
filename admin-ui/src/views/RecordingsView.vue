<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { useRoute } from "vue-router";
import {
  CalendarDays,
  Download,
  Film,
  LoaderCircle,
  Play,
  RefreshCcw,
  Search,
  ShieldAlert,
  X,
} from "@lucide/vue";
import { api, errorMessage, withToken } from "../services/api";
import type {
  ApiRecording,
  MonthlyStats,
  TimelineRange,
} from "../types/api";
import { formatBytes, formatDate, formatDuration } from "../utils/format";
import { useUiStore } from "../stores/ui";
import RemoteChannelSelect from "../components/RemoteChannelSelect.vue";

const ui = useUiStore();
const route = useRoute();
const query = ref("");
const channelFilter = ref(
  typeof route.query.channel === "string" ? route.query.channel : "all"
);
function localDateInput(value: Date) {
  const year = value.getFullYear();
  const month = String(value.getMonth() + 1).padStart(2, "0");
  const day = String(value.getDate()).padStart(2, "0");
  return `${year}-${month}-${day}`;
}

const selectedDate = ref(localDateInput(new Date()));
const loading = ref(false);
const loadingMore = ref(false);
const loadError = ref("");
const recordings = ref<ApiRecording[]>([]);
const channelNames = ref<Record<string, string>>({});
const timeline = ref<TimelineRange[]>([]);
const monthly = ref<MonthlyStats | null>(null);
const total = ref(0);
const page = ref(1);
const downloading = ref<number | null>(null);
const PAGE_SIZE = 100;
let loadSequence = 0;
const channelNamePending = new Set<string>();

const contextLabel = computed(() =>
  route.query.event
    ? `事件 #${route.query.event} · ${
        channelFilter.value === "all"
          ? "全部通道"
          : channelNames.value[channelFilter.value] || channelFilter.value
      }`
    : channelFilter.value === "all"
    ? "全部通道"
    : channelNames.value[channelFilter.value] || channelFilter.value
);
const date = computed(() => new Date(`${selectedDate.value}T00:00:00`));
const dateRange = computed(() => ({
  start_ms: date.value.getTime(),
  end_ms: date.value.getTime() + 86400000 - 1,
}));
const days = computed(() =>
  Array.from({ length: 7 }, (_, index) => {
    const item = new Date(date.value);
    item.setDate(item.getDate() + index - 3);
    return {
      value: localDateInput(item),
      day: item.getDate(),
      week: ["日", "一", "二", "三", "四", "五", "六"][item.getDay()],
    };
  })
);
const filtered = computed(() =>
  recordings.value.filter((record) =>
    `${channelNames.value[record.cid || ""] || record.cid || ""} REC-${
      record.id
    } ${record.app || ""} ${record.stream || ""}`
      .toLowerCase()
      .includes(query.value.toLowerCase())
  )
);
const recordDays = computed(
  () =>
    new Set(
      (monthly.value?.has_video || "")
        .split("")
        .flatMap((flag, index) => (flag === "1" ? [index + 1] : []))
    )
);
const totalSize = computed(() =>
  recordings.value.reduce((sum, item) => sum + Number(item.size || 0), 0)
);
const eventCount = computed(() =>
  recordings.value.reduce(
    (sum, item) => sum + Number(item.object_count || 0),
    0
  )
);
const canPlayDay = computed(
  () => channelFilter.value !== "all" && recordings.value.length > 0
);
const hasSearch = computed(() => Boolean(query.value));
const canLoadMore = computed(() => recordings.value.length < total.value);

function resetSearch() {
  query.value = "";
}

function selectToday() {
  selectedDate.value = localDateInput(new Date());
}

async function load(options: { append?: boolean } = {}) {
  const append = Boolean(options.append);
  const sequence = ++loadSequence;
  const requestedPage = append ? page.value + 1 : 1;
  if (append) loadingMore.value = true;
  else loading.value = true;
  loadError.value = "";
  try {
    const channel =
      channelFilter.value === "all" ? undefined : channelFilter.value;
    const [listResponse, monthlyResponse, timelineResponse] = await Promise.allSettled([
      api.recordings({ page: requestedPage, size: PAGE_SIZE, cid: channel, ...dateRange.value }),
      append
        ? Promise.resolve(null)
        : api.monthly({
            cid: channel,
            year: date.value.getFullYear(),
            month: date.value.getMonth() + 1,
          }),
      append
        ? Promise.resolve(null)
        : channel
        ? api.timeline({ cid: channel, ...dateRange.value })
        : Promise.resolve(null),
    ]);
    if (sequence !== loadSequence) return;
    if (listResponse.status === "rejected") throw listResponse.reason;
    const nextRecordings = listResponse.value.data?.items || [];
    recordings.value = append
      ? [...new Map([...recordings.value, ...nextRecordings].map((item) => [item.id, item])).values()]
      : nextRecordings;
    page.value = requestedPage;
    total.value = listResponse.value.data?.total ?? recordings.value.length;
    if (!append) {
      monthly.value = monthlyResponse.status === "fulfilled" && monthlyResponse.value
        ? monthlyResponse.value.data
        : null;
      timeline.value = timelineResponse.status === "fulfilled" && timelineResponse.value
        ? timelineResponse.value.data?.items || []
        : [];
    }
    const auxiliaryFailure = [monthlyResponse, timelineResponse].find((item) => item.status === "rejected");
    const auxiliaryError = auxiliaryFailure?.status === "rejected"
      ? errorMessage(auxiliaryFailure.reason)
      : "";
    if (auxiliaryError) {
      loadError.value = `录像列表已加载，部分辅助数据暂不可用：${auxiliaryError}`;
    }
    void resolveChannelNames(recordings.value, channel);
  } catch (cause) {
    if (sequence === loadSequence) loadError.value = errorMessage(cause, "录像数据加载失败");
  } finally {
    if (sequence === loadSequence) {
      loading.value = false;
      loadingMore.value = false;
    }
  }
}

async function resolveChannelNames(items: ApiRecording[], selectedId?: string) {
  const ids = [
    ...new Set(
      [...items.map((item) => item.cid), selectedId]
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
    void load({ append: true });
}

function timelineStyle(item: TimelineRange) {
  const start = Math.max(dateRange.value.start_ms, item.start_ms);
  const end = Math.min(dateRange.value.end_ms, item.end_ms);
  return {
    left: `${((start - dateRange.value.start_ms) / 86400000) * 100}%`,
    width: `${Math.max(0.35, ((end - start) / 86400000) * 100)}%`,
  };
}

function playDay() {
  if (channelFilter.value === "all")
    return ui.toast("请先选择一个通道再播放当天录像");
  const url = withToken(
    api.hlsPlaylist(
      channelFilter.value,
      dateRange.value.start_ms,
      dateRange.value.end_ms
    )
  );
  window.open(url, "_blank", "noopener,noreferrer");
}

function playRecord(record: ApiRecording) {
  if (!record.cid || !record.started_at || !record.ended_at)
    return ui.toast("当前录像缺少播放时间或通道信息");
  window.open(
    withToken(
      api.hlsPlaylist(
        record.cid,
        new Date(record.started_at).getTime(),
        new Date(record.ended_at).getTime()
      )
    ),
    "_blank",
    "noopener,noreferrer"
  );
}

async function download(record: ApiRecording) {
  downloading.value = record.id;
  try {
    const { data } = await api.downloadRecording(record.id);
    const url = URL.createObjectURL(data);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = `recording-${record.id}.mp4`;
    anchor.click();
    URL.revokeObjectURL(url);
    ui.toast(`录像 REC-${record.id} 已开始下载`);
  } catch (cause) {
    ui.toast(errorMessage(cause, "录像下载失败"));
  } finally {
    downloading.value = null;
  }
}

watch([channelFilter, selectedDate], () => load());
onMounted(load);
</script>

<template>
  <main class="page-content">
    <header class="page-head">
      <div>
        <h1 class="page-title">录像中心</h1>
        <p class="page-desc">
          按通道、日期、月历和时间轴定位平台录像。当前上下文：{{
            contextLabel
          }}。
        </p>
      </div>
      <div class="head-actions">
        <button class="btn" type="button" @click="selectToday">今天</button
        ><label class="btn"
          ><CalendarDays /><input
            v-model="selectedDate"
            class="bg-transparent outline-none"
            aria-label="选择录像日期"
            type="date" /></label
        ><button class="btn" :disabled="loading" @click="load()">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新</button
        ><button
          class="btn btn-primary"
          :disabled="!canPlayDay"
          :title="channelFilter === 'all' ? '请先选择一个通道' : undefined"
          @click="playDay"
        >
          <Play />{{
            channelFilter === "all" ? "选择通道后播放" : "播放当天录像"
          }}
        </button>
      </div>
    </header>
    <div v-if="loadError" class="warning-box mb-4" role="alert">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="load()">重试</button>
    </div>
    <section class="content-grid">
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">
              {{ date.getFullYear() }} 年 {{ date.getMonth() + 1 }} 月 ·
              录像时间轴
            </h2>
            <p class="card-sub">
              蓝色为连续录像，琥珀色表示存在 AI 对象 · {{ contextLabel }}
            </p>
          </div>
          <div class="legend">
            <span><i />连续录像</span><span class="event"><i />关联事件</span>
          </div>
        </div>
        <div class="date-strip">
          <button
            v-for="item in days"
            :key="item.value"
            class="date-cell"
            :class="{ active: selectedDate === item.value }"
            @click="selectedDate = item.value"
          >
            <span>{{ item.week }}</span
            ><strong>{{ item.day }}</strong
            ><i v-if="recordDays.has(item.day)" class="slot-led" />
          </button>
        </div>
        <div class="timeline">
          <div
            v-for="hour in [0, 4, 8, 12, 16, 20, 24]"
            :key="hour"
            class="timeline-hour"
          >
            <span>{{ String(hour).padStart(2, "0") }}:00</span>
            <div class="timeline-track" />
          </div>
          <div class="relative mt-2 h-8 rounded bg-slate-100">
            <i
              v-for="item in timeline"
              :key="item.id"
              class="segment absolute top-2 h-4"
              :class="{ event: item.object_count }"
              :style="timelineStyle(item)"
            />
          </div>
        </div>
        <div
          v-if="!loading && !timeline.length"
          class="empty-state empty-action compact"
        >
          <Film /><strong>{{
            channelFilter === "all"
              ? "选择通道后查看时间轴"
              : "所选日期没有录像时间轴"
          }}</strong
          ><span>{{
            channelFilter === "all"
              ? "列表仍可展示全部通道录像，连续播放需要先选定通道。"
              : "可切换相邻日期或返回录像列表核对片段。"
          }}</span>
        </div>
      </article>
      <aside class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">当前检索</h2>
            <p class="card-sub">{{ selectedDate }}</p>
          </div>
          <Film />
        </div>
        <dl class="definition-grid !grid-cols-1">
          <div>
            <dt>通道 / 来源</dt>
            <dd>{{ contextLabel }}</dd>
          </div>
          <div>
            <dt>录像片段</dt>
            <dd>{{ total }} 条 · 已加载 {{ formatBytes(totalSize) }}</dd>
          </div>
          <div>
            <dt>已加载 AI 对象</dt>
            <dd>{{ eventCount }} 个</dd>
          </div>
          <div>
            <dt>月度录像日</dt>
            <dd>{{ recordDays.size }} 天</dd>
          </div>
        </dl>
        <div class="warning-box mt-4">
          <ShieldAlert /><span
            >删除入口未开放：当前后端删除记录时不会同步处理物理文件。</span
          >
        </div>
      </aside>
    </section>
    <section class="card table-card mt-4">
      <div class="toolbar">
        <label class="field"
          ><Search /><input
            v-model="query"
            class="input"
            aria-label="搜索录像"
            placeholder="搜索通道、录像编号或流" /></label
        ><RemoteChannelSelect
          v-model="channelFilter"
          aria-label="按通道筛选"
        />
        <button
          v-if="hasSearch"
          class="btn btn-sm filter-reset"
          type="button"
          @click="resetSearch"
        >
          <X />清除搜索</button
        ><span class="toolbar-spacer" /><span
          class="section-note"
          aria-live="polite"
          >当前显示 {{ filtered.length }} / 已加载
          {{ recordings.length }} 条</span
        >
      </div>
      <p class="table-scroll-hint">左右滑动查看完整录像信息</p>
      <div class="table-wrap">
        <table class="data-table">
          <thead>
            <tr>
              <th>录像</th>
              <th>开始时间</th>
              <th>结束时间</th>
              <th>时长</th>
              <th>AI 对象</th>
              <th>大小</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="record in filtered" :key="record.id">
              <td>
                <div class="row-title">
                  <span class="device-glyph"><Film /></span
                  ><span
                    ><strong>{{
                      channelNames[record.cid || ""] || record.cid || "未知通道"
                    }}</strong
                    ><small
                      >REC-{{ record.id }} · {{ record.app || "—" }}/{{
                        record.stream || "—"
                      }}</small
                    ></span
                  >
                </div>
              </td>
              <td class="mono">{{ formatDate(record.started_at) }}</td>
              <td class="mono">{{ formatDate(record.ended_at) }}</td>
              <td>{{ formatDuration(record.duration) }}</td>
              <td>
                <span :class="record.object_count ? 'status warning' : 'status'"
                  >{{ record.object_count || 0 }} 个</span
                >
              </td>
              <td>{{ formatBytes(record.size) }}</td>
              <td>
                <span
                  class="status"
                  :class="record.delete_flag ? 'warning' : 'online'"
                  >{{ record.delete_flag ? "待清理" : "可播放" }}</span
                >
              </td>
              <td>
                <div class="row-actions">
                  <button class="btn btn-sm" @click="playRecord(record)">
                    <Play />播放</button
                  ><button
                    class="more-btn"
                    :disabled="downloading === record.id"
                    aria-label="下载录像"
                    @click="download(record)"
                  >
                    <LoaderCircle
                      v-if="downloading === record.id"
                      class="animate-spin"
                    /><Download v-else />
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-if="loading" class="empty-state">
          <LoaderCircle
            class="mx-auto mb-3 h-6 w-6 animate-spin"
          />正在加载录像…
        </div>
        <div v-else-if="!filtered.length" class="empty-state empty-action">
          <Film /><strong>{{
            recordings.length
              ? "没有符合搜索条件的录像"
              : "所选日期和通道暂无录像"
          }}</strong
          ><span>{{
            recordings.length
              ? "清除搜索后可恢复当前录像列表。"
              : "可切换日期或通道继续检索。"
          }}</span
          ><button v-if="hasSearch" class="btn" @click="resetSearch">
            清除搜索
          </button>
        </div>
      </div>
      <div class="pagination">
        <span>当前已加载 {{ recordings.length }} / {{ total }} 条录像 · {{ formatBytes(totalSize) }}</span
        ><button
          v-if="canLoadMore"
          class="btn btn-sm"
          type="button"
          :disabled="loadingMore"
          @click="loadMore"
        ><LoaderCircle v-if="loadingMore" class="animate-spin" />{{ loadingMore ? "正在加载…" : "加载更多录像" }}</button
        ><span v-else class="section-note">当前结果已全部加载</span>
      </div>
    </section>
  </main>
</template>
