package codes

import "fmt"

// ErrCode 轻量业务错误类型，避免每次 fmt.Errorf。
// 必须有三个字段：MachineCode（机器用）、Msg（用户可见）、Internal（可选）。
type ErrCode struct {
	MachineCode string
	Msg         string
	Internal    error
}

func (e ErrCode) Error() string {
	if e.Internal != nil {
		return fmt.Sprintf("%s: %s (%v)", e.MachineCode, e.Msg, e.Internal)
	}
	return fmt.Sprintf("%s: %s", e.MachineCode, e.Msg)
}

func newErr(machine, msg string) ErrCode {
	return ErrCode{MachineCode: machine, Msg: msg}
}

// 静态错误实例（全局区，避免重复分配）。
var (
	ErrInvalidPassword    = newErr("INVALID_PASSWORD", "密码错误")
	ErrUserNameTaken      = newErr("USERNAME_TAKEN", "用户名已被占用")
	ErrCodeNotFound       = newErr("CODE_NOT_FOUND", "卡密不存在")
	ErrCodeUsed           = newErr("CODE_USED", "该卡密已使用")
	ErrCodeRevoked        = newErr("CODE_REVOKED", "卡密已作废")
	ErrInsufficient       = newErr("INSUFFICIENT_POINTS", "果果币不足")
	ErrBoundElsewhere     = newErr("BOUND_ELSEWHERE", "该 Jellyfin 账号已绑到别的 TG")
	ErrSelfOperation      = newErr("SELF_OPERATION", "不能对自己执行此操作")
	errBadFormat          = newErr("BAD_FORMAT", "格式错误")
	ErrNoPendingRequest   = newErr("NO_PENDING_REQUEST", "没有待处理工单")
	ErrDramaLimitExceeded = newErr("DRAMA_LIMIT_EXCEEDED", "今日求剧次数已上限")
)
