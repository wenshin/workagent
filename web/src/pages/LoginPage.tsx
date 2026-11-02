import { Banner, Button } from '@douyinfe/semi-ui';
import { useSearchParams } from 'react-router-dom';

export default function LoginPage() {
  const [params] = useSearchParams();
  const error = params.get('error');

  const login = () => {
    window.location.href = '/auth/dingtalk/login';
  };

  return (
    <div className="login-wrap">
      <div className="login-card">
        <div className="brand-mark" />
        <h1 className="login-title">AutoAgent</h1>
        <p className="login-sub">企业级 Agent 工作台</p>

        {error ? <Banner type="danger" description={error} className="login-banner" /> : null}

        <Button theme="solid" type="primary" size="large" block onClick={login}>
          使用钉钉登录
        </Button>
        <p className="login-tip">登录后即可访问你的个人空间与共享空间</p>
      </div>
    </div>
  );
}
