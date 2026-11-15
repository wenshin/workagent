// Package secret 提供凭证的加密存取（技术方案 §5.5.2 的 MVP 形态）。
//
// 分层：SECRET_KEK（环境变量，不进仓库）包裹每条凭证独立的 DEK，DEK 再用
// AES-256-GCM 加密明文。密文落盘，明文只在本进程内存里短暂出现。
//
// 对外只暴露 sha256 指纹：配置校验、界面展示、审计日志都只用指纹，
// 明文只有平台内部（搜索 adapter、MCP 代理）能取，绝不进入模型上下文。
// 后续切到 Vault / KMS 只需替换 Store 实现，调用方不变。
package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotFound 凭证不存在
	ErrNotFound = errors.New("凭证不存在")
	// ErrDisabled 未配置 SECRET_KEK，凭证存储不可用
	ErrDisabled = errors.New("凭证存储未启用：未配置 SECRET_KEK")
	// ErrBadKEK KEK 太短，无法提供有效强度
	ErrBadKEK = errors.New("SECRET_KEK 太短，至少 16 个字符")
	// ErrNameInvalid scope / name 含非法字符
	ErrNameInvalid = errors.New("凭证名称只允许字母、数字、下划线与短横线，且不超过 64 字符")
)

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Item 是凭证的元信息，不含明文。
type Item struct {
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Store 是凭证存取接口。
//
// Get 只能由平台内部调用；结果不得进入模型上下文、日志或审计。
type Store interface {
	Enabled() bool
	Put(ctx context.Context, scope, name, value string) error
	Get(ctx context.Context, scope, name string) (string, error)
	Status(ctx context.Context, scope string) ([]Item, error)
	// SecretNames 只回答某个 scope 里有哪些名字，不返回也不解密任何值。
	// 「这个插件还差哪条凭证」是每次刷新列表都要问的问题，不能用 Get 去试。
	SecretNames(ctx context.Context, scope string) ([]string, error)
	Delete(ctx context.Context, scope, name string) error
}

type record struct {
	// DEK 是被 KEK 包裹的数据密钥；每条凭证一把，便于单独轮换与删除
	DEK         string    `json:"dek"`
	Value       string    `json:"value"`
	Fingerprint string    `json:"fingerprint"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type fileDoc struct {
	Items map[string]record `json:"items"`
}

// FileStore 把同一 scope 的凭证落成一个加密文件：{root}/{scope}.json。
type FileStore struct {
	root string
	kek  []byte
	now  func() time.Time
	mu   sync.Mutex
}

// NewFileStore 构造文件实现。kek 为空时返回 Enabled()==false 的实例，不影响服务启动。
func NewFileStore(root, kek string) (*FileStore, error) {
	s := &FileStore{root: root, now: time.Now}
	if root != "" {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, fmt.Errorf("创建凭证目录失败: %w", err)
		}
	}
	if strings.TrimSpace(kek) == "" {
		return s, nil
	}
	if len(kek) < 16 {
		return nil, ErrBadKEK
	}
	sum := sha256.Sum256([]byte(kek))
	s.kek = sum[:]
	return s, nil
}

// Enabled 表示是否可读写凭证。
func (s *FileStore) Enabled() bool { return len(s.kek) > 0 }

// Put 写入或覆盖凭证；value 为空等同于删除。
func (s *FileStore) Put(ctx context.Context, scope, name, value string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	if err := validateName(scope); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return s.Delete(ctx, scope, name)
	}

	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return err
	}
	wrapped, err := seal(s.kek, dek)
	if err != nil {
		return err
	}
	enc, err := seal(dek, []byte(value))
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load(scope)
	if err != nil {
		return err
	}
	doc.Items[name] = record{
		DEK:         wrapped,
		Value:       enc,
		Fingerprint: Fingerprint(value),
		UpdatedAt:   s.now().UTC(),
	}
	return s.save(scope, doc)
}

// Get 取出明文。调用方必须保证结果不落到模型可见的任何地方。
func (s *FileStore) Get(ctx context.Context, scope, name string) (string, error) {
	if !s.Enabled() {
		return "", ErrDisabled
	}
	if err := validateName(scope); err != nil {
		return "", err
	}
	if err := validateName(name); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load(scope)
	if err != nil {
		return "", err
	}
	rec, ok := doc.Items[name]
	if !ok {
		return "", ErrNotFound
	}
	dek, err := open(s.kek, rec.DEK)
	if err != nil {
		return "", fmt.Errorf("凭证密钥解包失败（SECRET_KEK 可能已更换）: %w", err)
	}
	plain, err := open(dek, rec.Value)
	if err != nil {
		return "", fmt.Errorf("凭证解密失败: %w", err)
	}
	return string(plain), nil
}

// Scopes 列出存储里已有凭证的 scope（即 {root}/{scope}.json 的文件名前缀）。
//
// 密钥配置页靠它枚举「所有存过东西的 scope」：引用关系是运行期状态（随插件
// 安装/卸载变化），按静态 refs 推导会让界面上只剩下 refs 里那一条。
func (s *FileStore) Scopes() ([]string, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		scope := strings.TrimSuffix(e.Name(), ".json")
		if validateName(scope) != nil {
			continue
		}
		out = append(out, scope)
	}
	sort.Strings(out)
	return out, nil
}

// Status 返回某 scope 下全部凭证的元信息，按名称排序。
func (s *FileStore) Status(ctx context.Context, scope string) ([]Item, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if err := validateName(scope); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load(scope)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(doc.Items))
	for name, rec := range doc.Items {
		out = append(out, Item{Name: name, Fingerprint: rec.Fingerprint, UpdatedAt: rec.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SecretNames 返回某 scope 下已配置的凭证名（不返回、不解密任何值）。
func (s *FileStore) SecretNames(ctx context.Context, scope string) ([]string, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if err := validateName(scope); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load(scope)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc.Items))
	for name := range doc.Items {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Delete 删除凭证；不存在时视为成功。
func (s *FileStore) Delete(ctx context.Context, scope, name string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	if err := validateName(scope); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load(scope)
	if err != nil {
		return err
	}
	if _, ok := doc.Items[name]; !ok {
		return nil
	}
	delete(doc.Items, name)
	return s.save(scope, doc)
}

func (s *FileStore) path(scope string) string {
	return filepath.Join(s.root, scope+".json")
}

func (s *FileStore) load(scope string) (fileDoc, error) {
	doc := fileDoc{Items: map[string]record{}}
	data, err := os.ReadFile(s.path(scope))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return doc, nil
		}
		return doc, err
	}
	if len(data) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("凭证文件损坏: %w", err)
	}
	if doc.Items == nil {
		doc.Items = map[string]record{}
	}
	return doc, nil
}

// save 先写临时文件再改名，避免并发写与进程崩溃留下半个文件。
func (s *FileStore) save(scope string, doc fileDoc) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	path := s.path(scope)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func validateName(v string) error {
	if !nameRe.MatchString(v) {
		return ErrNameInvalid
	}
	return nil
}

func aead(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal 返回 base64(nonce || ciphertext)。
func seal(key, plain []byte) (string, error) {
	g, err := aead(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := g.Seal(nil, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(append(nonce, out...)), nil
}

func open(key []byte, encoded string) ([]byte, error) {
	g, err := aead(key)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	n := g.NonceSize()
	if len(raw) <= n {
		return nil, errors.New("密文长度非法")
	}
	return g.Open(nil, raw[:n], raw[n:], nil)
}

// Fingerprint 返回明文凭证的稳定指纹（sha256 前 4 字节，8 位十六进制）。
// 可用于「是否已更换密钥」的比对与界面展示，无法反推明文。
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}

// Redact 从文本里抹掉指定凭证的原文，用于日志与错误信息的兜底。
// 短于 8 字符的值不处理，避免把正文里的普通词替换掉。
func Redact(text string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= 8 {
			text = strings.ReplaceAll(text, sec, "[redacted]")
		}
	}
	return text
}
