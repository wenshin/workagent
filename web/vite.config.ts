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
    // 开发态代理，规则与生产 Nginx 反代保持一致。
    // 必须覆盖后端全部路由前缀：漏掉的前缀会被 Vite 当成 SPA 路由返回 index.html，
    // 前端 api() 收到 HTML 会抛 bad_response，表现为「接口没数据」而不是明确的 404。
    // 新增后端前缀（如 /admin）时要同步这里与生产反代配置。
    proxy: Object.fromEntries(
      [
        '/auth',
        '/spaces',
        '/sessions',
        // 续流入口：刷新/切页回来靠它接上正在跑的那一轮。漏掉就会落到 SPA 路由，
        // 拿到 index.html —— 表现是「切页回来会话没了」，且不报任何错
        '/runs',
        '/resources',
        '/admin',
        '/healthz',
        '/agent',
        '/plugins',
      ].map((prefix) => [prefix, 'http://127.0.0.1:8321']),
    ),
  },
});
