<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, shallowRef, watch } from 'vue'
import { CircleAlert, LoaderCircle, Play } from '@lucide/vue'
import type { PlayResult } from '../types/api'
import {
  normalizeStreamSources,
  playbackCandidates,
  type PlayerSession,
  type StreamProtocol,
} from '../player'
import {
  isEasyPlayerPosterUrl,
  resolveProtocolPriority,
} from '../player/easyplayer.config'
import { usePlayerSettingsStore } from '../stores/player'

const props = withDefaults(defineProps<{
  result?: PlayResult | null
  muted?: boolean
  poster?: string
  autoplay?: boolean
  protocolPriority?: StreamProtocol[]
}>(), {
  result: null,
  muted: false,
  poster: '',
  autoplay: true,
})

const emit = defineEmits<{
  error: [message: string]
  protocolChange: [protocol: StreamProtocol]
  ready: [protocol: StreamProtocol]
}>()

const host = shallowRef<HTMLElement | null>(null)
const playerSettings = usePlayerSettingsStore()
const session = shallowRef<PlayerSession | null>(null)
const state = shallowRef<'idle' | 'loading' | 'ready' | 'error'>('idle')
const message = shallowRef('')
const activeProtocol = shallowRef<StreamProtocol | null>(null)
let generation = 0

const sources = computed(() => normalizeStreamSources(props.result))
const idlePoster = computed(() =>
  props.poster || (
    isEasyPlayerPosterUrl(playerSettings.settings.posterUrl)
      ? playerSettings.settings.posterUrl.trim()
      : ''
  )
)

async function destroyPlayer() {
  const current = session.value
  session.value = null
  if (current) await current.destroy()
  if (host.value) host.value.replaceChildren()
}

async function start() {
  const currentGeneration = ++generation
  await destroyPlayer()
  if (currentGeneration !== generation) return

  const priority = props.protocolPriority?.length
    ? props.protocolPriority
    : resolveProtocolPriority(playerSettings.settings.preferredProtocol)
  const candidates = playbackCandidates(sources.value, priority)
  if (!candidates.length) {
    state.value = sources.value.length ? 'error' : 'idle'
    message.value = sources.value.length
      ? '当前浏览器没有可用的播放适配器'
      : '等待获取播放地址'
    if (state.value === 'error') emit('error', message.value)
    return
  }

  const errors: string[] = []
  for (const selected of candidates) {
    state.value = 'loading'
    message.value = `正在加载 ${selected.source.protocol.toUpperCase()}`
    activeProtocol.value = selected.source.protocol
    emit('protocolChange', selected.source.protocol)
    await nextTick()
    if (!host.value || currentGeneration !== generation) return

    try {
      const nextSession = await selected.adapter.play(host.value, selected.source, {
        muted: props.muted,
        poster: props.poster || undefined,
        settings: playerSettings.settings,
      })
      if (currentGeneration !== generation) {
        await nextSession.destroy()
        return
      }
      session.value = nextSession
      state.value = 'ready'
      message.value = ''
      emit('ready', selected.source.protocol)
      return
    } catch (cause) {
      if (currentGeneration !== generation) return
      const reason = cause instanceof Error ? cause.message : '播放器启动失败'
      errors.push(`${selected.source.protocol}: ${reason}`)
      if (host.value) host.value.replaceChildren()
    }
  }

  await destroyPlayer()
  state.value = 'error'
  message.value = errors[errors.length - 1] || '播放器启动失败'
  emit('error', message.value)
}

watch(
  () => [props.result, props.protocolPriority, props.autoplay, playerSettings.revision] as const,
  () => {
    if (props.autoplay) void start()
    else void destroyPlayer()
  },
  { immediate: true, deep: true, flush: 'post' }
)

watch(() => props.muted, (muted) => session.value?.setMuted?.(muted))

onBeforeUnmount(() => {
  generation += 1
  void destroyPlayer()
})

defineExpose({ play: start, destroy: destroyPlayer })
</script>

<template>
  <div class="stream-player">
    <img
      v-if="idlePoster && state !== 'ready'"
      :src="idlePoster"
      class="stream-player-poster"
      :alt="poster ? '通道快照' : '播放器默认封面'"
    />
    <div ref="host" class="stream-player-host" />
    <div v-if="state !== 'ready'" class="stream-player-state">
      <LoaderCircle v-if="state === 'loading'" class="animate-spin" />
      <CircleAlert v-else-if="state === 'error'" />
      <Play v-else />
      <small>{{ message || '等待播放' }}</small>
      <button v-if="state === 'error'" type="button" @click.stop="start">重试</button>
    </div>
    <span v-if="activeProtocol" class="stream-player-protocol">{{ activeProtocol }}</span>
  </div>
</template>

<style scoped>
.stream-player { position: absolute; inset: 0; overflow: hidden; color: #91a3ba; background: #0b1422; }
.stream-player-host, .stream-player-host :deep(> *) { width: 100%; height: 100%; }
.stream-player-poster { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: cover; opacity: .7; }
.stream-player-state { position: absolute; inset: 0; display: grid; place-content: center; justify-items: center; gap: 8px; padding: 18px; text-align: center; background: radial-gradient(circle at 50% 44%, rgba(23, 38, 58, .86), rgba(11, 20, 34, .96) 68%); }
.stream-player-state svg { width: 24px; height: 24px; }
.stream-player-state small { max-width: 320px; font-size: 10px; line-height: 1.5; }
.stream-player-state button { min-height: 27px; padding: 0 10px; color: #d8e8fb; background: #1b2b42; border: 1px solid #344963; border-radius: 6px; font-size: 10px; cursor: pointer; }
.stream-player-protocol { position: absolute; right: 8px; bottom: 8px; padding: 2px 6px; color: #d8e8fb; background: rgba(7, 12, 20, .72); border-radius: 4px; font: 700 8px "SFMono-Regular", monospace; text-transform: uppercase; pointer-events: none; }
</style>
