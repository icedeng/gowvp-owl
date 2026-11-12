import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import ts from 'typescript'
import { computed, effectScope, nextTick, reactive, ref, watch } from 'vue'

// 执行页面实际的 script setup，模拟定时器和接口，不依赖浏览器或真实30秒等待。
const page = readFileSync(new URL('../src/views/EventsView.vue', import.meta.url), 'utf8')
const script = page.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1]
const { outputText } = ts.transpileModule(script, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
  transformers: { before: [context => source => ts.visitEachChild(source, node => ts.isImportDeclaration(node) ? undefined : node, context)] },
})

async function fixture(t) {
  const mounted = [], unmounted = [], timers = new Map(), windowEvents = new Map(), documentEvents = new Map()
  const requests = [], route = reactive({ query: {} })
  const document = {
    visibilityState: 'visible',
    addEventListener: (event, handler) => documentEvents.set(event, handler),
    removeEventListener: event => documentEvents.delete(event),
  }
  const window = {
    localStorage: { getItem: () => null },
    setInterval: (handler, delay) => { assert.equal(delay, 30000); timers.set(1, handler); return 1 },
    clearInterval: id => timers.delete(id),
    addEventListener: (event, handler) => windowEvents.set(event, handler),
    removeEventListener: event => windowEvents.delete(event),
  }
  const api = {
    events: params => new Promise((resolve, reject) => { requests.push({ params, resolve, reject }) }),
  }
  const deps = {
    computed, ref, watch, onMounted: fn => mounted.push(fn), onBeforeUnmount: fn => unmounted.push(fn),
    useRoute: () => route, useRouter: () => ({ replace: ({ query }) => { route.query = query } }),
    useEventNoticesStore: () => ({ refresh() {} }), api, errorMessage: error => error.message, window, document,
  }
  const scope = effectScope()
  const state = scope.run(() => new Function(...Object.keys(deps), outputText.replace(/^export \{\};?\s*$/m, '') + '\nreturn { events, total, selected, selectedId, bulkItems, autoRefresh, refreshing, lastRefreshedAt, view, toggleRefresh, changePresentation, load };')(...Object.values(deps)))
  mounted.forEach(fn => fn())
  t.after(() => { unmounted.forEach(fn => fn()); scope.stop() })
  const flush = async () => { await Promise.resolve(); await nextTick(); await Promise.resolve() }
  const respond = async (index, items = [{ id: index + 1 }], total = items.length) => {
    requests[index].resolve({ data: { items, total } }); await flush()
  }
  await respond(0)
  return { state, route, requests, respond, flush, tick: () => timers.get(1)?.(), timers, windowEvents, documentEvents, document, unmounted }
}

test('默认开启，每30秒更新列表和时间，卡片悬停或聚焦不阻止刷新', async t => {
  const f = await fixture(t)
  assert.equal(f.state.autoRefresh.value, true)
  assert.ok(f.state.lastRefreshedAt.value)
  assert.doesNotMatch(page, /@(?:mouseenter|focusin)="cards/)
  f.tick()
  assert.equal(f.requests.length, 2)
  await f.respond(1, [{ id: 2, occurrence_count: 6 }])
  assert.equal(f.state.events.value[0].occurrence_count, 6)
  assert.equal(f.state.total.value, 1)
})

test('勾选、详情与批量处理时暂停，结束后立即补刷', async t => {
  const f = await fixture(t)
  for (const [field, paused, resumed] of [['selected', [1], []], ['selectedId', 1, 0], ['bulkItems', [{ id: 1 }], []]]) {
    f.state[field].value = paused
    await f.flush()
    const before = f.requests.length
    f.tick(); assert.equal(f.requests.length, before)
    f.state[field].value = resumed
    await f.flush()
    assert.equal(f.requests.length, before + 1)
    assert.equal(f.requests.at(-1).params.status, 'pending')
    assert.equal(f.requests.at(-1).params.page, 1)
    await f.respond(before)
  }
})

test('自动刷新沿用已选择的筛选和页码，联动策略页暂停', async t => {
  const f = await fixture(t)
  f.route.query = { status: 'all', page: '2', size: '20', did: 'camera', source: 'alarm', evidence: 'image' }
  await f.flush()
  await f.respond(1, [{ id: 21 }], 80)
  f.tick()
  assert.deepEqual(f.requests[2].params, { page: 2, size: 20, key: undefined, status: undefined, source: 'alarm', did: 'camera', cid: undefined, evidence: 'image' })
  await f.respond(2, [{ id: 22 }], 80)
  f.state.view.value = 'policies'; await f.flush()
  f.tick(); assert.equal(f.requests.length, 3)
  f.state.view.value = 'events'; await f.flush()
  assert.equal(f.requests.length, 4)
  await f.respond(3, [{ id: 23 }], 80)
})

test('隐藏页面不轮询，返回窗口立即更新；关闭开关停止更新', async t => {
  const f = await fixture(t)
  f.document.visibilityState = 'hidden'; f.documentEvents.get('visibilitychange')(); await f.flush()
  f.tick(); assert.equal(f.requests.length, 1)
  f.document.visibilityState = 'visible'; f.documentEvents.get('visibilitychange')(); await f.flush()
  assert.equal(f.requests.length, 2)
  await f.respond(1)
  f.state.toggleRefresh(); await f.flush()
  f.tick(); f.windowEvents.get('focus')(); assert.equal(f.requests.length, 2)
  f.state.toggleRefresh(); await f.flush()
  assert.equal(f.requests.length, 3)
  await f.respond(2)
})

test('慢请求不重叠，处理期间到达的响应不覆盖列表；失败后继续刷新', async t => {
  const f = await fixture(t)
  f.tick(); f.tick(); assert.equal(f.requests.length, 2)
  f.state.selected.value = [1]; await f.flush()
  await f.respond(1, [{ id: 99 }])
  assert.equal(f.state.events.value[0].id, 1)
  assert.deepEqual([...f.state.selected.value], [1])
  f.state.selected.value = []; await f.flush()
  assert.equal(f.requests.length, 3)
  f.requests[2].reject(new Error('offline')); await f.flush()
  assert.equal(f.state.refreshing.value, false)
  f.tick(); assert.equal(f.requests.length, 4)
  await f.respond(3, [{ id: 4 }])
  assert.equal(f.state.events.value[0].id, 4)
})

test('离开页面清理定时器、监听和迟到响应', async t => {
  const f = await fixture(t)
  f.tick()
  f.unmounted.forEach(fn => fn())
  assert.equal(f.timers.size, 0)
  assert.equal(f.windowEvents.size, 0)
  assert.equal(f.documentEvents.size, 0)
  await f.respond(1, [{ id: 99 }])
  assert.equal(f.state.events.value[0].id, 1)
})
