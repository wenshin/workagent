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
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </BrowserRouter>
      </QueryClientProvider>
    </ConfigProvider>
  );
}
