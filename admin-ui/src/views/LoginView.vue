<script setup lang="ts">
import { computed, nextTick, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ArrowRight, Eye, EyeOff, LoaderCircle, LockKeyhole, RefreshCw, ShieldCheck, UserRound } from '@lucide/vue'
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
const captchaInputElement = ref<HTMLInputElement | null>(null)
const usernameError = ref('')
const passwordError = ref('')
const captchaError = ref('')

const captchaChars = computed(() => captchaCode.value.split(''))

function refreshCaptcha(clearError = true) {
  const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789'
  captchaCode.value = Array.from({ length: 4 }, () => chars[Math.floor(Math.random() * chars.length)]).join('')
  captchaInput.value = ''
  if (clearError) captchaError.value = ''
  void nextTick(() => captchaInputElement.value?.focus())
}

function clearUsernameError() {
  usernameError.value = ''
}

function clearPasswordError() {
  passwordError.value = ''
  session.error = ''
}

function clearCaptchaError() {
  captchaError.value = ''
}

async function submit() {
  usernameError.value = username.value.trim() ? '' : '请输入登录账号'
  passwordError.value = password.value ? '' : '请输入登录密码'
  captchaError.value = ''
  session.error = ''

  if (usernameError.value || passwordError.value) return
  if (!captchaInput.value.trim()) {
    captchaError.value = '请输入图形验证码'
    captchaInputElement.value?.focus()
    return
  }
  if (captchaInput.value.trim().toUpperCase() !== captchaCode.value) {
    captchaError.value = '验证码不正确，请重新输入'
    refreshCaptcha(false)
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
        <span>多协议信号链路</span>
      </div>
    </section>
    <section class="login-panel">
      <form class="login-form" novalidate @submit.prevent="submit">
        <div class="login-heading">
          <span class="login-platform-name">国标视频管理平台</span>
          <h1>登录系统</h1>
          <p>使用平台账号进入视频管理工作台</p>
        </div>
        <div class="form-group">
          <label class="form-label" for="login-username">登录账号</label>
          <span class="field"
            ><UserRound /><input
              id="login-username"
              v-model="username"
              name="username"
              class="input w-full"
              placeholder="请输入账号"
              autocomplete="username"
              :aria-invalid="Boolean(usernameError)"
              :aria-describedby="usernameError ? 'login-username-error' : undefined"
              @input="clearUsernameError"
          /></span>
          <p v-if="usernameError" id="login-username-error" class="field-error" role="alert">{{ usernameError }}</p>
        </div>
        <div class="form-group">
          <label class="form-label" for="login-password">登录密码</label>
          <span class="field"
            ><LockKeyhole /><input
              id="login-password"
              v-model="password"
              name="password"
              class="input w-full pr-10"
              placeholder="请输入密码"
              :type="showPassword ? 'text' : 'password'"
              autocomplete="current-password"
              :aria-invalid="Boolean(passwordError || session.error)"
              :aria-describedby="passwordError || session.error ? 'login-password-error' : undefined"
              @input="clearPasswordError"
            /><button
              type="button"
              class="password-toggle"
              :aria-label="showPassword ? '隐藏密码' : '显示密码'"
              :aria-pressed="showPassword"
              @click="showPassword = !showPassword"
            >
              <EyeOff v-if="showPassword" class="h-4 w-4" />
              <Eye v-else class="h-4 w-4" /></button
          ></span>
          <p v-if="passwordError || session.error" id="login-password-error" class="field-error" role="alert" aria-live="polite">
            {{ passwordError || session.error }}
          </p>
        </div>
        <div class="captcha-head">
          <label class="form-label" for="login-captcha">图形验证码</label>
          <button type="button" class="captcha-refresh" aria-label="刷新图形验证码" @click="refreshCaptcha()">
            <RefreshCw />换一张
          </button>
        </div>
        <div class="captcha-row">
          <div class="field captcha-input-field"
            ><ShieldCheck /><input
              id="login-captcha"
              ref="captchaInputElement"
              v-model="captchaInput"
              name="captcha"
              class="input w-full"
              placeholder="请输入验证码"
              autocomplete="off"
              maxlength="4"
              inputmode="text"
              :aria-invalid="Boolean(captchaError)"
              :aria-describedby="captchaError ? 'login-captcha-error' : 'login-captcha-help'"
              @input="clearCaptchaError"
          /></div>
          <button type="button" class="captcha-visual" aria-label="刷新图形验证码" @click="refreshCaptcha()">
            <span
              v-for="(char, index) in captchaChars"
              :key="`${char}-${index}`"
              :style="{ transform: `rotate(${index % 2 ? 5 : -5}deg) translateY(${index % 2 ? 2 : -1}px)` }"
              >{{ char }}</span
            >
            <i v-for="line in 3" :key="line" :class="`captcha-line line-${line}`" />
          </button>
        </div>
        <p v-if="captchaError" id="login-captcha-error" class="field-error" role="alert" aria-live="polite">{{ captchaError }}</p>
        <p v-else id="login-captcha-help" class="captcha-help">看不清时，可点击验证码或“换一张”刷新</p>
        <button
          class="btn btn-primary login-submit"
          :disabled="session.loading"
          :aria-busy="session.loading"
        >
          <LoaderCircle v-if="session.loading" class="login-spinner" aria-hidden="true" />
          <template v-if="session.loading">正在登录…</template>
          <template v-else>进入系统<ArrowRight aria-hidden="true" /></template>
        </button>
      </form>
    </section>
  </main>
</template>
