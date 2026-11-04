package secret

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T, kek string) *FileStore {
	t.Helper()
	s, err := NewFileStore(filepath.Join(t.TempDir(), "secrets"), kek)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, "test-kek-at-least-16-chars")
	if !s.Enabled() {
		t.Fatal("配置了 KEK 应处于启用状态")
	}

	if err := s.Put(ctx, "search", "bocha", "sk-secret-value-123456"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, "search", "bocha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-secret-value-123456" {
		t.Fatalf("Get = %q", got)
	}
}

func TestCiphertextOnDiskDoesNotContainPlaintext(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "secrets")
	s, err := NewFileStore(dir, "test-kek-at-least-16-chars")
	if err != nil {
		t.Fatal(err)
	}
	const plaintext = "sk-very-secret-a1b2c3"
	if err := s.Put(ctx, "search", "bocha", plaintext); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "search.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), plaintext) {
		t.Fatal("落盘文件里出现了明文凭证")
	}
	if strings.Contains(string(raw), "sk-") {
		t.Fatal("落盘文件里出现了明文前缀")
	}
}

func TestWrongKEKCannotDecrypt(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "secrets")

	first, err := NewFileStore(dir, "kek-number-one-16chars")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Put(ctx, "search", "bocha", "sk-value-000000"); err != nil {
		t.Fatal(err)
	}

	second, err := NewFileStore(dir, "kek-number-two-16chars")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Get(ctx, "search", "bocha"); err == nil {
		t.Fatal("用另一把 KEK 不应能解出明文")
	}
}

func TestDisabledStore(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, "")
	if s.Enabled() {
		t.Fatal("未配置 KEK 时应处于未启用状态")
	}
	if err := s.Put(ctx, "search", "bocha", "x"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Put err = %v, want ErrDisabled", err)
	}
	if _, err := s.Get(ctx, "search", "bocha"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Get err = %v, want ErrDisabled", err)
	}
}

func TestShortKEKRejected(t *testing.T) {
	if _, err := NewFileStore(t.TempDir(), "short"); !errors.Is(err, ErrBadKEK) {
		t.Fatalf("err = %v, want ErrBadKEK", err)
	}
}

func TestInvalidNames(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, "test-kek-at-least-16-chars")
	for _, name := range []string{"../escape", "a/b", ".hidden", ""} {
		if err := s.Put(ctx, "search", name, "v"); !errors.Is(err, ErrNameInvalid) {
			t.Errorf("Put(name=%q) err = %v, want ErrNameInvalid", name, err)
		}
	}
}

func TestDeleteAndEmptyValue(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, "test-kek-at-least-16-chars")

	if err := s.Put(ctx, "search", "bocha", "sk-value-000000"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "search", "bocha"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "search", "bocha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 Get err = %v, want ErrNotFound", err)
	}

	// 空值等同删除
	if err := s.Put(ctx, "search", "other", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "search", "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空值写入后 Get err = %v, want ErrNotFound", err)
	}
}

func TestStatusReturnsFingerprintOnly(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, "test-kek-at-least-16-chars")
	if err := s.Put(ctx, "search", "bocha", "sk-value-abcdef"); err != nil {
		t.Fatal(err)
	}

	items, err := s.Status(ctx, "search")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if len(items[0].Fingerprint) != 8 {
		t.Fatalf("指纹长度 = %d, want 8", len(items[0].Fingerprint))
	}
	if items[0].Fingerprint != Fingerprint("sk-value-abcdef") {
		t.Fatal("指纹应稳定")
	}
}

func TestFingerprintStableAndDiffers(t *testing.T) {
	if Fingerprint("a") != Fingerprint("a") {
		t.Fatal("同一明文指纹应稳定")
	}
	if Fingerprint("a") == Fingerprint("b") {
		t.Fatal("不同明文指纹不应相同")
	}
}

func TestRedact(t *testing.T) {
	out := Redact("request failed with key sk-abcdef123456", "sk-abcdef123456")
	if strings.Contains(out, "sk-abcdef123456") {
		t.Fatalf("Redact 未抹掉密钥: %q", out)
	}
	// 过短的串不处理，避免误伤正文
	if got := Redact("use abc in text", "abc"); got != "use abc in text" {
		t.Fatalf("短串不应被替换: %q", got)
	}
}
