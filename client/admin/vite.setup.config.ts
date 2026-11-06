import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";

// The first-run setup wizard (#187), served by the host agent from its own
// directory, so it is built separately from the /admin/ console.
export default defineConfig({
  root: fileURLToPath(new URL("./setup", import.meta.url)),
  base: "/",
  publicDir: false,
  plugins: [react()],
  resolve: {
    alias: { "/src": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  build: {
    outDir: fileURLToPath(new URL("./dist-setup", import.meta.url)),
    emptyOutDir: true,
  },
});
