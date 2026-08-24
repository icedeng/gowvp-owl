<script setup lang="ts">
import { onBeforeUnmount, onMounted, shallowRef, watch } from 'vue'
import * as echarts from 'echarts/core'
import { GridComponent, TooltipComponent } from 'echarts/components'
import { LineChart } from 'echarts/charts'
import { CanvasRenderer } from 'echarts/renderers'

echarts.use([GridComponent, TooltipComponent, LineChart, CanvasRenderer])
const el = shallowRef<HTMLDivElement>()
let chart: echarts.ECharts | undefined
let motionPreference: MediaQueryList | undefined
let reduceMotion = false
const props = withDefaults(defineProps<{ values?: number[]; labels?: string[]; ariaLabel?: string }>(), {
  values: () => [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0],
  labels: () => ['03','05','07','09','11','13','15','17','19','21','23','01'],
  ariaLabel: '趋势图',
})

function render() {
  if (!el.value) return
  if (!chart) chart = echarts.init(el.value)
  chart.setOption({
    animation: !reduceMotion,
    animationDuration: reduceMotion ? 0 : 220,
    grid: { left: 4, right: 8, top: 12, bottom: 2, containLabel: true },
    tooltip: { trigger: 'axis', backgroundColor: '#171e21', borderColor: '#334047', textStyle: { color: '#e9edf0', fontSize: 11 } },
    xAxis: { type: 'category', boundaryGap: false, data: props.labels, axisLabel: { color: '#66737b', fontSize: 9 }, axisLine: { lineStyle: { color: '#283236' } }, axisTick: { show: false } },
    yAxis: { type: 'value', splitLine: { lineStyle: { color: 'rgba(188,209,217,.07)' } }, axisLabel: { color: '#66737b', fontSize: 9 }, axisLine: { show: false } },
    series: [{ type: 'line', data: props.values, smooth: .28, symbol: 'none', lineStyle: { width: 2, color: '#80f0d0' }, areaStyle: { color: 'rgba(128,240,208,.06)' } }],
  })
}
function resize() { chart?.resize() }
function onMotionPreferenceChange(event: MediaQueryListEvent) { reduceMotion = event.matches; render() }
onMounted(() => {
  motionPreference = window.matchMedia('(prefers-reduced-motion: reduce)')
  reduceMotion = motionPreference.matches
  motionPreference.addEventListener('change', onMotionPreferenceChange)
  render()
  window.addEventListener('resize', resize)
})
onBeforeUnmount(() => {
  window.removeEventListener('resize', resize)
  motionPreference?.removeEventListener('change', onMotionPreferenceChange)
  chart?.dispose()
})
watch(() => [props.values, props.labels], () => render(), { deep: true })
</script>

<template><div ref="el" class="h-[220px] w-full" role="img" :aria-label="ariaLabel" /></template>
