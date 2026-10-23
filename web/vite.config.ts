import { defineConfig } from 'vitest/config';
import { svelte } from '@sveltejs/vite-plugin-svelte';

export default defineConfig({
  plugins: [svelte()],
  test: { css: { include: [/app\.css/] } }, // highlight.test.ts reads the colour tokens
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    assetsInlineLimit: 0,
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: { '/api': 'http://127.0.0.1:4444', '/healthz': 'http://127.0.0.1:4444' },
  },
});
