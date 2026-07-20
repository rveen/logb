import { defineConfig } from "vite";

export default defineConfig({
  // Relative asset URLs: the bundle is served from the Go binary's embedded
  // filesystem at whatever root the user's browser lands on.
  base: "./",
  esbuild: { jsx: "automatic", jsxImportSource: "preact" },
  resolve: {
    alias: { react: "preact/compat", "react-dom": "preact/compat" },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // The Go binary embeds dist/, so a single unhashed pair of asset names
    // keeps the committed tree stable across rebuilds and keeps the diff
    // readable when the bundle changes.
    rollupOptions: {
      output: {
        entryFileNames: "assets/app.js",
        chunkFileNames: "assets/[name].js",
        assetFileNames: "assets/[name][extname]",
      },
    },
  },
});
