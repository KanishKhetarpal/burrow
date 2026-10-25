import { defineConfig } from 'vite';
import preact from '@preact/preset-vite';

// The agent serves this build from its own origin under a strict CSP: no
// inline script, no inline style, no external origins. Everything therefore
// has to be bundled into files, which is why cssCodeSplit stays on and no
// plugin may inline assets as data: URIs beyond images.
// Declared locally rather than pulling in @types/node for one variable: this
// config is the only place in the project that reads the build environment.
declare const process: { env: Record<string, string | undefined> };

// Stamped into the service worker's cache name. scripts/build.ps1 passes the
// real version through the environment; a plain `npm run build` gets "dev".
const version = process.env.BURROW_VERSION ?? 'dev';

export default defineConfig({
  plugins: [preact()],
  define: {
    __BURROW_VERSION__: JSON.stringify(version),
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // Vite inlines small assets as data: URIs by default. img-src allows
    // data:, but style-src does not, so the limit is set to 0 to keep CSS
    // free of inlined url() data.
    assetsInlineLimit: 0,
    target: 'es2022',
    sourcemap: false,
    rollupOptions: {
      input: {
        index: 'index.html',
        // The service worker is built as its own entry so it lands at a
        // stable /sw.js rather than a hashed name a registration cannot find.
        sw: 'src/sw.ts',
      },
      output: {
        entryFileNames: (chunk) => (chunk.name === 'sw' ? 'sw.js' : 'assets/[name]-[hash].js'),
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
      },
    },
  },
  server: {
    port: 5173,
  },
});
