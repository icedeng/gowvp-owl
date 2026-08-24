import { http } from './http'
import type {
  ApiChannel, ApiDevice, ApiEvent, ApiMetrics, ApiPage, ApiRecording, ApiErrorBody, DeviceHistoryRecord,
  ApiChannel as Channel, DeviceExt, GbMetrics, HealthInfo, MediaServer, MonthlyStats,
  PlayResult, SipAccessInfo, SipConfig, TimelineRange, VersionCheck, Zone,
  ResourceStats,
} from '../types/api'

export type ListParams = Record<string, string | number | boolean | undefined>

export interface CollectedPage<T> extends ApiPage<T> {
  items: T[]
  total: number
}

type PageRequest<T> = (params?: ListParams) => Promise<{ data: ApiPage<T> }>

/**
 * 分页接口会把单页数量限制在 10000；使用超大 size 只会静默截断第一页。
 * 需要完整数据集的少量管理端场景统一通过正常页码逐页收集，避免漏数。
 */
export async function collectPages<T>(
  request: PageRequest<T>,
  params: ListParams = {},
  pageSize = 1000,
): Promise<CollectedPage<T>> {
  const size = Math.min(10000, Math.max(1, Math.trunc(pageSize)))
  const items: T[] = []
  let page = 1
  let total: number | undefined

  while (true) {
    const response = await request({ ...params, page, size })
    const batch = response.data?.items || []
    const reportedTotal = Number(response.data?.total)
    if (Number.isFinite(reportedTotal) && reportedTotal >= 0) total = reportedTotal
    items.push(...batch)

    if (!batch.length || batch.length < size || (total !== undefined && items.length >= total)) break
    page += 1
  }

  return { items, total: total ?? items.length }
}

export async function countItems<T>(
  request: PageRequest<T>,
  params: ListParams = {},
): Promise<number> {
  const response = await request({ ...params, page: 1, size: 1 })
  return Number(response.data?.total ?? response.data?.items?.length ?? 0)
}

export const api = {
  health: () => http.get<HealthInfo>('/health'),
  metrics: () => http.get<ApiMetrics>('/app/metrics/api'),
  stats: () => http.get<ResourceStats>('/stats'),
  gbMetrics: () => http.get<GbMetrics>('/gb28181/metrics'),
  versionCheck: () => http.get<VersionCheck>('/app/version/check'),
  upgrade: () => http.post('/app/upgrade', undefined, { timeout: 0, responseType: 'text' }),

  devices: (params?: ListParams) => http.get<ApiPage<ApiDevice>>('/devices', { params }),
  deviceChannels: (params?: ListParams) => http.get<ApiPage<ApiChannel>>('/devices/channels', { params }),
  device: (id: string) => http.get<ApiDevice>(`/devices/${encodeURIComponent(id)}`),
  addDevice: (body: Record<string, unknown>) => http.post<ApiDevice>('/devices', body),
  editDevice: (id: string, body: Record<string, unknown>) => http.put<ApiDevice>(`/devices/${encodeURIComponent(id)}`, body),
  deleteDevice: (id: string) => http.delete<ApiDevice>(`/devices/${encodeURIComponent(id)}`),
  catalog: (id: string) => http.post(`/devices/${encodeURIComponent(id)}/catalog`),
  deviceHistory: (id: string, kind: 'heartbeat' | 'register', params?: ListParams) => http.get<ApiPage<DeviceHistoryRecord>>(`/devices/${encodeURIComponent(id)}/history`, { params: { ...params, kind } }),
  timeSync: (id: string) => http.post(`/devices/${encodeURIComponent(id)}/time_sync`),
  subscribe: (id: string, body: Record<string, unknown>) => http.post(`/devices/${encodeURIComponent(id)}/subscribe`, body),
  optionsProbe: (id: string, body: Record<string, unknown> = {}) => http.post(`/devices/${encodeURIComponent(id)}/options_probe`, body),
  devicePtzProbe: (id: string, body: Record<string, unknown> = {}) => http.post(`/devices/${encodeURIComponent(id)}/ptz_probe`, body),
  gbConfig: (id: string, body: Record<string, unknown>) => http.post(`/devices/${encodeURIComponent(id)}/gb/config`, body),
  gbDiagnostics: (id: string) => http.get<Record<string, unknown>>(`/devices/${encodeURIComponent(id)}/gb/diagnostics`),
  gbA4Snapshot: (id: string, params?: ListParams) => http.get<Record<string, unknown>>(`/devices/${encodeURIComponent(id)}/gb/a4_snapshot`, { params }),

  channels: (params?: ListParams) => http.get<ApiPage<ApiChannel>>('/channels', { params }),
  channel: async (id: string) => {
    const response = await http.get<ApiPage<ApiChannel>>('/channels', { params: { page: 1, size: 20, key: id } })
    const item = (response.data?.items || []).find((channel) => channel.id === id || channel.channel_id === id)
    if (!item) throw new Error('通道不存在或已被删除')
    return { ...response, data: item }
  },
  addChannel: (body: Record<string, unknown>) => http.post<ApiChannel>('/channels', body),
  editChannel: (id: string, body: Record<string, unknown>) => http.put<ApiChannel>(`/channels/${encodeURIComponent(id)}`, body),
  deleteChannel: (id: string) => http.delete<ApiChannel>(`/channels/${encodeURIComponent(id)}`),
  play: (id: string) => http.post<PlayResult>(`/channels/${encodeURIComponent(id)}/play`),
  snapshot: (id: string, body: Record<string, unknown> = {}) => http.post<{ link?: string; method?: string }>(`/channels/${encodeURIComponent(id)}/snapshot`, body),
  snapshotImage: (id: string, cacheBust?: number) => withQuery(
    withToken(apiUrl(`/channels/${encodeURIComponent(id)}/snapshot`)),
    cacheBust ? { t: cacheBust } : undefined,
  ),
  ptz: (id: string, body: Record<string, unknown>) => http.post(`/channels/${encodeURIComponent(id)}/ptz`, body),
  ptzProbe: (id: string, body: Record<string, unknown> = {}) => http.post(`/channels/${encodeURIComponent(id)}/ptz_probe`, body),
  enableAI: (id: string) => http.post(`/channels/${encodeURIComponent(id)}/ai/enable`),
  disableAI: (id: string) => http.post(`/channels/${encodeURIComponent(id)}/ai/disable`),
  recordMode: (id: string, mode: string) => http.post(`/channels/${encodeURIComponent(id)}/record_mode`, { mode }),
  zones: (id: string) => http.get<Zone[] | { items?: Zone[] }>(`/channels/${encodeURIComponent(id)}/zones`),
  addZone: (id: string, body: Zone) => http.post<{ items?: Zone[] }>(`/channels/${encodeURIComponent(id)}/zones`, body),
  voiceStart: (id: string, body: Record<string, unknown> = {}) => http.post(`/channels/${encodeURIComponent(id)}/voice/start`, body),
  voiceStop: (id: string, body: Record<string, unknown> = {}) => http.post(`/channels/${encodeURIComponent(id)}/voice/stop`, body),
  historyStart: (id: string, body: Record<string, unknown>) => http.post(`/channels/${encodeURIComponent(id)}/history/start`, body),
  historyStop: (id: string, body: Record<string, unknown>) => http.post(`/channels/${encodeURIComponent(id)}/history/stop`, body),
  historyControl: (id: string, body: Record<string, unknown>) => http.post(`/channels/${encodeURIComponent(id)}/history/control`, body),
  queryDeviceRecords: (id: string, body: Record<string, unknown>) => http.post<Record<string, unknown>>(`/channels/${encodeURIComponent(id)}/records/query`, body),

  events: (params?: ListParams) => http.get<ApiPage<ApiEvent>>('/events', { params }),
  event: (id: number | string) => http.get<ApiEvent>(`/events/${id}`),
  deleteEvent: (id: number | string) => http.delete<ApiEvent>(`/events/${id}`),
  eventImage: (path: string) => withToken(apiUrl(`/events/image/${path.replace(/^\/+/, '')}`)),

  recordings: (params?: ListParams) => http.get<ApiPage<ApiRecording>>('/recordings', { params }),
  recording: (id: number | string) => http.get<ApiRecording>(`/recordings/${id}`),
  timeline: (params?: ListParams) => http.get<{ items?: TimelineRange[] }>('/recordings/timeline', { params }),
  monthly: (params?: ListParams) => http.get<MonthlyStats>('/recordings/monthly', { params }),
  downloadRecording: (id: number | string) => http.get<Blob>(`/recordings/${id}/download`, { responseType: 'blob', timeout: 0 }),
  recordingDownloadUrl: (id: number | string) => withToken(apiUrl(`/recordings/${id}/download`)),
  hlsPlaylist: (cid: string, startMs: number, endMs: number) => apiUrl(`/recordings/channels/${encodeURIComponent(cid)}/index.m3u8?start_ms=${startMs}&end_ms=${endMs}`),

  mediaServers: (params?: ListParams) => http.get<ApiPage<MediaServer>>('/media_servers', { params }),
  editMediaServer: (id: string, body: Record<string, unknown>) => http.put<MediaServer>(`/media_servers/${encodeURIComponent(id)}`, body),
  configInfo: () => http.get<{ sip?: SipConfig; access_info?: SipAccessInfo }>('/configs/info'),
  updateSip: (body: SipConfig) => http.put('/configs/info/sip', body),
}

export function apiUrl(path: string) {
  const rawBase = String(http.defaults.baseURL || '/')
  const base = new URL(rawBase.endsWith('/') ? rawBase : `${rawBase}/`, window.location.origin)
  return new URL(path.replace(/^\/+/, ''), base).toString()
}

export function withToken(url: string) {
  const token = localStorage.getItem('owl-token')
  return token ? `${url}${url.includes('?') ? '&' : '?'}token=${encodeURIComponent(token)}` : url
}

export function withQuery(url: string, params?: Record<string, string | number>) {
  if (!params) return url
  const next = new URL(url, window.location.origin)
  Object.entries(params).forEach(([key, value]) => next.searchParams.set(key, String(value)))
  return next.toString()
}

export function errorMessage(error: unknown, fallback = '请求失败，请稍后重试') {
  const body = (error as { response?: { data?: ApiErrorBody }; message?: string })?.response?.data
  return body?.msg || (error as { message?: string })?.message || fallback
}

export function typeLabel(type?: string, idHint?: string) {
  const normalized = String(type || '').trim().toUpperCase()
  const known = ({ GB28181: 'GB28181', ONVIF: 'ONVIF', RTMP: 'RTMP', RTSP: 'RTSP' } as Record<string, string>)[normalized]
  if (known) return known

  const hint = String(idHint || '').trim().toLowerCase()
  if (hint.startsWith('gb') || /^\d{18,20}$/.test(hint)) return 'GB28181'
  if (hint.startsWith('onvif')) return 'ONVIF'
  if (hint.startsWith('rtmp')) return 'RTMP'
  if (hint.startsWith('rtsp')) return 'RTSP'
  return type || '未知'
}

export function extOf(value: ApiDevice | Channel) { return (value.ext || {}) as DeviceExt }
