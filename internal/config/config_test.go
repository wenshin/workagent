package config

import "testing"

// 形态决定默认执行面：服务端连执行体，客户端与开发机起本机进程。
func TestLoadRoleDecidesBackendDefault(t *testing.T) {
	cases := []struct {
		role    string
		backend string
	}{
		{RoleServer, "remote"},
		{RoleClient, "local"},
		{RoleDev, "local"},
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
func TestLoadRejectsUnknownRole(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost/db")
	t.Setenv("WA_ROLE", "cloud")
	if _, err := Load(); err == nil {
		t.Fatal("未知 WA_ROLE 应该报错")
	}
}

// 显式配置优先于形态默认值（dev 连执行体调试）。
func TestLoadExplicitBackendWins(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost/db")
	t.Setenv("WA_ROLE", RoleDev)
	t.Setenv("SANDBOX_BACKEND", "remote")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SandboxBackend != "remote" {
		t.Fatalf("SandboxBackend = %q，期望 remote", cfg.SandboxBackend)
	}
}
