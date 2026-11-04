package llm

import "strings"

// EstimateTokens 粗略估算文本的 token 数。
//
// 不引入分词器依赖：CJK 字符按 0.6 token/字，其余字符按 0.25 token/字符估算。
// 实测误差通常在 20% 以内，足以支撑上下文预算控制（不用于计费）。
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	units := 0
	for _, r := range s {
		if isWideRune(r) {
			units += 6
		} else {
			units += 3
		}
	}
	return (units + 9) / 10
}

// TruncateByTokens 按 token 预算截断文本，返回截断后的文本与是否发生截断。
//
// 截断点尽量回退到换行处，避免把一句话或代码块切断。
func TruncateByTokens(s string, maxTokens int) (string, bool) {
	if maxTokens <= 0 || EstimateTokens(s) <= maxTokens {
		return s, false
	}

	units := 0
	limit := maxTokens * 10
	cut := len(s)
	for i, r := range s {
		if isWideRune(r) {
			units += 6
		} else {
			units += 3
		}
		if units > limit {
			cut = i
			break
		}
	}

	out := strings.ToValidUTF8(s[:cut], "")
	// 截断处离最近的换行不远时回退到行边界，读起来更完整
	if nl := strings.LastIndexByte(out, '\n'); nl > 0 && nl > len(out)*2/3 {
		out = out[:nl]
	}
	return out, true
}

// isWideRune 判断是否为中日韩等「一字多 token」的宽字符。
func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 韩文字母
		r >= 0x2E80 && r <= 0xA4CF, // CJK 部首、汉字、假名
		r >= 0xAC00 && r <= 0xD7A3, // 韩文音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容汉字
		r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角字符
		r >= 0xFFE0 && r <= 0xFFE6: // 全角符号
		return true
	}
	return false
}
