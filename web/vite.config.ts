import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  // Relative URLs, resolved against the <base href> the server sets, so the
  // dashboard and its icons work under a URL prefix (KUBERIGHT_BASE_PATH).
  base: './',
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
    include: ['src/**/*.test.{ts,tsx}', 'scripts/**/*.test.mjs'],
    coverage: { reporter: ['text', 'lcov'] },
  },
})
