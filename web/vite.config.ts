import babel from "@rolldown/plugin-babel";
import tailwindcss from "@tailwindcss/vite";
import react, { reactCompilerPreset } from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// Built assets land in ../internal/serve/dist so go:embed picks them up.
export default defineConfig({
  plugins: [react(), babel({ presets: [reactCompilerPreset()] }), tailwindcss()],
  build: { outDir: "../internal/serve/dist", emptyOutDir: true },
  server: { proxy: { "/api": "http://127.0.0.1:7777" } },
});
