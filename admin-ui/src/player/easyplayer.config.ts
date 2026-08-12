export const PLAYER_PROTOCOLS = ['ws-flv', 'http-flv', 'webrtc', 'hls'] as const

export type StreamProtocol = typeof PLAYER_PROTOCOLS[number]
export type EasyPlayerDecoderMode = 'auto' | 'mse' | 'wcs' | 'wasm'

export const EASYPLAYER_BUTTON_NAMES = [
  'play',
  'audio',
  'screenshot',
  'fullscreen',
  'record',
  'zoom',
  'ptz',
  'quality',
  'stretch',
] as const

export type EasyPlayerButtons = Record<
  (typeof EASYPLAYER_BUTTON_NAMES)[number],
  boolean
>

export type PlayerWatermarkPosition =
  | 'top-left'
  | 'top-right'
  | 'bottom-left'
  | 'bottom-right'

export interface PlayerWatermarkSettings {
  enabled: boolean
  text: string
  position: PlayerWatermarkPosition
  color: string
  fontSize: number
  opacity: number
}

export interface PlayerFullscreenWatermarkSettings {
  enabled: boolean
  text: string
  color: string
  fontSize: number
  opacity: number
  angle: number
}

export const DEFAULT_EASYPLAYER_BUTTONS: EasyPlayerButtons = {
  play: true,
  audio: true,
  screenshot: true,
  fullscreen: true,
  record: false,
  zoom: true,
  ptz: false,
  quality: false,
  stretch: false,
}

/** EasyPlayer 静态默认值；页面配置会在播放器创建时覆盖可调字段。 */
export const EASYPLAYER_CONFIG = {
  isLive: true,
  bufferTime: 0.2,
  stretch: false,
  hasAudio: true,
  // 静音由 StreamPlayer 的 muted 属性控制，这里保留为默认值。
  isMute: false,
  // MSE/WCS/WASM 等解码模式可在此开启；不填时使用 EasyPlayer 默认策略。
  MSE: false,
  WCS: false,
  WASM: false,
  btns: DEFAULT_EASYPLAYER_BUTTONS,
} satisfies Record<string, unknown>

export interface EasyPlayerSettings {
  preferredProtocol: StreamProtocol
  bufferTime: number
  loadTimeOut: number
  loadTimeReplay: number
  decoderMode: EasyPlayerDecoderMode
  wasmSimd: boolean
  gpuDecoder: boolean
  webGPU: boolean
  canvasRender: boolean
  hlsH265: boolean
  hasAudio: boolean
  stretch: boolean
  showBandwidth: boolean
  posterUrl: string
  showLoadingLogo: boolean
  loadingText: string
  watermark: PlayerWatermarkSettings
  fullscreenWatermark: PlayerFullscreenWatermarkSettings
  debug: boolean
  buttons: EasyPlayerButtons
  /** 透传给 EasyPlayer 的额外 JSON 配置，用于未在界面列出的上游参数。 */
  advancedOptions: string
}

export const DEFAULT_PROTOCOL_PRIORITY: StreamProtocol[] = [...PLAYER_PROTOCOLS]

export const DEFAULT_EASYPLAYER_SETTINGS: EasyPlayerSettings = {
  preferredProtocol: 'ws-flv',
  bufferTime: 0.2,
  loadTimeOut: 10,
  loadTimeReplay: 3,
  decoderMode: 'auto',
  wasmSimd: false,
  gpuDecoder: false,
  webGPU: false,
  canvasRender: false,
  hlsH265: true,
  hasAudio: true,
  stretch: false,
  showBandwidth: true,
  posterUrl: '',
  showLoadingLogo: false,
  loadingText: '',
  watermark: {
    enabled: false,
    text: '',
    position: 'bottom-right',
    color: '#ffffff',
    fontSize: 14,
    opacity: 0.72,
  },
  fullscreenWatermark: {
    enabled: false,
    text: '',
    color: '#ffffff',
    fontSize: 18,
    opacity: 0.15,
    angle: -15,
  },
  debug: false,
  buttons: { ...DEFAULT_EASYPLAYER_BUTTONS },
  advancedOptions: '{}',
}

export function cloneEasyPlayerSettings(
  settings: EasyPlayerSettings = DEFAULT_EASYPLAYER_SETTINGS
): EasyPlayerSettings {
  return {
    ...settings,
    buttons: { ...settings.buttons },
    watermark: { ...settings.watermark },
    fullscreenWatermark: { ...settings.fullscreenWatermark },
  }
}

export function resolveProtocolPriority(
  preferredProtocol: StreamProtocol
): StreamProtocol[] {
  return [
    preferredProtocol,
    ...DEFAULT_PROTOCOL_PRIORITY.filter((item) => item !== preferredProtocol),
  ]
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function watermarkPosition(
  position: PlayerWatermarkPosition
): Partial<Record<'left' | 'right' | 'top' | 'bottom', number>> {
  const offset = 16
  if (position === 'top-left') return { left: offset, top: offset }
  if (position === 'top-right') return { right: offset, top: offset }
  if (position === 'bottom-left') return { left: offset, bottom: offset }
  return { right: offset, bottom: offset }
}

function buildWatermarkOptions(settings: PlayerWatermarkSettings) {
  if (!settings.enabled || !settings.text.trim()) return undefined
  return {
    ...watermarkPosition(settings.position),
    text: {
      content: settings.text.trim(),
      color: settings.color,
      fontSize: settings.fontSize,
      opacity: settings.opacity,
    },
  }
}

function buildFullscreenWatermarkOptions(settings: PlayerFullscreenWatermarkSettings) {
  if (!settings.enabled || !settings.text.trim()) return undefined
  return {
    // EasyPlayer 会将全屏水印嵌入 SVG 字符串，需先转义用户输入。
    text: settings.text.trim()
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&apos;'),
    color: settings.color,
    fontSize: settings.fontSize,
    opacity: settings.opacity,
    angle: settings.angle,
  }
}

/** 默认封面会写入 EasyPlayer 的内联样式，仅允许普通 HTTP(S) 或同源绝对路径。 */
export function isEasyPlayerPosterUrl(value: string) {
  const url = value.trim()
  if (!url) return true
  if (url.length > 2048 || /[<>'"`()\\;]/.test(url) || /[\r\n]/.test(url)) return false
  try {
    const parsed = new URL(url, 'https://owl.local')
    if (url.startsWith('/')) return parsed.origin === 'https://owl.local'
    return parsed.protocol === 'http:' || parsed.protocol === 'https:'
  } catch {
    return false
  }
}

export function parseEasyPlayerAdvancedOptions(value: string): Record<string, unknown> {
  const text = value.trim()
  if (!text || text === '{}') return {}
  const parsed: unknown = JSON.parse(text)
  if (!isRecord(parsed)) throw new Error('高级选项必须是 JSON 对象')
  return parsed
}

export function isEasyPlayerAdvancedOptions(value: string) {
  try {
    parseEasyPlayerAdvancedOptions(value)
    return true
  } catch {
    return false
  }
}

export function buildEasyPlayerOptions(
  settings: EasyPlayerSettings,
  runtime: { muted: boolean; protocol: StreamProtocol; poster?: string }
): Record<string, unknown> {
  const advanced = parseEasyPlayerAdvancedOptions(settings.advancedOptions)
  const advancedButtons = isRecord(advanced.btns) ? advanced.btns : {}
  const {
    btns: _buttons,
    isLive: _isLive,
    isMute: _isMute,
    isRtcZLM: _isRtcZLM,
    MSE: _mse,
    WCS: _wcs,
    WASM: _wasm,
    WASMSIMD: _wasmSimd,
    gpuDecoder: _gpuDecoder,
    webGPU: _webGPU,
    canvasRender: _canvasRender,
    isH265: _h265,
    hasAudio: _hasAudio,
    stretch: _stretch,
    isBand: _showBandwidth,
    poster: _poster,
    background: _background,
    isLogo: _isLogo,
    loadingText: _loadingText,
    watermark: _watermark,
    fullWatermark: _fullscreenWatermark,
    watermarkConfig: _watermarkConfig,
    fullscreenWatermarkConfig: _fullscreenWatermarkConfig,
    bufferTime: _bufferTime,
    loadTimeOut: _loadTimeOut,
    loadTimeReplay: _loadTimeReplay,
    debug: _debug,
    ...extraOptions
  } = advanced

  const poster = runtime.poster?.trim() || settings.posterUrl.trim()
  const autoMse = settings.decoderMode === 'auto' &&
    (runtime.protocol === 'ws-flv' || runtime.protocol === 'http-flv')

  const options = {
    ...EASYPLAYER_CONFIG,
    ...extraOptions,
    bufferTime: settings.bufferTime,
    loadTimeOut: settings.loadTimeOut,
    loadTimeReplay: settings.loadTimeReplay,
    // FLV 在自动模式下优先使用浏览器 MSE，避免回落到需要额外 WASM 初始化的路径。
    MSE: settings.decoderMode === 'mse' || autoMse,
    WCS: settings.decoderMode === 'wcs',
    WASM: settings.decoderMode === 'wasm',
    WASMSIMD: settings.wasmSimd,
    gpuDecoder: settings.gpuDecoder,
    webGPU: settings.webGPU,
    canvasRender: settings.canvasRender,
    isH265: settings.hlsH265,
    hasAudio: settings.hasAudio,
    stretch: settings.stretch,
    isBand: settings.showBandwidth,
    poster: isEasyPlayerPosterUrl(poster) ? poster || undefined : undefined,
    isLogo: settings.showLoadingLogo,
    loadingText: settings.loadingText.trim(),
    watermark: buildWatermarkOptions(settings.watermark),
    fullWatermark: buildFullscreenWatermarkOptions(settings.fullscreenWatermark),
    debug: settings.debug,
    btns: {
      ...DEFAULT_EASYPLAYER_BUTTONS,
      ...advancedButtons,
      ...settings.buttons,
    },
    // 由平台运行态决定，避免高级 JSON 意外破坏实时播放链路。
    isLive: true,
    // EasyPlayer 的 isMute 参数实际表示“初始非静音”（内部映射为 isNotMute）。
    // 统一以组件的 muted 语义传入，避免与原生 HLS 适配器行为相反。
    isMute: !runtime.muted,
    isRtcZLM: runtime.protocol === 'webrtc',
  }
  // EasyPlayer Pro 会把存在但值为 undefined 的字段视为非法配置。
  return Object.fromEntries(
    Object.entries(options).filter(([, value]) => value !== undefined)
  )
}

export type EasyPlayerConfig = typeof EASYPLAYER_CONFIG
