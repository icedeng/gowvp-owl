<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import {
  CheckCircle2,
  DownloadCloud,
  GitBranch,
  Info,
  LoaderCircle,
  RefreshCcw,
  RotateCcw,
  ShieldAlert,
  ShieldCheck,
} from "@lucide/vue";
import { api, apiUrl, errorMessage } from "../services/api";
import type { HealthInfo, VersionCheck } from "../types/api";
import { formatDate, formatUptime } from "../utils/format";
import { useUiStore } from "../stores/ui";
import ModalDialog from "../components/ModalDialog.vue";

const ui = useUiStore();
const health = ref<HealthInfo>({});
const version = ref<VersionCheck>({});
const loading = ref(false);
const loadError = ref("");
const upgrading = ref(false);
const progress = ref(0);
const upgradeMessage = ref("等待开始");
const confirmOpen = ref(false);
const releaseNotes = computed(() =>
  (version.value.description || "").split("\n").filter(Boolean).slice(0, 6)
);

async function check() {
  loading.value = true;
  loadError.value = "";
  try {
    const [healthResponse, versionResponse] = await Promise.allSettled([
      api.health(),
      api.versionCheck(),
    ]);
    if (versionResponse.status === "rejected") throw versionResponse.reason;
    version.value = versionResponse.value.data || {};
    if (healthResponse.status === "fulfilled") health.value = healthResponse.value.data || {};
    else loadError.value = `版本信息已加载，当前服务状态暂不可用：${errorMessage(healthResponse.reason)}`;
    ui.toast(
      version.value.has_new_version
        ? `发现新版本 ${version.value.new_version}`
        : "当前已是最新版本"
    );
  } catch (cause) {
    loadError.value = errorMessage(cause, "版本检查失败");
  } finally {
    loading.value = false;
  }
}

async function start() {
  if (!version.value.has_new_version || upgrading.value) return;
  confirmOpen.value = false;
  upgrading.value = true;
  progress.value = 0;
  upgradeMessage.value = "正在建立升级连接";
  try {
    const response = await fetch(apiUrl("/app/upgrade"), {
      method: "POST",
      headers: {
        Authorization: `Bearer ${localStorage.getItem("owl-token") || ""}`,
      },
    });
    if (!response.ok || !response.body)
      throw new Error(`升级请求失败（HTTP ${response.status}）`);
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    let event = "";
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split("\n");
      buffer = lines.pop() || "";
      for (const line of lines) {
        if (line.startsWith("event:")) event = line.slice(6).trim();
        if (line.startsWith("data:")) {
          const data = JSON.parse(line.slice(5).trim()) as {
            percent?: number;
            msg?: string;
          };
          if (typeof data.percent === "number") progress.value = data.percent;
          if (data.msg) upgradeMessage.value = data.msg;
          if (event === "error") throw new Error(data.msg || "升级下载失败");
          if (event === "complete") progress.value = 100;
        }
      }
    }
    ui.toast(upgradeMessage.value || "升级包下载完成，请手动重启服务");
  } catch (cause) {
    upgradeMessage.value = errorMessage(cause, "升级下载失败");
    ui.toast(upgradeMessage.value);
  } finally {
    upgrading.value = false;
  }
}

function requestStart() {
  if (!version.value.has_new_version || upgrading.value) return;
  confirmOpen.value = true;
}

onMounted(check);
</script>

<template>
  <main class="page-content">
    <header class="page-head">
      <div>
        <h1 class="page-title">版本升级</h1>
        <p class="page-desc">
          检查最新版本、查看更新说明，并通过 SSE 观察升级包真实下载进度。
        </p>
      </div>
      <button class="btn" :disabled="loading" @click="check">
        <RefreshCcw :class="{ 'animate-spin': loading }" />检查更新
      </button>
    </header>
    <div v-if="loadError" class="warning-box mb-4">
      <ShieldAlert /><span>{{ loadError }}</span
      ><button class="btn btn-sm ml-auto" @click="check">重试</button>
    </div>
    <section class="grid two-col">
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">当前版本</h2>
            <p class="card-sub">Owl Server</p>
          </div>
          <span class="status" :class="health.version ? 'online' : 'offline'">{{
            health.version ? "运行中" : "状态未知"
          }}</span>
        </div>
        <div class="details-identity">
          <span class="details-icon"><ShieldCheck /></span>
          <div>
            <h1>{{ health.version || version.current_version || "—" }}</h1>
            <p>
              Build {{ health.git_hash || "—" }} ·
              {{ health.git_branch || "—" }}
            </p>
          </div>
        </div>
        <dl class="definition-grid mt-4">
          <div>
            <dt>启动时间</dt>
            <dd>{{ formatDate(health.start_at) }}</dd>
          </div>
          <div>
            <dt>运行时间</dt>
            <dd>{{ formatUptime(health.start_at) }}</dd>
          </div>
        </dl>
      </article>
      <article class="card card-pad">
        <div class="card-head">
          <div>
            <h2 class="card-title">可用更新</h2>
            <p class="card-sub">
              {{
                version.has_new_version
                  ? `release / ${version.new_version}`
                  : "暂无新版本"
              }}
            </p>
          </div>
          <GitBranch />
        </div>
        <div class="button-row">
          <span
            class="protocol-tag"
            :class="version.has_new_version ? 'blue' : ''"
            >{{ version.has_new_version ? "NEW" : "LATEST" }}</span
          ><strong>{{
            version.new_version ||
            version.current_version ||
            health.version ||
            "—"
          }}</strong>
        </div>
        <ul
          v-if="releaseNotes.length"
          class="mt-4 space-y-2 pl-5 text-[10px] text-slate-600"
        >
          <li v-for="item in releaseNotes" :key="item">{{ item }}</li>
        </ul>
        <div v-else class="read-only mt-4">
          {{ loading ? "正在检查版本…" : "版本服务未返回更新说明。" }}
        </div>
        <button
          class="btn btn-primary mt-4"
          :disabled="upgrading || !version.has_new_version"
          @click="requestStart"
        >
          <LoaderCircle v-if="upgrading" class="animate-spin" /><DownloadCloud
            v-else
          />{{
            upgrading
              ? "正在下载…"
              : version.has_new_version
              ? "下载升级包"
              : "无需升级"
          }}
        </button>
      </article>
    </section>
    <section class="card card-pad mt-4">
      <div class="card-head">
        <div>
          <h2 class="card-title">升级进度</h2>
          <p class="card-sub">来自后端 SSE 事件流</p>
        </div>
        <span
          class="status"
          :class="progress === 100 ? 'online' : progress ? 'info' : ''"
          >{{
            progress === 100
              ? "下载完成"
              : progress
              ? `${progress}%`
              : "等待开始"
          }}</span
        >
      </div>
      <div class="progress" style="height: 10px">
        <i :style="{ width: `${progress}%` }" />
      </div>
      <div class="step-list mt-4">
        <div class="step-item">
          <span class="step-index"><CheckCircle2 /></span
          ><span
            ><strong>检查可用版本</strong
            ><small
              >{{ version.current_version || health.version || "—" }} →
              {{ version.new_version || "—" }}</small
            ></span
          ><span
            class="status"
            :class="version.has_new_version ? 'online' : ''"
            >{{ version.has_new_version ? "可升级" : "最新" }}</span
          >
        </div>
        <div class="step-item">
          <span class="step-index"><DownloadCloud /></span
          ><span
            ><strong>下载升级包</strong
            ><small>{{ upgradeMessage }}</small></span
          ><span
            class="status"
            :class="progress === 100 ? 'online' : progress ? 'info' : ''"
            >{{
              progress === 100 ? "完成" : progress ? "进行中" : "等待"
            }}</span
          >
        </div>
        <div class="step-item">
          <span class="step-index"><RotateCcw /></span
          ><span
            ><strong>手动重启服务</strong
            ><small>下载完成后必须由运维人员确认并重启</small></span
          ><span class="status">待操作</span>
        </div>
      </div>
      <div class="warning-box mt-4">
        <Info /><span
          >点击升级会真实下载升级包，但不会自动重启服务；执行前请确认部署环境与维护窗口。</span
        >
      </div>
    </section>
    <ModalDialog
      :open="confirmOpen"
      title="确认下载升级包"
      description="升级下载会真实写入服务端环境，完成后需要运维人员手动重启。"
      @close="confirmOpen = false"
    >
      <div class="warning-box">
        <ShieldAlert />
        <span>
          将从
          <strong>{{ version.current_version || health.version || "当前版本" }}</strong>
          下载到
          <strong>{{ version.new_version }}</strong>。请先确认当前处于维护窗口。
        </span>
      </div>
      <template #footer>
        <button class="btn" @click="confirmOpen = false">取消</button>
        <button class="btn btn-danger" @click="start">
          <DownloadCloud />确认下载
        </button>
      </template>
    </ModalDialog>
  </main>
</template>
