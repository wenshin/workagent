import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Semi 的 package.json exports 未暴露 dist 下的样式，这里用别名绕开深层导入限制
const semiCss = new URL('./node_modules/@douyinfe/semi-ui/dist/css/semi.min.css', import.meta.url).pathname;

// 前端独立构建、部署到 CDN；对浏览器始终同源访问 API
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
    // 默认 host=localhost 在 macOS 上会解析到 ::1，只监听 IPv6，导致 127.0.0.1 无法访问
    host: '0.0.0.0',
    port: 5173,
    strictPort: true,
    // 开发态代理，规则与生产 Nginx 反代保持一致
    proxy: {
      '/auth': 'http://127.0.0.1:8000',
      '/spaces': 'http://127.0.0.1:8000',
      '/healthz': 'http://127.0.0.1:8000',
    },
  },
});
