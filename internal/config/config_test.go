package config

import "testing"

// 形态决定默认执行面：服务端建容器，客户端起本机进程。
func TestLoadRoleDecidesBackendDefault(t *testing.T) {
	cases := []struct {
		role    string
		backend string
	}{
		{RoleServer, "k8s"},
		{RoleClient, "local"},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://u:p@localhost/db")
			t.Setenv("WA_ROLE", tc.role)
			t.Setenv("SANDBOX_BACKEND", "")
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Role != tc.role {
				t.Fatalf("Role = %q，期望 %q", cfg.Role, tc.role)
			}
			if cfg.SandboxBackend != tc.backend {
				t.Fatalf("SandboxBackend = %q，期望 %q", cfg.SandboxBackend, tc.backend)
			}
		})
	}
}

// 形态写错要在启动时拒绝，而不是回落到某个默认值——回落会让服务端
// 在一台没有执行体的机器上以「本机沙箱」形态静默跑起来。
// dev 已删除：本机调试同样是 server，不再有「自动探测 + 可降级」的档位。
func TestLoadRejectsUnknownRole(t *testing.T) {
	for _, role := range []string{"cloud", "dev"} {
		t.Run(role, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://u:p@localhost/db")
			t.Setenv("WA_ROLE", role)
			if _, err := Load(); err == nil {
				t.Fatalf("WA_ROLE=%s 应该报错", role)
			}
		})
	}
}

// 上传上限默认 100MB：space 包的 DefaultUploadMaxBytes 也是这个值，两处不能漂移。
func TestLoadUploadMaxFileMB(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost/db")
	t.Setenv("UPLOAD_MAX_FILE_MB", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UploadMaxFileMB != 100 {
		t.Fatalf("UploadMaxFileMB = %d，期望默认 100", cfg.UploadMaxFileMB)
	}

	t.Setenv("UPLOAD_MAX_FILE_MB", "-1")
	if _, err := Load(); err == nil {
		t.Fatal("负的 UPLOAD_MAX_FILE_MB 应该在启动时报错")
	}
}

// 显式配置优先于形态默认值；与形态不匹配的组合由 newShellRunner 在启动阶段拒绝，
// Load 不做静默改写（否则「写了 local 却跑成 remote」更难查）。
func TestLoadExplicitBackendWins(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost/db")
	t.Setenv("WA_ROLE", RoleServer)
	t.Setenv("SANDBOX_BACKEND", "local")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SandboxBackend != "local" {
		t.Fatalf("SandboxBackend = %q，期望 local（Load 不应改写显式值）", cfg.SandboxBackend)
	}
}
