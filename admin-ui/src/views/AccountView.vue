<script setup lang="ts">
import { computed, ref } from 'vue'
import { KeyRound, LoaderCircle, LogOut, Save, ShieldAlert, ShieldCheck, UserRound } from '@lucide/vue'
import { useRouter } from 'vue-router'
import { http } from '../services/http'
import { errorMessage } from '../services/api'
import { useSessionStore } from '../stores/session'
import { useUiStore } from '../stores/ui'

const router = useRouter()
const ui = useUiStore()
const session = useSessionStore()
const username = ref(session.user)
const password = ref('')
const confirmPassword = ref('')
const saving = ref(false)
const formError = ref('')
const valid = computed(() => username.value.trim().length > 0 && password.value.length >= 8 && password.value === confirmPassword.value)

async function save() {
  formError.value = ''
  if (!valid.value) {
    formError.value = password.value.length < 8 ? '新密码至少需要 8 个字符' : '两次输入的密码不一致'
    return
  }
  saving.value = true
  try {
    await http.put('/users', { username: username.value.trim(), password: password.value })
    ui.toast('管理员凭据已更新，请使用新凭据重新登录')
    session.signOut()
    await router.replace('/login')
  } catch (cause) {
    formError.value = errorMessage(cause, '凭据更新失败')
  } finally { saving.value = false }
}

async function logout() {
  session.signOut()
  await router.replace('/login')
}
</script>

<template><main class="page-content"><header class="page-head"><div><h1 class="page-title">账号安全</h1><p class="page-desc">当前平台为单管理员模式，可更新用户名与密码；保存后需要重新登录。</p></div><button class="btn" @click="logout"><LogOut />退出登录</button></header><section class="content-grid"><form class="card form-section" @submit.prevent="save"><div class="card-head"><div><h2 class="card-title">修改管理员凭据</h2><p class="card-sub">保存后后端将凭据写回配置文件</p></div><KeyRound /></div><div v-if="formError" class="warning-box mb-4" role="alert"><ShieldAlert /><span>{{ formError }}</span></div><div class="form-grid"><label class="form-group full"><span class="form-label">管理员用户名</span><input v-model="username" class="input plain w-full" autocomplete="username" required /></label><label class="form-group"><span class="form-label">新密码</span><input v-model="password" class="input plain w-full" type="password" minlength="8" autocomplete="new-password" required /></label><label class="form-group"><span class="form-label">确认新密码</span><input v-model="confirmPassword" class="input plain w-full" type="password" autocomplete="new-password" required /></label></div><div class="settings-savebar"><span>该操作会立即修改当前环境的登录凭据</span><button class="btn btn-primary" :disabled="saving || !valid"><LoaderCircle v-if="saving" class="animate-spin" /><Save v-else />{{ saving ? '正在保存…' : '保存并重新登录' }}</button></div></form><aside class="grid"><article class="card card-pad"><div class="card-head"><div><h2 class="card-title">当前账号</h2><p class="card-sub">单管理员模式</p></div><UserRound /></div><div class="details-identity"><span class="avatar !h-12 !w-12">{{ (session.user || 'OP').slice(0,2).toUpperCase() }}</span><div><h2>{{ session.user || '管理员' }}</h2><p>系统管理员 · 当前会话</p></div></div></article><article class="card card-pad"><div class="card-head"><div><h2 class="card-title">会话安全</h2><p class="card-sub">JWT 默认有效期 3 天</p></div><ShieldCheck /></div><div class="read-only">登录凭据通过 RSA-OAEP 加密后提交；遇到 401 响应时管理端会清理令牌并返回登录页，登录后可回到原页面。</div></article></aside></section></main></template>
