package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// DefaultEnvFactsPath 是执行镜像自述文件的位置，可用 SANDBOX_ENV_FACTS 覆盖。
const DefaultEnvFactsPath = "/etc/wa/env.json"

// EnvFacts 是执行容器对自身运行环境的自述。
//
// 为什么由镜像产出、而不是控制面写死：镜像决定「有什么工具」，控制面猜不准；
// 手写一份环境说明必然和真实镜像漂移，而且镜像升级后没人记得改。
// 所以约定镜像构建时把自检结果写进 DefaultEnvFactsPath，执行体启动读一次，
// 控制面通过 EnvProvider 取走，随系统提示词注入。
//
// 这份事实只描述「镜像基线」。装在空间 deps 目录里的东西是运行时产物，
// 随时会变，不能写进提示词——那部分让模型自己 `ls $WA_DEPS/bin`。
type EnvFacts struct {
	// Image 是镜像标识（tag 或 digest），用于排障时对账「提示词说的是哪个镜像」
	Image string `json:"image,omitempty"`
	// OS 是发行版与架构，例如 debian 12 (linux/arm64)
	OS string `json:"os,omitempty"`
	// Tools 是镜像里确认可用的命令。没列出的按不可用处理。
	Tools []ToolFact `json:"tools,omitempty"`
	// Writable 是除空间目录之外还可写的位置（如 .pkg）
	Writable []string `json:"writable,omitempty"`
	// Network 描述 shell 的网络档位，例如「网络工具审批 + 脚本默认无网 + 其余有网」。
	// 它随平台策略变，不属于镜像镜像自述的稳定事实，收口方向见 docs/K8s容器沙箱方案.md
	Network string `json:"network,omitempty"`
	// Sources 是安装源，键为生态（npm/pip/go/cargo），值为镜像 ID 或 URL
	Sources map[string]string `json:"sources,omitempty"`
	// Persistent 表示容器销毁后系统目录的改动是否保留
	Persistent bool `json:"persistent,omitempty"`
}

// ToolFact 是一个可用命令及其版本。
type ToolFact struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Empty 表示没有任何可说的内容：调用方据此整段省略，而不是渲染一个空段落。
func (f EnvFacts) Empty() bool {
	return strings.TrimSpace(f.Image) == "" &&
		strings.TrimSpace(f.OS) == "" &&
		len(f.Tools) == 0
}

// Hash 是内容的稳定摘要，用于判断「环境换了没」。
//
// 工具按名字排序后再算：同一个镜像被两次构建、文件里顺序不同，
// 不应该算出两个 hash 而让提示词无谓地变一次。
func (f EnvFacts) Hash() string {
	c := f
	c.Tools = append([]ToolFact(nil), f.Tools...)
	sort.Slice(c.Tools, func(i, j int) bool { return c.Tools[i].Name < c.Tools[j].Name })

	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// LoadEnvFacts 读镜像自述文件。
//
// 文件不存在返回零值与 nil：本地开发、老镜像没有这个文件是正常情况，
// 调用方按「不知道」处理（提示词里整段省略），不是错误。
func LoadEnvFacts(path string) (EnvFacts, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultEnvFactsPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return EnvFacts{}, nil
		}
		return EnvFacts{}, err
	}

	var f EnvFacts
	if err := json.Unmarshal(b, &f); err != nil {
		return EnvFacts{}, fmt.Errorf("解析执行环境自述 %s 失败: %w", path, err)
	}
	f.Image = strings.TrimSpace(f.Image)
	f.OS = strings.TrimSpace(f.OS)
	f.Network = strings.TrimSpace(f.Network)

	// 空名字会渲染成「可用工具：、git」，清理掉而不是留给下游判断
	tools := make([]ToolFact, 0, len(f.Tools))
	for _, t := range f.Tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		tools = append(tools, ToolFact{Name: name, Version: strings.TrimSpace(t.Version)})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	f.Tools = tools

	dirs := make([]string, 0, len(f.Writable))
	for _, d := range f.Writable {
		if d = strings.TrimSpace(d); d != "" {
			dirs = append(dirs, d)
		}
	}
	f.Writable = dirs

	return f, nil
}

// EnvProvider 是执行体的可选能力：报告自己跑在什么环境里。
//
// 做成可选接口而不是扩 Runner：本地后端（bwrap / sandbox-exec / local）
// 没有「容器镜像」这个概念，硬塞进接口只能返回空值，还得让三个实现都写一遍。
type EnvProvider interface {
	Env() EnvFacts
}
