<script setup lang="ts">
import { computed, reactive } from 'vue';
import { ArrowUp, RefreshCw, Square } from '@lucide/vue';
import type { ApiChannel } from '../types/api';
import { buildPTZRequest, ptzActionDisabledReason, ptzCapabilityLabel, type PTZParameters, type PTZRequest } from '../utils/ptz';
import PTZActionButton from './PTZActionButton.vue';

const props = defineProps<{
  channel: ApiChannel;
  gbVersion: string;
  gbCapabilities?: string[];
  deviceAvailable: boolean;
  busy: boolean;
  feedback: string;
}>();
const emit = defineEmits<{ control: [request: PTZRequest]; invalid: [message: string] }>();
const form = reactive<PTZParameters>({
  speed: 30, duration: 1, preset: 1, group: 1, cruiseSpeed: 40, cruiseStay: 10,
  scanGroup: 1, scanSpeed: 40, aux: 1, auxValue: 1, homeSeconds: 60, homePreset: 1,
  pan: 0, tilt: 0, zoom: 1,
});
const context = computed(() => ({
  channel: props.channel, gbVersion: props.gbVersion, gbCapabilities: props.gbCapabilities,
  deviceAvailable: props.deviceAvailable, busy: props.busy,
}));
const unavailableReason = computed(() => ptzActionDisabledReason('stop', { ...context.value, busy: false }));
const directions = [
  { action: 'left_up', label: '左上', angle: -45 }, { action: 'up', label: '向上', angle: 0 }, { action: 'right_up', label: '右上', angle: 45 },
  { action: 'left', label: '向左', angle: -90 }, { action: 'stop', label: '停止方向与变倍', angle: 0 }, { action: 'right', label: '向右', angle: 90 },
  { action: 'left_down', label: '左下', angle: -135 }, { action: 'down', label: '向下', angle: 180 }, { action: 'right_down', label: '右下', angle: 135 },
];
const lenses = [
  { label: '变倍', actions: [{ action: 'zoom_out', label: '拉远 −' }, { action: 'zoom_in', label: '拉近 +' }] },
  { label: '聚焦', actions: [{ action: 'focus_near', label: '近焦 −' }, { action: 'focus_far', label: '远焦 +' }] },
  { label: '光圈', actions: [{ action: 'iris_close', label: '缩小 −' }, { action: 'iris_open', label: '放大 +' }] },
];
const presets = [{ action: 'preset_call', label: '调用' }, { action: 'preset_set', label: '设置' }, { action: 'preset_delete', label: '删除' }];
const cruiseActions = [
  { action: 'cruise_add', label: '加入预置位' }, { action: 'cruise_del', label: '移除预置位' },
  { action: 'cruise_speed', label: '设置速度' }, { action: 'cruise_stay', label: '设置停留' },
  { action: 'cruise_start', label: '开始巡航' }, { action: 'cruise_stop', label: '停止巡航' },
];
const scanActions = [
  { action: 'scan_left', label: '设左边界' }, { action: 'scan_right', label: '设右边界' },
  { action: 'scan_speed', label: '设置速度' }, { action: 'scan_start', label: '开始扫描' }, { action: 'scan_stop', label: '停止扫描' },
];
function reason(action: string) {
  const unsupported = ptzActionDisabledReason(action, context.value);
  if (unsupported) return unsupported;
  try { buildPTZRequest(action, form, context.value); return ''; }
  catch (cause) { return cause instanceof Error ? cause.message : '请检查控制参数'; }
}
function control(action: string) {
  try { emit('control', buildPTZRequest(action, form, context.value)); }
  catch (cause) { emit('invalid', cause instanceof Error ? cause.message : '请检查控制参数'); }
}
</script>

<template>
  <article class="card control-panel channel-ptz-panel">
    <div class="card-head">
      <div><h3 class="card-title">云台控制</h3><p class="card-sub">{{ channel.type === 'ONVIF' ? '方向与变倍点动 1 秒' : `连续动作点动 ${form.duration || '—'} 秒` }}，自动停止</p></div>
      <span class="status" :class="channel.ptz_capable ? 'online' : ''">{{ !['GB28181', 'ONVIF'].includes(channel.type || '') ? '不支持' : channel.ptz_capable ? '声明支持' : '能力未知' }}</span>
    </div>
    <div class="ptz-settings">
      <label><span>速度 · 1–255</span><input v-model.number="form.speed" class="input plain" type="number" min="1" max="255" step="1" /></label>
      <label :title="channel.type === 'ONVIF' ? '当前 ONVIF 接入固定点动 1 秒，暂不支持调整时长' : '连续动作到期后由服务端自动停止'"><span>点动时长</span><select v-model.number="form.duration" class="select" :disabled="channel.type === 'ONVIF'"><option v-for="seconds in [1, 2, 3, 5, 10]" :key="seconds" :value="seconds">{{ seconds }} 秒</option></select></label>
    </div>
    <div class="ptz-common">
      <div class="ptz-directions" aria-label="八方向云台控制">
        <PTZActionButton v-for="item in directions" :key="item.action" :label="item.label" :reason="reason(item.action)" :class="{ 'ptz-stop': item.action === 'stop' }" @click="control(item.action)">
          <Square v-if="item.action === 'stop'" aria-hidden="true" /><ArrowUp v-else :style="{ transform: `rotate(${item.angle}deg)` }" aria-hidden="true" />
        </PTZActionButton>
      </div>
      <div class="ptz-lenses">
        <div v-for="row in lenses" :key="row.label" class="ptz-lens-row"><span>{{ row.label }}</span><PTZActionButton v-for="item in row.actions" :key="item.action" :label="item.label" :reason="reason(item.action)" @click="control(item.action)" /></div>
      </div>
    </div>
    <PTZActionButton class="ptz-wide" label="停止聚焦 / 光圈" :reason="reason('fi_stop')" @click="control('fi_stop')" />

    <section class="ptz-section" aria-label="预置位控制">
      <h4>预置位</h4>
      <label class="ptz-inline"><span>编号</span><input v-model.number="form.preset" class="input plain" type="number" min="1" max="255" step="1" aria-label="预置位编号" /></label>
      <div class="ptz-buttons three"><PTZActionButton v-for="item in presets" :key="item.action" :label="item.label" :reason="reason(item.action)" :hint="item.action === 'preset_set' ? '将当前位置保存到该编号；已有预置位将被覆盖' : item.action === 'preset_delete' ? '删除该编号的预置位' : '转到该编号的预置位'" @click="control(item.action)" /></div>
    </section>

    <details class="ptz-section ptz-advanced">
      <summary>高级控制<span>巡航 · 扫描 · 看守</span></summary>
      <section class="ptz-section">
        <h4>巡航</h4><p>先在预置位区选择编号，再加入或移除。</p>
        <div class="ptz-settings">
          <label><span>巡航组 · 1–255</span><input v-model.number="form.group" class="input plain" type="number" min="1" max="255" step="1" /></label>
          <label><span>巡航速度 · 1–4095</span><input v-model.number="form.cruiseSpeed" class="input plain" type="number" min="1" max="4095" step="1" /></label>
          <label><span>停留时间（秒）</span><input v-model.number="form.cruiseStay" class="input plain" type="number" min="1" max="4095" step="1" /></label>
        </div>
        <div class="ptz-buttons"><PTZActionButton v-for="item in cruiseActions" :key="item.action" :label="item.label" :reason="reason(item.action)" @click="control(item.action)" /></div>
      </section>
      <section class="ptz-section">
        <h4>水平扫描</h4><p>先点动到边界位置，再设置左右边界。巡航与扫描须手动停止。</p>
        <div class="ptz-settings">
          <label><span>扫描组 · 1–255</span><input v-model.number="form.scanGroup" class="input plain" type="number" min="1" max="255" step="1" /></label>
          <label><span>扫描速度 · 1–4095</span><input v-model.number="form.scanSpeed" class="input plain" type="number" min="1" max="4095" step="1" /></label>
        </div>
        <div class="ptz-buttons"><PTZActionButton v-for="item in scanActions" :key="item.action" :label="item.label" :reason="reason(item.action)" @click="control(item.action)" /></div>
      </section>
      <section class="ptz-section">
        <h4>辅助开关</h4><p>编号与用途以设备说明为准。</p>
        <div class="ptz-settings">
          <label><span>开关编号 · 1–255</span><input v-model.number="form.aux" class="input plain" type="number" min="1" max="255" step="1" /></label>
          <label><span>参数值 · 1–4095</span><input v-model.number="form.auxValue" class="input plain" type="number" min="1" max="4095" step="1" /></label>
        </div>
        <div class="ptz-buttons"><PTZActionButton label="开启" :reason="reason('aux_on')" @click="control('aux_on')" /><PTZActionButton label="关闭" :reason="reason('aux_off')" @click="control('aux_off')" /></div>
      </section>
      <section class="ptz-section">
        <h4>看守位<span>GB28181-2016 / 2022</span></h4>
        <div class="ptz-settings">
          <label><span>等待时间（秒）</span><input v-model.number="form.homeSeconds" class="input plain" type="number" min="1" max="86400" step="1" /></label>
          <label><span>看守预置位 · 1–255</span><input v-model.number="form.homePreset" class="input plain" type="number" min="1" max="255" step="1" /></label>
        </div>
        <div class="ptz-buttons"><PTZActionButton label="启用看守" :reason="reason('home_enable')" @click="control('home_enable')" /><PTZActionButton label="关闭看守" :reason="reason('home_disable')" @click="control('home_disable')" /></div>
      </section>
      <section class="ptz-section">
        <h4>精准定位<span>GB28181-2022</span></h4>
        <div class="ptz-settings">
          <label><span>水平角 · 0–360°</span><input v-model.number="form.pan" class="input plain" type="number" min="0" max="360" step="0.1" /></label>
          <label><span>俯仰角 · −30–90°</span><input v-model.number="form.tilt" class="input plain" type="number" min="-30" max="90" step="0.1" /></label>
          <label><span>变倍倍率 · ≥1</span><input v-model.number="form.zoom" class="input plain" type="number" min="1" step="0.1" /></label>
        </div>
        <PTZActionButton class="ptz-wide" label="转到指定位置" :reason="reason('ptz_precise')" @click="control('ptz_precise')" />
      </section>
    </details>
    <p class="capability-reason">{{ unavailableReason || ptzCapabilityLabel(channel) }}</p>
    <p v-if="feedback" class="ptz-feedback" role="status" aria-live="polite">{{ feedback }}</p>
    <PTZActionButton class="ptz-wide" label="检测信令连通性" hint="发送停止指令检测信令，不验证摄像头是否实际转动" :reason="reason('probe')" @click="control('probe')"><RefreshCw aria-hidden="true" />检测信令连通性</PTZActionButton>
  </article>
</template>

<style scoped>
.channel-ptz-panel { min-width: 0; }
.ptz-settings { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px; margin-bottom: 10px; }
.ptz-settings label { display: grid; gap: 5px; min-width: 0; }
.ptz-settings label > span, .ptz-inline > span { color: var(--muted); font-size: 12px; }
.ptz-settings .input, .ptz-inline .input { width: 100%; min-width: 0; }
.ptz-common { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; margin: 12px 0; }
.ptz-directions { display: grid; grid-template-columns: repeat(3, 34px); gap: 4px; }
.ptz-directions :deep(.btn) { height: 34px; min-height: 34px; padding: 0; }
.ptz-directions :deep(svg) { width: 17px; height: 17px; }
.ptz-stop :deep(.btn) { color: var(--blue); background: var(--blue-soft); }
.ptz-lenses { display: grid; gap: 5px; flex: 1; min-width: 140px; }
.ptz-lens-row { display: grid; grid-template-columns: 24px repeat(2, minmax(0, 1fr)); align-items: center; gap: 4px; font-size: 12px; }
.ptz-lens-row :deep(.btn) { font-size: 12px; padding: 0 5px; }
.ptz-wide { width: 100%; }
.ptz-section { margin-top: 12px; padding-top: 12px; border-top: 1px solid var(--line); }
.ptz-section h4 { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 5px; margin: 0 0 8px; font-size: 13px; font-weight: 500; }
.ptz-section h4 span, .ptz-advanced summary span { color: var(--muted); font-size: 11px; font-weight: 400; }
.ptz-section p { margin: 0 0 8px; font-size: 12px; line-height: 1.6; color: var(--muted); }
.ptz-inline { display: grid; grid-template-columns: 32px minmax(0, 1fr); align-items: center; gap: 8px; margin-bottom: 8px; }
.ptz-buttons { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 6px; }
.ptz-buttons.three { grid-template-columns: repeat(3, minmax(0, 1fr)); }
.ptz-advanced > summary { cursor: pointer; font-size: 13px; }
.ptz-advanced > summary span { float: right; }
.ptz-feedback { padding: 8px; border: 1px solid var(--line); border-radius: var(--radius-sm); background: var(--panel-2); font-size: 12px; line-height: 1.7; overflow-wrap: anywhere; }
</style>
