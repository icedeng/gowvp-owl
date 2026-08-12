import { computed, reactive, ref } from 'vue'
import { defineStore } from 'pinia'
import {
  cloneEasyPlayerSettings,
  DEFAULT_EASYPLAYER_SETTINGS,
  isEasyPlayerAdvancedOptions,
  isEasyPlayerPosterUrl,
  type EasyPlayerButtons,
  type EasyPlayerSettings,
  type PlayerFullscreenWatermarkSettings,
  type PlayerWatermarkPosition,
  type PlayerWatermarkSettings,
  type StreamProtocol,
} from '../player/easyplayer.config'

const STORAGE_KEY = 'owl-easyplayer-settings-v1'

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function booleanValue(value: unknown, fallback: boolean) {
  return typeof value === 'boolean' ? value : fallback
}

function numberValue(value: unknown, fallback: number, min: number, max: number) {
  const number = Number(value)
  return Number.isFinite(number) && number >= min && number <= max ? number : fallback
}

function protocolValue(value: unknown, fallback: StreamProtocol): StreamProtocol {
  return value === 'ws-flv' || value === 'http-flv' || value === 'webrtc' || value === 'hls'
    ? value
    : fallback
}

function decoderValue(value: unknown, fallback: EasyPlayerSettings['decoderMode']) {
  return value === 'auto' || value === 'mse' || value === 'wcs' || value === 'wasm'
    ? value
    : fallback
}

function textValue(value: unknown, fallback: string, maxLength: number) {
  if (typeof value !== 'string') return fallback
  return value.slice(0, maxLength)
}

function colorValue(value: unknown, fallback: string) {
  return typeof value === 'string' && /^#[0-9a-fA-F]{6}$/.test(value)
    ? value
    : fallback
}

function watermarkPositionValue(
  value: unknown,
  fallback: PlayerWatermarkPosition
): PlayerWatermarkPosition {
  return value === 'top-left' || value === 'top-right' ||
    value === 'bottom-left' || value === 'bottom-right'
    ? value
    : fallback
}

function hydrateWatermark(value: unknown, defaults: PlayerWatermarkSettings): PlayerWatermarkSettings {
  const source = isRecord(value) ? value : {}
  return {
    enabled: booleanValue(source.enabled, defaults.enabled),
    text: textValue(source.text, defaults.text, 100),
    position: watermarkPositionValue(source.position, defaults.position),
    color: colorValue(source.color, defaults.color),
    fontSize: numberValue(source.fontSize, defaults.fontSize, 10, 48),
    opacity: numberValue(source.opacity, defaults.opacity, 0.05, 1),
  }
}

function hydrateFullscreenWatermark(
  value: unknown,
  defaults: PlayerFullscreenWatermarkSettings
): PlayerFullscreenWatermarkSettings {
  const source = isRecord(value) ? value : {}
  return {
    enabled: booleanValue(source.enabled, defaults.enabled),
    text: textValue(source.text, defaults.text, 100),
    color: colorValue(source.color, defaults.color),
    fontSize: numberValue(source.fontSize, defaults.fontSize, 10, 48),
    opacity: numberValue(source.opacity, defaults.opacity, 0.05, 0.8),
    angle: numberValue(source.angle, defaults.angle, -60, 60),
  }
}

function hydrateSettings(value: unknown): EasyPlayerSettings {
  const defaults = cloneEasyPlayerSettings()
  if (!isRecord(value)) return defaults
  const buttons = isRecord(value.buttons) ? value.buttons : {}
  const advancedOptions = typeof value.advancedOptions === 'string'
    ? value.advancedOptions
    : defaults.advancedOptions
  const posterUrl = textValue(value.posterUrl, defaults.posterUrl, 2048)

  return {
    preferredProtocol: protocolValue(value.preferredProtocol, defaults.preferredProtocol),
    bufferTime: numberValue(value.bufferTime, defaults.bufferTime, 0, 30),
    loadTimeOut: numberValue(value.loadTimeOut, defaults.loadTimeOut, 1, 120),
    loadTimeReplay: numberValue(value.loadTimeReplay, defaults.loadTimeReplay, -1, 99),
    decoderMode: decoderValue(value.decoderMode, defaults.decoderMode),
    wasmSimd: booleanValue(value.wasmSimd, defaults.wasmSimd),
    gpuDecoder: booleanValue(value.gpuDecoder, defaults.gpuDecoder),
    webGPU: booleanValue(value.webGPU, defaults.webGPU),
    canvasRender: booleanValue(value.canvasRender, defaults.canvasRender),
    hlsH265: booleanValue(value.hlsH265, defaults.hlsH265),
    hasAudio: booleanValue(value.hasAudio, defaults.hasAudio),
    stretch: booleanValue(value.stretch, defaults.stretch),
    showBandwidth: booleanValue(value.showBandwidth, defaults.showBandwidth),
    posterUrl: isEasyPlayerPosterUrl(posterUrl) ? posterUrl : defaults.posterUrl,
    showLoadingLogo: booleanValue(value.showLoadingLogo, defaults.showLoadingLogo),
    loadingText: textValue(value.loadingText, defaults.loadingText, 100),
    watermark: hydrateWatermark(value.watermark, defaults.watermark),
    fullscreenWatermark: hydrateFullscreenWatermark(
      value.fullscreenWatermark,
      defaults.fullscreenWatermark
    ),
    debug: booleanValue(value.debug, defaults.debug),
    buttons: Object.fromEntries(
      Object.entries(defaults.buttons).map(([key, defaultValue]) => [
        key,
        booleanValue(buttons[key], defaultValue),
      ])
    ) as EasyPlayerButtons,
    advancedOptions: isEasyPlayerAdvancedOptions(advancedOptions)
      ? advancedOptions
      : defaults.advancedOptions,
  }
}

function readSettings() {
  try {
    return hydrateSettings(JSON.parse(localStorage.getItem(STORAGE_KEY) || '{}'))
  } catch {
    return cloneEasyPlayerSettings()
  }
}

export const usePlayerSettingsStore = defineStore('player-settings', () => {
  const settings = reactive<EasyPlayerSettings>(readSettings())
  const revision = ref(0)
  const savedAt = ref<number | null>(null)
  const isAdvancedOptionsValid = computed(() =>
    isEasyPlayerAdvancedOptions(settings.advancedOptions)
  )

  function save(next: EasyPlayerSettings) {
    if (!isEasyPlayerAdvancedOptions(next.advancedOptions)) {
      throw new Error('高级选项必须是合法的 JSON 对象')
    }
    if (!isEasyPlayerPosterUrl(next.posterUrl)) {
      throw new Error('默认封面仅支持 HTTP(S) 地址或以 / 开头的同源路径')
    }
    Object.assign(settings, cloneEasyPlayerSettings(next))
    localStorage.setItem(STORAGE_KEY, JSON.stringify(settings))
    savedAt.value = Date.now()
    revision.value += 1
  }

  function restoreDefaults() {
    const defaults = cloneEasyPlayerSettings(DEFAULT_EASYPLAYER_SETTINGS)
    Object.assign(settings, defaults)
    localStorage.setItem(STORAGE_KEY, JSON.stringify(settings))
    savedAt.value = Date.now()
    revision.value += 1
  }

  return {
    settings,
    revision,
    savedAt,
    isAdvancedOptionsValid,
    save,
    restoreDefaults,
  }
})
