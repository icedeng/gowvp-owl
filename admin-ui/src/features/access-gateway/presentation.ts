export const adminStates = { draft: '草稿', enabled: '已启用', draining: '排空中', disabled: '已停用' }
export const connectionStates = { disconnected: '未连接', connecting: '连接中', authenticating: '认证中', connected: '已连接', reconnecting: '重连中' }
export const healthStates = { unknown: '待确认', ready: '就绪', degraded: '性能降级', unavailable: '不可用' }
const labels: Record<string, string> = {
  ...adminStates, ...connectionStates, ...healthStates,
  staged: '待使用', active: '使用中', retired: '已退役', revoked: '已撤销', expired: '已过期', valid: '有效', expiring: '即将到期',
  accepted: '已受理', running: '执行中', waiting_external: '等待外部响应', succeeded: '已完成', failed: '失败', cancelled: '已取消',
  pending: '待处理', completed: '已完成', committed: '已提交', drained: '已排空',
  pass: '通过', passed: '通过', ok: '通过', success: '通过', fail: '未通过', skipped: '未执行', warning: '需关注',
  online: '在线', offline: '离线', live: '实时预览', playback: '录像回放', download: '录像下载', talk: '语音对讲', broadcast: '语音广播',
  opening: '建立中', receiving: '接收中', closing: '关闭中', closed: '已关闭', direct: '直连', gateway: '网关', waiting_register: '等待重新注册',
  state: '状态变更', reconnect: '重新连接', config_update: '配置更新', certificate_rotation: '证书轮换', migration: '入口迁移', migration_cancel: '取消迁移',
}
export const probeLabels: Record<string, string> = { config: '配置校验', identity: '身份校验', dns: '域名解析', tcp: 'TCP 连接', tls: 'TLS 安全连接', mtls: '双向证书认证', protocol: '协议兼容性', capabilities: '协议能力' }
export const configLabels: Record<string, string> = {
  name: '网关名称', group: '分组', description: '备注', url: 'WSS 地址', platform_id: 'A 平台身份', credential_id: '凭据 ID', default_media_server_id: '默认媒体节点', proxy_ref: '代理引用',
  required_capabilities: '必要能力', public_ip: '公网通告 IP', sip_port: 'SIP 端口', media_port_start: '媒体起始端口', media_port_end: '媒体结束端口',
  max_media_sessions: '媒体会话上限', max_transfer_sessions: '文件任务上限', max_to_a_bps: '到 A 带宽上限（bps）', max_to_device_bps: '到设备带宽上限（bps）',
  heartbeat_interval_ms: '心跳间隔（ms）', heartbeat_timeout_ms: '心跳超时（ms）', lease_ttl_ms: '租约有效期（ms）', media_connect_timeout_ms: '媒体连接超时（ms）', media_first_packet_timeout_ms: '媒体首包超时（ms）', source_cidrs: '允许的媒体来源 CIDR', task_public_base_url: '任务 HTTPS 公网地址',
}
export function stateLabel(value: unknown) { return value == null || value === '' ? '—' : labels[String(value)] || String(value) }
export function stateTone(value: string) {
  if (['enabled', 'connected', 'ready', 'active', 'valid', 'succeeded', 'completed', 'committed', 'online', 'pass', 'passed', 'ok', 'success'].includes(value)) return 'online'
  if (['failed', 'unavailable', 'revoked', 'expired', 'fail'].includes(value)) return 'danger'
  if (['draining', 'degraded', 'reconnecting', 'waiting_external', 'expiring', 'warning'].includes(value)) return 'warning'
  if (['connecting', 'authenticating', 'accepted', 'running', 'receiving', 'opening'].includes(value)) return 'info'
  return ''
}
export const metricLabels: Record<string, string> = {
  media_sessions: '媒体会话', transfer_sessions: '文件任务', sip_flows: 'SIP 连接', port_used: '已用端口', cpu_percent: 'CPU 使用率（%）',
  rss_bytes: '内存占用（B）', to_a_bps: '到 A 带宽（bps）', to_device_bps: '到设备带宽（bps）', device_online: '在线设备', active_sources: '活动媒体源', queue_bytes: '队列大小（B）', rtt_ms: '往返时延（ms）',
}
