import { onBeforeUnmount, onMounted } from 'vue'
export function useVisiblePoll(load: (signal: AbortSignal) => Promise<void>, interval = 5000) {
  let controller: AbortController | undefined
  let timer: ReturnType<typeof setTimeout> | undefined
  let disposed = false
  async function refresh() {
    clearTimeout(timer)
    controller?.abort()
    if (disposed || document.hidden) return
    const current = new AbortController()
    controller = current
    try { await load(current.signal) } catch (e) { if (!current.signal.aborted) throw e }
    finally { if (!disposed && !document.hidden && controller === current) timer = setTimeout(() => { void refresh() }, interval) }
  }
  function visibility() { if (document.hidden) { clearTimeout(timer); controller?.abort() } else void refresh() }
  onMounted(() => { document.addEventListener('visibilitychange', visibility); void refresh() })
  onBeforeUnmount(() => { disposed = true; clearTimeout(timer); controller?.abort(); document.removeEventListener('visibilitychange', visibility) })
  return { refresh }
}
