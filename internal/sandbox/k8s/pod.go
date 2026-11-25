package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/workagent/auto-agent/internal/sandbox"
)

const (
	labelKind   = "wa.kind"
	labelSpace  = "wa.space"
	labelTenant = "wa.tenant"
	labelApp    = "app.kubernetes.io/name"

	podShell  = "wa-shell-"
	podPlugin = "wa-plugin-"
)

// shortHash 把任意标识压成 DNS 安全、长度固定的后缀。
// 空间 / 租户 ID 是 uuid 或中文企业名，直接进资源名会不合法。
func shortHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:12]
}

func cachePVCName(tenant string) string { return "wa-cache-" + shortHash(tenant) }

func podNameFor(kind sandbox.Kind, key string) string {
	if kind == sandbox.KindPlugin {
		return podPlugin + shortHash(key)
	}
	return podShell + shortHash(key)
}

func (r *Runner) podLabels(t target) map[string]string {
	l := map[string]string{
		// 与 legacy 的 sandbox-runner 同名：NetworkPolicy 按这个标签选执行体，
		// 换一个值会让执行体在启用 NetworkPolicy 的集群上绕过私网封禁。
		labelApp:    "sandbox-runner",
		labelKind:   string(t.kind),
		labelTenant: t.tenantHash,
	}
	if t.kind == sandbox.KindShell {
		l[labelSpace] = t.space
	}
	return l
}

// buildStoragePVC 是存储卷的兜底定义。
//
// 正常情况下这个 PVC 由部署创建、控制面先挂上（STORAGE_ROOT 指向它），
// 这里只在集群里根本没有时补一个空卷，免得 Pod 一直 Pending 在「卷不存在」。
func (r *Runner) buildStoragePVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.cfg.StoragePVCName,
			Namespace: r.cfg.Namespace,
			Labels:    map[string]string{labelApp: "sandbox-executor", labelKind: "storage"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: storageClass(r.cfg.StorageClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(r.cfg.StoragePVCSize),
				},
			},
		},
	}
}

// buildCachePVC 是租户缓存卷：apt / npm / pip / go / deno 的下载缓存。
func (r *Runner) buildCachePVC(tenant string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cachePVCName(tenant),
			Namespace: r.cfg.Namespace,
			Labels: map[string]string{
				labelApp:    "sandbox-executor",
				labelKind:   "cache",
				labelTenant: shortHash(tenant),
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: storageClass(r.cfg.StorageClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(r.cfg.CachePVCSize),
				},
			},
		},
	}
}

func storageClass(name string) *string {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	return &name
}

// buildPod 生成执行体 Pod。
//
// 空间容器与插件容器的存储面是同一个卷（控制面写进去的文件必须在容器里
// 可见，而控制面挂不了「每空间一个 PVC」）；隔离靠两件事：每个 Pod 有
// 独立的可写层（/usr 不共享），以及 bwrap 只挂清单声明的子目录。
func (r *Runner) buildPod(t target) *corev1.Pod {
	memory, cpu := r.cfg.ShellMemory, r.cfg.ShellCPU
	if t.kind == sandbox.KindPlugin {
		memory, cpu = r.cfg.PluginMemory, r.cfg.PluginCPU
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t.name,
			Namespace: r.cfg.Namespace,
			Labels:    r.podLabels(t),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			TerminationGracePeriodSeconds: int64Ptr(30),
			// 执行体不需要访问 API Server：拿不到令牌就少一条逃逸后的横向路径。
			AutomountServiceAccountToken: boolPtr(false),
			// subPath 挂载要求目标目录已存在，否则 Pod 起不来（CreateContainerConfigError）。
			InitContainers: []corev1.Container{{
				Name:            "init-cache",
				Image:           r.cfg.InitImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"sh", "-c",
					"mkdir -p /cache/apt/lists /cache/apt/archives /cache/npm /cache/pip /cache/go/mod /cache/deno /cache/xdg /cache/mise"},
				VolumeMounts: []corev1.VolumeMount{{Name: "cache", MountPath: cacheDir}},
			}},
			Containers: []corev1.Container{{
				Name:            "runner",
				Image:           r.cfg.Image,
				ImagePullPolicy: corev1.PullPolicy(r.cfg.ImagePullPolicy),
				Ports: []corev1.ContainerPort{{
					Name: "http", ContainerPort: int32(r.cfg.RunnerPort),
				}},
				Env: []corev1.EnvVar{
					{Name: "SANDBOX_AGENT_ADDR", Value: ":" + strconv.Itoa(r.cfg.RunnerPort)},
					{Name: "SANDBOX_AGENT_ROOT", Value: rootDir},
					{Name: "WA_CACHE_DIR", Value: cacheDir},
					{Name: "SANDBOX_RUNNER_TOKEN", ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: r.cfg.TokenSecret},
							Key:                  r.cfg.TokenKey,
						},
					}},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "storage", MountPath: rootDir},
					{Name: "cache", MountPath: cacheDir},
					{Name: "cache", MountPath: "/var/lib/apt/lists", SubPath: "apt/lists"},
					{Name: "cache", MountPath: "/var/cache/apt/archives", SubPath: "apt/archives"},
					{Name: "shm", MountPath: "/dev/shm"},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(cpu),
						corev1.ResourceMemory: resource.MustParse(memory),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					Capabilities: &corev1.Capabilities{
						// SYS_ADMIN：bwrap 的 bind mount / proc / dev；
						// NET_ADMIN：--unshare-net 后拉 lo 与封私网。
						Add: []corev1.Capability{"SYS_ADMIN", "NET_ADMIN"},
					},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/healthz", Port: intstr.FromInt32(int32(r.cfg.RunnerPort)),
						},
					},
					PeriodSeconds: 5,
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: "storage",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: r.cfg.StoragePVCName,
						},
					},
				},
				r.cacheVolume(t.tenant),
				shmVolume(),
			},
		},
	}
}

// cacheVolume 是租户缓存。共享模式（RWX PVC）能跨空间命中下载缓存，
// 本地模式（emptyDir）随 Pod 生命周期清空，但不需要集群支持 RWX。
func (r *Runner) cacheVolume(tenant string) corev1.Volume {
	if r.cfg.CacheShared {
		return corev1.Volume{
			Name: "cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: cachePVCName(tenant),
				},
			},
		}
	}
	size := resource.MustParse(r.cfg.CachePVCSize)
	return corev1.Volume{
		Name: "cache",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size},
		},
	}
}

func shmVolume() corev1.Volume {
	size := resource.MustParse("512Mi")
	return corev1.Volume{Name: "shm", VolumeSource: corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &size},
	}}
}

func int64Ptr(v int64) *int64 { return &v }
func boolPtr(v bool) *bool    { return &v }
