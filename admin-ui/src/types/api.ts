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
  gb_registration_closed?: boolean
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

export interface GBOperationOutput {
  sn?: number
  cmd_type?: string
  device_id?: string
  target_id?: string
  result?: string
  xml?: string
  data?: unknown
  appendix_a4?: unknown[]
  incomplete?: Record<string, unknown>
}

export interface GBSubscriptionState {
  device_id: string
  target_id: string
  event: 'alarm' | 'catalog' | 'mobile_position' | 'ptz_position' | string
  status: 'active' | 'refreshing' | 'recovering' | 'blocked' | 'terminating' | 'expired' | string
  expires: number
  expires_at?: string
  refresh_at?: string
  persisted?: boolean
  updated_at?: string
  next_attempt_at?: string
  last_error?: string
  retry_blocked?: boolean
  termination_reason?: string
  refreshing?: boolean
  cancel_pending?: boolean
  notify_cseq?: number
  notify_expires_at?: string
  start_alarm_priority?: string
  end_alarm_priority?: string
  alarm_method?: string
  alarm_type?: string
  start_alarm_time?: string
  end_alarm_time?: string
  start_time?: string
  end_time?: string
  interval?: number
}

export interface GBUpgradeOutput {
  sn?: number
  device_id?: string
  channel_id?: string
  session_id: string
  result?: string
}

export interface GBUpgradeState extends GBUpgradeOutput {
  status: string
  firmware?: string
  failed_reason?: string
  updated_at?: string
}

export interface GBSnapshotState {
  device_id?: string
  channel_id?: string
  cover_key?: string
  session_id: string
  status: string
  expected_count?: number
  received_count?: number
  file_ids?: string[]
  updated_at?: string
}

export interface SnapshotRefreshOutput {
  link?: string
  method?: string
  attempts?: string[]
  session_id?: string
}

export interface GBHistoryDownloadState {
  session_id: string
  device_id?: string
  channel_id?: string
  transport?: 'rtp' | 'direct_tcp' | string
  status: string
  received?: number
  file_size?: number
  file_size_known?: boolean
  bytes_speed?: number
  progress_percent?: number
  progress_known?: boolean
  approximate?: boolean
  size_verified?: boolean
  output?: string
  sha256?: string
  started_at?: string
  updated_at?: string
  completed_at?: string
  end_reason?: string
  error?: string
}

export interface GBHistoryStartOutput {
  msg?: string
  download?: GBHistoryDownloadState
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
  register_redirect?: string
  register_certificate_auth?: SipRegisterCertificateAuth
  signal_digest?: SipSignalDigest
  strict_source_check?: boolean
  require_message_auth?: boolean
  ptz_weak_confirm?: boolean
  device_history?: { max_records?: number; max_days?: number }
  direct_tcp_download?: SipDirectTcpDownload
  annex_g?: SipAnnexG
  // GB/T 28181 9.4 本域接警 SIP 客户端，默认关闭。
  alarm_receivers?: SipAlarmReceiver[]
  upstreams?: SipUpstream[]
  secret_clears?: SipSecretClearInput
}

export type DurationValue = number | string

export interface SipRegisterCertificateAuth {
  enabled?: boolean
  required?: boolean
  platform_cert?: string
  platform_key?: string
  device_ca?: string
  crl?: string
  device_certificates?: Record<string, string>
}

export interface SipSignalDigest {
  enabled?: boolean
  required?: boolean
  seed?: string
  algorithm?: 'MD5' | 'SHA-1' | 'SHA-256' | 'SM3' | string
  encoding?: 'base64' | 'hex' | string
  accept_legacy_hex?: boolean
  window?: DurationValue
}

export interface SipDirectTcpDownload {
  enabled?: boolean
  cascade_relay_enabled?: boolean
  device_allowlist?: string[]
  storage_dir?: string
  retain_days?: number
  offer_port?: number
  relay_listen_ip?: string
  relay_advertise_ip?: string
  relay_port_start?: number
  relay_port_end?: number
  max_file_size?: number
  global_concurrency?: number
  device_concurrency?: number
  dial_timeout?: DurationValue
  first_byte_timeout?: DurationValue
  idle_timeout?: DurationValue
  total_timeout?: DurationValue
  allow_address_mismatch?: boolean
  allowed_address_cidrs?: string[]
}

export interface SipAnnexGSystem {
  id: string
  role: 'emergency_command_system' | 'tollgate_system' | 'city_information_system' | string
  version: '1.0' | '1.1' | '2.0' | string
  password?: string
  signal_digest_seed?: string
  realm?: string
  address: string
  transport?: 'udp' | 'tcp' | 'tls' | string
  source_cidrs?: string[]
  allow_insecure_transport?: boolean
  tls_ca?: string
  tls_server_name?: string
  tls_cert?: string
  tls_key?: string
}

export interface SipAnnexG {
  enabled?: boolean
  max_send_records?: number
  inbound_rate?: number
  inbound_burst?: number
  pending_ttl?: DurationValue
  max_pending?: number
  systems?: SipAnnexGSystem[]
}

export interface SipSecretClearInput {
  signal_digest_seed?: boolean
  upstream_passwords?: string[]
  upstream_signal_digest_seeds?: string[]
  annex_g_passwords?: string[]
  annex_g_signal_digest_seeds?: string[]
}

export interface SipPeerSecretStatus {
  password_configured?: boolean
  signal_digest_seed_configured?: boolean
}

export interface SipSecretStatus {
  signal_digest_seed_configured?: boolean
  upstreams?: Record<string, SipPeerSecretStatus>
  annex_g_systems?: Record<string, SipPeerSecretStatus>
}

export interface SipAlarmReceiver {
  name: string
  enabled: boolean
  device_id: string
  source_ids?: string[]
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
  signal_digest_seed?: string
  monitor_user_identity?: {
    enabled?: boolean
    required?: boolean
    local_gateway_id?: string
    remote_gateway_id?: string
    local_user_id?: string
    local_organization?: string
    local_category?: string
    local_rank?: string
    trusted_gateway_ids?: string[]
    allowed_user_ids?: string[]
    allowed_organizations?: string[]
    allowed_categories?: string[]
    allowed_ranks?: string[]
    max_hops?: number
  }
  version: "1.0" | "1.1" | "2.0" | "3.0"
  expires: number
  keepalive_interval: number
  alarm_dispatch_enabled?: boolean
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
  annex_g_inbound_requests?: number
  annex_g_inbound_accepted?: number
  annex_g_inbound_rejected?: number
  annex_g_inbound_rate_limited?: number
  annex_g_business_failures?: number
  annex_g_pending?: number
}

export interface AnnexGAlarmAudit {
  id: number
  kind: 'mp' | 'ecs' | 'tgs' | string
  alarm_no?: string
  alarm_time?: string
  device_id?: string
  alarm_class?: string
  alarm_priority?: string
  alarm_method?: string
  alarm_address?: string
  tollgate_id?: string
  car_plate?: string
  plate_type?: string
  payload?: Record<string, unknown>
  created_at?: string
}

export interface AnnexGDefenceState {
  id: number
  tollgate_id?: string
  car_plate?: string
  plate_type?: string
  defence_type?: string
  active?: boolean
  defence_time?: string
  updated_at?: string
}

export interface AnnexGDefenceAudit extends AnnexGDefenceState {
  created_at?: string
}

export interface AnnexGAuditPage<T> extends ApiPage<T> {
  page?: number
  page_size?: number
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
