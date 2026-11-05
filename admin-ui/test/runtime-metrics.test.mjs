import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import ts from 'typescript'

const { outputText } = ts.transpileModule(
  readFileSync(new URL('../src/utils/runtimeMetrics.ts', import.meta.url), 'utf8'),
  { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } },
)
const { goroutineMetric } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

test('实时数量优先于历史采样，保留零值', () => {
  assert.deepEqual(goroutineMetric({ goroutine_count: 50, goroutines: [{ num: 44 }] }), { count: 50, sampled: false })
  assert.deepEqual(goroutineMetric({ goroutine_count: 0 }), { count: 0, sampled: false })
})

test('旧接口历史数组取最近有效采样，不把数组作为当前值', () => {
  assert.deepEqual(goroutineMetric({ goroutines: [{ time: '04:30', num: 44 }, { time: '04:40', num: 48 }] }), { count: 48, sampled: true, time: '04:40' })
  assert.equal(goroutineMetric({ goroutines: [{ num: 0 }, null, { num: -1 }] }).count, 0)
  assert.deepEqual(goroutineMetric({ goroutines: 44 }), { count: 44, sampled: false })
})

test('空数据与异常值显示未知，不能伪造零值或渲染对象', () => {
  for (const goroutines of [undefined, [], null, {}, [{ num: '44' }], [{ num: Infinity }], -1]) {
    assert.deepEqual(goroutineMetric({ goroutines }), { count: null, sampled: false })
  }
})
