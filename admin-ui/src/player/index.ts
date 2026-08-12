import type { PlayAddress, PlayResult } from '../types/api'
import {
  buildEasyPlayerOptions,
  DEFAULT_PROTOCOL_PRIORITY as EASYPLAYER_PROTOCOL_PRIORITY,
  isEasyPlayerPosterUrl,
  type EasyPlayerSettings,
  type StreamProtocol,
} from './easyplayer.config'

export type { StreamProtocol } from './easyplayer.config'

export interface StreamSource {
  protocol: StreamProtocol
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
    options: { muted: boolean; poster?: string; settings: EasyPlayerSettings }
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
  play: (url: string) => void | Promise<void>
  setMute?: (muted: boolean) => void
}

type EasyPlayerConstructor = new (
  container: HTMLElement,
  config: Record<string, unknown>
) => EasyPlayerInstance

declare global {
  interface Window {
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
  easyPlayerLoads.set(url, load)
  return load
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
    try {
      await player.play(source.url)
    } catch (cause) {
      await player.destroy()
      throw cause
    }
    return {
      destroy: () => player.destroy(),
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
    try {
      await video.play()
    } catch (cause) {
      video.removeAttribute('src')
      video.load()
      video.remove()
      throw cause
    }
    return {
      destroy() {
        video.pause()
        video.removeAttribute('src')
        video.load()
        video.remove()
      },
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
  priority: StreamProtocol[] = DEFAULT_PROTOCOL_PRIORITY
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
