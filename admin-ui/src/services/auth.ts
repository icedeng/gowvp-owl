import { http } from './http'

interface LoginKeyResponse { key: string }
interface LoginResponse { token: string; user: string }

function base64ToBytes(value: string): Uint8Array {
  const binary = window.atob(value)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i)
  return bytes
}

function pemToDer(pem: string): ArrayBuffer {
  const body = pem.replace(/-----BEGIN PUBLIC KEY-----/g, '').replace(/-----END PUBLIC KEY-----/g, '').replace(/\s/g, '')
  return Uint8Array.from(base64ToBytes(body)).buffer
}

function bytesToBase64(bytes: ArrayBuffer): string {
  const data = new Uint8Array(bytes)
  let binary = ''
  for (const byte of data) binary += String.fromCharCode(byte)
  return window.btoa(binary)
}

export async function login(username: string, password: string): Promise<LoginResponse> {
  if (!window.crypto?.subtle) throw new Error('当前浏览器不支持安全加密登录')
  const { data: keyResponse } = await http.get<LoginKeyResponse>('/login/key')
  const publicKey = await window.crypto.subtle.importKey(
    'spki',
    pemToDer(new TextDecoder().decode(base64ToBytes(keyResponse.key))),
    { name: 'RSA-OAEP', hash: 'SHA-256' },
    false,
    ['encrypt'],
  )
  const plaintext = new TextEncoder().encode(JSON.stringify({ username, password }))
  const ciphertext = await window.crypto.subtle.encrypt({ name: 'RSA-OAEP' }, publicKey, plaintext)
  const { data } = await http.post<LoginResponse>('/login', { data: bytesToBase64(ciphertext) })
  return data
}

export function clearSession() {
  localStorage.removeItem('owl-token')
  localStorage.removeItem('owl-user')
}

export function storeSession(response: LoginResponse) {
  localStorage.setItem('owl-token', response.token)
  localStorage.setItem('owl-user', response.user)
}

export function currentToken() { return localStorage.getItem('owl-token') }
export function currentUser() { return localStorage.getItem('owl-user') || '' }
