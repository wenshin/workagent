package space

import (
	"sort"

	"github.com/workagent/auto-agent/internal/store"
)

// BuildTree 把扁平列表组装成树序（父节点后紧跟其子节点），个人空间排最前。
// 父节点不可见（无权限）的子空间会被提升为根，避免数据静默消失。
func BuildTree(list []store.Space) []store.Space {
	exists := make(map[string]bool, len(list))
	for _, s := range list {
		exists[s.ID] = true
	}

	children := make(map[string][]store.Space, len(list))
	var roots []store.Space
	for _, s := range list {
		if s.ParentID == "" || !exists[s.ParentID] {
			roots = append(roots, s)
			continue
		}
		children[s.ParentID] = append(children[s.ParentID], s)
	}

	sortSiblings(roots)
	for k := range children {
		sortSiblings(children[k])
	}

	out := make([]store.Space, 0, len(list))
	var walk func(items []store.Space, depth int)
	walk = func(items []store.Space, depth int) {
		for _, s := range items {
			s.Depth = depth
			out = append(out, s)
			walk(children[s.ID], depth+1)
		}
	}
	walk(roots, 0)
	return out
}

func sortSiblings(items []store.Space) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Type != b.Type {
			return a.Type == "personal"
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
}
