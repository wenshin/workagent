import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Semi 的 package.json exports 未暴露 dist 下的样式，这里用别名绕开深层导入限制
const semiCss = new URL('./node_modules/@douyinfe/semi-ui/dist/css/semi.min.css', import.meta.url).pathname;

// 前端独立部署（CDN），构建产物挂根路径；API 地址由 VITE_API_BASE 注入
export default defineConfig({
  base: '/',
  plugins: [react()],
  resolve: {
    alias: {
      '@semi/css': semiCss,
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
    chunkSizeWarningLimit: 1500,
  },
  server: {
    port: 5173,
    proxy: {
      '/auth': 'http://127.0.0.1:8000',
      '/spaces': 'http://127.0.0.1:8000',
      '/healthz': 'http://127.0.0.1:8000',
    },
  },
});
