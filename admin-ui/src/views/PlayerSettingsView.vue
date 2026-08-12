<script setup lang="ts">
import { computed, reactive, watch } from 'vue'
import {
  AudioLines,
  Braces,
  CircleAlert,
  Image,
  Gauge,
  MonitorCog,
  PanelsTopLeft,
  RotateCcw,
  Save,
  Settings2,
  SlidersHorizontal,
  Stamp,
} from '@lucide/vue'
import {
  cloneEasyPlayerSettings,
  DEFAULT_EASYPLAYER_SETTINGS,
  EASYPLAYER_BUTTON_NAMES,
  isEasyPlayerAdvancedOptions,
  isEasyPlayerPosterUrl,
  type EasyPlayerSettings,
} from '../player/easyplayer.config'
import { usePlayerSettingsStore } from '../stores/player'
import { useUiStore } from '../stores/ui'

const ui = useUiStore()
const playerSettings = usePlayerSettingsStore()
const form = reactive<EasyPlayerSettings>(
  cloneEasyPlayerSettings(playerSettings.settings)
)

const buttonLabels: Record<(typeof EASYPLAYER_BUTTON_NAMES)[number], string> = {
  play: '播放/暂停',
  audio: '音量',
  screenshot: '截图',
  fullscreen: '全屏',
  record: '本地录制',
  zoom: '电子放大',
  ptz: '云台控制',
  quality: '清晰度',
  stretch: '画面比例',
}

const advancedOptionsValid = computed(() =>
  isEasyPlayerAdvancedOptions(form.advancedOptions)
)
const posterUrlValid = computed(() => isEasyPlayerPosterUrl(form.posterUrl))
const canSave = computed(() =>
  advancedOptionsValid.value &&
  posterUrlValid.value &&
  form.bufferTime >= 0 && form.bufferTime <= 30 &&
  form.loadTimeOut >= 1 && form.loadTimeOut <= 120 &&
  form.loadTimeReplay >= -1 && form.loadTimeReplay <= 99 &&
  form.watermark.fontSize >= 10 && form.watermark.fontSize <= 48 &&
  form.watermark.opacity >= 0.05 && form.watermark.opacity <= 1 &&
  form.fullscreenWatermark.fontSize >= 10 && form.fullscreenWatermark.fontSize <= 48 &&
  form.fullscreenWatermark.opacity >= 0.05 && form.fullscreenWatermark.opacity <= 0.8 &&
  form.fullscreenWatermark.angle >= -60 && form.fullscreenWatermark.angle <= 60
)

function syncForm() {
  Object.assign(form, cloneEasyPlayerSettings(playerSettings.settings))
}

function save() {
  if (!canSave.value) {
    ui.toast('请修正超时、缓冲、封面、水印或高级 JSON 配置后再保存')
    return
  }
  try {
    playerSettings.save(cloneEasyPlayerSettings(form))
    ui.toast('播放器配置已保存，当前播放实例将自动重载')
  } catch (cause) {
    ui.toast(cause instanceof Error ? cause.message : '播放器配置保存失败')
  }
}

function restoreDefaults() {
  Object.assign(form, cloneEasyPlayerSettings(DEFAULT_EASYPLAYER_SETTINGS))
  ui.toast('已恢复默认值，点击保存后生效')
}

watch(() => playerSettings.revision, syncForm)
</script>

<template>
  <main class="page-content player-settings-page">
    <header class="page-head">
      <div>
        <h1 class="page-title">播放器设置</h1>
        <p class="page-desc">配置 EasyPlayer 的协议优先级、解码、缓冲、封面与水印。保存后当前浏览器中新建或重载的播放器立即采用这些参数。</p>
      </div>
      <div class="head-actions">
        <button class="btn" type="button" @click="restoreDefaults"><RotateCcw />恢复默认</button>
        <button class="btn btn-primary" type="button" :disabled="!canSave" @click="save"><Save />保存并应用</button>
      </div>
    </header>

    <div class="player-settings-note" role="status">
      <MonitorCog aria-hidden="true" />
      <span><strong>本浏览器配置</strong>保存到当前浏览器本地，不会修改服务器或影响其他登录用户。</span>
    </div>

    <form class="player-settings-grid" @submit.prevent="save">
      <section class="card form-section player-settings-section">
        <div class="card-head">
          <div><h2 class="card-title">播放策略</h2><p class="card-sub">优先选择可用协议，失败后自动尝试其余地址。</p></div>
          <Gauge aria-hidden="true" />
        </div>
        <div class="form-grid">
          <label class="form-group">
            <span class="form-label">首选协议</span>
            <select v-model="form.preferredProtocol" class="input plain w-full">
              <option value="ws-flv">WS-FLV（低延迟）</option>
              <option value="http-flv">HTTP-FLV</option>
              <option value="webrtc">WebRTC</option>
              <option value="hls">HLS / fMP4</option>
            </select>
            <span class="form-help">系统会按首选协议优先，其余协议作为故障降级。</span>
          </label>
          <label class="form-group">
            <span class="form-label">解码模式</span>
            <select v-model="form.decoderMode" class="input plain w-full">
              <option value="auto">自动选择</option>
              <option value="mse">MSE</option>
              <option value="wcs">WebCodecs（WCS）</option>
              <option value="wasm">WASM</option>
            </select>
            <span class="form-help">H.265 兼容性优先时可选择 WASM。</span>
          </label>
          <label class="form-group">
            <span class="form-label">最小缓冲（秒）</span>
            <input v-model.number="form.bufferTime" class="input plain w-full" type="number" min="0" max="30" step="0.1" />
            <span class="form-help">数值越低延迟越小，但弱网更容易卡顿。</span>
          </label>
          <label class="form-group">
            <span class="form-label">加载超时（秒）</span>
            <input v-model.number="form.loadTimeOut" class="input plain w-full" type="number" min="1" max="120" step="1" />
            <span class="form-help">超时后播放器会按重连次数自动恢复。</span>
          </label>
          <label class="form-group">
            <span class="form-label">超时重连次数</span>
            <input v-model.number="form.loadTimeReplay" class="input plain w-full" type="number" min="-1" max="99" step="1" />
            <span class="form-help"><code>-1</code> 表示持续重连。</span>
          </label>
          <div class="player-settings-summary" aria-label="当前播放策略摘要">
            <span>当前优先</span>
            <strong>{{ form.preferredProtocol.toUpperCase() }}</strong>
            <small>{{ form.decoderMode === 'auto' ? '自动解码' : form.decoderMode.toUpperCase() + ' 解码' }}</small>
          </div>
        </div>
      </section>

      <section class="card form-section player-settings-section player-appearance-section">
        <div class="card-head">
          <div><h2 class="card-title">封面与加载状态</h2><p class="card-sub">默认封面用于未提供通道快照时；通道快照始终优先显示。</p></div>
          <Image aria-hidden="true" />
        </div>
        <div class="player-appearance-grid">
          <label class="form-group player-appearance-cover">
            <span class="form-label">默认封面地址</span>
            <input v-model.trim="form.posterUrl" class="input plain w-full" type="url" maxlength="2048" placeholder="https://example.com/player-cover.jpg" />
            <span class="form-help">可填写 HTTP(S) 或以 <code>/</code> 开头的同源静态路径；留空则不展示默认封面。</span>
            <span v-if="!posterUrlValid" class="player-field-error" role="alert"><CircleAlert />默认封面仅支持 HTTP(S) 地址或同源绝对路径。</span>
          </label>
          <label class="form-group">
            <span class="form-label">加载提示</span>
            <input v-model.trim="form.loadingText" class="input plain w-full" type="text" maxlength="100" placeholder="例如：正在连接视频流…" />
            <span class="form-help">显示在 EasyPlayer 的加载层，留空使用播放器默认提示。</span>
          </label>
          <label class="toggle-row player-appearance-logo"><span><strong>显示加载 Logo</strong><small>加载时显示 EasyPlayer 内置 Logo 样式</small></span><span class="switch"><input v-model="form.showLoadingLogo" type="checkbox" /><span class="slider" /></span></label>
        </div>
      </section>

      <section class="card form-section player-settings-section">
        <div class="card-head">
          <div><h2 class="card-title">解码与画面</h2><p class="card-sub">按终端能力启用高级渲染，优先从默认配置开始验证。</p></div>
          <Settings2 aria-hidden="true" />
        </div>
        <div class="player-toggle-grid">
          <label class="toggle-row"><span><strong>启用 HLS H.265</strong><small>HLS 流携带 H.265 时尝试兼容解码</small></span><span class="switch"><input v-model="form.hlsH265" type="checkbox" /><span class="slider" /></span></label>
          <label class="toggle-row"><span><strong>WASM SIMD</strong><small>WASM 解码的 SIMD 优化，需浏览器支持</small></span><span class="switch"><input v-model="form.wasmSimd" type="checkbox" /><span class="slider" /></span></label>
          <label class="toggle-row"><span><strong>GPU 解码线程</strong><small>启用多线程/硬件能力路径，兼容性因浏览器而异</small></span><span class="switch"><input v-model="form.gpuDecoder" type="checkbox" /><span class="slider" /></span></label>
          <label class="toggle-row"><span><strong>WebGPU 渲染</strong><small>仅在已验证支持的终端启用</small></span><span class="switch"><input v-model="form.webGPU" type="checkbox" /><span class="slider" /></span></label>
          <label class="toggle-row"><span><strong>Canvas 渲染</strong><small>用于部分 H.265、HLS 和 WebRTC 渲染路径</small></span><span class="switch"><input v-model="form.canvasRender" type="checkbox" /><span class="slider" /></span></label>
          <label class="toggle-row"><span><strong>拉伸填充</strong><small>关闭时保留源视频宽高比</small></span><span class="switch"><input v-model="form.stretch" type="checkbox" /><span class="slider" /></span></label>
          <label class="toggle-row"><span><strong>解析音频</strong><small>关闭可降低无音频监控流的资源消耗</small></span><span class="switch"><input v-model="form.hasAudio" type="checkbox" /><span class="slider" /></span></label>
          <label class="toggle-row"><span><strong>显示实时带宽</strong><small>在播放器控制栏显示当前网络速率</small></span><span class="switch"><input v-model="form.showBandwidth" type="checkbox" /><span class="slider" /></span></label>
        </div>
      </section>

      <section class="card form-section player-settings-section player-watermark-section">
        <div class="card-head">
          <div><h2 class="card-title">水印与标识</h2><p class="card-sub">常规水印附着在画面内；全屏水印在进入全屏时以平铺方式显示。</p></div>
          <Stamp aria-hidden="true" />
        </div>
        <div class="player-watermark-grid">
          <section class="player-watermark-panel" aria-labelledby="player-watermark-title">
            <div class="player-watermark-panel-head">
              <div><h3 id="player-watermark-title">常规文字水印</h3><p>适合通道名称、平台标识或责任单位。</p></div>
              <span class="switch"><input v-model="form.watermark.enabled" type="checkbox" aria-label="启用常规文字水印" /><span class="slider" /></span>
            </div>
            <div class="player-watermark-fields">
              <label class="form-group player-watermark-wide"><span class="form-label">水印文字</span><input v-model.trim="form.watermark.text" class="input plain w-full" type="text" maxlength="100" placeholder="例如：国标视频管理平台" :disabled="!form.watermark.enabled" /></label>
              <label class="form-group"><span class="form-label">位置</span><select v-model="form.watermark.position" class="input plain w-full" :disabled="!form.watermark.enabled"><option value="top-left">左上角</option><option value="top-right">右上角</option><option value="bottom-left">左下角</option><option value="bottom-right">右下角</option></select></label>
              <label class="form-group"><span class="form-label">颜色</span><input v-model="form.watermark.color" class="player-color-input" type="color" :disabled="!form.watermark.enabled" /><span class="form-help mono">{{ form.watermark.color }}</span></label>
              <label class="form-group"><span class="form-label">字号</span><input v-model.number="form.watermark.fontSize" class="input plain w-full" type="number" min="10" max="48" step="1" inputmode="numeric" :disabled="!form.watermark.enabled" /></label>
              <label class="form-group"><span class="form-label">不透明度</span><input v-model.number="form.watermark.opacity" class="input plain w-full" type="number" min="0.05" max="1" step="0.05" inputmode="decimal" :disabled="!form.watermark.enabled" /></label>
            </div>
          </section>
          <section class="player-watermark-panel" aria-labelledby="player-fullscreen-watermark-title">
            <div class="player-watermark-panel-head">
              <div><h3 id="player-fullscreen-watermark-title">全屏防录屏水印</h3><p>进入全屏后以低透明度平铺显示，适合账号或使用者标识。</p></div>
              <span class="switch"><input v-model="form.fullscreenWatermark.enabled" type="checkbox" aria-label="启用全屏防录屏水印" /><span class="slider" /></span>
            </div>
            <div class="player-watermark-fields">
              <label class="form-group player-watermark-wide"><span class="form-label">水印文字</span><input v-model.trim="form.fullscreenWatermark.text" class="input plain w-full" type="text" maxlength="100" placeholder="例如：仅限授权人员使用" :disabled="!form.fullscreenWatermark.enabled" /></label>
              <label class="form-group"><span class="form-label">颜色</span><input v-model="form.fullscreenWatermark.color" class="player-color-input" type="color" :disabled="!form.fullscreenWatermark.enabled" /><span class="form-help mono">{{ form.fullscreenWatermark.color }}</span></label>
              <label class="form-group"><span class="form-label">字号</span><input v-model.number="form.fullscreenWatermark.fontSize" class="input plain w-full" type="number" min="10" max="48" step="1" inputmode="numeric" :disabled="!form.fullscreenWatermark.enabled" /></label>
              <label class="form-group"><span class="form-label">不透明度</span><input v-model.number="form.fullscreenWatermark.opacity" class="input plain w-full" type="number" min="0.05" max="0.8" step="0.05" inputmode="decimal" :disabled="!form.fullscreenWatermark.enabled" /></label>
              <label class="form-group"><span class="form-label">旋转角度</span><input v-model.number="form.fullscreenWatermark.angle" class="input plain w-full" type="number" min="-60" max="60" step="1" inputmode="numeric" :disabled="!form.fullscreenWatermark.enabled" /></label>
            </div>
          </section>
        </div>
      </section>

      <section class="card form-section player-settings-section">
        <div class="card-head">
          <div><h2 class="card-title">控制栏按钮</h2><p class="card-sub">只显示当前运维工作流中需要的功能，避免遮挡监控画面。</p></div>
          <PanelsTopLeft aria-hidden="true" />
        </div>
        <div class="player-button-grid">
          <label v-for="name in EASYPLAYER_BUTTON_NAMES" :key="name" class="player-button-option">
            <input v-model="form.buttons[name]" type="checkbox" />
            <span>{{ buttonLabels[name] }}</span>
          </label>
        </div>
      </section>

      <section class="card form-section player-settings-section">
        <div class="card-head">
          <div><h2 class="card-title">高级参数</h2><p class="card-sub">用于透传 EasyPlayer 未列出的配置项。运行态参数和界面已配置项优先。</p></div>
          <Braces aria-hidden="true" />
        </div>
        <label class="form-group">
          <span class="form-label">额外 JSON 配置</span>
          <textarea v-model.trim="form.advancedOptions" class="input plain player-json-input" rows="7" spellcheck="false" aria-describedby="advanced-options-help" :aria-invalid="!advancedOptionsValid" />
          <span id="advanced-options-help" class="form-help">示例：<code>{ "controlAutoHide": true, "loadingText": "连接中" }</code></span>
          <span v-if="!advancedOptionsValid" class="player-field-error" role="alert"><CircleAlert />请输入合法的 JSON 对象，例如 <code>{}</code>。</span>
        </label>
        <label class="toggle-row player-debug-toggle"><span><strong>调试日志</strong><small>在浏览器控制台输出 EasyPlayer 调试信息</small></span><span class="switch"><input v-model="form.debug" type="checkbox" /><span class="slider" /></span></label>
      </section>

      <footer class="settings-savebar player-settings-savebar">
        <span><AudioLines aria-hidden="true" />保存后，实时监控与通道详情中的播放器会自动重新创建并加载新参数。</span>
        <button class="btn btn-primary" :disabled="!canSave"><Save />保存并应用</button>
      </footer>
    </form>
  </main>
</template>
