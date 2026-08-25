export interface ApiPage<T> {
  items?: T[]
  total?: number
}

export interface DeviceExt {
  manufacturer?: string
  model?: string
  firmware?: string
  name?: string
  gb_version?: string
  gb_declared_version?: string
  gb_effective_version?: string
  gb_manual_version?: string
  gb_version_source?: string
  gb_version_updated_at?: number
  gb_version_capabilities?: string[]
  gb_disabled_capabilities?: string[]
  gb_last_unsupported_command?: string
  gb_last_unsupported_version?: string
  gb_last_unsupported_updated_at?: number
  enabled_ai?: boolean
  record_mode?: 'always' | 'ai' | 'none' | string
  zones?: Zone[]
  ptz_verified?: boolean
}

export interface Zone {
  name: string
  coordinates: number[]
  color?: string
  labels?: string[]
}

export interface StreamConfig {
  media_server_id?: string
  push_addr?: string
  session?: string
  is_auth_disabled?: boolean
  source_url?: string
  transport?: number
  pushed_at?: string
  stopped_at?: string
  enabled?: boolean
  enabled_audio?: boolean
  timeout_s?: number
  enabled_disabled_none_reader?: boolean
  enabled_remove_none_reader?: boolean
  stream_key?: string
}

export interface ApiDevice {
  id: string
  type?: string
  device_id?: string
  name?: string
  transport?: string
  stream_mode?: number
  ip?: string
  port?: number
  address?: string
  is_online?: boolean
  registered_at?: string
  keepalive_at?: string
  keepalives?: number
  expires?: number
  channels?: number
  password?: string
  username?: string
  created_at?: string
  updated_at?: string
  ext?: DeviceExt
  children?: ApiChannel[]
  ptz_capable?: boolean
  ptz_verified?: boolean
}

export interface DeviceHistoryRecord {
  id: number
  device_id: string
  kind: 'heartbeat' | 'register' | string
  recorded_at?: string
  interval_seconds?: number
  address?: string
  status?: string
}

export interface ApiChannel {
  id: string
  did?: string
  device_id?: string
  channel_id?: string
  name?: string
  ptztype?: number
  is_online?: boolean
  is_playing?: boolean
  type?: string
  app?: string
  stream?: string
  ext?: DeviceExt
  config?: StreamConfig
  created_at?: string
  updated_at?: string
  has_recording?: boolean
  ptz_capable?: boolean
  ptz_verified?: boolean
}

export interface ApiEvent {
  id: number
  did?: string
  cid?: string
  label?: string
  model?: string
  score?: number
  zones?: string
  image_path?: string
  started_at?: string
  ended_at?: string
  created_at?: string
  updated_at?: string
}

export interface ApiRecording {
  id: number
  cid?: string
  app?: string
  stream?: string
  started_at?: string
  ended_at?: string
  duration?: number
  size?: number
  path?: string
  object_count?: number
  delete_flag?: boolean
}

export interface TimelineRange {
  id: number
  start_ms: number
  end_ms: number
  duration?: number
  object_count?: number
  delete_flag?: boolean
}

export interface MonthlyStats {
  year: number
  month: number
  days: number
  has_video: string
}

export interface MediaServerPorts {
  http?: number
  https?: number
  rtmp?: number
  flv?: number
  flvs?: number
  ws_flv?: number
  ws_flvs?: number
  rtmps?: number
  rtpporxy?: number
  rtsp?: number
  rtsps?: number
}

export interface MediaServer {
  id: string
  type?: string
  ip?: string
  hook_ip?: string
  stream_ip?: string
  sdp_ip?: string
  status?: boolean
  last_keepalive_at?: string
  ports?: MediaServerPorts
  rtpport_range?: string
  record_path?: string
  record_day?: number
  auto_config?: boolean
  secret?: string
  hook_alive_interval?: number
}

export interface SipConfig {
  host?: string
  port?: number
  id?: string
  domain?: string
  password?: string
  enable_tls?: boolean
  tls_port?: number
  tls_cert?: string
  tls_key?: string
  tls_client_ca?: string
  tls_require_client_cert?: boolean
  register_certificate_auth?: {
    enabled?: boolean
    required?: boolean
    platform_cert?: string
    platform_key?: string
    device_ca?: string
    crl?: string
    device_certificates?: Record<string, string>
  }
  strict_source_check?: boolean
  require_message_auth?: boolean
  ptz_weak_confirm?: boolean
  device_history?: { max_records?: number; max_days?: number }
  direct_tcp_download?: Record<string, unknown>
  upstreams?: SipUpstream[]
}

export interface SipUpstream {
  name: string
  enabled: boolean
  server_id: string
  domain?: string
  host: string
  port: number
  transport?: "udp" | "tcp" | "tls"
  tls_ca?: string
  tls_cert?: string
  tls_key?: string
  tls_server_name?: string
  local_id?: string
  local_domain?: string
  local_host?: string
  local_port?: number
  password?: string
  register_certificate_auth?: {
    enabled?: boolean
    required?: boolean
    local_cert?: string
    local_key?: string
    server_cert?: string
    server_ca?: string
    crl?: string
  }
  version: "1.0" | "1.1" | "2.0" | "3.0"
  expires: number
  keepalive_interval: number
  shared_channels?: string[]
  channel_id_map?: Record<string, string>
  media_allowed_cidrs?: string[]
}

export interface CascadePlatformStatus {
  name: string
  server_id: string
  address: string
  configured_version: string
  negotiated_version?: string
  state: string
  registered: boolean
  last_register_at?: string
  last_keepalive_at?: string
  expires_at?: string
  last_error?: string
}

export interface SipAccessInfo {
  server_ip?: string
  id?: string
  domain?: string
  port?: number
  password?: string
}

export interface HealthInfo {
  version?: string
  start_at?: string
  git_branch?: string
  git_hash?: string
}

export interface ApiMetrics {
  real_time_requests?: number
  total_requests?: number
  total_responses?: number
  request_top10?: { Key?: string; Value?: number; key?: string; value?: number }[]
  status_code_top10?: { Key?: string; Value?: number; key?: string; value?: number }[]
  goroutines?: number
  num_gc?: number
  sys_alloc?: number
  start_at?: string
}

export interface ResourcePoint { time?: string; used?: number; up?: number; down?: number }
export interface ResourceStats {
  cpu?: ResourcePoint[]
  mem?: ResourcePoint[]
  net?: ResourcePoint[]
  disk?: { name?: string; used?: number; total?: number }[]
}

export interface GbMetrics {
  register_requests?: number
  register_success?: number
  register_failures?: number
  catalog_requests?: number
  catalog_success?: number
  catalog_timeouts?: number
  catalog_partial?: number
  media_requests?: number
  media_success?: number
  media_failures?: number
  media_disconnects?: number
  direct_tcp_started?: number
  direct_tcp_completed?: number
  direct_tcp_failed?: number
  direct_tcp_cancelled?: number
  direct_tcp_bytes?: number
}

export interface VersionCheck {
  has_new_version?: boolean
  current_version?: string
  new_version?: string
  description?: string
}

export interface PlayAddress {
  label?: string
  schema?: string
  url?: string
  type?: string
  flv?: string
  http_flv?: string
  'http-flv'?: string
  ws_flv?: string
  'ws-flv'?: string
  hls?: string
  webrtc?: string
  rtmp?: string
  rtsp?: string
  [key: string]: unknown
}

export interface PlayResult {
  app?: string
  stream?: string
  items?: PlayAddress[]
}

export interface ApiErrorBody {
  msg?: string
  reason?: string
  trace_id?: string
  details?: unknown
}
