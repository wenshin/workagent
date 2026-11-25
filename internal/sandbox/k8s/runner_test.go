package k8s

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/workagent/auto-agent/internal/sandbox"
)

// 本机（不在集群内）必须能退回本机 kubeconfig：否则 make run 只会报
// 「KUBERNETES_SERVICE_HOST 未定义」，看不出该配什么。
func TestRestConfigFallsBackToLocalKubeconfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: ctx
  context: {cluster: c, user: u}
current-context: ctx
users:
- name: u
  user: {token: test}
`
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	rc, err := restConfig(Config{})
	if err != nil {
		t.Fatalf("restConfig: %v", err)
	}
	if rc.Host != "https://127.0.0.1:6443" {
		t.Fatalf("Host = %q", rc.Host)
	}
}

// 一条凭据都没有时，报错必须点明配什么，而不是只抛 in-cluster 的原始错误。
func TestRestConfigFailsWithHintWhenNoCredentials(t *testing.T) {
	// 指向一个不存在的文件：client-go 不会再去看真实的 ~/.kube/config。
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	_, err := restConfig(Config{})
	if err == nil || !strings.Contains(err.Error(), "SANDBOX_KUBECONFIG") {
		t.Fatalf("应提示 SANDBOX_KUBECONFIG，实际 %v", err)
	}
}

func testRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := New(Config{
		Token:       "test-token",
		CacheShared: true,
		Client:      fake.NewSimpleClientset(),
		TenantOf: func(ctx context.Context, spaceID string) (string, error) {
			if spaceID == "no-tenant" {
				return "", nil
			}
			return "acme", nil
		},
	})
	if err != nil {
		t.Fatalf("构造执行面失败: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

// 资源名会被 k8s 校验：企业名是中文、空间是 uuid，直接拼进名字都不合法。
func TestPodNamesAreDNSValid(t *testing.T) {
	r := testRunner(t)
	valid := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	cases := []sandbox.Scope{
		{Space: "0c4f8a3e-1f7b-4a2f-9a1e-7c1f0b2d3e4f", Kind: sandbox.KindShell},
		{Space: "s1", Tenant: "测试企业", Kind: sandbox.KindPlugin},
		{Space: "s1", Tenant: "acme", Kind: sandbox.KindPlugin},
	}
	for _, sc := range cases {
		tg, err := r.resolve(context.Background(), sc)
		if err != nil {
			t.Fatalf("解析 %+v 失败: %v", sc, err)
		}
		if !valid.MatchString(tg.name) {
			t.Errorf("Pod 名 %q 不是合法 DNS label（scope=%+v）", tg.name, sc)
		}
		if len(tg.name) > 63 {
			t.Errorf("Pod 名过长: %q", tg.name)
		}
	}
}

// 同一个空间永远解析到同一个容器：不然每次调用都新建一个，
// 空闲回收和连接复用都会失效。
func TestResolveIsStable(t *testing.T) {
	r := testRunner(t)
	a, err := r.resolve(context.Background(), sandbox.Scope{Space: "s1", Kind: sandbox.KindShell})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.resolve(context.Background(), sandbox.Scope{Space: "s1", Kind: sandbox.KindShell})
	if err != nil {
		t.Fatal(err)
	}
	if a.name != b.name {
		t.Fatalf("同一空间解析出两个容器: %s != %s", a.name, b.name)
	}
	if !strings.HasPrefix(a.name, podShell) {
		t.Fatalf("空间容器名应以 %s 开头，实际 %s", podShell, a.name)
	}
}

// 插件容器按租户共享：同租户两个空间落到同一个 Pod，不同租户分开。
func TestPluginContainerSharedPerTenant(t *testing.T) {
	r := testRunner(t)
	one, _ := r.resolve(context.Background(), sandbox.Scope{Space: "s1", Kind: sandbox.KindPlugin})
	two, _ := r.resolve(context.Background(), sandbox.Scope{Space: "s2", Kind: sandbox.KindPlugin})
	if one.name != two.name {
		t.Fatalf("同租户的插件应共用一个容器: %s != %s", one.name, two.name)
	}
	other, _ := r.resolve(context.Background(), sandbox.Scope{Space: "s1", Tenant: "other", Kind: sandbox.KindPlugin})
	if other.name == one.name {
		t.Fatal("不同租户不能共用一个插件容器")
	}
}

// 租户解析不出来时退化为按空间隔离：宁可多起容器，不能跨企业共用一个 Pod。
func TestUnknownTenantFallsBackToSpace(t *testing.T) {
	r := testRunner(t)
	one, err := r.resolve(context.Background(), sandbox.Scope{Space: "no-tenant", Kind: sandbox.KindPlugin})
	if err != nil {
		t.Fatal(err)
	}
	two, err := r.resolve(context.Background(), sandbox.Scope{Space: "other-untagged", Kind: sandbox.KindPlugin})
	if err != nil {
		t.Fatal(err)
	}
	if one.name == two.name {
		t.Fatal("租户未知时两个空间的插件容器必须分开")
	}
}

func TestResolveRejectsMissingSpace(t *testing.T) {
	r := testRunner(t)
	if _, err := r.resolve(context.Background(), sandbox.Scope{Kind: sandbox.KindShell}); err == nil {
		t.Fatal("缺少空间归属必须报错，否则所有调用会挤进同一个容器")
	}
	if _, err := r.resolve(context.Background(), sandbox.Scope{Space: "s1", Kind: "mystery"}); err == nil {
		t.Fatal("未知执行体种类必须报错")
	}
}

// 控制面把文件写在共享卷上，容器必须挂同一个卷，否则文件工具找不到文件。
func TestPodMountsSharedStorage(t *testing.T) {
	r := testRunner(t)
	tg, _ := r.resolve(context.Background(), sandbox.Scope{Space: "s1", Kind: sandbox.KindShell})
	pod := r.buildPod(tg)

	if got := pod.Labels[labelTenant]; got != shortHash("acme") {
		t.Fatalf("租户标签不对: %q", got)
	}
	if got := pod.Labels[labelSpace]; got != "s1" {
		t.Fatalf("空间标签不对: %q", got)
	}

	var storage, cache *corev1.Volume
	for i := range pod.Spec.Volumes {
		switch pod.Spec.Volumes[i].Name {
		case "storage":
			storage = &pod.Spec.Volumes[i]
		case "cache":
			cache = &pod.Spec.Volumes[i]
		}
	}
	if storage == nil || storage.PersistentVolumeClaim == nil ||
		storage.PersistentVolumeClaim.ClaimName != r.cfg.StoragePVCName {
		t.Fatalf("存储卷没挂共享 PVC: %+v", storage)
	}
	if cache == nil || cache.PersistentVolumeClaim == nil ||
		cache.PersistentVolumeClaim.ClaimName != cachePVCName("acme") {
		t.Fatalf("缓存卷必须是租户级 PVC: %+v", cache)
	}

	// 控制面发来的 Mount.Key 是 spaces/<id>/…，容器根必须挂在 /var/lib/workagent。
	found := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "storage" && m.MountPath == rootDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("存储卷应挂在 %s: %+v", rootDir, pod.Spec.Containers[0].VolumeMounts)
	}
}

// 两类容器的资源限额按种类区分：插件容器要扛常驻进程池。
func TestPodResourcesByKind(t *testing.T) {
	r := testRunner(t)

	shell, _ := r.resolve(context.Background(), sandbox.Scope{Space: "s1", Kind: sandbox.KindShell})
	if got := r.buildPod(shell).Spec.Containers[0].Resources.Limits.Memory().String(); got != "2Gi" {
		t.Fatalf("空间容器内存限额不对: %s", got)
	}
	plugin, _ := r.resolve(context.Background(), sandbox.Scope{Space: "s1", Kind: sandbox.KindPlugin})
	if got := r.buildPod(plugin).Spec.Containers[0].Resources.Limits.Memory().String(); got != "4Gi" {
		t.Fatalf("插件容器内存限额不对: %s", got)
	}
}

// 回收只针对空间容器：插件容器常驻，删了等于把常驻进程和连接全断掉。
func TestReapOnlyRemovesIdleShellPods(t *testing.T) {
	r := testRunner(t)
	r.cfg.ShellIdle = time.Minute

	old := time.Now().Add(-2 * time.Minute)
	fresh := time.Now()
	mk := func(name string, kind string, created time.Time) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: r.cfg.Namespace,
				Labels:            map[string]string{labelApp: "sandbox-runner", labelKind: kind},
				CreationTimestamp: metav1.NewTime(created),
			},
		}
	}
	cs := r.cs.(*fake.Clientset)
	for _, pod := range []*corev1.Pod{
		mk("idle-shell", "shell", old),
		mk("busy-shell", "shell", fresh),
		mk("plugin", "plugin", old),
	} {
		if _, err := cs.CoreV1().Pods(r.cfg.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	r.reapOnce(context.Background())

	if _, err := cs.CoreV1().Pods(r.cfg.Namespace).Get(context.Background(), "idle-shell", metav1.GetOptions{}); err == nil {
		t.Fatal("空闲的空间容器应被回收")
	}
	if _, err := cs.CoreV1().Pods(r.cfg.Namespace).Get(context.Background(), "busy-shell", metav1.GetOptions{}); err != nil {
		t.Fatalf("刚用过的空间容器不该被回收: %v", err)
	}
	if _, err := cs.CoreV1().Pods(r.cfg.Namespace).Get(context.Background(), "plugin", metav1.GetOptions{}); err != nil {
		t.Fatalf("插件容器常驻，不该被空闲回收: %v", err)
	}
}

// 正在跑长命令的容器不是空转：一条命令跑得比空闲 TTL 还久时不能被删。
func TestReapSkipsActiveShellPods(t *testing.T) {
	r := testRunner(t)
	r.cfg.ShellIdle = time.Minute

	cs := r.cs.(*fake.Clientset)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "long-command", Namespace: r.cfg.Namespace,
			Labels:            map[string]string{labelApp: "sandbox-runner", labelKind: "shell"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
		},
	}
	if _, err := cs.CoreV1().Pods(r.cfg.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.begin("long-command")

	r.reapOnce(context.Background())

	if _, err := cs.CoreV1().Pods(r.cfg.Namespace).Get(context.Background(), "long-command", metav1.GetOptions{}); err != nil {
		t.Fatalf("正在执行的容器被回收了: %v", err)
	}
}

// PVC 创建必须幂等：并发调用会同时发现「不存在」，
// 后到的 AlreadyExists 不是错误。
func TestEnsurePVCIsIdempotent(t *testing.T) {
	r := testRunner(t)
	for i := 0; i < 3; i++ {
		if err := r.ensurePVC(context.Background(), r.buildStoragePVC()); err != nil {
			t.Fatalf("第 %d 次创建存储卷失败: %v", i+1, err)
		}
		if err := r.ensurePVC(context.Background(), r.buildCachePVC("acme")); err != nil {
			t.Fatalf("第 %d 次创建缓存卷失败: %v", i+1, err)
		}
	}
	if got := r.buildStoragePVC().Spec.AccessModes[0]; got != corev1.ReadWriteMany {
		t.Fatalf("存储卷要跨 Pod 共享，必须是 RWX: %v", got)
	}
	if got := r.buildCachePVC("acme").Spec.AccessModes[0]; got != corev1.ReadWriteMany {
		t.Fatalf("租户缓存要跨容器共享，必须是 RWX: %v", got)
	}
}

// 集群不支持 RWX（kind / local-path）时必须能退回 emptyDir，
// 否则 Pod 会一直 Pending 在「等缓存卷」上。
func TestCacheFallsBackToEmptyDir(t *testing.T) {
	r, err := New(Config{
		Token:  "test-token",
		Client: fake.NewSimpleClientset(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tg, _ := r.resolve(context.Background(), sandbox.Scope{Space: "s1", Kind: sandbox.KindShell})
	pod := r.buildPod(tg)
	for _, v := range pod.Spec.Volumes {
		if v.Name == "cache" && v.EmptyDir == nil {
			t.Fatalf("未开启共享缓存时应使用 emptyDir: %+v", v.VolumeSource)
		}
	}
}

// Pod 报告就绪后必须连得上：这是宿主上的控制面（make run）到执行体之间唯一的通路。
func TestDialConnectsToReadyPod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"backend":"bwrap","isolated":true}`))
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	r := testRunner(t)
	r.cfg.RunnerPort = port
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "wa-shell-test", Namespace: "workagent"},
		Status:     corev1.PodStatus{PodIP: "127.0.0.1"},
	}
	c, tunnel, err := r.dial(context.Background(), pod)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if tunnel != nil {
		t.Fatal("直连成功时不应该建隧道")
	}
	if c.Name() != "bwrap" {
		t.Fatalf("后端 = %q", c.Name())
	}
}

// 只有传输层错误值得重试；鉴权、协议错误重试也变不成成功。
func TestIsDialError(t *testing.T) {
	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no route to host")}
	if !isDialError(netErr) {
		t.Fatal("no route to host 应判定为可重试")
	}
	if isDialError(errors.New("执行体返回 401")) {
		t.Fatal("鉴权错误不应重试")
	}
}
