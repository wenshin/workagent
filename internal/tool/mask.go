package tool

import "regexp"

// 脱敏规则（§8.2 数据安全）：工具结果回传给模型前先抹掉凭证与个人敏感信息，
// 避免模型把它写进回答或上下文。
var maskRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// 手机号
	{regexp.MustCompile(`\b1[3-9]\d{9}\b`), "1**********"},
	// 身份证
	{regexp.MustCompile(`\b\d{17}[\dXx]\b`), "******************"},
	// 邮箱
	{regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`), "***@***"},
	// 银行卡
	{regexp.MustCompile(`\b\d{16,19}\b`), "**** **** **** ****"},
	// API Key / Token
	{regexp.MustCompile(`\b(sk|pk|ghp|gho|xoxb)-[A-Za-z0-9_\-]{16,}\b`), "$1-***"},
	{regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._\-]{16,}`), "$1 ***"},
}

// Mask 对文本做脱敏。
func Mask(s string) string {
	if s == "" {
		return s
	}
	for _, rule := range maskRules {
		s = rule.re.ReplaceAllString(s, rule.repl)
	}
	return s
}
