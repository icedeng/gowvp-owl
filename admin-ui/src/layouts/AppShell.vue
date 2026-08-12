<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import type { Component } from "vue";
import { RouterLink, RouterView, useRoute } from "vue-router";
import {
  Activity,
  Bell,
  Camera,
  CircleGauge,
  Film,
  Menu,
  PanelLeftClose,
  PanelLeftOpen,
  Radio,
  RadioTower,
  Search,
  Server,
  ShieldCheck,
  Siren,
  SlidersHorizontal,
  UploadCloud,
  Video,
  X,
} from "@lucide/vue";
import CommandPalette from "../components/CommandPalette.vue";
import brandMark from "../assets/brand-mark.svg";
import { useUiStore } from "../stores/ui";
import { useSessionStore } from "../stores/session";
import { api } from "../services/api";
import type { HealthInfo } from "../types/api";
import { formatUptime } from "../utils/format";

const route = useRoute();
const ui = useUiStore();
const session = useSessionStore();
const health = ref<HealthInfo>({});
const eventCount = ref(0);
const eventBadge = computed(() =>
  eventCount.value > 99 ? "99+" : String(eventCount.value)
);
const serviceConnected = computed(() => Boolean(health.value.version));
type NavItem = {
  name: string;
  label: string;
  icon: Component;
  badge?: boolean;
};
const nav: { label: string; items: NavItem[] }[] = [
  {
    label: "工作台",
    items: [{ name: "overview", label: "运行总览", icon: CircleGauge }],
  },
  {
    label: "视频值守",
    items: [
      { name: "live", label: "实时监控", icon: Video },
      { name: "events", label: "事件中心", icon: Siren, badge: true },
      { name: "recordings", label: "录像中心", icon: Film },
    ],
  },
  {
    label: "资源管理",
    items: [
      { name: "devices", label: "设备管理", icon: Camera },
      { name: "channels", label: "通道管理", icon: Radio },
      { name: "push-streams", label: "RTMP 推流", icon: UploadCloud },
      { name: "pull-streams", label: "RTSP 拉流", icon: RadioTower },
    ],
  },
  {
    label: "平台运维",
    items: [
      { name: "media-servers", label: "媒体节点", icon: Server },
      { name: "system-status", label: "系统状态", icon: Activity },
      { name: "sip-settings", label: "SIP 设置", icon: SlidersHorizontal },
      { name: "diagnostics", label: "协议诊断", icon: ShieldCheck },
      { name: "upgrade", label: "版本升级", icon: UploadCloud },
    ],
  },
];
const pageTitle = computed(() => String(route.meta.title || "运行总览"));
const pageGroup = computed(() => String(route.meta.group || "工作台"));

function active(name: string) {
  return (
    route.name === name ||
    (route.name === "device-detail" && name === "devices") ||
    (route.name === "channel-detail" && name === "channels")
  );
}

function onKeydown(event: KeyboardEvent) {
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
    event.preventDefault();
    ui.openCommand();
  }
  if (event.key === "Escape") ui.closeCommand();
}

onMounted(() => {
  window.addEventListener("keydown", onKeydown);
  const now = Date.now();
  Promise.all([
    api.health(),
    api.events({ page: 1, size: 1, start_ms: now - 24 * 3600000, end_ms: now }),
  ])
    .then(([healthResponse, eventResponse]) => {
      health.value = healthResponse.data;
      eventCount.value = eventResponse.data.total || 0;
    })
    .catch(() => undefined);
});
onBeforeUnmount(() => window.removeEventListener("keydown", onKeydown));
</script>

<template>
  <div
    class="app-shell"
    :class="{
      'menu-open': ui.sidebarOpen,
      'sidebar-collapsed': ui.sidebarCollapsed,
    }"
  >
    <button
      v-if="ui.sidebarOpen"
      class="mobile-scrim"
      aria-label="关闭导航"
      @click="ui.closeSidebar"
    />
    <aside class="sidebar" aria-label="主导航">
      <RouterLink
        class="brand"
        to="/overview"
        aria-label="国标视频管理平台"
        @click="ui.closeSidebar"
      >
        <span class="brand-mark"
          ><img :src="brandMark" alt="国标视频管理平台标志"
        /></span>
        <span class="brand-name">国标视频管理平台</span>
      </RouterLink>
      <nav class="nav" aria-label="功能导航">
        <section v-for="group in nav" :key="group.label" class="nav-group">
          <p class="nav-label">{{ group.label }}</p>
          <RouterLink
            v-for="item in group.items"
            :key="item.name"
            class="nav-link"
            :to="{ name: item.name }"
            :class="{ active: active(item.name) }"
            :aria-label="item.label"
            :data-tooltip="item.label"
            @click="ui.closeSidebar"
          >
            <component :is="item.icon" aria-hidden="true" />
            <span>{{ item.label }}</span>
            <span
              v-if="item.badge && eventCount"
              class="nav-badge"
              :aria-label="`过去 24 小时 ${eventCount} 条事件`"
              >{{ eventBadge }}</span
            >
          </RouterLink>
        </section>
      </nav>
    </aside>

    <section class="workspace">
      <header class="topbar">
        <button
          class="icon-btn menu-btn"
          aria-label="打开导航"
          @click="ui.toggleSidebar"
        >
          <X v-if="ui.sidebarOpen" /><Menu v-else />
        </button>
        <button
          class="icon-btn desktop-collapse-btn"
          :aria-label="ui.sidebarCollapsed ? '展开导航' : '收起导航'"
          :title="ui.sidebarCollapsed ? '展开导航' : '收起导航'"
          @click="ui.toggleSidebarCollapsed"
        >
          <PanelLeftOpen v-if="ui.sidebarCollapsed" />
          <PanelLeftClose v-else />
        </button>
        <div class="crumb">
          <small>{{ pageGroup }} /</small><strong>{{ pageTitle }}</strong>
        </div>
        <button
          type="button"
          class="command-trigger"
          aria-label="打开全局搜索"
          @click="ui.openCommand"
        >
          <Search /><span>搜索设备、通道或功能</span
          ><span class="kbd">⌘ K</span>
        </button>
        <RouterLink
          class="service-pill"
          :class="serviceConnected ? 'connected' : 'waiting'"
          to="/system-status"
          aria-live="polite"
        >
          <span class="service-led" aria-hidden="true" />
          <span class="service-copy"
            ><strong>{{
              serviceConnected ? "核心服务已连接" : "服务连接中"
            }}</strong
            ><small
              >{{ health.version || "—" }} ·
              {{ formatUptime(health.start_at) }}</small
            ></span
          >
        </RouterLink>
        <div class="top-actions">
          <button
            class="icon-btn notification-btn relative"
            aria-label="通知"
            @click="ui.toast(`过去 24 小时共 ${eventCount} 条事件`)"
          >
            <Bell /><span v-if="eventCount" class="notice-count">{{
              eventBadge
            }}</span>
          </button>
          <RouterLink class="user-chip" to="/account">
            <strong>{{ session.user || "管理员" }}</strong>
          </RouterLink>
        </div>
      </header>
      <RouterView />
    </section>

    <CommandPalette />
    <Transition name="toast">
      <div v-if="ui.isToastVisible" class="toast show" role="status">
        <span class="toast-mark"><ShieldCheck class="h-3.5 w-3.5" /></span>
        <span>{{ ui.toastMessage }}</span>
      </div>
    </Transition>
  </div>
</template>
