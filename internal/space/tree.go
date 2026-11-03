package space

import (
	"sort"

	"github.com/workagent/auto-agent/internal/store"
)

// BuildTree 对扁平空间列表排序：个人空间置顶，其余按创建时间。
//
// 空间只有一级（子空间概念已取消），第二层起都是普通目录，因此这里不再组装父子关系。
func BuildTree(list []store.Space) []store.Space {
	out := make([]store.Space, len(list))
	copy(out, list)
	sortSiblings(out)
	return out
}

// spacePriority 决定左侧树的空间顺序：个人空间 → 系统空间 → 共享空间。
func spacePriority(spaceType string) int {
	switch spaceType {
	case "personal":
		return 0
	case "system":
		return 1
	default:
		return 2
	}
}

func sortSiblings(items []store.Space) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if pa, pb := spacePriority(a.Type), spacePriority(b.Type); pa != pb {
			return pa < pb
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
}
