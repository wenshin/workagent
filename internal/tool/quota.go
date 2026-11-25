package tool

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/spacefs"
)

// 空间磁盘配额，默认 5 GiB。0 表示不限制。
//
// 这个值是「软配额」：执行前统计一次，超了拒绝启动新命令。
// 不做硬配额是因为硬配额要么靠文件系统（xfs project quota / ext4 prjquota），
// 要么靠 cgroup v2 的 io 限制，两者都要求部署侧配合，改不动。
// 软配额的缺口是「一条命令就能写超」，但能在下一条命令前拦住并提示，
// 对 agent 这种「一步一步来」的用法已经够用。
var (
	quotaMu   sync.RWMutex
	quotaByte int64 = 5 << 30
	// 统计结果按目录缓存 30 秒：du 遍历几万文件要几十毫秒，
	// 每条命令都算一遍会把「执行前检查」变成主要开销。
	quotaStatMu sync.Mutex
	quotaStat   = map[string]quotaSample{}
)

type quotaSample struct {
	at  time.Time
	use int64
}

// SetSpaceQuota 设置每空间的磁盘上限（字节），<=0 表示不限制。
func SetSpaceQuota(n int64) {
	quotaMu.Lock()
	quotaByte = n
	quotaMu.Unlock()
}

// SpaceQuota 返回当前配额（字节）。
func SpaceQuota() int64 {
	quotaMu.RLock()
	defer quotaMu.RUnlock()
	return quotaByte
}

// CheckSpaceQuota 在执行前检查空间目录占用。
//
// 统计失败一律放行：配额是保护措施，不该成为新的可用性风险。
func CheckSpaceQuota(dir string) error {
	limit := SpaceQuota()
	if limit <= 0 || dir == "" {
		return nil
	}
	use, err := spaceUsage(dir)
	if err != nil {
		return nil
	}
	if use > limit {
		return fmt.Errorf("空间已用 %s，超过配额 %s：先删掉不需要的文件（%s 下的大文件可以直接删）",
			humanBytes(use), humanBytes(limit), dir)
	}
	return nil
}

func spaceUsage(dir string) (int64, error) {
	quotaStatMu.Lock()
	s, ok := quotaStat[dir]
	quotaStatMu.Unlock()
	if ok && time.Since(s.at) < 30*time.Second {
		return s.use, nil
	}

	// -s 汇总、-k 以 1024 字节为单位。
	// 不用 -b：GNU du 支持，BSD（macOS）不支持，而这个函数两边都要跑。
	out, err := exec.Command("du", "-sk", dir).Output()
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("du 输出为空")
	}
	kib, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, err
	}
	use := kib * 1024
	// 运行日志是平台写的，不该占用户配额：单独量一次扣掉。
	// 量不到按 0 处理——配额统计失败本来就一律放行，日志更不该成为拦截理由。
	use -= dirUsage(filepath.Join(dir, "files", spacefs.LogsDir))

	quotaStatMu.Lock()
	quotaStat[dir] = quotaSample{at: time.Now(), use: use}
	quotaStatMu.Unlock()
	return use, nil
}

// dirUsage 返回单个目录占用的字节数，量不到返回 0。
func dirUsage(dir string) int64 {
	out, err := exec.Command("du", "-sk", dir).Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0
	}
	kib, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return kib * 1024
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
