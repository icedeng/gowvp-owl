<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Eye, EyeOff, LockKeyhole, Radio, ShieldAlert, ShieldCheck, UserRound } from '@lucide/vue'
import brandMark from '../assets/brand-mark.svg'
import { useSessionStore } from '../stores/session'

const router = useRouter()
const route = useRoute()
const session = useSessionStore()
const username = ref('')
const password = ref('')
const showPassword = ref(false)

async function submit() {
  try {
    await session.signIn(username.value.trim(), password.value)
    const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/overview'
    await router.replace(redirect)
  } catch {
    // 错误信息由会话 store 统一处理。
  }
}
</script>
<template>
  <main class="login-page">
    <section class="login-art" aria-label="产品简介">
      <div class="brand">
        <span class="brand-mark"
          ><img :src="brandMark" alt="国标视频管理平台标志"
        /></span>
        <span class="brand-name">国标视频管理平台</span>
      </div>
      <div class="login-copy">
        <h1>看清每一条<span>视频信号。</span></h1>
        <p>
          统一管理 GB28181、ONVIF、RTMP 与
          RTSP，从设备注册、实时监控到事件检索、录像取证和协议诊断保持同一条运维链路。
        </p>
      </div>
      <div class="login-node">
        <span><strong>4</strong>接入协议</span
        ><span><strong>实时</strong>运行数据</span
        ><span><strong>24/7</strong>连续值守</span>
      </div>
    </section>
    <section class="login-panel">
      <form class="login-form" @submit.prevent="submit">
        <div class="brand md:hidden">
          <span class="brand-mark"
            ><img :src="brandMark" alt="国标视频管理平台标志"
          /></span>
          <span class="brand-name">国标视频管理平台</span>
        </div>
        <h2>登录管理平台</h2>
        <p>使用部署环境中的管理员账号登录。</p>
        <div class="warning-box mb-5">
          <ShieldCheck />
          <span>凭据通过 RSA-OAEP 加密传输，登录成功后使用 JWT 会话。</span>
        </div>
        <div v-if="session.error" class="warning-box mb-5">
          <ShieldAlert /><span>{{ session.error }}</span>
        </div>
        <label class="form-group">
          <span class="form-label">账号</span>
          <span class="field"
            ><UserRound /><input
              v-model="username"
              class="input w-full"
              autocomplete="username"
              autofocus
          /></span>
        </label>
        <label class="form-group">
          <span class="form-label">密码</span>
          <span class="field"
            ><LockKeyhole /><input
              v-model="password"
              class="input w-full pr-10"
              :type="showPassword ? 'text' : 'password'"
              autocomplete="current-password"
            /><button
              type="button"
              class="absolute right-3 text-slate-500"
              :aria-label="showPassword ? '隐藏密码' : '显示密码'"
              @click="showPassword = !showPassword"
            >
              <EyeOff v-if="showPassword" class="h-4 w-4" />
              <Eye v-else class="h-4 w-4" /></button
          ></span>
        </label>
        <button
          class="btn btn-primary login-submit"
          :disabled="session.loading || !username.trim() || !password"
        >
          <Radio />{{ session.loading ? "正在安全登录…" : "登录并进入" }}
        </button>
        <p class="login-note">账号信息仅用于本次登录，不会写入前端配置。</p>
      </form>
    </section>
  </main>
</template>
