<script setup lang="ts">
import { stateLabel, stateTone } from '../presentation'
defineProps<{ value: string }>()
</script>

<template><span class="status" :class="stateTone(value)">{{ stateLabel(value) }}</span></template>
