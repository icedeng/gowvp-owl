import { http } from '../../services/http'
import type { DesiredConfig, Gateway, Page, Operation, Device, Session, Event, MetricSeries, Migration, Subject, ChannelDiagnostic } from './types'
const root = '/access_gateways'
const path = (id: string) => `${root}/${encodeURIComponent(id)}`
export function mutationHeaders(key: string, revision?: string) {
  return { 'Idempotency-Key': key, ...(revision ? { 'If-Match': `"${revision}"` } : {}) }
}
export const gatewayAPI = {
  migrateSubjects: async (source: string | null, target: string | null, subjects: Subject[], expires_at: string, key: string) => (await http.post<Operation>('/access_gateway_migrations', { source_gateway_id: source, target_gateway_id: target, subjects, expires_at }, { headers: mutationHeaders(key) })).data,
  list: async (params: Record<string, string | number> = {}, signal?: AbortSignal) => (await http.get<Page<Gateway>>(root, { params, signal })).data,
  get: async (id: string, signal?: AbortSignal) => (await http.get<Gateway>(path(id), { signal })).data,
  create: async (id: string, config: DesiredConfig, key: string) => (await http.post<Gateway>(root, { id, config, enable: false }, { headers: mutationHeaders(key) })).data,
  update: async (g: Gateway, config: DesiredConfig, key: string) => (await http.put<Gateway | Operation>(path(g.id), { config }, { headers: mutationHeaders(key, g.revision) })).data,
  action: async (g: Gateway, action: string, body: unknown, key: string) => (await http.request<Operation>({ url: `${path(g.id)}/${action}`, method: action === 'state' ? 'PUT' : 'POST', data: body, headers: mutationHeaders(key, ['state', 'certificate-rotation'].includes(action) ? g.revision : undefined) })).data,
  remove: async (g: Gateway, key: string) => (await http.delete(path(g.id), { headers: mutationHeaders(key, g.revision) })).data,
  credential: async (id: string, form: FormData, key: string) => (await http.post<{ id: string }>(`${path(id)}/credentials`, form, { headers: mutationHeaders(key) })).data,
  diagnoseChannel: async (g: Gateway, channel_id: string, key: string) => (await http.post<ChannelDiagnostic>(`${path(g.id)}/channel-diagnostic`, { channel_id }, { headers: mutationHeaders(key, g.revision), timeout: 65000 })).data,
  probe: async (id: string, key: string) => (await http.post<{ can_enable: boolean; checks: { name: string; status: string; msg: string }[] }>(`${path(id)}/test`, {}, { headers: mutationHeaders(key) })).data,
  operation: async (id: string, op: string, signal?: AbortSignal) => (await http.get<Operation>(`${path(id)}/operations/${encodeURIComponent(op)}`, { signal })).data,
  devices: async (id: string, page: number, signal?: AbortSignal, filters: Record<string, string> = {}) => (await http.get<Page<Device>>(`${path(id)}/devices`, { params: { page, size: 20, ...filters }, signal })).data,
  sessions: async (id: string, page: number, signal?: AbortSignal, filters: Record<string, string> = {}) => (await http.get<Page<Session>>(`${path(id)}/sessions`, { params: { page, size: 20, ...filters }, signal })).data,
  events: async (id: string, page: number, signal?: AbortSignal, filters: Record<string, string> = {}) => (await http.get<Page<Event>>(`${path(id)}/events`, { params: { page, size: 20, ...filters }, signal })).data,
  platforms: async (id: string, page: number, signal?: AbortSignal, filters: Record<string, string> = {}) => (await http.get<Page<Record<string, unknown>>>(`${path(id)}/platforms`, { params: { page, size: 20, ...filters }, signal })).data,
  metrics: async (id: string, hours: number, signal?: AbortSignal) => (await http.get<MetricSeries>(`${path(id)}/metrics`, { params: { start: new Date(Date.now() - hours * 3600000).toISOString(), end: new Date().toISOString(), step_seconds: hours > 24 ? 300 : 60 }, signal })).data,
  template: async (id: string, format: string) => (await http.get<{ content: string; warnings: string[] }>(`${path(id)}/deployment-template`, { params: { format } })).data,
  migrate: async (source: string | null, target: string | null, devices: Device[], key: string) => (await http.post<Operation>('/access_gateway_migrations', { source_gateway_id: source, target_gateway_id: target, subjects: devices.map(d => ({ type: 'device', id: d.device_id, binding_revision: d.binding_revision })), expires_at: new Date(Date.now() + 3600000).toISOString() }, { headers: mutationHeaders(key) })).data,
  migration: async (id: string, page = 1, signal?: AbortSignal) => (await http.get<Migration>(`/access_gateway_migrations/${encodeURIComponent(id)}`, { params: { page, size: 20 }, signal })).data,
  cancelMigration: async (m: Migration, reason: string, key: string) => (await http.post(`/access_gateway_migrations/${encodeURIComponent(m.id)}/cancel`, { reason }, { headers: mutationHeaders(key, m.revision) })).data,
}
