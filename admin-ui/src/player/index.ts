import type { PlayAddress, PlayResult } from '../types/api'
import * as CryptoJS from 'crypto-js'
import {
  buildEasyPlayerOptions,
  DEFAULT_PROTOCOL_PRIORITY as EASYPLAYER_PROTOCOL_PRIORITY,
  isEasyPlayerPosterUrl,
  type EasyPlayerSettings,
  type StreamProtocol,
  type MediaProtocol,
} from './easyplayer.config'

export type { StreamProtocol, MediaProtocol } from './easyplayer.config'

export interface StreamSource {
  protocol: MediaProtocol
  url: string
  label: string
}

export interface PlayerSession {
  destroy: () => void | Promise<void>
  setMuted?: (muted: boolean) => void
}

export interface PlayerAdapter {
  id: string
  supports: (source: StreamSource) => boolean
  play: (
    container: HTMLElement,
    source: StreamSource,
    options: {
      muted: boolean
      poster?: string
      duration?: number
      settings: EasyPlayerSettings
      onError?: (message: string) => void
      onEnded?: () => void
    }
  ) => Promise<PlayerSession>
}

export const DEFAULT_PROTOCOL_PRIORITY: StreamProtocol[] = [
  ...EASYPLAYER_PROTOCOL_PRIORITY,
]

const protocolAliases: Record<string, StreamProtocol | undefined> = {
  webrtc: 'webrtc',
  rtc: 'webrtc',
  'ws-flv': 'ws-flv',
  wsflv: 'ws-flv',
  flv: 'http-flv',
  'http-flv': 'http-flv',
  httpflv: 'http-flv',
  hls: 'hls',
  m3u8: 'hls',
}

function normalizeProtocol(value: string) {
  return protocolAliases[value.trim().toLowerCase().replace(/_/g, '-')]
}

function sourceFromEntry(
  item: PlayAddress,
  key: string,
  value: unknown
): StreamSource | null {
  if (typeof value !== 'string' || !value.trim()) return null
  const protocol = normalizeProtocol(key)
  if (!protocol) return null
  return {
    protocol,
    url: value,
    label: `${String(item.label || '').trim()} ${key}`.trim(),
  }
}

/** 将新旧接口字段统一为播放器可消费的 Web 协议地址。 */
export function normalizeStreamSources(result?: PlayResult | null) {
  const sources: StreamSource[] = []
  const seen = new Set<string>()

  for (const item of result?.items || []) {
    for (const [key, value] of Object.entries(item)) {
      let source = sourceFromEntry(item, key, value)
      if (!source && key === 'url' && typeof value === 'string') {
        const protocol = normalizeProtocol(String(item.schema || item.type || ''))
        if (protocol) {
          source = {
            protocol,
            url: value,
            label: `${String(item.label || '').trim()} ${protocol}`.trim(),
          }
        }
      }
      if (!source || seen.has(source.url)) continue
      seen.add(source.url)
      sources.push(source)
    }
  }
  return sources
}

interface EasyPlayerInstance {
  destroy: () => void | Promise<void>
  play: (url: string, options?: Record<string, unknown>) => void | Promise<void>
  setRate: (rate: number) => void | Promise<void>
  setMute?: (muted: boolean) => void
  on: (event: string, listener: (...args: unknown[]) => void) => void
  off?: (event: string, listener: (...args: unknown[]) => void) => void
}

type EasyPlayerConstructor = new (
  container: HTMLElement,
  config: Record<string, unknown>
) => EasyPlayerInstance

declare global {
  interface Window {
    CryptoJS?: typeof CryptoJS
    EasyPlayerPro?: EasyPlayerConstructor
    'EasyPlayer-pro'?: EasyPlayerConstructor
  }
}

const easyPlayerLoads = new Map<string, Promise<EasyPlayerConstructor>>()

function easyPlayerConstructor() {
  return window.EasyPlayerPro || window['EasyPlayer-pro']
}

function loadEasyPlayer() {
  const loaded = easyPlayerConstructor()
  if (loaded) return Promise.resolve(loaded)

  const configuredURL = String(
    import.meta.env.VITE_EASYPLAYER_SCRIPT_URL || './easyplayer/EasyPlayer-pro.js'
  ).trim()
  const url = new URL(configuredURL, document.baseURI).toString()
  const pending = easyPlayerLoads.get(url)
  if (pending) return pending

  const load = new Promise<EasyPlayerConstructor>((resolve, reject) => {
    // EasyPlayer 的 UMD 构建在浏览器环境从全局读取 CryptoJS。
    window.CryptoJS = CryptoJS
    const script = document.createElement('script')
    const complete = () => {
      const constructor = easyPlayerConstructor()
      if (constructor) resolve(constructor)
      else reject(new Error('EasyPlayer 脚本已加载，但未导出 EasyPlayerPro'))
    }
    script.addEventListener('load', complete, { once: true })
    script.addEventListener(
      'error',
      () => reject(new Error(`EasyPlayer 脚本加载失败：${url}`)),
      { once: true }
    )
    script.src = url
    script.async = true
    script.dataset.owlEasyPlayer = url
    document.head.appendChild(script)
  })
  const retryableLoad = load.catch((cause) => {
    easyPlayerLoads.delete(url)
    document
      .querySelectorAll<HTMLScriptElement>('script[data-owl-easy-player]')
      .forEach((script) => {
        if (script.dataset.owlEasyPlayer === url) script.remove()
      })
    throw cause
  })
  easyPlayerLoads.set(url, retryableLoad)
  return retryableLoad
}

const easyPlayerAdapter: PlayerAdapter = {
  id: 'easy-player',
  supports: () => true,
  async play(container, source, options) {
    const EasyPlayer = await loadEasyPlayer()
    const easyPlayerOptions = buildEasyPlayerOptions(options.settings, {
      muted: options.muted,
      protocol: source.protocol,
      poster: options.poster,
    })
    const player = new EasyPlayer(
      container,
      easyPlayerOptions
    )
    let disposed = false
    let started = false
    let ended = false
    let rejectStartup: (cause: Error) => void = () => {}
    const startupError = new Promise<never>((_, reject) => { rejectStartup = reject })
    // 不把 SDK 的错误参数或带票据的播放地址直接展示给用户。
    const onError = () => {
      if (disposed) return
      const message = source.protocol === 'mp4'
        ? '录像暂时无法播放，请重试或下载后查看。'
        : '视频播放异常，请重试。'
      if (started) options.onError?.(message)
      else rejectStartup(new Error(message))
    }
    const onEnded = () => {
      if (disposed || ended || source.protocol !== 'mp4') return
      ended = true
      options.onEnded?.()
    }
    const onRate = (value: unknown) => {
      if (disposed || source.protocol !== 'mp4') return
      const rate = Number(value)
      if (Number.isFinite(rate) && rate > 0) {
        void Promise.resolve(player.setRate(rate)).catch(onError)
      }
    }
    player.on('error', onError)
    player.on('playbackEnd', onEnded)
    player.on('playbackRate', onRate)
    const destroy = async () => {
      disposed = true
      player.off?.('error', onError)
      player.off?.('playbackEnd', onEnded)
      player.off?.('playbackRate', onRate)
      await player.destroy()
    }
    try {
      // SDK 要求绝对地址；文件路径上的鉴权查询参数由 URL 原样保留。
      const playbackOptions = source.protocol === 'mp4' ? {
        controlType: 'simple',
        duration: Math.max(0, options.duration || 0),
        startTime: 0,
        showControl: true,
        showRateBtn: true,
      } : undefined
      await Promise.race([player.play(new URL(source.url, document.baseURI).toString(), playbackOptions), startupError])
      started = true
    } catch {
      await destroy()
      throw new Error(source.protocol === 'mp4'
        ? '录像暂时无法播放，请重试或下载后查看。'
        : '视频播放失败，请重试。')
    }
    return {
      destroy,
      setMuted: (muted) => player.setMute?.(muted),
    }
  },
}

const nativeHlsAdapter: PlayerAdapter = {
  id: 'native-hls',
  supports(source) {
    if (source.protocol !== 'hls') return false
    const video = document.createElement('video')
    return Boolean(
      video.canPlayType('application/vnd.apple.mpegurl') ||
      video.canPlayType('application/x-mpegURL')
    )
  },
  async play(container, source, options) {
    const video = document.createElement('video')
    video.autoplay = true
    video.controls = true
    video.muted = options.muted
    video.playsInline = true
    const poster = options.poster || options.settings.posterUrl
    if (isEasyPlayerPosterUrl(poster)) video.poster = poster
    video.src = source.url
    video.style.width = '100%'
    video.style.height = '100%'
    video.style.objectFit = 'contain'
    container.appendChild(video)
    const onError = () => options.onError?.('视频播放异常，请重试。')
    const onEnded = () => options.onEnded?.()
    video.addEventListener('error', onError)
    video.addEventListener('ended', onEnded)
    const destroy = () => {
      video.removeEventListener('error', onError)
      video.removeEventListener('ended', onEnded)
      video.pause()
      video.removeAttribute('src')
      video.load()
      video.remove()
    }
    try {
      await video.play()
    } catch (cause) {
      destroy()
      throw cause
    }
    return {
      destroy,
      setMuted(muted) {
        video.muted = muted
      },
    }
  },
}

/**
 * 适配器注册表。后续替换播放器时只需新增适配器并调整顺序，业务组件无需改动。
 */
export const playerAdapters: PlayerAdapter[] = [easyPlayerAdapter, nativeHlsAdapter]

export function playbackCandidates(
  sources: StreamSource[],
  priority: MediaProtocol[] = DEFAULT_PROTOCOL_PRIORITY
) {
  const ordered = [...sources].sort(
    (left, right) => {
      const leftIndex = priority.indexOf(left.protocol)
      const rightIndex = priority.indexOf(right.protocol)
      return (leftIndex < 0 ? priority.length : leftIndex) -
        (rightIndex < 0 ? priority.length : rightIndex)
    }
  )
  return ordered.flatMap((source) =>
    playerAdapters
      .filter((adapter) => adapter.supports(source))
      .map((adapter) => ({ adapter, source }))
  )
}
