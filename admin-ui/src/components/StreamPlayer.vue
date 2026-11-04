<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, shallowRef, watch } from 'vue'
import { CircleAlert, LoaderCircle, Play } from '@lucide/vue'
import type { PlayResult } from '../types/api'
import {
  normalizeStreamSources,
  playbackCandidates,
  type PlayerSession,
  type StreamProtocol,
  type MediaProtocol,
  type StreamSource,
} from '../player'
import {
  isEasyPlayerPosterUrl,
  resolveProtocolPriority,
} from '../player/easyplayer.config'
import { usePlayerSettingsStore } from '../stores/player'

const props = withDefaults(defineProps<{
  result?: PlayResult | null
  src?: string
  duration?: number
  loading?: boolean
  loadingText?: string
  error?: string
  idleAction?: string
  idleHint?: string
  disabled?: boolean
  muted?: boolean
  poster?: string
  autoplay?: boolean
  idleTheme?: 'dark' | 'light'
  protocolPriority?: StreamProtocol[]
}>(), {
  result: null,
  src: '',
  duration: 0,
  loading: false,
  loadingText: '正在加载视频…',
  error: '',
  idleAction: '',
  idleHint: '等待播放',
  disabled: false,
  muted: false,
  poster: '',
  autoplay: true,
  idleTheme: 'light',
})

const emit = defineEmits<{
  error: [message: string]
  protocolChange: [protocol: MediaProtocol]
  ready: [protocol: MediaProtocol]
  requestPlay: []
  retry: []
  ended: []
}>()

const host = shallowRef<HTMLElement | null>(null)
const playerSettings = usePlayerSettingsStore()
const session = shallowRef<PlayerSession | null>(null)
const state = shallowRef<'idle' | 'loading' | 'ready' | 'error'>('idle')
const message = shallowRef('')
const activeProtocol = shallowRef<MediaProtocol | null>(null)
const posterFailed = shallowRef(false)
let generation = 0
let sessionHost: HTMLElement | null = null

const sources = computed<StreamSource[]>(() => props.src.trim()
  ? [{ protocol: 'mp4', url: props.src.trim(), label: '录像' }]
  : normalizeStreamSources(props.result))
const displayState = computed(() => props.error ? 'error' : props.loading ? 'loading' : state.value)
const displayMessage = computed(() => props.error || (props.loading ? props.loadingText : message.value))
const idlePoster = computed(() =>
  props.poster || (
    isEasyPlayerPosterUrl(playerSettings.settings.posterUrl)
      ? playerSettings.settings.posterUrl.trim()
      : ''
  )
)

async function destroyPlayer() {
  const current = session.value
  const currentHost = sessionHost
  session.value = null
  sessionHost = null
  currentHost?.remove()
  if (current) await current.destroy()
}

async function stop() {
  generation += 1
  state.value = 'idle'
  message.value = ''
  activeProtocol.value = null
  await destroyPlayer()
}

async function start() {
  const currentGeneration = ++generation
  state.value = sources.value.length ? 'loading' : 'idle'
  activeProtocol.value = null
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

    // 每次启动使用独立挂载点，迟到的销毁或失败不会清空新播放器。
    const container = document.createElement('div')
    host.value.appendChild(container)
    sessionHost = container
    try {
      const nextSession = await selected.adapter.play(container, selected.source, {
        muted: props.muted,
        poster: props.poster || undefined,
        settings: playerSettings.settings,
        duration: props.duration,
        onError: (reason) => {
          if (currentGeneration !== generation) return
          state.value = 'error'
          message.value = reason
          emit('error', reason)
        },
        onEnded: () => {
          if (currentGeneration === generation) emit('ended')
        },
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
      container.remove()
      if (sessionHost === container) sessionHost = null
    }
  }

  await destroyPlayer()
  if (currentGeneration !== generation) return
  state.value = 'error'
  message.value = errors[errors.length - 1] || '播放器启动失败'
  emit('error', message.value)
}

watch(
  () => [props.result, props.src, props.duration, props.protocolPriority, props.autoplay, props.loading, playerSettings.revision] as const,
  () => {
    if (props.autoplay && !props.loading) void start()
    else void stop()
  },
  { immediate: true, deep: true, flush: 'post' }
)

watch(() => props.muted, (muted) => session.value?.setMuted?.(muted))
watch(idlePoster, () => { posterFailed.value = false })

onBeforeUnmount(() => {
  void stop()
})

function retry() {
  if (props.error || !sources.value.length) emit('retry')
  else void start()
}

defineExpose({ play: start, destroy: stop })
</script>

<template>
  <div class="stream-player" :class="{ 'stream-player-light-state': idleTheme === 'light' && displayState !== 'ready', 'stream-player-has-poster': idlePoster && !posterFailed }" :aria-busy="displayState === 'loading'">
    <img
      v-if="idlePoster && !posterFailed && displayState !== 'ready'"
      :src="idlePoster"
      class="stream-player-poster"
      :alt="poster ? '通道快照' : '播放器默认封面'"
      @error="posterFailed = true"
    />
    <div ref="host" class="stream-player-host" />
    <div v-if="displayState !== 'ready'" class="stream-player-state" :role="displayState === 'error' ? 'alert' : 'status'">
      <template v-if="displayState === 'idle'">
        <slot name="idle">
          <button v-if="idleAction" class="stream-player-action" type="button" :disabled="disabled" :aria-label="idleAction" :title="idleAction" @click.stop="emit('requestPlay')"><Play aria-hidden="true" /></button>
          <span v-else class="stream-player-action stream-player-symbol" aria-hidden="true"><Play /></span>
          <small class="stream-player-hint">{{ idleHint }}</small>
        </slot>
      </template>
      <template v-else>
        <LoaderCircle v-if="displayState === 'loading'" class="animate-spin" aria-hidden="true" />
        <CircleAlert v-else aria-hidden="true" />
        <small>{{ displayMessage }}</small>
        <button v-if="displayState === 'error'" class="stream-player-action" type="button" :disabled="disabled" aria-label="重试播放" title="重试播放" @click.stop="retry"><Play aria-hidden="true" /></button>
      </template>
    </div>
    <span v-if="activeProtocol && activeProtocol !== 'mp4' && displayState === 'ready'" class="stream-player-protocol">{{ activeProtocol }}</span>
  </div>
</template>

<style scoped>
.stream-player { position: absolute; inset: 0; overflow: hidden; color: #91a3ba; background: #0b1422; }
.stream-player-host, .stream-player-host :deep(> *) { width: 100%; height: 100%; }
.stream-player-poster { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: cover; opacity: .7; }
.stream-player-state { position: absolute; inset: 0; display: grid; place-content: center; justify-items: center; gap: 10px; padding: 18px; text-align: center; background: #0b1422; }
.stream-player-state svg { width: 24px; height: 24px; }
.stream-player-state small { max-width: 360px; font-size: 12px; line-height: 1.5; overflow-wrap: anywhere; }
.stream-player-action { display: inline-flex; align-items: center; justify-content: center; flex-shrink: 0; width: 56px; height: 56px; padding: 0; color: var(--muted); background: transparent; border: 1px solid var(--line-strong); border-radius: 50%; cursor: pointer; }
.stream-player-action svg { width: 24px; height: 24px; transform: translateX(1px); }
button.stream-player-action:hover:not(:disabled) { color: var(--blue); background: var(--blue-soft); border-color: var(--blue); }
.stream-player-symbol { cursor: default; }
.stream-player-action:disabled { opacity: .55; cursor: not-allowed; }
.stream-player-light-state { color: var(--muted); background: var(--panel-3); }
.stream-player-light-state .stream-player-state { background: var(--panel-3); }
.stream-player-light-state.stream-player-has-poster .stream-player-state { background: rgba(240, 242, 245, .9); }
.stream-player-protocol { position: absolute; right: 8px; bottom: 8px; padding: 2px 6px; color: #d8e8fb; background: rgba(7, 12, 20, .72); border-radius: 4px; font: 700 11px "SFMono-Regular", monospace; text-transform: uppercase; pointer-events: none; }
</style>
