import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";

// Tauri expects a fixed dev-server port it can point the webview at. The Rust
// shell reads TAURI_DEV_HOST when running on a device; localhost otherwise.
const host = process.env.TAURI_DEV_HOST;

// https://vitejs.dev/config/
export default defineConfig(({ mode }) => {
  // Load PIE_RELAY_URL without requiring a VITE_ duplicate. The browser bundle
  // receives only this explicit value; no other process environment leaks in.
  const env = loadEnv(mode, process.cwd(), "PIE_RELAY_URL");
  return {
    define: {
      __PIE_RELAY_URL__: JSON.stringify(env.PIE_RELAY_URL || ""),
    },
    plugins: [react()],
    build: {
      // Keep the shell/control-plane bundle small. Terminal and Markdown code is
      // cacheable independently and changes much less often than application UI.
      rollupOptions: {
        output: {
          manualChunks(id) {
            const moduleId = id.replaceAll("\\", "/");
            if (
              moduleId.includes("/node_modules/react-markdown/") ||
              moduleId.includes("/node_modules/remark-gfm/")
            )
              return "markdown";
            if (
              moduleId.includes("/node_modules/@xterm/xterm/") ||
              moduleId.includes("/node_modules/@xterm/addon-fit/")
            )
              return "terminal";
            if (moduleId.includes("/node_modules/qrcode/")) return "qrcode";
            if (
              moduleId.includes("/node_modules/react/") ||
              moduleId.includes("/node_modules/react-dom/")
            )
              return "react";
          },
        },
      },
    },
    // Prevent Vite from obscuring Rust errors and keep a stable port for Tauri.
    clearScreen: false,
    server: {
      port: 1420,
      strictPort: true,
      host: host || false,
      hmr: host ? { protocol: "ws", host, port: 1421 } : undefined,
      watch: {
        // Don't watch the Rust side; cargo handles that.
        ignored: ["**/src-tauri/**"],
      },
    },
  };
});
