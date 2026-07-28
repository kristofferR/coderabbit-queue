import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Built assets land in ../internal/serve/dist so go:embed picks them up.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: { outDir: "../internal/serve/dist", emptyOutDir: true },
  server: { proxy: { "/api": "http://127.0.0.1:7777" } },
});
