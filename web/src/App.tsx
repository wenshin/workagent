import { ConfigProvider, Spin } from '@douyinfe/semi-ui';
import zh_CN from '@douyinfe/semi-ui/lib/es/locale/source/zh_CN';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactElement } from 'react';
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';

import { useMe } from './api/hooks';
import LoginPage from './pages/LoginPage';
import WorkspacePage from './pages/WorkspacePage';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      refetchOnWindowFocus: false,
      staleTime: 10_000,
    },
  },
});

function RequireAuth({ children }: { children: ReactElement }) {
  const { data, isLoading, isError } = useMe();

  if (isLoading) {
    return (
      <div className="boot-screen">
        <Spin size="large" />
      </div>
    );
  }
  if (isError || !data) {
    return <Navigate to="/login" replace />;
  }
  return children;
}

export default function App() {
  return (
    <ConfigProvider locale={zh_CN}>
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route
              path="/"
              element={
                <RequireAuth>
                  <WorkspacePage />
                </RequireAuth>
              }
            />
            {/* 工作空间与插件同级；插件下分「插件市场 / 已装插件」。
                发布记录是工作空间的次级页，保留旧路径兼容。 */}
            <Route path="/plugins" element={<Navigate to="/plugins/market" replace />} />
            <Route
              path="/plugins/market"
              element={
                <RequireAuth>
                  <WorkspacePage />
                </RequireAuth>
              }
            />
            {/* 市场分类走二级路由：左侧菜单与内容共用同一份路径状态 */}
            <Route
              path="/plugins/market/:category"
              element={
                <RequireAuth>
                  <WorkspacePage />
                </RequireAuth>
              }
            />
            <Route path="/plugins/installed" element={<Navigate to="/plugins/market" replace />} />
            <Route path="/resources" element={<Navigate to="/plugins/market" replace />} />
            <Route
              path="/resources/snapshots"
              element={
                <RequireAuth>
                  <WorkspacePage />
                </RequireAuth>
              }
            />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </BrowserRouter>
      </QueryClientProvider>
    </ConfigProvider>
  );
}
