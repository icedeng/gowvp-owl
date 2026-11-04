import assert from 'node:assert/strict'
import { copyFileSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'
import { loadEnv } from 'vite'

test('本地开发代理不会进入生产构建，显式生产地址仍可覆盖默认值', () => {
  const directory = mkdtempSync(join(tmpdir(), 'owl-build-env-'))
  const previous = process.env.VITE_API_BASE_URL
  try {
    delete process.env.VITE_API_BASE_URL
    writeFileSync(join(directory, '.env.local'), 'VITE_API_BASE_URL=/api\n')
    copyFileSync(new URL('../.env.production', import.meta.url), join(directory, '.env.production'))

    assert.equal(loadEnv('development', directory).VITE_API_BASE_URL, '/api')
    assert.equal(loadEnv('production', directory).VITE_API_BASE_URL, '/')

    process.env.VITE_API_BASE_URL = 'https://gateway.example.test'
    assert.equal(loadEnv('production', directory).VITE_API_BASE_URL, 'https://gateway.example.test')
  } finally {
    if (previous === undefined) delete process.env.VITE_API_BASE_URL
    else process.env.VITE_API_BASE_URL = previous
    rmSync(directory, { recursive: true, force: true })
  }
})
