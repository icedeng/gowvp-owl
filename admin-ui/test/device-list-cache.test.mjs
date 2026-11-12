import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import ts from 'typescript'
import { computed, effectScope, nextTick, reactive, ref, watch } from 'vue'

const source = readFileSync(new URL('../src/views/DevicesView.vue', import.meta.url), 'utf8')
const script = source.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1]
const { outputText } = ts.transpileModule(script, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
  transformers: { before: [context => source => ts.visitEachChild(source, node => ts.isImportDeclaration(node) ? undefined : node, context)] },
})

function fixture(t) {
  const unmounted = [], requests = []
  const deps = {
    computed, reactive, ref, watch, onMounted() {}, onBeforeUnmount: fn => unmounted.push(fn),
    useUiStore: () => ({}), api: { devices() {} }, typeLabel: type => type,
    errorMessage: error => error.message,
    collectPages: (_api, params) => new Promise(resolve => requests.push({ params, resolve })),
    window: { clearTimeout() {}, setTimeout() { return 1 } },
  }
  const scope = effectScope()
  const state = scope.run(() => new Function(...Object.keys(deps), outputText.replace(/^export \{\};?\s*$/m, '') + '\nreturn { gatewayID, version, query, rows, supportsServerFilters, load };')(...Object.values(deps)))
  state.supportsServerFilters.value = false
  t.after(() => { unmounted.forEach(fn => fn()); scope.stop() })
  return { state, requests, unmounted }
}
const device = id => ({ id, type: 'GB28181', is_online: true, ext: { gb_version: '3.0' } })

test('旧接口缓存按网关隔离，同入口的协议和搜索筛选复用缓存', async t => {
  const f = fixture(t)
  let pending = f.state.load()
  f.requests[0].resolve({ items: [device('direct')] }); await pending
  f.state.gatewayID.value = 'gateway-b'; await nextTick()
  pending = f.state.load()
  assert.equal(f.requests.length, 2)
  assert.equal(f.requests[1].params.gateway_id, 'gateway-b')
  f.requests[1].resolve({ items: [device('remote')] }); await pending
  assert.equal(f.state.rows.value[0].id, 'remote')
  f.state.version.value = '3.0'; await nextTick(); await f.state.load()
  f.state.query.value = 'remote'; await nextTick(); await f.state.load()
  assert.equal(f.requests.length, 2)
  assert.equal(f.state.rows.value[0].id, 'remote')
})

test('筛选切换和页面卸载均拒绝迟到响应', async t => {
  const f = fixture(t)
  let pending = f.state.load()
  f.state.gatewayID.value = 'gateway-b'; await nextTick()
  f.requests[0].resolve({ items: [device('stale')] }); await pending
  assert.equal(f.state.rows.value.length, 0)
  pending = f.state.load()
  f.unmounted.forEach(fn => fn())
  f.requests[1].resolve({ items: [device('unmounted')] }); await pending
  assert.equal(f.state.rows.value.length, 0)
})
