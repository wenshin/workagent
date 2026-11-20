package tool

import (
	"strings"
	"testing"
)

// web_search 是唯一一条「不过审批、但内容会到第三方」的通道。
// 这组用例盯两件事：正常检索词必须零打扰；凭证与超长内容必须拦。
func TestCheckSearchQueryAcceptsNormalQueries(t *testing.T) {
	normal := []string{
		"Go 泛型 用法",
		"2024年第三季度华东区销售额同比下降的主要原因分析报告",
		"task-manager-configuration-file", // sk- 前缀误报的典型
		"PostgreSQL 主从延迟排查",
		"AKIA 是什么意思", // 只有前缀，没有 16 位密钥体
		"JWT 结构和验证方式",
		strings.Repeat("a", webSearchMaxQuery), // 正好到上限
	}
	for _, q := range normal {
		if err := checkSearchQuery(q); err != nil {
			t.Errorf("正常检索词被拦：%q → %v", q, err)
		}
	}
}

func TestCheckSearchQueryRejectsTooLong(t *testing.T) {
	q := strings.Repeat("语", webSearchMaxQuery+1)
	err := checkSearchQuery(q)
	if err == nil || !strings.Contains(err.Error(), "过长") {
		t.Fatalf("超长 query 应被拒绝，得到 %v", err)
	}
}

func TestCheckSearchQueryRejectsSecrets(t *testing.T) {
	cases := map[string]string{
		"sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789":              "API key",
		"AKIAIOSFODNN7EXAMPLE":                                      "AWS Access Key",
		"ghp_1234567890abcdefghijklmnopqrstuvwxyz":                  "GitHub token",
		"xoxb-1234567890-abcdefghij":                                "Slack token",
		"-----BEGIN RSA PRIVATE KEY-----":                           "私钥",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefghijklmnop": "JWT",
	}
	for q, want := range cases {
		err := checkSearchQuery(q)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q 应命中 %s，得到 %v", q, want, err)
		}
	}
}
