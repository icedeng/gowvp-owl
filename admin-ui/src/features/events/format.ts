import type { ApiEvent, EventStatus } from '../../types/api'
export const statusLabels: Record<EventStatus, string> = { pending: '待处理', processing: '处理中', resolved: '已处理', ignored: '已忽略' }
export function isAlarm(item: ApiEvent) { return /gb28181|alarm|报警/i.test(`${item.model || ''}${item.label || ''}`) }
export function alarmData(item: ApiEvent): Record<string, unknown> {
 try { const value: unknown = JSON.parse(item.zones || '{}'); return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {} } catch { return {} }
}
export function eventTitle(item: ApiEvent) {
 if (!isAlarm(item)) return item.label || '未分类事件'
 const data = alarmData(item)
 const methods: Record<string, string> = { '1': '电话报警', '2': '设备报警', '3': '短信报警', '4': 'GPS 报警', '5': '视频报警', '6': '设备故障', '7': '其他报警' }
 const types: Record<string, Record<string, string>> = {
 '2': { '1': '视频丢失', '2': '设备防拆', '3': '存储磁盘满', '4': '设备高温', '5': '设备低温' },
 '5': { '1': '人工视频报警', '2': '移动侦测', '3': '遗留物检测', '4': '物体移除检测', '5': '绊线检测', '6': '入侵检测', '7': '逆行检测', '8': '徘徊检测', '9': '流量统计', '10': '密度检测', '11': '视频异常检测', '12': '快速移动' },
 '6': { '1': '存储磁盘故障', '2': '存储风扇故障' },
 }
 const method = String(data.alarm_method || item.label?.replace(/^gb_alarm_/, '') || '')
 const type = String(data.alarm_type || '')
 return types[method]?.[type] || `${methods[method] || '设备报警'}${type ? `（类型 ${type}）` : ''}`
}
export function alarmSummary(item: ApiEvent) {
 const data = alarmData(item)
 const versions: Record<string, string> = { '1.0': '2011', '1.1': '2014', '2.0': '2016', '3.0': '2022' }
 const phases: Record<string, string> = { '1': '进入', '2': '离开' }
 return [data.protocol_version ? `国标 ${versions[String(data.protocol_version)] || data.protocol_version}` : item.model, data.source_method, data.alarm_priority ? `等级 ${data.alarm_priority}` : '', data.event_type != null ? phases[String(data.event_type)] || `阶段 ${data.event_type}` : '', data.alarm_description].filter(Boolean).join(' · ')
}
export function deviceLabel(item: ApiEvent) { return item.device_name || item.device_code || String(alarmData(item).device_id || '') || item.did || (item.cid ? '独立通道' : '设备档案已移除') }
export function channelLabel(item: ApiEvent) { return item.channel_name || item.channel_code || String(alarmData(item).channel_id || '') || item.cid || '设备级事件' }
export function evidenceLabel(state?: string) { return ({ queued: '排队中', working: '取证中', succeeded: '已保存', failed: '失败', skipped: '已跳过', not_requested: '未启用' } as Record<string, string>)[state || ''] || '未启用' }
