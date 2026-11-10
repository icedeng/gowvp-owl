import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import ts from 'typescript'

const { outputText } = ts.transpileModule(
  readFileSync(new URL('../src/utils/clipboard.ts', import.meta.url), 'utf8'),
  { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } },
)
const { copyText } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

function browser(t, { secure = false, writeText, copyResult = true, copyError, inDialog = false } = {}) {
  let selected = null
  let copied = null
  const nodes = []
  class Element {
    style = {}
    attributes = {}
    value = ''
    setAttribute(name, value) { this.attributes[name] = value }
    focus(options) { doc.activeElement = this; this.focusOptions = options }
    closest() { return inDialog ? dialog : null }
    appendChild(node) { nodes.push(node); node.parent = this }
    remove() { nodes.splice(nodes.indexOf(this), 1) }
  }
  class Input extends Element {
    selectionStart = 2
    selectionEnd = 5
    selectionDirection = 'backward'
    setSelectionRange(start, end, direction) { this.selectionStart = start; this.selectionEnd = end; this.selectionDirection = direction }
  }
  class TextArea extends Input {
    select() { selected = this }
  }
  const body = new Element()
  const dialog = new Element()
  const active = new Input()
  const range = { cloneRange: () => range }
  const selection = {
    ranges: [range], get rangeCount() { return this.ranges.length },
    getRangeAt(index) { return this.ranges[index] },
    removeAllRanges() { this.ranges = [] }, addRange(range) { this.ranges.push(range) },
  }
  const doc = {
    activeElement: active, body, getSelection: () => selection,
    createElement: tag => { assert.equal(tag, 'textarea'); return new TextArea() },
    execCommand(command) {
      assert.equal(command, 'copy')
      assert.equal(doc.activeElement, selected)
      assert.equal(selected.parent, inDialog ? dialog : body)
      assert.equal(selected.attributes.readonly, '')
      assert.equal(selected.selectionStart, 0)
      assert.equal(selected.selectionEnd, selected.value.length)
      if (copyError) throw copyError
      if (copyResult) copied = selected.value
      return copyResult
    },
  }
  for (const [key, value] of Object.entries({ window: { isSecureContext: secure }, navigator: { clipboard: writeText ? { writeText } : undefined }, document: doc, HTMLElement: Element, HTMLInputElement: Input, HTMLTextAreaElement: TextArea })) {
    const descriptor = Object.getOwnPropertyDescriptor(globalThis, key)
    Object.defineProperty(globalThis, key, { configurable: true, value })
    t.after(() => { if (descriptor) Object.defineProperty(globalThis, key, descriptor); else delete globalThis[key] })
  }
  return { doc, active, selection, range, nodes, copied: () => copied }
}

test('HTTP 缺少 Clipboard API 时同步复制完整地址，清理并恢复焦点和输入选区', async t => {
  const state = browser(t)
  const address = `ws://10.144.0.20:15123/live.flv?token=${'abc'.repeat(300)}&name=通道`
  const pending = copyText(address)
  assert.equal(state.copied(), address)
  await pending
  assert.equal(state.nodes.length, 0)
  assert.equal(state.doc.activeElement, state.active)
  assert.deepEqual([state.active.selectionStart, state.active.selectionEnd, state.active.selectionDirection], [2, 5, 'backward'])
  assert.deepEqual(state.active.focusOptions, { preventScroll: true })
})

test('HTTPS 优先新 API，不创建临时文本框', async t => {
  const writes = []
  const state = browser(t, { secure: true, writeText: async text => writes.push(text) })
  await copyText('https://example.test/live')
  assert.deepEqual(writes, ['https://example.test/live'])
  assert.equal(state.copied(), null)
  assert.equal(state.nodes.length, 0)
})

test('HTTPS API 权限拒绝时回退，弹窗内复制不逃离焦点约束', async t => {
  const state = browser(t, { secure: true, inDialog: true, writeText: async () => { throw new Error('NotAllowedError') } })
  await copyText('配置片段\n下一行')
  assert.equal(state.copied(), '配置片段\n下一行')
  assert.equal(state.doc.activeElement, state.active)
  assert.equal(state.nodes.length, 0)
})

test('HTTPS 缺少新 API 也能回退，并恢复普通文本选区', async t => {
  const state = browser(t, { secure: true })
  state.doc.activeElement = null
  await copyText('示例代码')
  assert.equal(state.copied(), '示例代码')
  assert.deepEqual(state.selection.ranges, [state.range])
})

for (const scenario of [{ copyResult: false }, { copyError: new Error('copy denied') }]) {
  test(`复制命令${scenario.copyError ? '抛异常' : '返回 false'}时报告失败并移除临时文本框`, async t => {
    const state = browser(t, scenario)
    await assert.rejects(copyText('不能误报成功'))
    assert.equal(state.copied(), null)
    assert.equal(state.nodes.length, 0)
    assert.equal(state.doc.activeElement, state.active)
  })
}
