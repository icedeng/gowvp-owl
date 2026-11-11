import { ref } from 'vue'
import { defineStore } from 'pinia'
import { api } from '../services/api'

export const useEventNoticesStore = defineStore('event-notices', () => {
  const pendingCount = ref(0)
  let sequence = 0

  async function refresh() {
    const current = ++sequence
    try {
      const { data } = await api.events({ page: 1, size: 1, status: 'pending' })
      if (current === sequence) pendingCount.value = data.total || 0
    } catch {
      // 请求失败时保留上次统计，避免把连接故障误报为已全部处理。
    }
  }

  function reset() {
    ++sequence
    pendingCount.value = 0
  }

  return { pendingCount, refresh, reset }
})
