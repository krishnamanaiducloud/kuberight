import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://localhost:8080',
      '/healthz': 'http://localhost:8080',
      '/readyz': 'http://localhost:8080',
    },
  },
  build: {
    sourcemap: true,
    reportCompressedSize: true,
  },
  test: {
    environment: 'jsdom',
    globals: true,
    pool: 'threads',
    fileParallelism: false,
    setupFiles: './src/test/setup.ts',
    include: ['src/**/*.test.{ts,tsx}'],
    coverage: { reporter: ['text', 'lcov'] },
  },
})
