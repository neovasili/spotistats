import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// VITE_API_TARGET selects the backend for local development (docs/SPECS.md 7.4):
//
//   Mode A — the deployed site:  VITE_API_TARGET=https://spotistats.neovasili.com npm run dev
//   Mode B — fully offline:      VITE_API_TARGET=http://127.0.0.1:8787 npm run dev
//
// Both /api and /data are proxied, because the dashboard reads its snapshot from the same origin
// rather than through the API.
//
// This is a PROXY rather than CORS on the API by design. The browser only ever talks to
// localhost; Vite makes the cross-origin hop server-side, where CORS does not apply. That keeps
// the system free of any CORS configuration at all, so production stays genuinely same-origin
// instead of same-origin-plus-a-development-exception.
const target = process.env.VITE_API_TARGET ?? 'http://127.0.0.1:8787'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target, changeOrigin: true },
      '/data': { target, changeOrigin: true },
    },
  },
  build: {
    // Hashed asset names, so the CloudFront /assets/* behaviour can cache them for a year.
    assetsDir: 'assets',
    sourcemap: true,
  },
  test: {
    environment: 'jsdom',
    globals: true,
    // Shims for the browser APIs jsdom leaves incomplete; see the file for what and why.
    setupFiles: ['./src/test-setup.ts'],
    // e2e/ holds the Playwright smoke suite. Its files match Vitest's default *.spec.ts
    // pattern, and Vitest loading them fails with "Playwright Test did not expect
    // test.describe() to be called here" -- two runners, two directories.
    exclude: ['node_modules/**', 'dist/**', 'e2e/**'],
  },
})
