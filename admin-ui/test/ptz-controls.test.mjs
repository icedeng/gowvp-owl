import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import ts from 'typescript'

const { outputText } = ts.transpileModule(
  readFileSync(new URL('../src/utils/ptz.ts', import.meta.url), 'utf8'),
  { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } },
)
const { buildPTZRequest, ptzActionDisabledReason } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)
const context = (overrides = {}) => ({ channel: { id: 'ch-test', type: 'GB28181', is_online: true, ptz_capable: false }, gbVersion: '1.0', deviceAvailable: true, ...overrides })
const form = (overrides = {}) => ({
  speed: 30, duration: 2, preset: 3, group: 4, cruiseSpeed: 40, cruiseStay: 10,
  scanGroup: 5, scanSpeed: 50, aux: 6, auxValue: 1, homeSeconds: 60, homePreset: 7,
  pan: 180, tilt: 30, zoom: 2, ...overrides,
})

test('四版本的基础 PTZ 和预置位不依赖扩展能力声明或硬件能力探测', () => {
  for (const gbVersion of ['1.0', '1.1', '2.0', '3.0']) {
    for (const action of ['left_up', 'zoom_in', 'focus_near', 'iris_open', 'preset_set', 'preset_call', 'preset_delete', 'cruise_add', 'cruise_start', 'scan_start', 'aux_on']) {
      assert.equal(ptzActionDisabledReason(action, context({ gbVersion, gbCapabilities: [] })), '', `${gbVersion}/${action}`)
    }
  }
})

test('看守和精准定位按最低协议版本以及显式能力声明分别禁用', () => {
  for (const gbVersion of ['1.0', '1.1']) assert.match(ptzActionDisabledReason('home_enable', context({ gbVersion })), /协议版本不支持.*2016\/2022/)
  for (const gbVersion of ['1.0', '1.1', '2.0']) assert.match(ptzActionDisabledReason('ptz_precise', context({ gbVersion })), /协议版本不支持.*2022/)
  assert.equal(ptzActionDisabledReason('home_enable', context({ gbVersion: '2.0' })), '')
  assert.equal(ptzActionDisabledReason('ptz_precise', context({ gbVersion: '3.0', gbCapabilities: ['ptz_position'] })), '')
  assert.match(ptzActionDisabledReason('home_enable', context({ gbVersion: '3.0', gbCapabilities: [] })), /未声明/)
  assert.match(ptzActionDisabledReason('ptz_precise', context({ gbVersion: '' })), /尚未识别/)
  assert.match(ptzActionDisabledReason('ptz_precise', context({ gbVersion: '3.0', deviceAvailable: false })), /设备信息/)
})

test('ONVIF 的适配限制不会误报成协议本身不支持', () => {
  const onvif = context({ channel: { id: 'onvif-test', type: 'ONVIF', is_online: true } })
  for (const action of ['up', 'right_down', 'zoom_out', 'iris_open', 'preset_call', 'stop']) assert.equal(ptzActionDisabledReason(action, onvif), '')
  for (const action of ['cruise_add', 'scan_start', 'aux_on', 'home_enable', 'ptz_precise']) assert.match(ptzActionDisabledReason(action, onvif), /ONVIF 接入暂未实现/)
  assert.match(ptzActionDisabledReason('focus_near', onvif), /尚未实现聚焦自动停止/)
  assert.equal(buildPTZRequest('up', form({ duration: 5 }), onvif).body.timeout, 1)
})

test('协议不支持、离线、忙碌均禁止实际构造控制请求', () => {
  const unavailable = [
    [context({ channel: { id: 'rtsp', type: 'RTSP', is_online: true, ptz_capable: true } }), /协议不支持/],
    [context({ channel: { id: 'offline', type: 'GB28181', is_online: false } }), /离线/],
    [context({ busy: true }), /请稍候/],
  ]
  for (const [ctx, reason] of unavailable) assert.throws(() => buildPTZRequest('left', form(), ctx), reason)
})

test('动作请求携带对应参数，保持停止与探测独立于无效表单', () => {
  assert.deepEqual(buildPTZRequest('left_up', form(), context()), { kind: 'ptz', body: { action: 'left_up', speed: 30, timeout: 2 } })
  assert.deepEqual(buildPTZRequest('preset_call', form(), context()).body, { action: 'preset_call', preset: 3 })
  assert.deepEqual(buildPTZRequest('cruise_add', form(), context()).body, { action: 'cruise_add', preset: 3, group: 4 })
  assert.deepEqual(buildPTZRequest('cruise_stay', form(), context()).body, { action: 'cruise_stay', group: 4, value: 10 })
  assert.deepEqual(buildPTZRequest('scan_speed', form(), context()).body, { action: 'scan_speed', group: 5, value: 50 })
  assert.deepEqual(buildPTZRequest('aux_off', form(), context()).body, { action: 'aux_off', aux: 6, value: 1 })
  const invalid = form({ speed: '', duration: 0, preset: 999, group: 0, scanGroup: 0 })
  for (const action of ['stop', 'fi_stop', 'cruise_stop', 'scan_stop']) assert.deepEqual(buildPTZRequest(action, invalid, context()), { kind: 'ptz', body: { action } })
  assert.deepEqual(buildPTZRequest('probe', invalid, context()), { kind: 'probe', body: { action: 'stop', timeout: 5 } })
})

test('无效数值不能溢出协议字节或12位参数', () => {
  for (const [action, field, values] of [
    ['up', 'speed', [0, 256, 1.5, '', NaN]], ['up', 'duration', [0, 11, Infinity]],
    ['preset_set', 'preset', [0, 256]], ['cruise_start', 'group', [0, 256]],
    ['cruise_speed', 'cruiseSpeed', [0, 4096]], ['cruise_stay', 'cruiseStay', [0, 4096]],
    ['scan_start', 'scanGroup', [0, 256]], ['scan_speed', 'scanSpeed', [0, 4096]],
    ['aux_on', 'aux', [0, 256]], ['aux_on', 'auxValue', [0, 4096]],
  ]) for (const value of values) assert.throws(() => buildPTZRequest(action, form({ [field]: value }), context()), /应为/, `${action}/${field}/${value}`)
  assert.equal(buildPTZRequest('scan_speed', form({ scanSpeed: 4095 }), context()).body.value, 4095)
})

test('看守关闭省略启用专用字段，精准定位遵循协议范围', () => {
  const latest = context({ gbVersion: '3.0' })
  assert.deepEqual(buildPTZRequest('home_enable', form(), latest), { kind: 'gb', body: { action: 'home_position', home_position: { enabled: 1, reset_time: 60, preset_index: 7 } } })
  assert.deepEqual(buildPTZRequest('home_disable', form({ homeSeconds: -1, homePreset: 999 }), latest), { kind: 'gb', body: { action: 'home_position', home_position: { enabled: 0 } } })
  assert.deepEqual(buildPTZRequest('ptz_precise', form(), latest), { kind: 'gb', body: { action: 'ptz_precise', ptz_precise: { pan: 180, tilt: 30, zoom: 2 } } })
  for (const overrides of [{ pan: 361 }, { tilt: -31 }, { tilt: 91 }, { zoom: 0 }, { pan: NaN }, { zoom: Infinity }]) assert.throws(() => buildPTZRequest('ptz_precise', form(overrides), latest), /超出有效范围/)
})
