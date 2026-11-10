import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

export default defineConfig({
  plugins: [react()],
  server: {
    // Go listens on IPv4 loopback only, hence 127.0.0.1. changeOrigin stays
    // false so Host remains localhost:5173 for Go's CrossOriginProtection.
    proxy: { '/api': { target: 'http://127.0.0.1:8080', changeOrigin: false } },
  },
  build: {
    // web/embed.go embeds dist; the npm build script recreates dist/.gitkeep.
    outDir: 'dist',
    // Single unsplit bundle (~710 kB, mostly Recharts) on purpose: one local page.
    chunkSizeWarningLimit: 800,
  },
})
