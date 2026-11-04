<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRoute } from "vue-router";
import {
  ChevronLeft,
  ChevronRight,
  Download,
  Film,
  LoaderCircle,
  Play,
  RefreshCcw,
  Search,
  ShieldAlert,
  X,
} from "@lucide/vue";
import { api, collectPages, errorMessage } from "../services/api";
import type { ApiRecording, MonthlyStats, TimelineRange } from "../types/api";
import { formatBytes, formatDate, formatDuration } from "../utils/format";
import {
  calendarCells,
  localDateInput,
  recordingDateRange,
  recordingTime,
} from "../utils/recordings";
import { useUiStore } from "../stores/ui";
import RemoteChannelSelect from "../components/RemoteChannelSelect.vue";
import ModalDialog from "../components/ModalDialog.vue";

const ui = useUiStore();
const route = useRoute();
const query = ref("");
const channelFilter = ref(
  typeof route.query.channel === "string" && route.query.channel
    ? route.query.channel
    : "all"
);
const today = localDateInput(new Date());
const selectedDate = ref("");
const selectedMonth = ref(today.slice(0, 7));
const scope = ref<"all" | "month" | "day">("all");
const loading = ref(false),
  loadingMore = ref(false),
  monthLoading = ref(false),
  timelineLoading = ref(false);
const loadError = ref(""),
  monthError = ref(""),
  timelineError = ref("");
const recordings = ref<ApiRecording[]>([]);
const channelNames = ref<Record<string, string>>({});
const timeline = ref<TimelineRange[]>([]);
const monthly = ref<MonthlyStats | null>(null);
const total = ref(0),
  page = ref(1);
const downloading = ref<number | null>(null);
const playerRecord = ref<ApiRecording | null>(null);
const playerUrl = ref(""),
  playerError = ref(""),
  playerLoading = ref(false);
const video = ref<HTMLVideoElement>();
const continuous = ref(false),
  queueLoading = ref(false);
const queue = ref<ApiRecording[]>([]);
const PAGE_SIZE = 50;
let loadSequence = 0,
  monthSequence = 0,
  timelineSequence = 0,
  playerSequence = 0;
let disposed = false;
const channelNamePending = new Set<string>();
const contextLabel = computed(() =>
  channelFilter.value === "all"
    ? "全部通道"
    : channelNames.value[channelFilter.value] || channelFilter.value
);
const resultLabel = computed(() =>
  scope.value === "all"
    ? "最新录像 · 全部日期"
    : scope.value === "month"
    ? `${selectedMonth.value} · 整月录像`
    : `${selectedDate.value} · 当天录像`
);
const cells = computed(() => calendarCells(selectedMonth.value));
const recordDays = computed(
  () =>
    new Set(
      (monthly.value?.has_video || "")
        .split("")
        .flatMap((flag, i) =>
          flag === "1"
            ? [`${selectedMonth.value}-${String(i + 1).padStart(2, "0")}`]
            : []
        )
    )
);
const range = computed(() =>
  scope.value === "all"
    ? {}
    : recordingDateRange(
        scope.value === "month" ? selectedMonth.value : selectedDate.value,
        scope.value === "month"
      )
);
const filtered = computed(() =>
  recordings.value.filter((record) =>
    `${channelNames.value[record.cid || ""] || record.cid || ""} REC-${
      record.id
    } ${record.app || ""} ${record.stream || ""}`
      .toLowerCase()
      .includes(query.value.trim().toLowerCase())
  )
);
const totalSize = computed(() =>
  recordings.value.reduce((sum, record) => sum + Number(record.size || 0), 0)
);
const canLoadMore = computed(() => recordings.value.length < total.value);
const showTimeline = computed(
  () => scope.value === "day" && channelFilter.value !== "all"
);
const queueIndex = computed(() =>
  queue.value.findIndex((record) => record.id === playerRecord.value?.id)
);
function playable(record: ApiRecording) {
  return Boolean(record.path && !record.delete_flag);
}
function closePlayer() {
  playerSequence += 1;
  video.value?.pause();
  playerRecord.value = null;
  playerUrl.value = "";
  playerError.value = "";
  playerLoading.value = false;
  continuous.value = false;
  queueLoading.value = false;
  queue.value = [];
}
function latest() {
  query.value = "";
  if (scope.value === "all") void load();
  else scope.value = "all";
}
function selectDay(value: string) {
  selectedDate.value = value;
  selectedMonth.value = value.slice(0, 7);
  scope.value = "day";
}
function changeMonth(offset: number) {
  const month = new Date(`${selectedMonth.value}-01T00:00:00`);
  month.setMonth(month.getMonth() + offset);
  selectedMonth.value = localDateInput(month).slice(0, 7);
  scope.value = "month";
}
function chooseMonth() {
  if (!/^\d{4}-\d{2}$/.test(selectedMonth.value))
    selectedMonth.value = today.slice(0, 7);
  scope.value = "month";
}
async function resolveChannelNames(items: ApiRecording[], selectedId?: string) {
  const ids = [
    ...new Set(
      [...items.map((item) => item.cid), selectedId].filter(
        (id): id is string =>
          Boolean(id) &&
          !channelNames.value[id!] &&
          !channelNamePending.has(id!)
      )
    ),
  ];
  ids.forEach((id) => channelNamePending.add(id));
  const results = await Promise.allSettled(ids.map((id) => api.channel(id)));
  if (disposed) return;
  const resolved: Record<string, string> = {};
  results.forEach((result, index) => {
    if (result.status === "fulfilled")
      resolved[ids[index]] =
        result.value.data.name || result.value.data.channel_id || ids[index];
  });
  channelNames.value = { ...channelNames.value, ...resolved };
  ids.forEach((id) => channelNamePending.delete(id));
}
async function loadMonth() {
  const sequence = ++monthSequence;
  monthly.value = null;
  monthError.value = "";
  monthLoading.value = true;
  const [year, month] = selectedMonth.value.split("-").map(Number);
  try {
    const { data } = await api.monthly({
      year,
      month,
      cid: channelFilter.value === "all" ? undefined : channelFilter.value,
    });
    if (sequence === monthSequence) monthly.value = data;
  } catch (cause) {
    if (sequence === monthSequence)
      monthError.value = errorMessage(cause, "录像日期加载失败");
  } finally {
    if (sequence === monthSequence) monthLoading.value = false;
  }
}
async function loadTimeline() {
  const sequence = ++timelineSequence;
  timeline.value = [];
  timelineError.value = "";
  timelineLoading.value = false;
  if (!showTimeline.value) return;
  timelineLoading.value = true;
  try {
    const { data } = await api.timeline({
      cid: channelFilter.value,
      ...range.value,
    });
    if (sequence === timelineSequence) timeline.value = data.items || [];
  } catch (cause) {
    if (sequence === timelineSequence)
      timelineError.value = errorMessage(cause, "时间轴加载失败");
  } finally {
    if (sequence === timelineSequence) timelineLoading.value = false;
  }
}
async function load(append = false) {
  const sequence = ++loadSequence;
  const requestedPage = append ? page.value + 1 : 1;
  loadError.value = "";
  if (append) loadingMore.value = true;
  else {
    loading.value = true;
    loadingMore.value = false;
    recordings.value = [];
    total.value = 0;
  }
  try {
    const { data } = await api.recordings({
      page: requestedPage,
      size: PAGE_SIZE,
      cid: channelFilter.value === "all" ? undefined : channelFilter.value,
      ...range.value,
    });
    if (sequence !== loadSequence) return;
    const next = data.items || [];
    recordings.value = append
      ? [
          ...new Map(
            [...recordings.value, ...next].map((item) => [item.id, item])
          ).values(),
        ]
      : next;
    total.value = data.total ?? recordings.value.length;
    page.value = requestedPage;
    void resolveChannelNames(
      recordings.value,
      channelFilter.value === "all" ? undefined : channelFilter.value
    );
    // 默认展示所有日期的最新结果，月历自动定位到最新录像所在月。
    if (!append && scope.value === "all" && next[0]?.started_at) {
      const time = recordingTime(next[0].started_at);
      if (Number.isFinite(time))
        selectedMonth.value = localDateInput(new Date(time)).slice(0, 7);
    }
  } catch (cause) {
    if (sequence === loadSequence)
      loadError.value = errorMessage(cause, "录像列表加载失败");
  } finally {
    if (sequence === loadSequence) {
      loading.value = false;
      loadingMore.value = false;
    }
  }
}
function refresh() {
  void load();
  void loadMonth();
  void loadTimeline();
}
function timelineStyle(item: TimelineRange) {
  const { start_ms, end_ms } = recordingDateRange(selectedDate.value);
  const duration = end_ms - start_ms + 1;
  return {
    left: `${Math.max(0, ((item.start_ms - start_ms) / duration) * 100)}%`,
    width: `${Math.max(
      0.3,
      ((Math.min(end_ms, item.end_ms) - Math.max(start_ms, item.start_ms)) /
        duration) *
        100
    )}%`,
  };
}
async function playRecord(record: ApiRecording, keepQueue = false) {
  if (!playable(record))
    return ui.toast("此录像待清理或缺少文件信息，暂时无法播放");
  video.value?.pause();
  const sequence = ++playerSequence;
  if (!keepQueue) {
    continuous.value = false;
    queue.value = [];
    queueLoading.value = false;
  }
  playerRecord.value = record;
  playerUrl.value = "";
  playerError.value = "";
  playerLoading.value = true;
  try {
    // 直接播放支持 Range 的原始 MP4，避免 HLS 时间边界查询遗漏片段。
    await api.checkRecordingMedia(record.path!);
    if (sequence === playerSequence)
      playerUrl.value = api.recordingMediaUrl(record.path!);
  } catch (cause) {
    if (sequence !== playerSequence) return;
    const status = (cause as { response?: { status?: number } })?.response
      ?.status;
    playerError.value =
      status === 404
        ? "录像文件不存在或已被清理，请刷新列表或选择其他片段。"
        : "录像文件暂时无法读取，请重试或下载核对。";
    playerLoading.value = false;
  }
}
async function playDay() {
  if (!showTimeline.value || !timeline.value.some((item) => !item.delete_flag))
    return;
  const sequence = ++playerSequence;
  queueLoading.value = true;
  try {
    const result = await collectPages(api.recordings, {
      cid: channelFilter.value,
      ...range.value,
    });
    if (sequence !== playerSequence) return;
    const items = result.items
      .filter(playable)
      .sort(
        (a, b) =>
          recordingTime(a.started_at) - recordingTime(b.started_at) ||
          a.id - b.id
      );
    if (!items.length) return ui.toast("当前日期没有可播放的录像片段");
    queue.value = items;
    continuous.value = true;
    await playRecord(items[0], true);
  } catch (cause) {
    if (sequence === playerSequence)
      ui.toast(errorMessage(cause, "当天录像加载失败"));
  } finally {
    if (sequence === playerSequence || continuous.value)
      queueLoading.value = false;
  }
}
async function playTimeline(item: TimelineRange) {
  const sequence = ++playerSequence;
  try {
    const { data } = await api.recording(item.id);
    if (sequence === playerSequence) await playRecord(data);
  } catch (cause) {
    if (sequence === playerSequence)
      ui.toast(errorMessage(cause, "无法读取录像片段"));
  }
}
function onEnded() {
  if (!continuous.value) return;
  const next = queue.value[queueIndex.value + 1];
  if (next) void playRecord(next, true);
  else {
    continuous.value = false;
    ui.toast("当天录像已播放完毕");
  }
}
function onMediaError() {
  playerLoading.value = false;
  playerError.value =
    "浏览器无法播放此录像，请重试；若格式不兼容，可下载后使用本地播放器查看。";
}
async function download(record: ApiRecording) {
  downloading.value = record.id;
  try {
    const { data } = await api.downloadRecording(record.id);
    const url = URL.createObjectURL(data),
      anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = `recording-${record.id}.mp4`;
    anchor.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    ui.toast(`录像 REC-${record.id} 已开始下载`);
  } catch (cause) {
    ui.toast(errorMessage(cause, "录像下载失败"));
  } finally {
    downloading.value = null;
  }
}
watch(
  [
    scope,
    () =>
      scope.value === "day"
        ? selectedDate.value
        : scope.value === "month"
        ? selectedMonth.value
        : "",
    channelFilter,
  ],
  () => {
    closePlayer();
    void load();
    void loadTimeline();
  }
);
watch([selectedMonth, channelFilter], () => {
  void loadMonth();
});
watch(channelFilter, () => {
  query.value = "";
});
onMounted(async () => {
  const dateQuery =
    typeof route.query.date === "string" ? route.query.date : "";
  if (
    /^\d{4}-\d{2}-\d{2}$/.test(dateQuery) &&
    Number.isFinite(recordingTime(dateQuery))
  )
    selectDay(dateQuery);
  else if (typeof route.query.event === "string") {
    try {
      const { data } = await api.event(route.query.event);
      if (disposed) return;
      const time = recordingTime(data.started_at || data.created_at);
      if (Number.isFinite(time)) selectDay(localDateInput(new Date(time)));
    } catch {
      ui.toast("事件日期读取失败，已显示该通道最新录像");
    }
  }
  if (scope.value === "all") await load();
  if (!disposed) void loadMonth();
});
onBeforeUnmount(() => {
  disposed = true;
  loadSequence += 1;
  monthSequence += 1;
  timelineSequence += 1;
  closePlayer();
});
</script>

<template>
  <main class="page-content recording-page">
    <header class="page-head">
      <div>
        <h1 class="page-title">录像中心</h1>
        <p class="page-desc">
          先查看最新录像，或从月历中选择有录像的日期。点击片段即可打开弹窗回放。
        </p>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="latest">最新录像</button
        ><button class="btn" :disabled="loading" @click="refresh">
          <RefreshCcw :class="{ 'animate-spin': loading }" />刷新
        </button>
      </div>
    </header>
    <div class="recording-workspace">
      <aside class="card recording-filter" aria-label="录像筛选">
        <div class="recording-filter-section">
          <h2 class="card-title">通道</h2>
          <RemoteChannelSelect
            v-model="channelFilter"
            aria-label="按通道筛选录像"
          />
          <p class="form-help">月历标记随通道筛选更新。</p>
        </div>
        <div
          class="recording-filter-section recording-calendar"
          :aria-busy="monthLoading"
        >
          <h2 class="card-title">录像日期</h2>
          <div class="recording-month-nav">
            <button
              class="icon-btn"
              aria-label="上个月"
              @click="changeMonth(-1)"
            >
              <ChevronLeft /></button
            ><input
              v-model="selectedMonth"
              type="month"
              aria-label="选择录像月份"
              @change="chooseMonth"
            /><button
              class="icon-btn"
              aria-label="下个月"
              @click="changeMonth(1)"
            >
              <ChevronRight />
            </button>
          </div>
          <div class="recording-calendar-week" aria-hidden="true">
            <span
              v-for="day in ['一', '二', '三', '四', '五', '六', '日']"
              :key="day"
              >{{ day }}</span
            >
          </div>
          <div class="recording-calendar-days">
            <template v-for="(day, index) in cells" :key="day || index"
              ><button
                v-if="day"
                class="recording-calendar-day"
                :class="{
                  selected: scope === 'day' && selectedDate === day,
                  'has-recording': recordDays.has(day),
                  today: day === today,
                }"
                :aria-label="`${day}，${
                  monthLoading
                    ? '正在查询'
                    : monthError
                    ? '录像状态未知'
                    : recordDays.has(day)
                    ? '有录像'
                    : '无录像'
                }`"
                :aria-pressed="scope === 'day' && selectedDate === day"
                @click="selectDay(day)"
              >
                {{ Number(day.slice(-2))
                }}<i v-if="recordDays.has(day)" aria-hidden="true" /></button
              ><span v-else
            /></template>
          </div>
          <p class="recording-calendar-note" aria-live="polite">
            <LoaderCircle v-if="monthLoading" class="animate-spin" /><span
              v-if="monthLoading"
              >正在查询录像日期…</span
            ><template v-else-if="!monthError"
              ><i />有录像 · 本月 {{ recordDays.size }} 天</template
            >
          </p>
          <div v-if="monthError" class="recording-inline-error" role="alert">
            <span>{{ monthError }}</span
            ><button class="btn btn-sm" @click="loadMonth">重试日期</button>
          </div>
          <div class="recording-date-actions">
            <button class="btn btn-sm" @click="scope = 'month'">查看整月</button
            ><button class="btn btn-sm" @click="selectDay(today)">今天</button>
          </div>
          <p
            v-if="!monthLoading && !monthError && !recordDays.size"
            class="form-help"
          >
            本月没有录像，可切换月份或查看最新录像。
          </p>
        </div>
      </aside>
      <div class="recording-results">
        <section
          v-if="showTimeline"
          class="card recording-timeline-panel"
          aria-label="当天录像时间轴"
        >
          <div class="recording-result-head">
            <div>
              <h2 class="card-title">{{ selectedDate }} · 录像分布</h2>
              <p class="card-sub">点击蓝色片段回放，琥珀色片段包含 AI 对象。</p>
            </div>
            <button
              class="btn btn-sm"
              :disabled="
                queueLoading ||
                timelineLoading ||
                !timeline.some((item) => !item.delete_flag)
              "
              @click="playDay"
            >
              <LoaderCircle v-if="queueLoading" class="animate-spin" /><Play
                v-else
              />连续播放
            </button>
          </div>
          <div v-if="timelineError" class="recording-inline-error" role="alert">
            {{ timelineError
            }}<button class="btn btn-sm" @click="loadTimeline">
              重试时间轴
            </button>
          </div>
          <div v-else class="recording-time-distribution">
            <div class="recording-time-axis">
              <span
                v-for="hour in ['00:00', '06:00', '12:00', '18:00', '24:00']"
                :key="hour"
                >{{ hour }}</span
              >
            </div>
            <div class="recording-time-track">
              <button
                v-for="item in timeline"
                :key="item.id"
                :style="timelineStyle(item)"
                :class="{
                  event: item.object_count,
                  unavailable: item.delete_flag,
                }"
                :disabled="item.delete_flag"
                :title="`${formatDate(item.start_ms)} — ${formatDate(
                  item.end_ms
                )}`"
                :aria-label="`播放 ${formatDate(item.start_ms)} 的录像`"
                @click="playTimeline(item)"
              />
            </div>
            <p v-if="timelineLoading" class="form-help">正在加载时间轴…</p>
            <p v-else-if="!timeline.length" class="form-help">
              该通道当天没有录像，可选择月历中有标记的日期。
            </p>
          </div>
        </section>
        <section class="card table-card recording-list" :aria-busy="loading">
          <div class="recording-result-head">
            <div>
              <h2 class="card-title">{{ resultLabel }}</h2>
              <p class="card-sub">
                {{ contextLabel }} ·
                {{ loading ? "正在检索…" : `共 ${total} 条，按开始时间倒序`
                }}<span v-if="route.query.event">
                  · 关联事件 #{{ route.query.event }}</span
                >
              </p>
            </div>
            <button v-if="scope !== 'all'" class="btn btn-sm" @click="latest">
              查看最新录像
            </button>
          </div>
          <div class="toolbar">
            <label class="field recording-search"
              ><Search /><input
                v-model="query"
                class="input"
                aria-label="搜索已加载录像"
                placeholder="在已加载结果中搜索通道、编号或流" /></label
            ><button v-if="query" class="btn btn-sm" @click="query = ''">
              <X />清除</button
            ><span class="toolbar-spacer" /><span class="section-note"
              >显示 {{ filtered.length }} / 已加载
              {{ recordings.length }} 条</span
            >
          </div>
          <div
            v-if="loadError"
            class="warning-box recording-load-error"
            role="alert"
          >
            <ShieldAlert /><span>{{ loadError }}</span
            ><button class="btn btn-sm" @click="load()">重试列表</button>
          </div>
          <div class="table-wrap">
            <table class="data-table">
              <thead>
                <tr>
                  <th>通道 / 录像</th>
                  <th>录像时间</th>
                  <th>时长 / 大小</th>
                  <th>AI 对象</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="record in filtered"
                  :key="record.id"
                  :class="{
                    'recording-row-active': playerRecord?.id === record.id,
                  }"
                >
                  <td data-label="通道 / 录像">
                    <div class="row-title">
                      <span class="device-glyph"><Film /></span
                      ><span
                        ><strong>{{
                          channelNames[record.cid || ""] ||
                          record.cid ||
                          "未知通道"
                        }}</strong
                        ><small
                          >REC-{{ record.id }} · {{ record.app || "—" }}/{{
                            record.stream || "—"
                          }}</small
                        ></span
                      >
                    </div>
                  </td>
                  <td data-label="录像时间">
                    <div class="recording-time-cell">
                      <span>{{ formatDate(record.started_at) }}</span
                      ><small>至 {{ formatDate(record.ended_at) }}</small>
                    </div>
                  </td>
                  <td data-label="时长 / 大小">
                    <div class="recording-time-cell">
                      <span>{{ formatDuration(record.duration) }}</span
                      ><small>{{ formatBytes(record.size) }}</small>
                    </div>
                  </td>
                  <td data-label="AI 对象">
                    <span
                      :class="
                        record.object_count ? 'status warning' : 'text-muted'
                      "
                      >{{ record.object_count || 0 }} 个</span
                    >
                  </td>
                  <td data-label="操作">
                    <div class="row-actions">
                      <span v-if="record.delete_flag" class="status warning"
                        >待清理</span
                      ><button
                        class="btn btn-sm"
                        :disabled="!playable(record)"
                        @click="playRecord(record)"
                      >
                        <Play />{{
                          playerRecord?.id === record.id ? "重新播放" : "播放"
                        }}</button
                      ><button
                        class="more-btn"
                        :disabled="
                          downloading === record.id || record.delete_flag
                        "
                        :aria-label="`下载录像 REC-${record.id}`"
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
              <LoaderCircle class="mx-auto mb-3 animate-spin" />正在检索录像…
            </div>
            <div
              v-else-if="!filtered.length && !loadError"
              class="empty-state empty-action"
            >
              <Film /><strong>{{
                query
                  ? "没有符合搜索条件的已加载录像"
                  : scope === "all"
                  ? "当前通道暂无录像"
                  : "所选日期范围暂无录像"
              }}</strong
              ><span>{{
                query
                  ? "可以清除搜索，或加载更多结果后搜索。"
                  : scope === "all"
                  ? "可切换通道，已生成的平台录像会出现在这里。"
                  : "月历中的蓝色标记表示有录像，也可以返回最新录像查看。"
              }}</span
              ><button v-if="query" class="btn" @click="query = ''">
                清除搜索</button
              ><button v-else-if="scope !== 'all'" class="btn" @click="latest">
                查看最新录像
              </button>
            </div>
          </div>
          <div class="pagination">
            <span
              >已加载 {{ recordings.length }} / {{ total }} 条 ·
              {{ formatBytes(totalSize) }}</span
            ><button
              v-if="canLoadMore"
              class="btn btn-sm"
              :disabled="loading || loadingMore"
              @click="load(true)"
            >
              <LoaderCircle v-if="loadingMore" class="animate-spin" />{{
                loadingMore ? "加载中…" : "加载更多"
              }}</button
            ><span v-else-if="!loading && !loadError" class="section-note"
              >已加载全部结果</span
            >
          </div>
        </section>
      </div>
    </div>
    <ModalDialog
      :open="Boolean(playerRecord)"
      :title="`${
        channelNames[playerRecord?.cid || ''] || playerRecord?.cid || '录像'
      } · 录像回放`"
      :description="
        playerRecord
          ? `REC-${playerRecord.id} · ${formatDate(playerRecord.started_at)}${
              continuous
                ? ` · 连续播放 ${queueIndex + 1} / ${queue.length}`
                : ''
            }`
          : ''
      "
      size="wide"
      @close="closePlayer"
    >
      <div v-if="playerRecord" class="recording-player-content">
        <div class="recording-video-stage">
          <video
            v-if="playerUrl"
            :key="playerUrl"
            ref="video"
            :src="playerUrl"
            controls
            autoplay
            playsinline
            preload="metadata"
            aria-label="录像播放器"
            @canplay="playerLoading = false"
            @error="onMediaError"
            @ended="onEnded"
          />
          <div
            v-if="playerLoading"
            class="recording-video-loading"
            role="status"
          >
            <LoaderCircle class="animate-spin" />正在读取录像…
          </div>
        </div>
        <div v-if="playerError" class="recording-player-error" role="alert">
          <ShieldAlert /><span>{{ playerError }}</span
          ><button class="btn btn-sm" @click="playRecord(playerRecord)">
            重试播放</button
          ><button
            class="btn btn-sm"
            :disabled="downloading === playerRecord.id"
            @click="download(playerRecord)"
          >
            <Download />下载录像
          </button>
        </div>
        <p v-else class="recording-player-help">
          {{
            continuous
              ? "按时间顺序播放当天片段，自动跳过无录像的间隔。"
              : "支持暂停、拖动进度和全屏；浏览器未自动播放时，点击播放器内的播放按钮。"
          }}
        </p>
      </div>
    </ModalDialog>
  </main>
</template>

<style scoped>
.recording-workspace {
  display: grid;
  grid-template-columns: 276px minmax(0, 1fr);
  align-items: start;
  gap: 16px;
}
.recording-filter {
  position: sticky;
  top: calc(var(--topbar-height) + 16px);
}
.recording-filter-section {
  padding: 16px;
}
.recording-filter-section + .recording-filter-section {
  border-top: 1px solid var(--line);
}
.recording-filter-section > .card-title {
  margin-bottom: 12px;
}
.recording-filter :deep(.remote-select) {
  width: 100%;
  min-width: 0;
}
.recording-month-nav {
  display: flex;
  align-items: center;
  gap: 4px;
  margin-bottom: 8px;
}
.recording-month-nav input {
  width: 100%;
  min-width: 0;
  height: 32px;
  color: var(--ink);
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: var(--radius-sm);
  padding: 0 8px;
  font-size: 13px;
}
.recording-month-nav .icon-btn {
  flex-shrink: 0;
}
.recording-calendar-week,
.recording-calendar-days {
  display: grid;
  grid-template-columns: repeat(7, minmax(0, 1fr));
  gap: 3px;
  text-align: center;
}
.recording-calendar-week {
  margin-bottom: 4px;
  color: var(--muted);
  font-size: 12px;
  line-height: 28px;
}
.recording-calendar-day {
  position: relative;
  height: 32px;
  padding-bottom: 3px;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--muted);
  font-size: 13px;
  cursor: pointer;
  font-variant-numeric: tabular-nums;
}
.recording-calendar-day.has-recording {
  color: var(--blue);
  background: var(--blue-soft);
  font-weight: 600;
}
.recording-calendar-day.today {
  border-color: var(--line-strong);
}
.recording-calendar-day:hover {
  border-color: var(--blue);
}
.recording-calendar-day.selected {
  color: white;
  background: var(--blue);
  border-color: var(--blue);
}
.recording-calendar-day i {
  position: absolute;
  bottom: 3px;
  left: calc(50% - 2px);
  width: 4px;
  height: 4px;
  border-radius: 50%;
  background: currentColor;
}
.recording-calendar-note {
  display: flex;
  align-items: center;
  gap: 6px;
  min-height: 20px;
  margin: 12px 0;
  color: var(--muted);
  font-size: 12px;
}
.recording-calendar-note > i {
  width: 6px;
  height: 6px;
  background: var(--blue);
  border-radius: 50%;
}
.recording-calendar-note svg {
  width: 14px;
  height: 14px;
}
.recording-date-actions {
  display: flex;
  gap: 8px;
}
.recording-date-actions > .btn {
  flex: 1;
}
.recording-results {
  min-width: 0;
  display: grid;
  gap: 16px;
}
.recording-result-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  flex-wrap: wrap;
  gap: 12px;
  padding: 14px 16px;
}
.recording-result-head > div {
  min-width: 0;
}
.recording-result-head .card-title,
.recording-result-head .card-sub {
  overflow-wrap: anywhere;
}
.recording-search {
  flex: 1;
  max-width: 380px;
}
.recording-search .input {
  width: 100%;
}
.recording-time-cell {
  display: grid;
  gap: 3px;
  font-size: 13px;
  font-variant-numeric: tabular-nums;
}
.recording-time-cell small {
  color: var(--muted);
  font-size: 12px;
}
.recording-row-active {
  background: var(--blue-soft);
}
.recording-list .row-title small {
  max-width: 180px;
  overflow-wrap: anywhere;
  white-space: normal;
}
.recording-list .table-wrap {
  overflow-x: auto;
}
.recording-list .pagination {
  flex-wrap: wrap;
  gap: 8px;
}
.recording-load-error {
  margin: 12px 16px;
}
.recording-inline-error {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  color: var(--red);
  font-size: 12px;
  overflow-wrap: anywhere;
}
.recording-time-distribution {
  padding: 0 16px 16px;
}
.recording-time-axis {
  display: flex;
  justify-content: space-between;
  color: var(--muted);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}
.recording-time-track {
  position: relative;
  height: 32px;
  margin-top: 8px;
  background: var(--panel-3);
  border-radius: var(--radius-sm);
}
.recording-time-track button {
  position: absolute;
  top: 0;
  height: 100%;
  min-width: 4px;
  background: var(--blue);
  border: 0;
  border-radius: 2px;
  cursor: pointer;
}
.recording-time-track button.event {
  background: var(--amber);
}
.recording-time-track button.unavailable {
  background: var(--muted);
}
.recording-video-stage {
  position: relative;
  display: grid;
  place-items: center;
  width: 100%;
  aspect-ratio: 16 / 9;
  max-height: 65dvh;
  background: #0b1422;
}
.recording-video-stage video {
  width: 100%;
  height: 100%;
  max-height: 65dvh;
  object-fit: contain;
}
.recording-video-loading {
  position: absolute;
  inset: 0;
  display: flex;
  justify-content: center;
  align-items: center;
  gap: 8px;
  color: white;
  font-size: 13px;
  pointer-events: none;
}
.recording-video-loading svg {
  width: 18px;
}
.recording-player-help {
  padding: 10px 16px;
  margin: 0;
  color: var(--muted);
  font-size: 12px;
}
.recording-player-error {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  padding: 12px 16px;
  color: var(--red);
  font-size: 13px;
}
.recording-player-error > svg {
  width: 16px;
  flex-shrink: 0;
}
.recording-player-error > span {
  flex: 1;
  min-width: 160px;
}
@media (max-width: 1200px) and (min-width: 901px) {
  .recording-workspace {
    grid-template-columns: 248px minmax(0, 1fr);
    gap: 12px;
  }
}
@media (max-width: 900px) {
  .recording-workspace {
    grid-template-columns: minmax(0, 1fr);
  }
  .recording-filter {
    position: static;
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(260px, 1fr);
  }
  .recording-filter-section + .recording-filter-section {
    border-top: 0;
    border-left: 1px solid var(--line);
  }
}
@media (max-width: 640px) {
  .recording-filter {
    display: block;
  }
  .recording-filter-section + .recording-filter-section {
    border-left: 0;
    border-top: 1px solid var(--line);
  }
  .recording-filter-section {
    padding: 12px;
  }
  .recording-calendar-day {
    height: 44px;
  }
  .recording-month-nav input {
    height: 44px;
    font-size: 16px;
  }
  .recording-date-actions > .btn,
  .recording-month-nav .icon-btn {
    min-height: 44px;
  }
  .recording-search {
    max-width: none;
    width: 100%;
  }
  .recording-result-head {
    padding: 12px;
  }
  .recording-list table,
  .recording-list tbody {
    display: block;
    width: 100%;
    min-width: 0;
  }
  .recording-list thead {
    display: none;
  }
  .recording-list tr {
    display: grid;
    padding: 12px;
    gap: 10px;
    border-top: 1px solid var(--line);
  }
  .recording-list td {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 0;
    border: 0;
    white-space: normal;
  }
  .recording-list td::before {
    content: attr(data-label);
    flex-shrink: 0;
    color: var(--muted);
    font-size: 12px;
  }
  .recording-list td:first-child::before {
    display: none;
  }
  .recording-list .row-actions .btn,
  .recording-list .row-actions .more-btn {
    min-height: 44px;
  }
  .recording-time-cell {
    text-align: right;
  }
}
@media (pointer: coarse) {
  .recording-calendar-day {
    height: 44px;
  }
}
</style>
