import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

export const useUiStore = defineStore('ui', () => {
  const sidebarOpen = ref(false)
  const sidebarCollapsed = ref(false)
  const commandOpen = ref(false)
  const toastMessage = ref('')
  let toastTimer: number | undefined

  const isToastVisible = computed(() => Boolean(toastMessage.value))
  function toggleSidebar() { sidebarOpen.value = !sidebarOpen.value }
  function closeSidebar() { sidebarOpen.value = false }
  function toggleSidebarCollapsed() { sidebarCollapsed.value = !sidebarCollapsed.value }
  function openCommand() { commandOpen.value = true }
  function closeCommand() { commandOpen.value = false }
  function toast(message: string) {
    toastMessage.value = message
    window.clearTimeout(toastTimer)
    toastTimer = window.setTimeout(() => { toastMessage.value = '' }, 2600)
  }

  return { sidebarOpen, sidebarCollapsed, commandOpen, toastMessage, isToastVisible, toggleSidebar, closeSidebar, toggleSidebarCollapsed, openCommand, closeCommand, toast }
})
