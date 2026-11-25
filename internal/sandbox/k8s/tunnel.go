package k8s

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// openTunnel 经 API Server 建一条到执行体端口的端口转发，返回本地端口。
//
// 直连 Pod IP 需要调用方能路由到 Pod 网段，这只在集群内天然成立。
// 控制面跑在宿主机上时（make run / CI），到 Pod 网段的转发受本机网络策略、
// CNI 与 macOS 本地网络权限影响，常见表现是 Pod 明明 Ready、宿主机 dial 却
// 一直 no route to host。隧道只走「本机 → API Server」这一跳，和 Pod 路由无关。
//
// 需要 pods/portforward 的 create 权限（见 deploy/local/05-rbac.yaml）。
func (r *Runner) openTunnel(pod *corev1.Pod) (io.Closer, int, error) {
	if r.rc == nil {
		return nil, 0, errors.New("没有可用的 REST 配置，无法建隧道")
	}
	req := r.cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("portforward")
	rt, upgrader, err := spdy.RoundTripperFor(r.rc)
	if err != nil {
		return nil, 0, err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, req.URL())

	stop := make(chan struct{})
	ready := make(chan struct{})
	// 本地端口用 0 让内核分配，避免同一台机器上多个控制面进程抢端口。
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", r.cfg.RunnerPort)},
		stop, ready, io.Discard, io.Discard)
	if err != nil {
		return nil, 0, err
	}
	go func() { _ = fw.ForwardPorts() }()

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		close(stop)
		return nil, 0, errors.New("端口转发 10 秒内未就绪")
	}
	ports, err := fw.GetPorts()
	if err != nil {
		close(stop)
		return nil, 0, err
	}
	if len(ports) != 1 {
		close(stop)
		return nil, 0, fmt.Errorf("端口转发返回了 %d 个端口，期望 1 个", len(ports))
	}
	return &tunnel{stop: stop, fw: fw}, int(ports[0].Local), nil
}

// tunnel 是端口转发的生命周期句柄：连接缓存被替换时要显式关掉，
// 否则每换一次 Pod 就漏一条 SPDY 连接和本地端口。
type tunnel struct {
	stop chan struct{}
	fw   *portforward.PortForwarder
	once sync.Once
}

func (t *tunnel) Close() error {
	t.once.Do(func() {
		close(t.stop)
		t.fw.Close()
	})
	return nil
}
