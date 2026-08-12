<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { Activity, Camera, CircleGauge, Film, MonitorCog, Radio, RadioTower, Search, Server, Siren, Truck, Video } from '@lucide/vue'
import { useUiStore } from '../stores/ui'

const ui = useUiStore()
const router = useRouter()
const query = ref('')
const input = ref<HTMLInputElement>()
const panel = ref<HTMLElement>()
const activeIndex = ref(0)
let previousFocus: HTMLElement | null = null
let previousBodyOverflow = ''
const items = [
  { label: '打开运行总览', hint: '工作台', path: '/overview', icon: CircleGauge },
  { label: '进入实时监控', hint: '视频值守', path: '/live', icon: Video },
  { label: '查询统一事件', hint: 'AI / 国标报警', path: '/events', icon: Siren },
  { label: '检索历史录像', hint: '月历 / 时间轴', path: '/recordings', icon: Film },
  { label: '查找国标设备', hint: 'GB/T 28181', path: '/devices', icon: Camera },
  { label: '查找部标设备', hint: 'JT/T 808 / 1078', path: '/transport-devices', icon: Truck },
  { label: '查找视频通道', hint: '通道能力', path: '/channels', icon: Radio },
  { label: '管理 RTSP 拉流', hint: '资源管理', path: '/pull-streams', icon: RadioTower },
  { label: '检查媒体节点', hint: '平台运维', path: '/media-servers', icon: Server },
  { label: '查看系统状态', hint: '资源与 API 指标', path: '/system-status', icon: Activity },
  { label: '调整播放器参数', hint: '协议 / 解码 / 缓冲', path: '/player-settings', icon: MonitorCog },
]
const filtered = computed(() => items.filter((item) => `${item.label}${item.hint}`.toLowerCase().includes(query.value.toLowerCase())))

watch(() => ui.commandOpen, async (open) => {
  if (open) {
    previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
    previousBodyOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    query.value = ''
    activeIndex.value = 0
    document.addEventListener('keydown', onKeydown)
    await nextTick()
    input.value?.focus()
  } else {
    document.removeEventListener('keydown', onKeydown)
    document.body.style.overflow = previousBodyOverflow
    await nextTick()
    previousFocus?.focus()
    previousFocus = null
  }
})

watch(query, () => { activeIndex.value = 0 })

function onKeydown(event: KeyboardEvent) {
  if (!ui.commandOpen) return
  if (event.key === 'Escape') {
    event.preventDefault()
    ui.closeCommand()
    return
  }
  if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
    event.preventDefault()
    const count = filtered.value.length
    if (!count) return
    activeIndex.value = event.key === 'ArrowDown'
      ? (activeIndex.value + 1) % count
      : (activeIndex.value - 1 + count) % count
    return
  }
  if (event.key === 'Enter' && document.activeElement === input.value) {
    const item = filtered.value[activeIndex.value]
    if (item) {
      event.preventDefault()
      go(item.path)
    }
    return
  }
  if (event.key !== 'Tab' || !panel.value) return
  const focusable = [...panel.value.querySelectorAll<HTMLElement>('button:not([disabled]), input:not([disabled]), [href], [tabindex]:not([tabindex="-1"])')]
  if (!focusable.length) return
  const first = focusable[0]
  const last = focusable[focusable.length - 1]
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault()
    last.focus()
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault()
    first.focus()
  }
}

function go(path: string) {
  ui.closeCommand()
  router.push(path)
}

onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown)
  document.body.style.overflow = previousBodyOverflow
})
</script>

<template>
  <Teleport to="body">
    <Transition name="palette">
      <div v-if="ui.commandOpen" class="command-backdrop open" role="presentation" @click.self="ui.closeCommand">
        <section ref="panel" class="command-box" role="dialog" aria-modal="true" aria-label="全局搜索">
          <div class="command-search"><Search /><input ref="input" v-model="query" class="command-input" role="combobox" aria-controls="command-results" :aria-expanded="Boolean(filtered.length)" :aria-activedescendant="filtered[activeIndex] ? `command-item-${activeIndex}` : undefined" placeholder="输入设备、通道或功能…" /></div>
          <div id="command-results" class="command-list" role="listbox">
            <button v-for="(item, index) in filtered" :id="`command-item-${index}`" :key="item.path" class="command-item" :class="{ active: activeIndex === index }" role="option" :aria-selected="activeIndex === index" @mouseenter="activeIndex = index" @click="go(item.path)">
              <component :is="item.icon" /><span>{{ item.label }}</span><small>{{ item.hint }}</small>
            </button>
            <div v-if="!filtered.length" class="empty-state">没有匹配结果，请换一个关键词。</div>
          </div>
          <footer class="command-foot">↑↓ 选择 · Enter 打开 · Esc 关闭</footer>
        </section>
      </div>
    </Transition>
  </Teleport>
</template>
