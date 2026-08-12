import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { clearSession, currentUser, login, storeSession } from '../services/auth'
import { errorMessage } from '../services/api'

export const useSessionStore = defineStore('session', () => {
  const user = ref(currentUser())
  const loading = ref(false)
  const error = ref('')
  const isAuthenticated = computed(() => Boolean(localStorage.getItem('owl-token')))

  async function signIn(username: string, password: string) {
    loading.value = true
    error.value = ''
    try {
      const response = await login(username, password)
      storeSession(response)
      user.value = response.user
      return response
    } catch (cause) {
      error.value = errorMessage(cause, '登录失败，请检查账号和密码')
      throw cause
    } finally {
      loading.value = false
    }
  }

  function signOut() {
    clearSession()
    user.value = ''
  }

  return { user, loading, error, isAuthenticated, signIn, signOut }
})
