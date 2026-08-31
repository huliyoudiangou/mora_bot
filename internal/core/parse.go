// Package corparse 输入解析工具。
package corparse

import (
	"strings"
	"unicode/utf8"
)

// LeadingInt 从字符串开头往后解析整数（忽略空格）。
// 例如 "123 天" -> 123；"foo 123" -> 0。
func LeadingInt(s string) int {
	s = strings.TrimSpace(s)
	v := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		v = v*10 + int(r-'0')
	}
	return v
}

// CutKeyword 解析 "关键词 + 剩余内容"。
// /invite ABC-123 -> ("ABC-123", true)
// /start -> ("", true) – 关键词完整但没参数
func CutKeyword(s, kw string) (string, bool) {
	if !strings.HasPrefix(s, kw) {
		return s, false
	}
	rest := strings.TrimSpace(s[len(kw):])
	return rest, true
}

// LengthRunes 按 rune 计数（中文 1 个字符 = 1，不上,上当的多字节符激增）。
func LengthRunes(s string) int { return utf8.RuneCountInString(s) }
