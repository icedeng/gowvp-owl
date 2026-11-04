<script setup lang="ts">
defineProps<{ label: string; reason?: string; hint?: string }>();
defineEmits<{ click: [] }>();
</script>

<template>
  <!-- 禁用按钮不接收鼠标事件，由外层承接悬停与键盘焦点，确保原因可查看。 -->
  <span class="ptz-action" :title="reason || hint || label" :tabindex="reason ? 0 : undefined" :aria-label="reason ? `${label}：${reason}` : undefined">
    <button class="btn" type="button" :disabled="Boolean(reason)" :aria-label="label" @click="$emit('click')"><slot>{{ label }}</slot></button>
  </span>
</template>

<style scoped>
.ptz-action { display: inline-flex; min-width: 0; border-radius: var(--radius-sm); }
.ptz-action:focus-visible { outline: 2px solid var(--blue); outline-offset: 2px; }
.ptz-action .btn { width: 100%; padding: 0 8px; }
.ptz-action .btn:disabled { pointer-events: none; }
</style>
