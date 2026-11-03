import { Banner, Button } from '@douyinfe/semi-ui';
import { useSearchParams } from 'react-router-dom';

import { useProviderInfo } from '../api/hooks';

export default function LoginPage() {
  const [params] = useSearchParams();
  const error = params.get('error');
  const { data: provider, isLoading } = useProviderInfo();

  const login = () => {
    // 端点与 IdP 无关：后端按 IDENTITY_PROVIDER 决定跳转哪个授权页
    window.location.href = '/auth/login';
  };

  const displayName = provider?.display_name ?? '';
  const missing = provider?.missing ?? [];
  const notReady = provider ? !provider.ready : false;

  return (
    <div className="login-wrap">
      <div className="login-card">
        <div className="brand-mark" />
        <h1 className="login-title">WorkAgent</h1>
        <p className="login-sub">企业级 Agent 工作台</p>

        {error ? <Banner type="danger" description={error} className="login-banner" /> : null}
        {notReady ? (
          <Banner
            type="warning"
            className="login-banner"
            description={
              missing.length > 0
                ? `登录未配置，缺失：${missing.join(' / ')}`
                : `身份接入方「${displayName || provider?.provider}」尚未实现`
            }
          />
        ) : null}

        <Button
          theme="solid"
          type="primary"
          size="large"
          block
          loading={isLoading}
          disabled={notReady}
          onClick={login}
        >
          {displayName ? `使用${displayName}登录` : '登录'}
        </Button>
        <p className="login-tip">登录后即可访问你的个人空间与共享空间</p>
      </div>
    </div>
  );
}
