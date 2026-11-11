export type Decimal = string
export interface Subject { type: 'device' | 'platform'; id: string; binding_revision: Decimal }
export interface Page<T> { items: T[]; total: number; page: number; size: number; sampled_at: string; stale: boolean }
export interface DesiredConfig {
  name: string; group: string; description: string; url: string; platform_id: string;
  credential_id: string | null; default_media_server_id: string | null; proxy_ref: string | null;
  required_capabilities: string[]; public_ip: string; sip_port: number; media_port_start: number; media_port_end: number;
  max_media_sessions: number; max_transfer_sessions: number; max_to_a_bps: Decimal; max_to_device_bps: Decimal;
  heartbeat_interval_ms: number; heartbeat_timeout_ms: number; lease_ttl_ms: number;
  media_connect_timeout_ms: number; media_first_packet_timeout_ms: number; source_cidrs: string[]; task_public_base_url: string | null;
}
export interface Failure { code: string; message: string }
export interface Gateway {
  id: string; name: string; group: string; description: string; revision: Decimal; config_revision: Decimal; applied_revision: Decimal;
  desired_config?: DesiredConfig; applied_config?: Record<string, unknown>; admin_state: string; connection_state: string; health_state: string;
  pending_operation_id: string | null; capabilities: string[]; last_seen_at: string | null; last_error: Failure | null;
  counts: Record<string, number> | null; counts_error: Failure | null; metrics: Record<string, string | number | null>;
  metrics_stale: boolean; stale: boolean; sampled_at: string;
  certificate: { id: string; fingerprint: string; ca_fingerprint: string; not_before: string; not_after: string; status: string } | null;
}
export interface Operation { operation_id: string; gateway_id: string | null; type: string; status: string; step: string; progress: { completed: number; total: number }; result: unknown; error: Failure | null }
export interface Device { device_id: string; device_code: string; name: string; binding_revision: Decimal; online: boolean; channel_total: number; channel_online: number }
export interface Session { media_server_id: string | null; stream_id: string | null; traffic: { sampled_at: string; rx_bytes: Decimal; tx_bytes: Decimal; rx_bps: Decimal | null; tx_bps: Decimal | null; queue_bytes: Decimal } | null; id: string; resource_type: string; subject_type: string; subject_id: string; channel_id: string | null; purpose: string; state: string; wire_mode: string; local_mode: string; direction: string; ssrc: string | null; public_endpoint: unknown; local_endpoint: unknown; created_at: string; cleanup_steps: unknown; error: Failure | null }
export interface Event { event_id: string; type: string; occurred_at: string; actor: string; subject_id: string | null; reason: string; trace_id: string; details: unknown }
export interface MetricSeries { stale: boolean; step_seconds: number; points: { sampled_at: string; valid: boolean; values: Record<string, number | null> | null }[] }
export interface Migration { id: string; state: string; revision: Decimal; counts: Record<string, number>; items: { subject_id: string; state: string; error: Failure | null }[]; page: number; size: number }
