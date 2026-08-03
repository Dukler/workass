import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Relative base so the daemon can serve dist/ from any mount point.
// index.html keeps a literal </head> so the daemon injects /lan-bridge.js before it.
export default defineConfig({
  base: './',
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2022',
    // Single chunk keeps the injected-bridge model simple and paints fast.
    modulePreload: { polyfill: false },
  },
  server: { port: 5199 },
});
