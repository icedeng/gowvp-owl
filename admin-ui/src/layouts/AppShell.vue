<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from "vue";
import type { Component } from "vue";
import { RouterLink, RouterView, useRoute, useRouter } from "vue-router";
import {
  Activity,
  Bell,
  Camera,
  ChevronDown,
  CircleGauge,
  Film,
  KeyRound,
  LogOut,
  Menu,
  MonitorCog,
  PanelLeftClose,
  PanelLeftOpen,
  RadioTower,
  Search,
  Server,
  ShieldCheck,
  Siren,
  SlidersHorizontal,
  UploadCloud,
  Video,
  Truck,
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
const router = useRouter();
const ui = useUiStore();
const session = useSessionStore();
const health = ref<HealthInfo>({});
const eventCount = ref(0);
const userMenuOpen = ref(false);
const userMenu = ref<HTMLElement | null>(null);
const userMenuTrigger = ref<HTMLButtonElement | null>(null);
const mobileMenuButton = ref<HTMLButtonElement | null>(null);
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
      { name: "devices", label: "国标设备", icon: Camera },
      { name: "transport-devices", label: "部标设备", icon: Truck },
      { name: "push-streams", label: "RTMP 推流", icon: UploadCloud },
      { name: "pull-streams", label: "RTSP 拉流", icon: RadioTower },
    ],
  },
  {
    label: "平台运维",
    items: [
      { name: "media-servers", label: "媒体节点", icon: Server },
      { name: "system-status", label: "系统状态", icon: Activity },
      { name: "player-settings", label: "播放器设置", icon: MonitorCog },
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
    (route.name === "transport-device-detail" && name === "transport-devices") ||
    (route.name === "channel-detail" && name === "devices")
  );
}

function onKeydown(event: KeyboardEvent) {
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
    event.preventDefault();
    ui.openCommand();
  }
  if (event.key === "Escape") {
    ui.closeCommand();
    if (ui.sidebarOpen) {
      closeSidebarAndRestoreFocus();
    }
    if (userMenuOpen.value) {
      userMenuOpen.value = false;
      userMenuTrigger.value?.focus();
    }
  }
}

function onDocumentPointerDown(event: PointerEvent) {
  if (userMenuOpen.value && !userMenu.value?.contains(event.target as Node)) {
    userMenuOpen.value = false;
  }
}

function toggleUserMenu() {
  userMenuOpen.value = !userMenuOpen.value;
}

function userMenuItems() {
  return [
    ...(userMenu.value?.querySelectorAll<HTMLElement>("[role='menuitem']") || []),
  ];
}

async function onUserMenuKeydown(event: KeyboardEvent) {
  if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
  event.preventDefault();
  if (!userMenuOpen.value) {
    userMenuOpen.value = true;
    await nextTick();
  }
  const items = userMenuItems();
  if (!items.length) return;
  if (event.key === "Home") return items[0].focus();
  if (event.key === "End") return items[items.length - 1].focus();
  const current = items.indexOf(document.activeElement as HTMLElement);
  const next = event.key === "ArrowDown"
    ? (current + 1 + items.length) % items.length
    : (current - 1 + items.length) % items.length;
  items[next].focus();
}

function closeSidebarAndRestoreFocus() {
  ui.closeSidebar();
  mobileMenuButton.value?.focus();
}

async function logout() {
  userMenuOpen.value = false;
  session.signOut();
  await router.replace("/login");
}

onMounted(() => {
  window.addEventListener("keydown", onKeydown);
  document.addEventListener("pointerdown", onDocumentPointerDown);
  const now = Date.now();
  Promise.allSettled([
    api.health(),
    api.events({ page: 1, size: 1, start_ms: now - 24 * 3600000, end_ms: now }),
  ])
    .then(([healthResponse, eventResponse]) => {
      if (healthResponse.status === "fulfilled") health.value = healthResponse.value.data || {};
      if (eventResponse.status === "fulfilled") eventCount.value = eventResponse.value.data?.total || 0;
    })
});
onBeforeUnmount(() => {
  window.removeEventListener("keydown", onKeydown);
  document.removeEventListener("pointerdown", onDocumentPointerDown);
});
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
      @click="closeSidebarAndRestoreFocus"
    />
    <aside id="app-sidebar" class="sidebar" aria-label="主导航">
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
          ref="mobileMenuButton"
          class="icon-btn menu-btn"
          :aria-label="ui.sidebarOpen ? '关闭导航' : '打开导航'"
          aria-controls="app-sidebar"
          :aria-expanded="ui.sidebarOpen"
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
          :aria-label="`${serviceConnected ? '核心服务已连接' : '服务连接中'}，版本 ${health.version || '未知'}，${formatUptime(health.start_at)}`"
        >
          <span class="service-led" aria-hidden="true" />
          <strong>{{ serviceConnected ? "服务正常" : "连接中" }}</strong>
          <span class="service-meta" aria-hidden="true"
            >{{ health.version || "—" }} · {{ formatUptime(health.start_at) }}</span
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
          <div ref="userMenu" class="user-menu-wrap" @keydown="onUserMenuKeydown">
            <button
              ref="userMenuTrigger"
              type="button"
              class="user-chip"
              aria-haspopup="menu"
              aria-controls="user-menu"
              :aria-expanded="userMenuOpen"
              @click="toggleUserMenu"
            >
              <strong>{{ session.user || "管理员" }}</strong>
              <ChevronDown :class="{ rotated: userMenuOpen }" />
            </button>
            <Transition name="user-menu">
              <div
                v-if="userMenuOpen"
                id="user-menu"
                class="user-menu"
                role="menu"
                aria-label="用户菜单"
              >
                <div class="user-menu-identity">
                  <strong>系统管理员</strong>
                  <span>{{ session.user || "管理员" }}</span>
                </div>
                <div class="user-menu-divider" />
                <RouterLink
                  class="user-menu-item"
                  role="menuitem"
                  to="/account"
                  @click="userMenuOpen = false"
                >
                  <KeyRound /><span>修改密码</span>
                </RouterLink>
                <button
                  type="button"
                  class="user-menu-item danger"
                  role="menuitem"
                  @click="logout"
                >
                  <LogOut /><span>退出登录</span>
                </button>
              </div>
            </Transition>
          </div>
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
