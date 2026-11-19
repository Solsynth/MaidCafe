import { fileURLToPath, URL } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import vue from '@vitejs/plugin-vue'
import { defineConfig, loadEnv } from 'vite'

// In development the SPA runs on the Vite server and every backend path is
// proxied to the cloud, so the app uses same-origin URLs in both modes: in
// production the cloud serves web/dist itself, and the HttpOnly session
// cookie the hand-off sets on this domain stays first-party either way.
const backendPaths = ['/api', '/health', '/favicon.ico', '/favicon.png']

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const backend = env.VITE_API_TARGET || 'http://localhost:8080'

  return {
    plugins: [vue(), tailwindcss()],
    resolve: {
      alias: {
        '@': fileURLToPath(new URL('./src', import.meta.url)),
      },
    },
    server: {
      proxy: Object.fromEntries(backendPaths.map((path) => [path, backend])),
    },
  }
})
