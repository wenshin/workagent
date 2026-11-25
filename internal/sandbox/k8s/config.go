// Package k8s 是容器化执行面：按空间 / 租户建 Pod，把 sandbox.Runner 的
// 一次调用翻译成「ensure 容器 + 调容器内的 sandbox-agent」。
//
// 为什么是每空间一个 Pod 而不是一个常驻进程服务所有空间：
//   - shell 里 agent 可以 apt install，可写层必须按空间隔离，否则两个空间的
//     系统包互相覆盖（同一个 ffmpeg 只能有一个版本）；
//   - 空间坏掉 / 空转时删掉整个 Pod 就回收了全部资源，不需要在进程内做清场。
//
// 插件容器按租户共享：常驻进程池的收益是摊掉 Deno 冷启动，按插件或按空间拆
// 容器会让冷启动回到秒级；租户之间仍然不共享容器。
package k8s

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
)

const (
	defaultNamespace = "workagent"
	defaultImage     = "workagent/sandbox-runner:dev"
	defaultPort      = 8323
	defaultIdle      = 15 * time.Minute
	defaultReady     = 90 * time.Second
	defaultReap      = 30 * time.Second

	// rootDir / cacheDir 与 sandbox-agent 的默认值一致：
	// 容器内是固定虚拟路径，控制面看不到宿主目录。
	rootDir  = "/var/lib/workagent"
	cacheDir = "/cache"

	defaultTokenSecret = "sandbox-runner-token"
	defaultTokenKey    = "token"
	defaultStoragePVC  = "workagent-storage"
)

// Config 是调度层参数。零值全部有默认，只有 Token 必填。
type Config struct {
	// Namespace 是执行体 Pod 所在的命名空间。
	Namespace string
	// Image 是执行体镜像；必须带 bwrap 与 sandbox-agent。
	Image string
	// Token 是控制面调 sandbox-agent 的共享令牌，同时用于从 Secret 注入 Pod。
	Token string
	// TokenSecret / TokenKey 指向集群里已有的 Secret。
	TokenSecret string
	TokenKey    string
	// RunnerPort 是 sandbox-agent 的监听端口。
	RunnerPort int
	// ImagePullPolicy 取 IfNotPresent / Always / Never，默认 IfNotPresent。
	ImagePullPolicy string
	// StorageClass 为空时用集群默认 StorageClass。
	StorageClass string
	// CacheShared 为 true 时租户缓存用 RWX PVC 跨 Pod 共享；
	// 为 false（默认）时退化成 emptyDir——kind / local-path 不支持 RWX，
	// 本地起不来会在「缓存卷一直 Pending」上浪费很久。
	CacheShared bool
	// InitImage 是准备缓存目录的初始容器镜像。
	InitImage string
	// StoragePVCName / StoragePVCSize 是控制面与执行体共享的存储卷。
	// 部署时由运维创建并挂给控制面（STORAGE_ROOT），这里只在缺失时补建。
	StoragePVCName string
	StoragePVCSize string
	// CachePVCSize 是租户缓存卷 / emptyDir 的容量，默认 20Gi。
	CachePVCSize string
	// ShellIdle 是空间容器的空闲回收时长；<=0 用默认 15 分钟。
	ShellIdle time.Duration
	// ReadyTimeout 是等容器就绪的上限；<=0 用 90 秒（首次拉镜像会慢）。
	ReadyTimeout time.Duration
	// ReapInterval 是回收协程的扫描间隔；<=0 用 30 秒。
	ReapInterval time.Duration
	// ShellMemory / ShellCPU / PluginMemory / PluginCPU 是资源限额。
	ShellMemory  string
	ShellCPU     string
	PluginMemory string
	PluginCPU    string
	// Kubeconfig 为空时依次尝试 in-cluster（Pod 内的 ServiceAccount）
	// 与本机 kubeconfig（$KUBECONFIG 或 ~/.kube/config），供 make run / CI 使用。
	Kubeconfig string
	// TenantOf 把空间解析成租户（插件容器按租户共享）。
	//
	// 返回空串或为 nil 时退化为「每空间一个插件容器」——宁可多起容器，
	// 也不把两个企业的插件进程放进同一个 Pod。
	TenantOf func(ctx context.Context, spaceID string) (string, error)
	// Client 可注入（测试用）；为 nil 时按 Kubeconfig / in-cluster 构造。
	Client kubernetes.Interface
	// Log 为空时用 slog.Default()。
	Log *slog.Logger
}

func (c *Config) applyDefaults() error {
	if strings.TrimSpace(c.Namespace) == "" {
		c.Namespace = defaultNamespace
	}
	if strings.TrimSpace(c.Image) == "" {
		c.Image = defaultImage
	}
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("k8s 执行面缺少令牌：控制面与执行体之间必须有鉴权")
	}
	if strings.TrimSpace(c.TokenSecret) == "" {
		c.TokenSecret = defaultTokenSecret
	}
	if strings.TrimSpace(c.TokenKey) == "" {
		c.TokenKey = defaultTokenKey
	}
	if c.RunnerPort <= 0 {
		c.RunnerPort = defaultPort
	}
	if strings.TrimSpace(c.ImagePullPolicy) == "" {
		c.ImagePullPolicy = "IfNotPresent"
	}
	if strings.TrimSpace(c.InitImage) == "" {
		c.InitImage = "busybox:1.36"
	}
	if strings.TrimSpace(c.StoragePVCName) == "" {
		c.StoragePVCName = defaultStoragePVC
	}
	if strings.TrimSpace(c.StoragePVCSize) == "" {
		c.StoragePVCSize = "100Gi"
	}
	if strings.TrimSpace(c.CachePVCSize) == "" {
		c.CachePVCSize = "20Gi"
	}
	if c.ShellIdle <= 0 {
		c.ShellIdle = defaultIdle
	}
	if c.ReadyTimeout <= 0 {
		c.ReadyTimeout = defaultReady
	}
	if c.ReapInterval <= 0 {
		c.ReapInterval = defaultReap
	}
	if strings.TrimSpace(c.ShellMemory) == "" {
		c.ShellMemory = "2Gi"
	}
	if strings.TrimSpace(c.PluginMemory) == "" {
		c.PluginMemory = "4Gi"
	}
	if strings.TrimSpace(c.ShellCPU) == "" {
		c.ShellCPU = "2"
	}
	if strings.TrimSpace(c.PluginCPU) == "" {
		c.PluginCPU = "2"
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	return nil
}
