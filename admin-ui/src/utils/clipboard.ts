/** 在点击事件中直接调用，保留 HTTP 页面的同步复制机会。 */
export async function copyText(text: string): Promise<void> {
  if (window.isSecureContext) {
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text)
        return
      }
    } catch {
      // HTTPS 下权限拒绝或 API 不可用时，仍尝试浏览器原生复制命令。
    }
  }
  copyWithSelection(text)
}

function copyWithSelection(text: string): void {
  const active = document.activeElement instanceof HTMLElement ? document.activeElement : null
  const selection = document.getSelection()
  const ranges = selection ? Array.from({ length: selection.rangeCount }, (_, index) => selection.getRangeAt(index).cloneRange()) : []
  const inputSelection = active instanceof HTMLInputElement || active instanceof HTMLTextAreaElement
    ? { start: active.selectionStart, end: active.selectionEnd, direction: active.selectionDirection }
    : null
  const element = document.createElement('textarea')
  element.value = text
  element.setAttribute('readonly', '')
  element.setAttribute('tabindex', '-1')
  Object.assign(element.style, { position: 'fixed', top: '0', left: '0', opacity: '0', fontSize: '16px', pointerEvents: 'none' })
  // 保持在当前弹窗内，避免焦点约束将临时文本框的焦点移走。
  const container = active?.closest('dialog, [role="dialog"]') || document.body
  try {
    container.appendChild(element)
    element.focus({ preventScroll: true })
    element.select()
    element.setSelectionRange(0, text.length)
    if (!document.execCommand('copy')) throw new Error('复制未成功')
  } finally {
    element.remove()
    active?.focus({ preventScroll: true })
    if (inputSelection?.start != null && inputSelection.end != null && (active instanceof HTMLInputElement || active instanceof HTMLTextAreaElement)) {
      active.setSelectionRange(inputSelection.start, inputSelection.end, inputSelection.direction || undefined)
    } else if (selection) {
      selection.removeAllRanges()
      for (const range of ranges) selection.addRange(range)
    }
  }
}
