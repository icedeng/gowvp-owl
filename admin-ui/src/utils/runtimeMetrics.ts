import type { ApiMetrics } from '../types/api'

// 新服务提供实时数量；旧服务保留最近采样或旧标量值，不能把历史数组直接渲染。
export function goroutineMetric(metrics: ApiMetrics) {
  const validCount = (value: unknown): value is number => typeof value === 'number' && Number.isInteger(value) && value >= 0
  if (validCount(metrics.goroutine_count)) return { count: metrics.goroutine_count, sampled: false }
  if (validCount(metrics.goroutines)) return { count: metrics.goroutines, sampled: false }
  if (Array.isArray(metrics.goroutines)) {
    for (let index = metrics.goroutines.length - 1; index >= 0; index--) {
      const point = metrics.goroutines[index]
      if (point && validCount(point.num)) return { count: point.num, sampled: true, time: point.time }
    }
  }
  return { count: null, sampled: false }
}
