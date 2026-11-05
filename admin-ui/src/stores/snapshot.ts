import { ref } from 'vue'
import { defineStore } from 'pinia'
import { api } from '../services/api'
import type { SnapshotConfig } from '../types/api'

export const defaultSnapshotConfig: SnapshotConfig = {
  upload_timeout_seconds: 120,
  ffmpeg_concurrency: 2,
  immediate_dispatch: false,
  scan_interval_ms: 1000,
  poll_interval_ms: 2000,
  ffmpeg_mode: 'multi_frame',
  frame_count: 5,
  skip_frames: 75,
  frame_interval_ms: 1000,
  stream_timeout_seconds: 60,
  ffmpeg_timeout_seconds: 35,
  rtsp_timeout_seconds: 10,
  probe_timeout_seconds: 10,
  analyze_duration_ms: 5000,
  probe_size_bytes: 5000000,
  jpeg_quality: 2,
}

export const useSnapshotStore = defineStore('snapshot', () => {
  const settings = ref<SnapshotConfig>({ ...defaultSnapshotConfig })
  let inflight: Promise<SnapshotConfig> | undefined
  function apply(value: SnapshotConfig) { settings.value = { ...defaultSnapshotConfig, ...value } }
  function load() {
    if (!inflight) {
      inflight = api.snapshotConfig().then(({ data }) => {
        apply(data)
        return settings.value
      }).finally(() => { inflight = undefined })
    }
    return inflight
  }
  return { settings, apply, load }
})
