import { defineConfig, loadEnv } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const apiTarget = env.VITE_DEV_API_TARGET || 'http://127.0.0.1:15123'

  return {
    plugins: [vue()],
    base: './',
    server: mode === 'development'
      ? {
          host: '127.0.0.1',
          proxy: {
            '/api': {
              target: apiTarget,
              changeOrigin: true,
              rewrite: (path: string) => path.replace(/^\/api/, ''),
            },
          },
        }
      : undefined,
  }
})
