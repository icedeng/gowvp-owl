import { http } from './http'

interface LoginKeyResponse { key: string; oaep_seed?: string }
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

function bytesToBinary(bytes: Uint8Array): string {
  let binary = ''
  for (const byte of bytes) binary += String.fromCharCode(byte)
  return binary
}

async function encryptCredentials(publicKeyPem: string, plaintext: string, oaepSeed?: string): Promise<string> {
  if (window.crypto?.subtle) {
    const publicKey = await window.crypto.subtle.importKey(
      'spki',
      pemToDer(publicKeyPem),
      { name: 'RSA-OAEP', hash: 'SHA-256' },
      false,
      ['encrypt'],
    )
    const ciphertext = await window.crypto.subtle.encrypt(
      { name: 'RSA-OAEP' },
      publicKey,
      new TextEncoder().encode(plaintext),
    )
    return bytesToBase64(ciphertext)
  }

  // 非安全上下文中的 HTTP 页面无法使用 Web Crypto，随机种子由后端 crypto/rand 随公钥响应提供。
  if (!oaepSeed) throw new Error('服务器未提供 HTTP 加密登录参数')
  const { default: forge } = await import('node-forge')
  const publicKey = forge.pki.publicKeyFromPem(publicKeyPem)
  const ciphertext = publicKey.encrypt(forge.util.encodeUtf8(plaintext), 'RSA-OAEP', {
    md: forge.md.sha256.create(),
    mgf1: { md: forge.md.sha256.create() },
    seed: bytesToBinary(base64ToBytes(oaepSeed)),
  })
  return forge.util.encode64(ciphertext)
}

export async function login(username: string, password: string): Promise<LoginResponse> {
  const { data: keyResponse } = await http.get<LoginKeyResponse>('/login/key')
  const publicKeyPem = new TextDecoder().decode(base64ToBytes(keyResponse.key))
  const ciphertext = await encryptCredentials(publicKeyPem, JSON.stringify({ username, password }), keyResponse.oaep_seed)
  const { data } = await http.post<LoginResponse>('/login', { data: ciphertext })
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
