import type { Config } from 'tailwindcss'

export default {
  content: ['./index.html', './src/**/*.{vue,ts}'],
  theme: {
    extend: {
      colors: {
        ink: '#e9edf0',
        signal: '#80f0d0',
        amber: '#ffb65c',
      },
    },
  },
  plugins: [],
} satisfies Config
