package plugin

import (
	"path/filepath"
	"testing"
)

// contains 用真实路径比较：macOS 上 /var 会被解析成 /private/var。
func contains(paths []string, want string) bool {
	want = realPath(want)
	for _, p := range paths {
		if realPath(p) == want {
			return true
		}
	}
	return false
}

// 不声明 fs：整个空间工作区可读，但**不可写**，插件目录可读。
func TestBuildSandboxFSDefault(t *testing.T) {
	space := t.TempDir()
	dir := t.TempDir()

	got, err := buildSandboxFS(space, dir, Manifest{})
	if err != nil {
		t.Fatalf("默认不应报错: %v", err)
	}
	if !contains(got.Read, space) || !contains(got.Read, dir) {
		t.Fatalf("默认读范围应含空间与插件目录: %#v", got.Read)
	}
	if len(got.Write) != 0 {
		t.Fatalf("没有声明 fs.write 就不该有任何可写路径: %#v", got.Write)
	}
}

// 声明 fs.read 之后必须真的收紧：空间根目录不能再整块挂进来。
// 这条曾经是 bug——先无条件加了 spaceRoot，read 声明等于没写。
func TestBuildSandboxFSNarrowsRead(t *testing.T) {
	space := t.TempDir()
	dir := t.TempDir()

	got, err := buildSandboxFS(space, dir, Manifest{
		FS: &FSSpec{Read: []string{"data/"}},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if contains(got.Read, space) {
		t.Fatalf("声明了 fs.read 就不该再挂整个空间: %#v", got.Read)
	}
	if !contains(got.Read, filepath.Join(space, "data")) {
		t.Fatalf("声明的路径没进白名单: %#v", got.Read)
	}
	// 插件自己的入口脚本必须始终可读，否则连启动都做不到
	if !contains(got.Read, dir) {
		t.Fatalf("插件目录必须可读: %#v", got.Read)
	}
	// 只声明 read 不影响写权限：没写 fs.write 仍然不可写
	if len(got.Write) != 0 {
		t.Fatalf("未声明 fs.write 时不该有写权限: %#v", got.Write)
	}
}

func TestBuildSandboxFSNarrowsWrite(t *testing.T) {
	space := t.TempDir()
	dir := t.TempDir()

	got, err := buildSandboxFS(space, dir, Manifest{
		FS: &FSSpec{Write: []string{"output/"}},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if contains(got.Write, space) {
		t.Fatalf("声明了 fs.write 就不该给整个空间写权限: %#v", got.Write)
	}
	if !contains(got.Write, filepath.Join(space, "output")) {
		t.Fatalf("声明的写路径没进白名单: %#v", got.Write)
	}
}

// read_only 是整插件只读：连自己的工作区都不给写。
func TestBuildSandboxFSReadOnly(t *testing.T) {
	space := t.TempDir()
	dir := t.TempDir()

	got, err := buildSandboxFS(space, dir, Manifest{
		FS: &FSSpec{ReadOnly: true, Write: []string{"output/"}},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if len(got.Write) != 0 {
		t.Fatalf("只读插件不该有任何可写路径: %#v", got.Write)
	}
}

// 声明整个空间（`.`）要报错：那等于绕开这次收敛。
func TestValidateFSRejectsWholeSpace(t *testing.T) {
	for _, bad := range []string{".", "./", "", " ", "/"} {
		if err := validateFS(&FSSpec{Read: []string{bad}}); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	if err := validateFS(&FSSpec{Read: []string{"data/"}, Write: []string{"output/"}}); err != nil {
		t.Errorf("子目录声明不该报错: %v", err)
	}
}

// 代理白名单只看 http.request 的 hosts：没声明就没有出网。
func TestNetHosts(t *testing.T) {
	m := Manifest{Capabilities: []Capability{
		{Method: MethodHTTP, Hosts: []string{"API.Example.com", "api.example.com", "cdn.example.com"}},
		{Method: MethodLog},
	}}
	got := m.NetHosts()
	if len(got) != 2 || got[0] != "api.example.com" || got[1] != "cdn.example.com" {
		t.Fatalf("hosts 应小写去重: %#v", got)
	}
	if len((Manifest{}).NetHosts()) != 0 {
		t.Fatal("没声明 http.request 不该有任何出网域名")
	}
}
