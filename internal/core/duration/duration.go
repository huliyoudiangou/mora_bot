// Package coreduration 时间/人类可读的时长工具，避免散落各处自己实现。
package coreduration

import (
	"fmt"
	"time"
)

// Humanize 把 time.Duration 变成 "3 天 2 小时 10 分钟"。
func Humanize(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m > 0 {
			return fmt.Sprintf("%d 小时 %d 分钟", h, m)
		}
		return fmt.Sprintf("%d 小时", h)
	}
	days := int(d.Hours()) / 24
	remain := time.Duration(int64(d) - int64(days)*int64(24*time.Hour))
	h := int(remain.Hours())
	if h > 0 {
		return fmt.Sprintf("%d 天 %d 小时", days, h)
	}
	return fmt.Sprintf("%d 天", days)
}
