import { writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Vite empties dist before every build, which would take the placeholder with
// it. The placeholder is what keeps the directory in git, so that the Go embed
// of dist still compiles in a clone where the UI has never been built. Put it
// back once the build is done.
const keepPlaceholder = {
  name: 'sombrero-keep-placeholder',
  closeBundle() {
    writeFileSync(fileURLToPath(new URL('./dist/.gitkeep', import.meta.url)), '')
  },
}

// During development, requests to /api are proxied to the Sombrero server
// (which does not send CORS headers). The path is passed through as it is:
// the server serves the API under /api and strips the prefix itself, so
// development and the embedded build see the same URLs. Set SOMBRERO_API_URL
// to point the proxy at a non-default address.
export default defineConfig({
  plugins: [react(), keepPlaceholder],
  server: {
    proxy: {
      '/api': {
        target: process.env.SOMBRERO_API_URL || 'http://localhost:9999',
        changeOrigin: true,
      },
    },
  },
})
