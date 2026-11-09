import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// The Go API listens on :8080; proxying keeps the browser on one origin (no CORS).
export default defineConfig({
  plugins: [vue()],
  server: { port: 5173, proxy: { '/api': 'http://localhost:8080', '/healthz': 'http://localhost:8080' } },
  test: {
    environment: 'jsdom',
    include: ['tests/**/*.test.js'],
    restoreMocks: true,
    unstubGlobals: true,
  },
})
