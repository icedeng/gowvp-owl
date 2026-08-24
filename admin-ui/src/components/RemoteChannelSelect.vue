<script setup lang="ts">
import {
  computed,
  nextTick,
  onBeforeUnmount,
  onMounted,
  ref,
  shallowRef,
  useId,
  watch,
} from "vue";
import { Check, ChevronDown, LoaderCircle, Search } from "@lucide/vue";
import { api, errorMessage } from "../services/api";
import type { ApiChannel } from "../types/api";

const props = withDefaults(
  defineProps<{
    modelValue: string;
    ariaLabel?: string;
    allLabel?: string;
  }>(),
  {
    ariaLabel: "选择通道",
    allLabel: "全部通道",
  }
);

const emit = defineEmits<{
  "update:modelValue": [value: string];
  change: [value: string];
}>();

const root = ref<HTMLElement | null>(null);
const trigger = ref<HTMLButtonElement | null>(null);
const searchInput = ref<HTMLInputElement | null>(null);
const open = ref(false);
const loading = ref(false);
const loadingMore = ref(false);
const loadError = ref("");
const query = ref("");
const options = shallowRef<ApiChannel[]>([]);
const selectedCache = shallowRef<ApiChannel | null>(null);
const total = ref(0);
const page = ref(1);
const activeIndex = ref(-1);
const pageSize = 30;
const baseId = useId();
const listboxId = `${baseId}-listbox`;
let searchTimer: number | undefined;
let loadSequence = 0;

const selected = computed(() =>
  options.value.find((item) => item.id === props.modelValue) ||
  (selectedCache.value?.id === props.modelValue ? selectedCache.value : undefined)
);
const selectedLabel = computed(() => {
  if (props.modelValue === "all") return props.allLabel;
  const item = selected.value;
  return item?.name || item?.channel_id || item?.id || props.modelValue || props.allLabel;
});
const canLoadMore = computed(() => page.value * pageSize < total.value);

function optionLabel(item: ApiChannel) {
  return item.name || item.channel_id || item.id;
}

function mergeOptions(items: ApiChannel[], reset: boolean) {
  const selectedItem = selected.value;
  const source = reset ? items : [...options.value, ...items];
  if (selectedItem && !source.some((item) => item.id === selectedItem.id)) {
    source.unshift(selectedItem);
  }
  options.value = [
    ...new Map(source.map((item) => [item.id, item])).values(),
  ];
}

async function load(reset = true) {
  const sequence = ++loadSequence;
  const requestedPage = reset ? 1 : page.value + 1;
  if (reset) loading.value = true;
  else loadingMore.value = true;
  loadError.value = "";
  try {
    const response = await api.channels({
      page: requestedPage,
      size: pageSize,
      key: query.value.trim() || undefined,
    });
    if (sequence !== loadSequence) return;
    mergeOptions(response.data?.items || [], reset);
    total.value = Number(response.data?.total ?? options.value.length);
    page.value = requestedPage;
    activeIndex.value = options.value.length ? 0 : -1;
  } catch (cause) {
    if (sequence === loadSequence)
      loadError.value = errorMessage(cause, "通道选项加载失败");
  } finally {
    if (sequence === loadSequence) {
      loading.value = false;
      loadingMore.value = false;
    }
  }
}

async function ensureSelected(value: string) {
  if (!value || value === "all") {
    selectedCache.value = null;
    return;
  }
  const existing = options.value.find((item) => item.id === value);
  if (existing) {
    selectedCache.value = existing;
    return;
  }
  try {
    const { data } = await api.channel(value);
    selectedCache.value = data;
    options.value = [
      data,
      ...options.value.filter((item) => item.id !== data.id),
    ];
  } catch {
    // 路由中的通道可能已被删除，保留原始 ID 便于用户识别筛选上下文。
  }
}

async function openPicker() {
  open.value = true;
  await load();
  await nextTick();
  searchInput.value?.focus();
}

function closePicker(restoreFocus = false) {
  open.value = false;
  window.clearTimeout(searchTimer);
  loadSequence += 1;
  loading.value = false;
  loadingMore.value = false;
  query.value = "";
  activeIndex.value = -1;
  if (restoreFocus) nextTick(() => trigger.value?.focus());
}

function selectValue(value: string) {
  selectedCache.value =
    value === "all"
      ? null
      : options.value.find((item) => item.id === value) || selectedCache.value;
  emit("update:modelValue", value);
  emit("change", value);
  closePicker(true);
}

function onSearchKeydown(event: KeyboardEvent) {
  if (event.key === "Escape") {
    event.preventDefault();
    closePicker(true);
    return;
  }
  if (event.key === "ArrowDown" || event.key === "ArrowUp") {
    event.preventDefault();
    if (!options.value.length) return;
    const direction = event.key === "ArrowDown" ? 1 : -1;
    activeIndex.value =
      (activeIndex.value + direction + options.value.length) %
      options.value.length;
    return;
  }
  if (event.key === "Home") {
    event.preventDefault();
    activeIndex.value = options.value.length ? 0 : -1;
    return;
  }
  if (event.key === "End") {
    event.preventDefault();
    activeIndex.value = options.value.length - 1;
    return;
  }
  if (event.key === "Enter" && activeIndex.value >= 0) {
    event.preventDefault();
    selectValue(options.value[activeIndex.value].id);
  }
}

function onDocumentPointerDown(event: PointerEvent) {
  if (open.value && !root.value?.contains(event.target as Node)) closePicker();
}

watch(query, () => {
  if (!open.value) return;
  window.clearTimeout(searchTimer);
  loadSequence += 1;
  options.value = [];
  total.value = 0;
  activeIndex.value = -1;
  loading.value = true;
  searchTimer = window.setTimeout(() => void load(), 280);
});
watch(
  () => props.modelValue,
  (value) => void ensureSelected(value),
  { immediate: true }
);

onMounted(() => document.addEventListener("pointerdown", onDocumentPointerDown));
onBeforeUnmount(() => {
  window.clearTimeout(searchTimer);
  document.removeEventListener("pointerdown", onDocumentPointerDown);
});
</script>

<template>
  <div ref="root" class="remote-select">
    <button
      ref="trigger"
      type="button"
      class="select remote-select-trigger"
      :aria-label="ariaLabel"
      aria-haspopup="listbox"
      :aria-expanded="open"
      :aria-controls="listboxId"
      @click="open ? closePicker() : openPicker()"
    >
      <span>{{ selectedLabel }}</span><ChevronDown />
    </button>
    <div v-if="open" class="remote-select-popover">
      <label class="field remote-select-search">
        <Search />
        <input
          ref="searchInput"
          v-model="query"
          class="input"
          role="combobox"
          aria-autocomplete="list"
          :aria-label="`搜索${ariaLabel}`"
          :aria-controls="listboxId"
          :aria-expanded="open"
          :aria-activedescendant="activeIndex >= 0 ? `${baseId}-option-${activeIndex}` : undefined"
          placeholder="输入通道名称或编号"
          @keydown="onSearchKeydown"
        />
      </label>
      <div :id="listboxId" class="remote-select-options" role="listbox" :aria-label="ariaLabel">
        <button
          v-if="allLabel && !query"
          type="button"
          class="remote-select-option"
          role="option"
          :aria-selected="modelValue === 'all'"
          @click="selectValue('all')"
        >
          <span><strong>{{ allLabel }}</strong><small>不限制通道</small></span>
          <Check v-if="modelValue === 'all'" />
        </button>
        <button
          v-for="(item, index) in options"
          :id="`${baseId}-option-${index}`"
          :key="item.id"
          type="button"
          class="remote-select-option"
          :class="{ active: index === activeIndex }"
          role="option"
          :aria-selected="modelValue === item.id"
          @pointerenter="activeIndex = index"
          @click="selectValue(item.id)"
        >
          <span><strong>{{ optionLabel(item) }}</strong><small class="mono">{{ item.channel_id || item.id }}</small></span>
          <Check v-if="modelValue === item.id" />
        </button>
        <div v-if="loading" class="remote-select-state" aria-live="polite">
          <LoaderCircle class="animate-spin" />正在搜索通道…
        </div>
        <div v-else-if="loadError" class="remote-select-state error" role="alert">
          <span>{{ loadError }}</span><button type="button" @click="load()">重试</button>
        </div>
        <div v-else-if="!options.length" class="remote-select-state">没有匹配的通道</div>
      </div>
      <button
        v-if="canLoadMore && !loading"
        type="button"
        class="remote-select-more"
        :disabled="loadingMore"
        @click="load(false)"
      >
        <LoaderCircle v-if="loadingMore" class="animate-spin" />
        {{ loadingMore ? "正在加载…" : `加载更多（${Math.min(page * pageSize, total)} / ${total}）` }}
      </button>
    </div>
  </div>
</template>
