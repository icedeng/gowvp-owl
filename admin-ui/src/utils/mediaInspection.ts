import type { MediaInspectionResult } from '../types/api'
import { formatBytes, formatDate, formatDuration } from './format'

export type InspectionRow = Record<string, unknown>
export type InspectionCell = 'text' | 'bytes' | 'rate' | 'bitrate' | 'duration' | 'time' | 'boolean' | 'tracks' | 'proxyStatus' | 'tcpMode' | 'trackFilter' | 'load' | 'delay'
export interface InspectionColumn { key: string; label: string; kind?: InspectionCell }

export function inspectionValue(row: InspectionRow, path: string): unknown {
  return path.split('.').reduce<unknown>((value, key) => value !== null && typeof value === 'object' ? (value as InspectionRow)[key] : undefined, row)
}

export function inspectionRows(result?: MediaInspectionResult): InspectionRow[] {
  if (result?.status !== 'ok') return []
  if (Array.isArray(result.data)) return result.data.map((item) => item !== null && typeof item === 'object' ? item as InspectionRow : { value: item })
  if (result.data !== null && typeof result.data === 'object') return Object.entries(result.data).map(([name, value]) => ({ name, value }))
  return result.data === undefined || result.data === null ? [] : [{ value: result.data }]
}

export function trackSummary(value: unknown): string {
  if (!Array.isArray(value) || !value.length) return '未返回轨道'
  return value.filter((track): track is InspectionRow => track !== null && typeof track === 'object').map((track) => {
    const codec = String(track.codec_id_name || `编码 ${track.codec_id ?? '未知'}`)
    const shape = track.width && track.height ? `${track.width}×${track.height}` : track.sample_rate ? `${track.sample_rate} Hz` : ''
    const fps = track.fps ? `${track.fps} fps` : ''
    return [codec, shape, fps, track.ready === false ? '未就绪' : ''].filter(Boolean).join(' · ')
  }).join(' / ') || '未返回轨道'
}

export function inspectionText(value: unknown, kind: InspectionCell = 'text'): string {
  if (value === undefined || value === null || value === '') return '—'
  if (kind === 'tracks') return trackSummary(value)
  if (kind === 'boolean') return value === true || value === 1 || value === '1' ? '是' : '否'
  if (kind === 'proxyStatus') return ({ 0: '在线', 1: '未连接' } as Record<string, string>)[String(value)] || `状态 ${value}`
  if (kind === 'tcpMode') return ({ 0: 'UDP', 1: 'TCP 被动', 2: 'TCP 主动' } as Record<string, string>)[String(value)] || String(value)
  if (kind === 'trackFilter') return ({ 0: '全部', 1: '仅音频', 2: '仅视频' } as Record<string, string>)[String(value)] || String(value)
  if (['bytes', 'rate', 'bitrate', 'duration', 'time', 'load', 'delay'].includes(kind)) {
    const number = Number(value)
    if (!Number.isFinite(number) || number < 0) return '—'
    if (kind === 'bytes' || kind === 'rate') return `${formatBytes(number)}${kind === 'rate' ? '/s' : ''}`
    if (kind === 'bitrate') return number >= 125000 ? `${(number * 8 / 1e6).toFixed(2)} Mbps` : `${(number * 8 / 1000).toFixed(1)} Kbps`
    if (kind === 'duration') return formatDuration(number)
    if (kind === 'time') return formatDate(number * 1000)
    if (kind === 'load') return `${number}%`
    if (kind === 'delay') return `${number} ms`
  }
  return typeof value === 'object' ? JSON.stringify(value) : String(value)
}

// ZLM 同一源会按 RTSP/RTMP/HLS 等输出多条记录，观看人数不能跨协议重复累加。
export function summarizeStreams(result?: MediaInspectionResult) {
  if (result?.status !== 'ok') return null
  const streams = new Map<string, { readers?: number; recording: boolean }>()
  for (const row of inspectionRows(result)) {
    const key = JSON.stringify([row.vhost, row.app, row.stream])
    const previous = streams.get(key)
    const count = Number(row.totalReaderCount)
    const readers = row.totalReaderCount === undefined || row.totalReaderCount === null || !Number.isFinite(count) || count < 0 ? undefined : count
    streams.set(key, {
      readers: readers === undefined ? previous?.readers : Math.max(previous?.readers ?? 0, readers),
      recording: Boolean(previous?.recording || row.isRecordingMP4 === true || row.isRecordingMP4 === 1 || row.isRecordingMP4 === '1'),
    })
  }
  const items = [...streams.values()]
  return {
    sources: streams.size,
    outputs: inspectionRows(result).length,
    readers: items.some((item) => item.readers === undefined) ? null : items.reduce((sum, item) => sum + (item.readers || 0), 0),
    recording: items.filter((item) => item.recording).length,
  }
}
