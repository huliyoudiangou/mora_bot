package bot

import (
	"strconv"
	"strings"
)

// escapeHTML HTML 转义（用于 sendHTML 消息）。
func escapeHTML(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return replacer.Replace(s)
}

// resolveUserID 从 args 解析第 idx 个参数为 int64；失败返回 0。
func resolveUserID(args []string, idx int) int64 {
	if idx < 0 || idx >= len(args) {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(args[idx]), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// itoa 整数转字符串。
func itoa(v int) string {
	return strconv.Itoa(v)
}