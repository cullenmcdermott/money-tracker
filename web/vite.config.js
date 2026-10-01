import { defineConfig } from 'vite'

export default defineConfig({
  server: { port: 5188, strictPort: true, proxy: { '/api': { target: 'http://127.0.0.1:8188', changeOrigin: false } } },
  build: { outDir: 'dist' },
})
