package plugin

import (
	"path"
	"path/filepath"
	"testing"

	"github.com/workagent/auto-agent/internal/sandbox"
)

// realPath 用真实路径比较：macOS 上 /var 会被解析成 /private/var。
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// mounted 返回挂到指定沙箱路径的挂载（没有则 false）。
func mounted(mounts []sandbox.Mount, target string) (sandbox.Mount, bool) {
	for _, m := range mounts {
		if m.Target == target {
			return m, true
		}
	}
	return sandbox.Mount{}, false
}

func hasTarget(mounts []sandbox.Mount, target string) bool {
	_, ok := mounted(mounts, target)
	return ok
}

// 不声明 fs：空间工作区不挂（读业务数据走工具调用），插件目录可读写。
// 插件目录为什么可写：运行时缓存（临时文件、下载中转）需要一个属于自己的位置，
// 而 DENO_DIR 只放 Deno 自己的缓存，插件业务缓存放不进去。
func TestBuildSandboxMountsDefault(t *testing.T) {
	space := t.TempDir()
	dir := t.TempDir()

	got, spaceMounted, err := buildSandboxMounts("spaces/s1", space, "spaces/s1/plugins/demo", dir, Manifest{})
	if err != nil {
		t.Fatalf("默认不应报错: %v", err)
	}
	if spaceMounted {
		t.Fatal("默认不该挂空间工作区")
	}
	plugin, ok := mounted(got, sandbox.PluginDir)
	if !ok || realPath(plugin.Source) != realPath(dir) {
		t.Fatalf("插件目录必须挂在 %s: %#v", sandbox.PluginDir, got)
	}
	if !plugin.Write {
		t.Fatal("插件目录应可写（运行时缓存）")
	}
	if hasTarget(got, sandbox.SpaceDir) {
		t.Fatalf("插件目录可写不等于空间可写: %#v", got)
	}
}

// 声明 fs.read 之后必须真的收紧：空间根目录不能再整块挂进来，
// 而且挂的必须是沙箱内的 /space/<rel>，不是宿主路径。
func TestBuildSandboxMountsNarrowsRead(t *testing.T) {
	space := t.TempDir()
	dir := t.TempDir()

	got, spaceMounted, err := buildSandboxMounts("spaces/s1", space, "spaces/s1/plugins/demo", dir, Manifest{
		FS: &FSSpec{Read: []string{"data/"}},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if !spaceMounted {
		t.Fatal("声明了 fs.read 就该有空间挂载")
	}
	if hasTarget(got, sandbox.SpaceDir) {
		t.Fatalf("声明了 fs.read 就不该再挂整个空间: %#v", got)
	}
	m, ok := mounted(got, path.Join(sandbox.SpaceDir, "data"))
	if !ok {
		t.Fatalf("声明的路径没进挂载表: %#v", got)
	}
	if m.Write {
		t.Fatal("只声明 read 时不该可写")
	}
	if m.Key != "spaces/s1/files/data" {
		t.Fatalf("过线的应该是存储 key，实际 %q", m.Key)
	}
}

func TestBuildSandboxMountsNarrowsWrite(t *testing.T) {
	space := t.TempDir()
	dir := t.TempDir()

	got, _, err := buildSandboxMounts("spaces/s1", space, "spaces/s1/plugins/demo", dir, Manifest{
		FS: &FSSpec{Write: []string{"output/"}},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	m, ok := mounted(got, path.Join(sandbox.SpaceDir, "output"))
	if !ok || !m.Write {
		t.Fatalf("声明的写路径必须是可写挂载: %#v", got)
	}
	if hasTarget(got, sandbox.SpaceDir) {
		t.Fatalf("声明了 fs.write 就不该给整个空间写权限: %#v", got)
	}
}

// read_only 是整插件只读：连自己的工作区都不给写。
func TestBuildSandboxMountsReadOnly(t *testing.T) {
	space := t.TempDir()
	dir := t.TempDir()

	got, _, err := buildSandboxMounts("spaces/s1", space, "spaces/s1/plugins/demo", dir, Manifest{
		FS: &FSSpec{ReadOnly: true, Write: []string{"output/"}},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	for _, m := range got {
		if m.Write {
			t.Fatalf("只读插件不该有任何可写路径: %#v", got)
		}
	}
}

// 声明整个空间（`.`）要报错：那等于绕开这次收敛。
func TestValidateFSRejectsWholeSpace(t *testing.T) {
	for _, bad := range []string{".", "./", "", " ", "/", "../x"} {
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
