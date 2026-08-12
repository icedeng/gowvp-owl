<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ArrowRight, Eye, EyeOff, LockKeyhole, RefreshCw, ShieldAlert, ShieldCheck, UserRound } from '@lucide/vue'
import brandMark from '../assets/brand-mark.svg'
import { useSessionStore } from '../stores/session'

const router = useRouter()
const route = useRoute()
const session = useSessionStore()
const username = ref('')
const password = ref('')
const showPassword = ref(false)
const captchaInput = ref('')
const captchaCode = ref('MZW9')
const captchaError = ref('')

const captchaChars = computed(() => captchaCode.value.split(''))

function refreshCaptcha() {
  const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789'
  captchaCode.value = Array.from({ length: 4 }, () => chars[Math.floor(Math.random() * chars.length)]).join('')
  captchaInput.value = ''
  captchaError.value = ''
}

async function submit() {
  captchaError.value = ''
  if (captchaInput.value.trim().toUpperCase() !== captchaCode.value) {
    captchaError.value = '验证码不正确，请重新输入'
    refreshCaptcha()
    return
  }
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
    <section class="login-art" aria-label="国标视频信号">
      <div class="signal-grid" aria-hidden="true" />
      <div class="signal-orbit orbit-one" aria-hidden="true" />
      <div class="signal-orbit orbit-two" aria-hidden="true" />
      <div class="signal-scanline" aria-hidden="true" />
      <div class="signal-node node-one" aria-hidden="true" />
      <div class="signal-node node-two" aria-hidden="true" />
      <div class="signal-node node-three" aria-hidden="true" />
      <div class="login-art-brand">
        <img :src="brandMark" alt="" />
        <span>国标视频管理平台</span>
      </div>
      <div class="login-art-content">
        <div class="signal-kicker"><span class="signal-kicker-dot" />平台信号矩阵 · LIVE OPERATIONS</div>
        <h2>看清每一条<span>视频信号。</span></h2>
        <p>统一管理 GB28181、ONVIF、RTMP 与 RTSP，从设备注册、实时监控到事件检索、录像取证和协议诊断，保持同一条运维链路。</p>
        <div class="signal-route" aria-label="信号处置链路">
          <div class="route-item route-active"><i /><strong>设备接入</strong><small>注册与同步</small></div>
          <div class="route-line" aria-hidden="true" />
          <div class="route-item"><i /><strong>实时预览</strong><small>多协议播放</small></div>
          <div class="route-line" aria-hidden="true" />
          <div class="route-item"><i /><strong>事件处置</strong><small>告警与录像</small></div>
        </div>
        <div class="signal-metrics">
          <div><strong>4</strong><span>接入协议</span></div>
          <div><strong>26</strong><span>在线通道</span></div>
          <div><strong>24/7</strong><span>持续值守</span></div>
        </div>
      </div>
      <div class="signal-caption">
        <span class="signal-live-dot" />
        <span>信号链路在线</span>
      </div>
    </section>
    <section class="login-panel">
      <form class="login-form" @submit.prevent="submit">
        <div class="login-heading">
          <h1>国标视频管理平台</h1>
        </div>
        <div v-if="session.error" class="warning-box mb-5">
          <ShieldAlert /><span>{{ session.error }}</span>
        </div>
        <div v-if="captchaError" class="warning-box mb-5">
          <ShieldAlert /><span>{{ captchaError }}</span>
        </div>
        <label class="form-group">
          <span class="form-label">登录账号</span>
          <span class="field"
            ><UserRound /><input
              v-model="username"
              class="input w-full"
              placeholder="请输入账号"
              autocomplete="username"
              autofocus
          /></span>
        </label>
        <label class="form-group">
          <span class="form-label">登录密码</span>
          <span class="field"
            ><LockKeyhole /><input
              v-model="password"
              class="input w-full pr-10"
              placeholder="请输入密码"
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
        <div class="captcha-head">
          <span class="form-label">图形验证码</span>
          <button type="button" class="captcha-refresh" @click="refreshCaptcha">
            <RefreshCw />换一张
          </button>
        </div>
        <div class="captcha-row">
          <label class="field captcha-input-field"
            ><ShieldCheck /><input
              v-model="captchaInput"
              class="input w-full"
              placeholder="请输入验证码"
              autocomplete="off"
              maxlength="4"
              aria-label="图形验证码"
          /></label>
          <button type="button" class="captcha-visual" aria-label="刷新验证码" @click="refreshCaptcha">
            <span
              v-for="(char, index) in captchaChars"
              :key="`${char}-${index}`"
              :style="{ transform: `rotate(${index % 2 ? 5 : -5}deg) translateY(${index % 2 ? 2 : -1}px)` }"
              >{{ char }}</span
            >
            <i v-for="line in 3" :key="line" :class="`captcha-line line-${line}`" />
          </button>
        </div>
        <button
          class="btn btn-primary login-submit"
          :disabled="session.loading || !username.trim() || !password || !captchaInput.trim()"
        >
          {{ session.loading ? "正在登录…" : "进入系统" }}<ArrowRight />
        </button>
      </form>
    </section>
  </main>
</template>
