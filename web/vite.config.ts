import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// base './' keeps asset URLs relative so the build deploys under any
// GitHub Pages path without configuration.
export default defineConfig({
  plugins: [react()],
  base: './',
})
