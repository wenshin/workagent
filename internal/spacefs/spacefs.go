// Package spacefs 定义空间文件区（spaces/<id>/files/）里由平台维护的隐藏项。
//
// 隐藏文件本身不是禁区：.env、.gitignore、.github/ 这类内容属于用户，
// 读写、预览与普通文件一视同仁。
//
// 只有平台自己写的隐藏项（能力索引、安装标记、改动备份）对用户与 Agent
// 都不可见、不可读写——它们一旦被改写，空间索引或插件安装态就会与磁盘内容对不上。
// 判断入口统一收敛在这里，避免各条读写路径各写一份规则后出现漏网的一侧。
package spacefs

import "strings"

const (
	// MetaFile 是空间能力索引（files/.meta.json），旧技能目录也用它存展示元数据。
	MetaFile = ".meta.json"
	// InstallFile 是插件安装标记（files/plugins/<slug>/.install.json）。
	InstallFile = ".install.json"
	// HistoryDir 是插件改动前的备份目录（files/plugins/.history/）。
	HistoryDir = ".history"
)

// reserved 是平台保留的隐藏项。
//
// 大小写不敏感：macOS / Windows 的文件系统不区分大小写，
// 按大小写敏感比较会被 .META.JSON 绕过去（读到同一份文件）。
var reserved = map[string]struct{}{
	MetaFile:    {},
	InstallFile: {},
	HistoryDir:  {},
}

// Hidden 报告名字是否是隐藏项（以 . 开头）。
// "." 与 ".." 是路径语义，不算隐藏文件。
func Hidden(name string) bool {
	return len(name) > 1 && name[0] == '.' && name != ".."
}

// Reserved 报告单个名字是否是平台保留项。
func Reserved(name string) bool {
	_, ok := reserved[strings.ToLower(name)]
	return ok
}

// ReservedPath 报告相对路径的任一段是否命中平台保留项。
func ReservedPath(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if Reserved(seg) {
			return true
		}
	}
	return false
}

// UserHidden 报告名字是否是用户自己的隐藏文件（隐藏但不是平台保留项）。
//
// 这类文件常常没有常规后缀（.env / .gitignore），调用方的后缀白名单
// 不该把它们挡在门外，否则「支持隐藏文件」只对 .foo.md 成立。
func UserHidden(name string) bool {
	return Hidden(name) && !Reserved(name)
}
