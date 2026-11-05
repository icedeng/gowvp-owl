<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import { CheckCircle2, Clock3, LoaderCircle, RefreshCcw, ShieldAlert, UploadCloud } from '@lucide/vue';
import { api, errorMessage } from '../services/api';
import { http } from '../services/http';
import type { HealthInfo } from '../types/api';
import { formatDate } from '../utils/format';
import ModalDialog from '../components/ModalDialog.vue';

interface Release {
  id: string; version: string; architecture: string; image_size: number;
  from_versions: string[]; notes: string;
  replaces_version?: string;
}
interface Task {
  id: string; version: string; previous_version: string; phase: string; message: string;
  backup?: string; updated_at: string;
  events: { time: string; phase: string; message: string }[];
}
interface UpgradeStatus { available: boolean; source: string; releases: Release[]; task?: Task }
const health = ref<HealthInfo>({});
const status = ref<UpgradeStatus>();
const task = ref<Task>();
const loading = ref(false);
const unavailable = ref('');
const actionError = ref('');
const fileInput = ref<HTMLInputElement>();
const file = ref<File>();
const uploading = ref(false);
const uploadPercent = ref(0);
const uploadMessage = ref('');
const selected = ref('');
const confirming = ref(false);
const starting = ref(false);
const confirmOpen = ref(false);
const activePhases = ['queued', 'validating', 'backing_up', 'importing', 'installing', 'checking', 'rolling_back'];
const busy = computed(() => !!task.value && activePhases.includes(task.value.phase));
const blocked = computed(() => busy.value || task.value?.phase === 'rollback_failed');
const releases = computed(() => status.value?.releases || []);
const chosen = computed(() => releases.value.find(item => item.id === selected.value));
const canInstall = (release: Release) => {
  const current = health.value.version || '';
  if (!release.from_versions.includes(current)) return false;
  if (release.replaces_version === current && release.from_versions.length === 1 && release.version !== current) return true;
  const parts = (version: string) => version.replace(/^v/, '').split('-')[0]!.split('.').map(Number);
  const target = parts(release.version), running = parts(current);
  for (let i = 0; i < 3; i++) {
    if (target[i] !== running[i]) return (target[i] || 0) > (running[i] || 0);
  }
  return false;
};
const phaseText: Record<string, string> = { queued: '等待执行', validating: '校验', backing_up: '备份', importing: '准备镜像', installing: '安装', checking: '健康检查', rolling_back: '正在回滚', succeeded: '升级成功', rolled_back: '已回滚', rollback_failed: '需人工恢复', failed: '安装前失败' };
let timer: ReturnType<typeof setTimeout> | undefined;
let disposed = false;
let polling = false;
const uploadAbort = new AbortController();
const statusAbort = new AbortController();

async function check() {
  if (polling || disposed) return;
  polling = true;
  loading.value = true;
  try {
    const [agent, service] = await Promise.allSettled([
      http.get<UpgradeStatus>('/app/upgrade/status', { signal: statusAbort.signal, timeout: 10000 }),
      api.health(),
    ]);
    if (disposed) return;
    if (service.status === 'fulfilled') health.value = service.value.data || {};
    if (agent.status === 'rejected') throw agent.reason;
    status.value = agent.value.data;
    task.value = agent.value.data.task;
    unavailable.value = '';
    if (!selected.value && releases.value.length) selected.value = releases.value.find(canInstall)?.id || releases.value[0]?.id || '';
  } catch (error) {
    if (!disposed) unavailable.value = busy.value ? '应用正在切换版本，暂时无法连接。页面会自动重试并恢复执行记录。' : errorMessage(error, '升级服务暂不可用，请稍后刷新或联系运维。');
  } finally {
    polling = false;
    loading.value = false;
    if (!disposed) { if (timer) clearTimeout(timer); timer = setTimeout(check, busy.value || unavailable.value ? 3000 : 10000); }
  }
}
function chooseFile(event: Event) {
  file.value = (event.target as HTMLInputElement).files?.[0];
  actionError.value = ''; uploadMessage.value = ''; uploadPercent.value = 0;
}
async function upload() {
  if (!file.value || uploading.value || blocked.value) return;
  if (!file.value.name.endsWith('.owl-release.tar.gz')) { actionError.value = '请选择本平台签发的 .owl-release.tar.gz 发布包。'; return; }
  if (!file.value.size || file.value.size > 2 * 1024 ** 3) { actionError.value = '发布包为空或超过 2GB。'; return; }
  uploading.value = true; actionError.value = ''; uploadPercent.value = 0;
  uploadMessage.value = '正在上传发布包…';
  try {
    const response = await http.post<Release>('/app/upgrade/packages', file.value, {
      headers: { 'Content-Type': 'application/octet-stream' }, timeout: 20 * 60 * 1000,
      signal: uploadAbort.signal,
      onUploadProgress(event) {
        uploadPercent.value = Math.min(100, Math.round(event.loaded * 100 / (event.total || file.value!.size)));
        if (uploadPercent.value === 100) uploadMessage.value = '上传完成，正在校验发布签名和镜像完整性…';
      },
    });
    if (disposed) return;
    selected.value = response.data.id;
    uploadMessage.value = `发布包 ${response.data.version} 校验通过，可在下方确认安装。`;
    file.value = undefined;
    if (fileInput.value) fileInput.value.value = '';
    await check();
  } catch (error) { if (!disposed) actionError.value = errorMessage(error, '上传或校验失败，请重试。'); }
  finally { uploading.value = false; }
}
async function install() {
  if (!chosen.value || blocked.value || starting.value || !confirming.value) return;
  starting.value = true; actionError.value = '';
  try {
    const response = await http.post<Task>('/app/upgrade/install', { package_id: chosen.value.id });
    task.value = response.data; confirmOpen.value = false; confirming.value = false;
    await check();
  } catch (error) { actionError.value = errorMessage(error, '无法创建升级任务，请刷新状态后重试。'); await check(); }
  finally { starting.value = false; }
}
onMounted(check);
function reloadPage() { window.location.reload(); }
onBeforeUnmount(() => { disposed = true; if (timer) clearTimeout(timer); uploadAbort.abort(); statusAbort.abort(); });
</script>

<template>
  <main class="page-content upgrade-page">
    <header class="page-head">
      <div><h1 class="page-title">版本升级</h1><p class="page-desc">上传本平台签名发布包，校验通过后安装；失败自动恢复旧版本。</p></div>
      <button class="btn" :disabled="loading" @click="check"><RefreshCcw :class="{ 'animate-spin': loading }" />刷新状态</button>
    </header>
    <div v-if="unavailable" class="warning-box mb-4" role="status"><ShieldAlert /><span>{{ unavailable }}</span></div>
    <div v-if="actionError" class="warning-box mb-4" role="alert"><ShieldAlert /><span>{{ actionError }}</span></div>
    <section class="grid equal-col mb-4">
      <article class="card card-pad">
        <div class="card-head"><h2 class="card-title">当前版本</h2><span class="status" :class="health.version ? 'online' : ''">{{ health.version ? '运行中' : '连接中' }}</span></div>
        <strong class="upgrade-version">{{ health.version || '—' }}</strong>
        <dl class="definition-grid mt-4"><div><dt>启动时间</dt><dd>{{ formatDate(health.start_at) }}</dd></div><div><dt>发布来源</dt><dd>{{ status?.source || '本平台签名发布库' }}</dd></div></dl>
        <p class="card-sub mt-4">仅更新应用服务，保留数据库、媒体服务和业务配置；含数据库迁移的版本由运维单独发布。</p>
      </article>
      <article class="card card-pad">
        <div class="card-head"><h2 class="card-title">上传发布包</h2><UploadCloud aria-hidden="true" /></div>
        <p class="card-sub">支持 .owl-release.tar.gz，最大 2GB。上传后会验证发布签名、镜像摘要和版本兼容范围。</p>
        <label class="upgrade-file"><span>选择发布包</span><input ref="fileInput" type="file" accept=".gz" :disabled="uploading || blocked || !status || !!unavailable" @change="chooseFile" /></label>
        <div class="button-row"><button class="btn" :disabled="!file || uploading || blocked || !!unavailable" @click="upload"><LoaderCircle v-if="uploading" class="animate-spin" /><UploadCloud v-else />{{ uploading ? '正在上传并校验…' : '上传并校验' }}</button><span v-if="uploading" class="card-sub">{{ uploadPercent }}%</span></div>
        <div v-if="uploading" class="progress mt-4" role="progressbar" aria-label="发布包上传进度" :aria-valuenow="uploadPercent" aria-valuemin="0" aria-valuemax="100"><i :style="{ width: `${uploadPercent}%` }" /></div>
        <p v-if="uploadMessage" class="card-sub mt-4" role="status">{{ uploadMessage }}</p>
      </article>
    </section>
    <section class="card card-pad mb-4">
      <div class="card-head"><div><h2 class="card-title">可用发布包</h2><p class="card-sub">来自本平台发布库，已完成上传校验。</p></div></div>
      <div v-if="!releases.length" class="compact-empty"><UploadCloud /><span>{{ status ? '暂无发布包，请先上传。' : '正在连接升级服务…' }}</span></div>
      <div v-else class="upgrade-release-list">
        <label v-for="release in releases" :key="release.id" class="upgrade-release" :class="{ selected: selected === release.id }">
          <input v-model="selected" type="radio" name="release" :value="release.id" :disabled="blocked || starting || uploading" />
          <span><strong>{{ release.version }}</strong><small>{{ release.notes || '未提供更新说明。' }}</small><small>兼容来源：{{ release.from_versions.join('、') }} · linux/{{ release.architecture }}</small><small v-if="release.replaces_version">版本编号调整：{{ release.replaces_version }} → {{ release.version }}</small></span>
          <span class="status" :class="canInstall(release) ? 'online' : ''">{{ canInstall(release) ? '可安装' : release.version === health.version ? '当前版本' : '不适用' }}</span>
        </label>
      </div>
      <div class="upgrade-install-bar"><span class="card-sub">安装前自动备份配置和数据库。切换期间页面会自动重连。</span><button class="btn btn-primary" :disabled="!chosen || !canInstall(chosen) || blocked || uploading || starting || !!unavailable" @click="confirming = false; confirmOpen = true">{{ busy ? '升级执行中' : '安装所选版本' }}</button></div>
    </section>
    <section class="card card-pad" aria-live="polite">
      <div class="card-head"><div><h2 class="card-title">执行记录</h2><p class="card-sub">保存在服务器上，页面刷新后可继续查看。</p></div><span class="status" :class="task?.phase === 'succeeded' ? 'online' : task?.phase === 'rollback_failed' || task?.phase === 'failed' ? 'danger' : busy ? 'info' : ''">{{ task ? phaseText[task.phase] || task.phase : '暂无任务' }}</span></div>
      <template v-if="task">
        <p class="upgrade-task-title"><strong>{{ task.previous_version }} → {{ task.version }}</strong><span>{{ task.message }}</span></p>
        <ol class="upgrade-events"><li v-for="(event, index) in task.events" :key="index"><Clock3 aria-hidden="true" /><time>{{ formatDate(event.time) }}</time><strong>{{ phaseText[event.phase] || event.phase }}</strong><span>{{ event.message }}</span></li></ol>
        <p v-if="task.backup" class="card-sub">备份编号：{{ task.backup }}</p>
        <button v-if="task.phase === 'succeeded'" class="btn mt-4" @click="reloadPage"><CheckCircle2 />刷新页面加载新版本</button>
      </template>
      <p v-else class="card-sub">还没有通过本页面执行升级。</p>
    </section>
    <ModalDialog :open="confirmOpen" title="确认安装应用更新" :description="`${health.version || '当前版本'} → ${chosen?.version || ''}`" @close="!starting && (confirmOpen = false)">
      <p>系统将先备份配置和数据库，再切换应用镜像。实时预览会短暂中断；健康检查失败会自动恢复旧版本。</p>
      <label class="upgrade-confirm"><input v-model="confirming" type="checkbox" :disabled="starting" />我已确认维护时间，开始安装所选版本。</label>
      <template #footer><button class="btn" :disabled="starting" @click="confirmOpen = false">取消</button><button class="btn btn-primary" :disabled="!confirming || starting || !!unavailable" @click="install"><LoaderCircle v-if="starting" class="animate-spin" />确认安装</button></template>
    </ModalDialog>
  </main>
</template>

<style scoped>
.upgrade-version { color: var(--ink-strong); font-size: 24px; overflow-wrap: anywhere; }
.upgrade-file { display: grid; gap: 8px; margin-block: 16px; color: var(--ink); font-size: 13px; }
.upgrade-file input { min-width: 0; max-width: 100%; }
.upgrade-release-list { display: grid; gap: 8px; }
.upgrade-release { display: flex; align-items: flex-start; gap: 12px; padding: 12px; border: 1px solid var(--line); border-radius: var(--radius-sm); cursor: pointer; }
.upgrade-release.selected { border-color: var(--blue); background: var(--blue-soft); }
.upgrade-release > input { margin-top: 4px; accent-color: var(--blue); }
.upgrade-release > span:first-of-type { min-width: 0; flex: 1; }
.upgrade-release small { display: block; margin-top: 6px; color: var(--muted); line-height: 1.6; white-space: pre-wrap; overflow-wrap: anywhere; }
.upgrade-install-bar { display: flex; align-items: center; justify-content: space-between; gap: 16px; flex-wrap: wrap; margin-top: 16px; padding-top: 16px; border-top: 1px solid var(--line); }
.upgrade-task-title { display: grid; gap: 8px; margin: 0 0 16px; font-size: 13px; overflow-wrap: anywhere; }
.upgrade-events { display: grid; gap: 12px; list-style: none; padding: 0; margin: 0 0 16px; }
.upgrade-events li { display: grid; grid-template-columns: 16px 150px 80px minmax(0, 1fr); gap: 8px; align-items: start; font-size: 12px; }
.upgrade-events svg { width: 16px; height: 16px; color: var(--muted); }
.upgrade-events time { color: var(--muted); }
.upgrade-events span { overflow-wrap: anywhere; }
.upgrade-confirm { display: flex; align-items: start; gap: 8px; margin-top: 20px; font-size: 13px; line-height: 1.6; }
@media (max-width: 640px) {
  .upgrade-events li { grid-template-columns: 16px minmax(0, 1fr) auto; }
  .upgrade-events li > span { grid-column: 2 / -1; }
  .upgrade-release { flex-wrap: wrap; }
  .upgrade-release > .status { margin-left: 28px; }
  .upgrade-install-bar > button { width: 100%; }
}
</style>
