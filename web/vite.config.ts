import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// During development, requests to /api are proxied to the Sombrero API
// server (which does not send CORS headers). Set SOMBRERO_API_URL to
// point the proxy at a non-default address.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': {
        target: process.env.SOMBRERO_API_URL || 'http://localhost:9999',
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api/, ''),
      },
    },
  },
})
