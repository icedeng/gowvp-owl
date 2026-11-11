<script setup lang="ts">
import { Camera, Clapperboard, ImageOff, Play, ShieldAlert } from '@lucide/vue'
import type { ApiEvent } from '../../types/api'
import { api } from '../../services/api'
import { formatDate } from '../../utils/format'
import { alarmSummary, channelLabel, deviceLabel, eventTitle, evidenceLabel, isAlarm, statusLabels } from './format'

defineProps<{ item: ApiEvent; selected: boolean; imageFailed: boolean }>()
const emit = defineEmits<{ open: [id: number]; select: [id: number, checked: boolean]; imageError: [id: number] }>()
</script>

<template>
  <article class="event-preview-card" :class="{ 'is-selected': selected }">
    <div class="event-preview-frame">
      <button type="button" class="event-preview-open" :aria-label="`查看 ${deviceLabel(item)} 的 ${eventTitle(item)}事件`" @click="emit('open', item.id)">
        <img v-if="item.image_path && !imageFailed" :src="api.eventThumbnail(item.image_path)" :alt="`${deviceLabel(item)} · ${eventTitle(item)}现场抓拍`" loading="lazy" @error="emit('imageError', item.id)">
        <span v-else class="event-preview-empty">
          <ImageOff v-if="imageFailed" aria-hidden="true" />
          <ShieldAlert v-else-if="isAlarm(item)" aria-hidden="true" />
          <Camera v-else aria-hidden="true" />
          <strong>{{ eventTitle(item) }}</strong>
          <span>{{ imageFailed ? '抓拍图片不可用' : item.evidence?.snapshot_state === 'queued' || item.evidence?.snapshot_state === 'working' ? `抓拍${evidenceLabel(item.evidence.snapshot_state)}` : '本事件未保存抓拍' }}</span>
        </span>
      </button>
      <label class="event-preview-select">
        <input type="checkbox" :checked="selected" :aria-label="`选择 ${deviceLabel(item)} 的 ${eventTitle(item)}事件`" @change="emit('select', item.id, ($event.target as HTMLInputElement).checked)">
      </label>
      <span class="status event-preview-status" :class="item.status === 'resolved' ? 'online' : item.status === 'ignored' ? '' : 'pending'">{{ statusLabels[item.status] }}</span>
      <div class="event-preview-reveal">
        <div class="event-preview-details">
          <dl>
            <div><dt>设备编码</dt><dd>{{ item.device_code || item.did || '—' }}</dd></div>
            <div><dt>通道编码</dt><dd>{{ item.channel_code || item.cid || '设备级事件' }}</dd></div>
          </dl>
          <p :title="alarmSummary(item)">事件 #{{ item.id }} · {{ alarmSummary(item) || item.model || '未提供附加报警信息' }}</p>
          <div class="event-preview-evidence">
            <span :class="{ 'is-failed': imageFailed || item.evidence?.snapshot_state === 'failed' }"><Camera aria-hidden="true" />{{ item.image_path ? imageFailed ? '图片不可用' : '有抓拍' : `抓拍${evidenceLabel(item.evidence?.snapshot_state)}` }}</span>
            <span :class="{ 'is-failed': item.evidence?.video_state === 'failed' }"><Clapperboard aria-hidden="true" />{{ item.video_path ? '有视频片段' : `视频${evidenceLabel(item.evidence?.video_state)}` }}</span>
          </div>
        </div>
        <div class="event-preview-actions">
          <button type="button" class="btn btn-sm btn-primary" @click="emit('open', item.id)">查看 / 处理</button>
          <RouterLink v-if="item.cid" class="btn btn-sm" :to="`/live?channel=${encodeURIComponent(item.cid)}`"><Play aria-hidden="true" />实时画面</RouterLink>
        </div>
      </div>
    </div>
    <div class="event-preview-body">
      <div class="event-preview-heading"><button type="button" @click="emit('open', item.id)">{{ eventTitle(item) }}</button><span>{{ isAlarm(item) ? '设备报警' : 'AI 检测' }}</span></div>
      <div class="event-preview-identity">
        <strong class="event-preview-device" :title="deviceLabel(item)">{{ deviceLabel(item) }}</strong>
        <span class="event-preview-separator" aria-hidden="true">/</span>
        <span class="event-preview-channel" :title="channelLabel(item)">{{ channelLabel(item) }}</span>
      </div>
      <time class="event-preview-timestamp">{{ formatDate(item.started_at || item.created_at) }}<template v-if="(item.occurrence_count || 1) > 1"> · 连续 {{ item.occurrence_count }} 次</template></time>
    </div>
  </article>
</template>

<style scoped>
.event-preview-card { min-width: 0; overflow: hidden; border: 1px solid var(--line); border-radius: var(--radius); background: var(--panel); transition: border-color .16s ease; }
.event-preview-card:hover, .event-preview-card:focus-within, .event-preview-card.is-selected { border-color: var(--blue); }
.event-preview-frame { position: relative; width: 100%; min-width: 0; max-width: 100%; aspect-ratio: 16 / 10; min-height: 224px; background: var(--panel-2); }
.event-preview-open { position: absolute; inset: 0; width: 100%; height: 100%; padding: 0; border: 0; background: transparent; color: var(--muted); cursor: pointer; }
.event-preview-open img { width: 100%; height: 100%; object-fit: cover; }
.event-preview-empty { height: 100%; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 8px; padding: 44px 14px 14px; font-size: 12px; }
.event-preview-empty > svg { width: 28px; height: 28px; color: var(--muted); }
.event-preview-empty strong { color: var(--ink); font-size: 14px; }
.event-preview-select { position: absolute; z-index: 2; top: 10px; left: 10px; display: grid; place-items: center; width: 28px; height: 28px; border: 1px solid var(--line-strong); border-radius: var(--radius-sm); background: var(--panel); cursor: pointer; }
.event-preview-select input { margin: 0; cursor: pointer; }
.event-preview-status { position: absolute; z-index: 2; top: 10px; right: 10px; }
.event-preview-reveal { position: absolute; inset: 0; display: flex; flex-direction: column; gap: 10px; padding: 48px 12px 12px; background: var(--panel-2); opacity: 0; visibility: hidden; transition: opacity .16s ease, visibility .16s ease; }
.event-preview-card:hover .event-preview-reveal, .event-preview-card:focus-within .event-preview-reveal { opacity: 1; visibility: visible; }
.event-preview-details { display: grid; align-content: start; gap: 8px; min-height: 0; overflow: auto; }
.event-preview-reveal dl { display: grid; gap: 6px; margin: 0; font-size: 12px; }
.event-preview-reveal dl > div { display: flex; align-items: baseline; gap: 8px; min-width: 0; }
.event-preview-reveal dt { flex-shrink: 0; color: var(--muted); }
.event-preview-reveal dd { margin: 0; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }
.event-preview-reveal p { display: -webkit-box; -webkit-box-orient: vertical; -webkit-line-clamp: 2; overflow: hidden; margin: 0; color: var(--muted); font-size: 12px; line-height: 1.5; }
.event-preview-actions { display: flex; flex-shrink: 0; flex-wrap: wrap; gap: 8px; align-items: center; margin-top: auto; }
.event-preview-body { display: grid; gap: 5px; padding: 12px; border-top: 1px solid var(--line); }
.event-preview-heading { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; margin-bottom: 4px; }
.event-preview-heading button { min-width: 0; padding: 0; border: 0; background: transparent; color: var(--blue); font-size: 14px; font-weight: 600; text-align: left; overflow-wrap: anywhere; cursor: pointer; }
.event-preview-heading button:hover { text-decoration: underline; text-underline-offset: 3px; }
.event-preview-heading > span { flex-shrink: 0; color: var(--muted); font-size: 12px; }
.event-preview-identity { display: flex; align-items: baseline; gap: 6px; min-width: 0; }
.event-preview-separator { flex-shrink: 0; color: var(--muted); font-size: 12px; }
.event-preview-device, .event-preview-channel { min-width: 0; max-width: calc(60% - 10px); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.event-preview-device { font-size: 13px; font-weight: 600; }
.event-preview-channel, .event-preview-timestamp { color: var(--muted); font-size: 12px; }
.event-preview-timestamp { font-variant-numeric: tabular-nums; }
.event-preview-evidence { display: flex; flex-wrap: wrap; gap: 6px 12px; color: var(--muted); font-size: 12px; }
.event-preview-evidence span { display: inline-flex; gap: 5px; align-items: center; }
.event-preview-evidence svg { width: 14px; height: 14px; }
.event-preview-evidence .is-failed { color: var(--red); }
@media (hover: none), (pointer: coarse) {
  .event-preview-frame { aspect-ratio: auto; }
  .event-preview-open { position: relative; display: block; height: auto; aspect-ratio: 16 / 10; }
  .event-preview-reveal { position: relative; padding-top: 12px; opacity: 1; visibility: visible; }
  .event-preview-details { overflow: visible; }
  .event-preview-select { width: 44px; height: 44px; }
  .event-preview-actions .btn { min-height: 44px; }
  .event-preview-heading button { min-height: 44px; }
}
@media (prefers-reduced-motion: reduce) { .event-preview-card, .event-preview-reveal { transition: none; } }
</style>
