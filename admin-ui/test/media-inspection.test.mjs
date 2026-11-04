import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import ts from 'typescript'

function moduleURL(source) {
  const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } })
  return `data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`
}
const formatURL = moduleURL(readFileSync(new URL('../src/utils/format.ts', import.meta.url), 'utf8'))
const source = readFileSync(new URL('../src/utils/mediaInspection.ts', import.meta.url), 'utf8').replace("'./format'", JSON.stringify(formatURL))
const { inspectionRows, inspectionText, inspectionValue, summarizeStreams, trackSummary } = await import(moduleURL(source))
const snapshot = (data, status = 'ok') => ({ resource: 'streams', status, data, fetched_at: '2026-11-05T00:00:00Z' })

test('同源多协议输出去重，观看数不重复累计，虚拟主机保持独立', () => {
  const result = snapshot([
    { vhost: 'a', app: 'rtp', stream: 'cam', schema: 'rtsp', totalReaderCount: 3, isRecordingMP4: true },
    { vhost: 'a', app: 'rtp', stream: 'cam', schema: 'rtmp', totalReaderCount: 3 },
    { vhost: 'b', app: 'rtp', stream: 'cam', schema: 'rtsp', totalReaderCount: 2, isRecordingMP4: '0' },
  ])
  assert.deepEqual(summarizeStreams(result), { sources: 2, outputs: 3, readers: 5, recording: 1 })
})

test('成功空集、未知人数和查询失败保持不同语义', () => {
  assert.deepEqual(summarizeStreams(snapshot([])), { sources: 0, outputs: 0, readers: 0, recording: 0 })
  assert.equal(summarizeStreams(snapshot([{ app: 'rtp', stream: 'cam' }])).readers, null)
  for (const status of ['unsupported', 'unavailable']) {
    assert.equal(summarizeStreams(snapshot(null, status)), null)
    assert.deepEqual(inspectionRows(snapshot([{ stream: 'stale' }], status)), [])
  }
  assert.equal(inspectionText(undefined), '—')
  assert.equal(inspectionText(0), '0')
})

test('按源取最高总观看数，缺失协议字段不覆盖已知计数', () => {
  assert.equal(summarizeStreams(snapshot([
    { app: 'rtp', stream: 'cam', totalReaderCount: 2 },
    { app: 'rtp', stream: 'cam', totalReaderCount: 4 },
    { app: 'rtp', stream: 'cam' },
  ])).readers, 4)
})

test('码率从字节每秒转换，保留零值并区分未知与不支持的状态码', () => {
  assert.equal(inspectionText(125000, 'bitrate'), '1.00 Mbps')
  assert.equal(inspectionText(12500, 'bitrate'), '100.0 Kbps')
  assert.equal(inspectionText(0, 'bitrate'), '0.0 Kbps')
  assert.equal(inspectionText(1024, 'rate'), '1 KB/s')
  assert.equal(inspectionText(3661, 'duration'), '01:01:01')
  assert.equal(inspectionText(-1, 'delay'), '—')
  assert.equal(inspectionText(0, 'proxyStatus'), '在线')
  assert.equal(inspectionText(1, 'proxyStatus'), '未连接')
  assert.equal(inspectionText(9, 'proxyStatus'), '状态 9')
  assert.equal(inspectionText(false, 'boolean'), '否')
  assert.equal(inspectionText(undefined, 'boolean'), '—')
  assert.equal(inspectionText(0, 'tcpMode'), 'UDP')
})

test('轨道格式容忍空项，动态统计对象和嵌套字段保持可查', () => {
  assert.equal(trackSummary([null]), '未返回轨道')
  assert.equal(trackSummary([null, { codec_id_name: 'H265', width: 1920, height: 1080, fps: 25, ready: false }]), 'H265 · 1920×1080 · 25 fps · 未就绪')
  assert.equal(inspectionValue({ originSock: { peer_port: 30000 } }, 'originSock.peer_port'), 30000)
  assert.equal(inspectionValue({ originSock: null }, 'originSock.peer_port'), undefined)
  assert.deepEqual(inspectionRows(snapshot({ Socket: 4 })), [{ name: 'Socket', value: 4 }])
  assert.deepEqual(inspectionRows(snapshot(['/index/api/version'])), [{ value: '/index/api/version' }])
})
