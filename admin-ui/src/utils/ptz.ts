import type { ApiChannel } from '../types/api'

// 未声明硬件能力的国标通道仍可试控，协议支持下发不等于云台已验证。
export function canAttemptPTZ(channel?: ApiChannel | null): boolean {
  return Boolean(channel && (channel.ptz_capable || ['GB28181', 'ONVIF'].includes(channel.type || '')))
}

export function ptzCapabilityLabel(channel?: ApiChannel | null): string {
  if (!canAttemptPTZ(channel)) return '当前协议不支持云台控制'
  return channel?.ptz_capable ? '设备声明支持，实际动作需观察画面确认' : '云台能力未知，可手动试控'
}

export interface PTZContext {
  channel: ApiChannel
  gbVersion?: string
  gbCapabilities?: string[]
  deviceAvailable?: boolean
  busy?: boolean
}

export interface PTZParameters {
  speed: number
  duration: number
  preset: number
  group: number
  cruiseSpeed: number
  cruiseStay: number
  scanGroup: number
  scanSpeed: number
  aux: number
  auxValue: number
  homeSeconds: number
  homePreset: number
  pan: number
  tilt: number
  zoom: number
}

export type PTZRequest = { kind: 'ptz' | 'gb' | 'probe'; body: Record<string, unknown> }

export function ptzActionDisabledReason(action: string, context: PTZContext): string {
  const { channel, gbVersion, gbCapabilities } = context
  if (!['GB28181', 'ONVIF'].includes(channel.type || '')) return '当前协议不支持云台控制'
  const extended = ['home_enable', 'home_disable', 'ptz_precise'].includes(action)
  if (channel.type === 'ONVIF') {
    if (/^(cruise_|scan_|aux_|home_)/.test(action) || action === 'ptz_precise') {
      return '当前 ONVIF 接入暂未实现该操作'
    }
    if (action.startsWith('focus_') || action === 'fi_stop') {
      return '当前 ONVIF 接入尚未实现聚焦自动停止，暂不可用'
    }
  }
  if (extended) {
    const home = action.startsWith('home_')
    const versions = home ? ['2.0', '3.0'] : ['3.0']
    if (!gbVersion) return '国标协议版本尚未识别，暂不可用'
    if (!versions.includes(gbVersion)) return `当前国标协议版本不支持，需 GB28181-${home ? '2016/2022' : '2022'}`
    if (gbCapabilities !== undefined && !gbCapabilities.includes(home ? 'home_position' : 'ptz_position')) {
      return '当前接入未声明支持该国标扩展能力'
    }
    if (!context.deviceAvailable) return '所属设备信息尚未就绪'
  }
  if (!channel.is_online) return '通道离线，无法发送控制指令'
  if (context.busy) return '正在下发操作，请稍候'
  return ''
}

// 参数按动作校验，错误的巡航参数不能阻止用户下发停止命令。
export function buildPTZRequest(action: string, form: PTZParameters, context: PTZContext): PTZRequest {
  const reason = ptzActionDisabledReason(action, context)
  if (reason) throw new Error(reason)
  const integer = (value: number, min: number, max: number, label: string) => {
    if (!Number.isInteger(value) || value < min || value > max) throw new Error(`${label}应为 ${min}–${max} 的整数`)
    return value
  }
  const finite = (value: number, min: number, max: number, label: string) => {
    if (!Number.isFinite(value) || value < min || value > max) throw new Error(`${label}超出有效范围`)
    return value
  }
  if (action === 'probe') return { kind: 'probe', body: { action: 'stop', timeout: 5 } }
  if (action === 'home_enable' || action === 'home_disable') {
    return { kind: 'gb', body: { action: 'home_position', home_position: action === 'home_disable' ? { enabled: 0 } : {
      enabled: 1,
      reset_time: integer(form.homeSeconds, 1, 86400, '看守等待秒数'),
      preset_index: integer(form.homePreset, 1, 255, '看守预置位'),
    } } }
  }
  if (action === 'ptz_precise') return { kind: 'gb', body: { action, ptz_precise: {
    pan: finite(form.pan, 0, 360, '水平角'),
    tilt: finite(form.tilt, -30, 90, '俯仰角'),
    zoom: finite(form.zoom, 1, Number.MAX_SAFE_INTEGER, '变倍倍率'),
  } } }
  const body: Record<string, unknown> = { action }
  const timed = ['left', 'right', 'up', 'down', 'left_up', 'left_down', 'right_up', 'right_down', 'zoom_in', 'zoom_out', 'focus_near', 'focus_far', 'iris_open', 'iris_close'].includes(action)
  if (timed) {
    body.speed = integer(form.speed, 1, 255, '速度')
    // ONVIF 适配器当前固定 1 秒；界面同步限制，避免显示与实际行为不一致。
    body.timeout = context.channel.type === 'ONVIF' ? 1 : integer(form.duration, 1, 10, '点动时长')
  }
  if (action.startsWith('preset_') || ['cruise_add', 'cruise_del'].includes(action)) body.preset = integer(form.preset, 1, 255, '预置位编号')
  if (action.startsWith('cruise_') && action !== 'cruise_stop') body.group = integer(form.group, 1, 255, '巡航组')
  if (action === 'cruise_speed') body.value = integer(form.cruiseSpeed, 1, 4095, '巡航速度')
  if (action === 'cruise_stay') body.value = integer(form.cruiseStay, 1, 4095, '停留时间')
  if (action.startsWith('scan_') && action !== 'scan_stop') body.group = integer(form.scanGroup, 1, 255, '扫描组')
  if (action === 'scan_speed') body.value = integer(form.scanSpeed, 1, 4095, '扫描速度')
  if (action.startsWith('aux_')) {
    body.aux = integer(form.aux, 1, 255, '辅助开关编号')
    body.value = integer(form.auxValue, 1, 4095, '辅助参数')
  }
  return { kind: 'ptz', body }
}
