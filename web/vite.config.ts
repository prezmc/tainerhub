import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// In development, run the hub on :8585 and `npm run dev` here; API calls are proxied.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: { '/api': 'http://localhost:8585' },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // Never inline assets as data: URLs, so the strict CSP (font-src 'self') holds.
    assetsInlineLimit: 0,
  },
})
