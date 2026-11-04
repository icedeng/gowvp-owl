import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import ts from 'typescript'

const { outputText } = ts.transpileModule(
  readFileSync(new URL('../src/utils/format.ts', import.meta.url), 'utf8'),
  { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } },
)
const { formatUptime } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

test('运行时长按分钟、小时和天显示，处理空值和异常时间', (t) => {
  const now = Date.parse('2026-11-04T08:00:00Z')
  t.mock.method(Date, 'now', () => now)
  const before = (seconds) => new Date(now - seconds * 1000).toISOString()
  for (const [seconds, expected] of [
    [0, '不足 1 分钟'], [59, '不足 1 分钟'], [60, '1 分钟'],
    [3599, '59 分钟'], [3600, '1 小时'], [3660, '1 小时 1 分钟'],
    [86399, '23 小时 59 分钟'], [86400, '1 天 0 小时'],
    [90000, '1 天 1 小时'], [-60, '不足 1 分钟'],
  ]) assert.equal(formatUptime(before(seconds)), expected)
  assert.equal(formatUptime(), '—')
  assert.equal(formatUptime(''), '—')
  assert.equal(formatUptime('invalid'), '—')
})
