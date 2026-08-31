package bot

import "strings"

// DomainKind 描述回调数据所属域。
type DomainKind string

const (
	DKMenu    DomainKind = "menu"
	DKProfile DomainKind = "profile"
	DKSign    DomainKind = "sign"
	DKShop    DomainKind = "shop"
	DKBind    DomainKind = "bind"
	DKAdmin   DomainKind = "admin"
	DKDrama   DomainKind = "drama"
	DKAccount DomainKind = "account"
)

// BuildCallbackData 拼装 "domain:action[:args...]"。
func BuildCallbackData(domain DomainKind, action string, args ...string) string {
	parts := append([]string{string(domain), action}, args...)
	return strings.Join(parts, ":")
}

// ParseCallbackData 逆向解析。
func ParseCallbackData(data string) (domain, action string, args []string) {
	parts := strings.Split(data, ":")
	if len(parts) < 2 {
		return "", "", nil
	}
	return parts[0], parts[1], parts[2:]
}
