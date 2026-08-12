<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { X } from '@lucide/vue'

const props = defineProps<{ open: boolean; title: string; description?: string }>()
const emit = defineEmits<{ close: [] }>()
const panel = ref<HTMLElement>()
const closeButton = ref<HTMLButtonElement>()
let previousFocus: HTMLElement | null = null
let previousBodyOverflow = ''

const focusableSelector = 'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'

function focusFirst() {
  const first = panel.value?.querySelector<HTMLElement>(focusableSelector)
  ;(first || closeButton.value)?.focus()
}

function onKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') {
    event.preventDefault()
    emit('close')
    return
  }
  if (event.key !== 'Tab' || !panel.value) return
  const items = [...panel.value.querySelectorAll<HTMLElement>(focusableSelector)]
  if (!items.length) return
  const first = items[0]
  const last = items[items.length - 1]
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault()
    last.focus()
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault()
    first.focus()
  }
}

watch(() => props.open, async (open) => {
  if (open) {
    previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
    previousBodyOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    document.addEventListener('keydown', onKeydown)
    await nextTick()
    focusFirst()
  } else {
    document.removeEventListener('keydown', onKeydown)
    document.body.style.overflow = previousBodyOverflow
    await nextTick()
    previousFocus?.focus()
    previousFocus = null
  }
})

onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown)
  document.body.style.overflow = previousBodyOverflow
})
</script>

<template>
  <Teleport to="body">
    <Transition name="palette">
      <div v-if="open" class="modal-backdrop" role="presentation" @mousedown.self="emit('close')">
        <section ref="panel" class="modal" role="dialog" aria-modal="true" :aria-label="title" @mousedown.stop>
          <div class="modal-head">
            <div><h2>{{ title }}</h2><p v-if="description">{{ description }}</p></div>
            <button ref="closeButton" type="button" class="icon-btn" aria-label="关闭对话框" @click="emit('close')"><X /></button>
          </div>
          <slot />
          <footer v-if="$slots.footer" class="modal-foot"><slot name="footer" /></footer>
        </section>
      </div>
    </Transition>
  </Teleport>
</template>
