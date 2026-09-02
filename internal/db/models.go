// Package db 提供 mora_bot 的数据模型与 SQLite 数据访问。
// 设计原则（沿用参考项目）：
//  1. 所有资产变动必须在事务内完成，且不能扣成负数；
//  2. 唯一性必须靠数据库唯一索引兜底，不依赖“先查再写”；
//  3. 高危操作写 audit_logs；积分变动写 point_transactions。
package db

import (
	"time"

	"gorm.io/gorm"
)

// ---------------- 用户 ----------------

const (
	UserStatusActive   = "active"   // 正常
	UserStatusInactive = "inactive" // 管理员在本地停用（与 Jellyfin 侧禁用区分开）
	UserStatusDisabled = "disabled" // 与 UserStatusInactive 同义的别名（回调/管理面板用）
	UserStatusDeleted  = "deleted"  // 用户自助注销（档案保留，账号已删/已解绑）

	BindTypeRegistered = "registered" // 通过邀请码新建的 Jellyfin 账号
	BindTypeExisting   = "existing"   // 绑定已有 Jellyfin 账号
)

// User 本地用户档案。主键即 Telegram ID。
type User struct {
	TelegramID         int64  `gorm:"primaryKey"`
	TgUsername         string `gorm:"size:64"`
	FirstName          string `gorm:"size:64"`
	LastName           string `gorm:"size:64"`
	GuoGuo             int    `gorm:"default:0"` // 果果币余额
	JellyfinUserID     string `gorm:"size:64;index"`
	JellyfinUsername   string `gorm:"size:128"`
	BindType           string `gorm:"size:16"` // registered / existing
	ExpireAt           *time.Time
	IsPermanent        bool   `gorm:"default:false"` // 白名单/永久不过期
	IsSuspended        bool   `gorm:"default:false"` // 封禁（含 Jellyfin 侧同步禁用）
	SuspendReason      string `gorm:"size:256"`
	Status             string `gorm:"size:16;default:active;index"`
	LastSignAt         *time.Time
	SignStreak         int    `gorm:"default:0"`
	SecurityCodeHash   string `gorm:"size:64"`   // 安全码 HMAC（改密/解绑校验），空=未设置
	PasswordResetCount int    `gorm:"default:0"` // 已通过安全码重置 Jellyfin 密码的次数（上限 2）
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// DisplayName 用于消息展示的友好名称。
func (u *User) DisplayName() string {
	if u.TgUsername != "" {
		return "@" + u.TgUsername
	}
	name := u.FirstName
	if u.LastName != "" {
		name += " " + u.LastName
	}
	if name != "" {
		return name
	}
	return "用户"
}

// IsExpired 判断是否已过有效期（永久账号恒不过期）。
func (u *User) IsExpired(now time.Time) bool {
	if u.IsPermanent {
		return false
	}
	return u.ExpireAt != nil && u.ExpireAt.Before(now)
}

// ---------------- 积分（果果币）流水 ----------------

const (
	TxSignInDaily      = "sign_in_daily"      // 每日签到
	TxExchangeInvite   = "exchange_invite"    // 兑换邀请码
	TxExchangeRenewal  = "exchange_renewal"   // 兑换续期码
	TxAdminAdjust      = "admin_adjust"       // 管理员调账
	TxDramaRewardBonus = "drama_accept_bonus" // 求剧被采纳奖励（预留，默认关闭）
)

// PointTransaction 果果币流水：每一笔余额变动必须有一条。
type PointTransaction struct {
	ID           uint      `gorm:"primaryKey"`
	UserID       int64     `gorm:"index"`
	Change       int       // 正数进账，负数支出
	BalanceAfter int       // 变动后余额（便于审计对账）
	Type         string    `gorm:"size:32;index"`
	Remark       string    `gorm:"size:256"`
	OperatorID   int64     `gorm:"default:0"` // 管理员操作时的 TG ID，用户自助为 0
	CreatedAt    time.Time `gorm:"index"`
}

// ---------------- 签到 ----------------

// SignInRecord 每日签到记录。(UserID, SignDay) 唯一，杜绝并发双签。
type SignInRecord struct {
	ID        uint   `gorm:"primaryKey"`
	UserID    int64  `gorm:"uniqueIndex:idx_sign_user_day"`
	SignDay   string `gorm:"size:10;uniqueIndex:idx_sign_user_day"` // 本地日期 YYYY-MM-DD
	Reward    int    // 本次到手果果币（含连签加成）
	Streak    int    // 签到后的连续天数
	CreatedAt time.Time
}

// ---------------- 卡密 ----------------

const (
	CodeStatusUnused  = "unused"
	CodeStatusUsed    = "used"
	CodeStatusRevoked = "revoked"
)

// CodeBatch 一批生成的卡密（邀请码/续期码共用）。
type CodeBatch struct {
	ID         uint   `gorm:"primaryKey"`
	CodeType   string `gorm:"size:16;index"` // invite / renewal
	Count      int
	Days       int    // 续期码天数（邀请码为 0）
	Note       string `gorm:"size:200"`
	OperatorID int64
	CreatedAt  time.Time
}

// InviteCode 邀请码。明文不落库：仅存 HMAC 散列 + 加密副本。
type InviteCode struct {
	ID        uint   `gorm:"primaryKey"`
	CodeHash  string `gorm:"size:64;uniqueIndex"` // HMAC-SHA256(code, pepper)
	CodeEnc   string `gorm:"size:300"`            // XChaCha20-Poly1305 加密明文（管理端取回用）
	BatchID   uint   `gorm:"index"`
	Status    string `gorm:"size:16;index"`
	UsedBy    *int64
	UsedAt    *time.Time
	Remark    string `gorm:"size:200"`
	Source    string `gorm:"size:16"` // ""=管理员生成；exchange=积分兑换（配额统计用）
	CreatedBy int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RenewalCode 续期码。结构同邀请码，多一个天数。
type RenewalCode struct {
	ID        uint   `gorm:"primaryKey"`
	CodeHash  string `gorm:"size:64;uniqueIndex"`
	CodeEnc   string `gorm:"size:300"`
	Days      int    `gorm:"default:30"`
	BatchID   uint   `gorm:"index"`
	Status    string `gorm:"size:16;index"`
	UsedBy    *int64
	UsedAt    *time.Time
	Remark    string `gorm:"size:200"`
	CreatedBy int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RenewalRecord 续期审计：每次核销续期码写一条，含续期前后到期时间。
type RenewalRecord struct {
	ID         uint  `gorm:"primaryKey"`
	UserID     int64 `gorm:"index"`
	CodeID     uint
	CodeHash   string `gorm:"size:64"`
	Days       int
	PrevExpire *time.Time
	NewExpire  *time.Time
	CreatedAt  time.Time `gorm:"index"`
}

// ---------------- 追剧中心 ----------------

const (
	DramaStatusPending   = "pending"   // 待处理
	DramaStatusClaimed   = "claimed"   // 已被管理员认领
	DramaStatusNeedInfo  = "need_info" // 需用户补充信息
	DramaStatusCompleted = "completed" // 已完成
	DramaStatusRejected  = "rejected"  // 已驳回
	DramaStatusCancelled = "cancelled" // 用户取消
)

// DramaRequest 求剧工单（用户提交红果短剧分享链接）。
type DramaRequest struct {
	ID            uint   `gorm:"primaryKey"`
	UserID        int64  `gorm:"index"`
	TgUsername    string `gorm:"size:64"`
	Title         string `gorm:"size:200"` // 从分享文本提取的剧名
	Link          string `gorm:"size:512"` // 红果短剧分享链接
	Note          string `gorm:"size:320"`
	Status        string `gorm:"size:16;index"`
	ClaimedBy     *int64
	ClaimedByName string    `gorm:"size:64"`
	RejectReason  string    `gorm:"size:256"`
	ResolveNote   string    `gorm:"size:256"`
	CreatedAt     time.Time `gorm:"index"`
	UpdatedAt     time.Time
	ResolvedAt    *time.Time
	DeletedAt     gorm.DeletedAt `gorm:"index"`
}

// DramaRequestLog 工单流转日志，每次状态变化写一条。
type DramaRequestLog struct {
	ID         uint   `gorm:"primaryKey"`
	RequestID  uint   `gorm:"index"`
	Action     string `gorm:"size:24"` // create/claim/need_info/complete/reject/cancel
	Detail     string `gorm:"size:320"`
	OperatorID int64  // 0=用户本人
	CreatedAt  time.Time
}

// ---------------- 系统 ----------------

// AuditLog 管理员高危操作审计。
type AuditLog struct {
	ID         uint      `gorm:"primaryKey"`
	ActorID    int64     `gorm:"index"`
	Action     string    `gorm:"size:32;index"`
	TargetType string    `gorm:"size:32"`
	TargetID   string    `gorm:"size:64"`
	Detail     string    `gorm:"size:512"`
	CreatedAt  time.Time `gorm:"index"`
}

// SystemConfig 运行时可在库中调整的开关/价格（init 时用 env 默认值播种，不覆盖已有值）。
type SystemConfig struct {
	Key       string `gorm:"primaryKey;size:64"`
	Value     string `gorm:"size:256"`
	UpdatedAt time.Time
}

// JellyfinLine 可用 Jellyfin 服务器线路（管理员维护，用户可查询）。
// 线路名可留空则展示 URL；URL 唯一。
type JellyfinLine struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"size:64"`
	URL       string `gorm:"size:256;uniqueIndex"`
	Note      string `gorm:"size:128"`
	Order     int    `gorm:"default:0"`
	CreatedAt time.Time
}
