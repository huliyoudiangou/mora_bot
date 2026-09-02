// Package codes 生成、校验、加密卡密（邀请码 / 续期码）。
// 设计要点：
//   - 明文只在生成瞬间出现在内存里；
//   - 库中只有 HMAC 散列值与 XChaCha20-Poly1305 加密副本（找回给管理员/用户使用）。
package codes

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// CodeKindInvite / CodeKindRenewal 邀请码 vs 续期卡。
	CodeKindInvite  = "invite"
	CodeKindRenewal = "renewal"
)

const (
	// CodeSecretVersion 加密信封版本号，便于未来换算法。
	CodeSecretVersion = "v1"

	// CodeBodyLen 卡密正文长度（前缀 `mora-` / `R<天数>-` 之后的随机字符数）。
	CodeBodyLen = 20
)

var (
	baseEnc = base64.RawURLEncoding
)

// CodeSecret 单个卡密的密文+哈希。永远不要把 CodePlain 打到日志。
type CodeSecret struct {
	Plain     string `json:"p,omitempty"` // 生成时暂时存在，处理完后立即置空
	Hash      string `json:"h"`           // hex(HMAC-SHA256)
	Enc       string `json:"e"`           // base64url(nonce|ciphertext)
	Kind      string `json:"k"`
	BatchID   uint   `json:"b"`
	Days      int    `json:"d"`
	Remark    string `json:"r"`
	ExpiresAt int64  `json:"ea"`
}

// GenerateCodeBatchInMemory 生成一批留库的卡密。明文只在本函数返回期间存在。
func GenerateCodeBatchInMemory(kind string, count int, pepper string, renewDays int, remark string) ([]CodeSecret, error) {
	if count <= 0 || count > 200 {
		return nil, fmt.Errorf("批量大小必须在 1-200")
	}
	if strings.TrimSpace(pepper) == "" {
		return nil, fmt.Errorf("pepper 为空")
	}
	if kind != CodeKindInvite && kind != CodeKindRenewal {
		return nil, fmt.Errorf("kind 不合法")
	}
	if kind == CodeKindRenewal && renewDays <= 0 {
		return nil, fmt.Errorf("renewDays 必须大于 0")
	}
	out := make([]CodeSecret, count)
	for i := 0; i < count; i++ {
		prefix := "mora"
		if kind == CodeKindRenewal {
			prefix = "R" + strconv.Itoa(renewDays)
		}
		plain, err := generateRandomCodeString(prefix)
		if err != nil {
			return nil, err
		}
		enc, hash, err := wrapSecret(plain, pepper)
		if err != nil {
			return nil, err
		}
		out[i] = CodeSecret{
			Plain:  plain,
			Hash:   hash,
			Enc:    enc,
			Kind:   kind,
			Days:   renewDays,
			Remark: strings.TrimSpace(remark),
		}
	}
	return out, nil
}

// generateRandomCodeString 生成 `<prefix>-` + CodeBodyLen 位随机字符的卡密。
// 例如邀请码：mora-A1B2C3...；续期码 30 天：R30-A1B2C3...
// 字符集排除易混淆的 0/O/1/I/L。
func generateRandomCodeString(prefix string) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, CodeBodyLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random read 失败: %w", err)
	}
	var sb strings.Builder
	sb.WriteString(prefix)
	sb.WriteByte('-')
	for i := 0; i < CodeBodyLen; i++ {
		sb.WriteByte(alphabet[int(b[i])%len(alphabet)])
	}
	return sb.String(), nil // 例如 mora-P8M2X7CQKW9X2B7D4F6H
}

// wrapSecret 把明文 -> 加密 + HMAC。keystream 派生：hkdf(pepper)[0:32]。
func wrapSecret(plain, pepper string) (enc string, hash string, err error) {
	rk, err := deriveKey(pepper)
	if err != nil {
		return "", "", err
	}
	aead, err := chacha20poly1305.NewX(rk)
	if err != nil {
		return "", "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	ciphertext := aead.Seal(nonce, nonce, []byte(plain), nil)
	enc = baseEnc.EncodeToString(ciphertext)
	// 哈希统一用大写形式，保证用户输入小写 mora- 前缀也能命中。
	m := hmac.New(sha256.New, rk)
	_, _ = m.Write([]byte(CodeSecretVersion))
	_, _ = m.Write([]byte(strings.ToUpper(plain)))
	hash = fmt.Sprintf("%x", m.Sum(nil))
	return enc, hash, nil
}

// UnwrapSecret 用 pepper 还原明文（示例：把续期码还给用户、把批量结果交给管理员）。
func UnwrapSecret(enc, pepper string) (string, error) {
	rk, err := deriveKey(pepper)
	if err != nil {
		return "", err
	}
	aead, err := chacha20poly1305.NewX(rk)
	if err != nil {
		return "", err
	}
	raw, err := baseEnc.DecodeString(enc)
	if err != nil || len(raw) < aead.NonceSize() {
		return "", fmt.Errorf("密文格式错误")
	}
	nonce, ct := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥不匹配?）")
	}
	return string(plain), nil
}

// HashCode 直接对一段明文算 HMAC（用于核销时查库）。
// 哈希统一用大写形式，与生成端 wrapSecret 一致（用户输入小写 mora- 前缀也能命中）。
func HashCode(plain, pepper string) (string, error) {
	if strings.TrimSpace(plain) == "" {
		return "", fmt.Errorf("卡密为空")
	}
	rk, err := deriveKey(pepper)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, rk)
	_, _ = m.Write([]byte(CodeSecretVersion))
	_, _ = m.Write([]byte(strings.ToUpper(plain)))
	return fmt.Sprintf("%x", m.Sum(nil)), nil
}

// 安全码（用户账号保护）：长度 4-20，区分大小写，可含字母数字与常见符号。
const (
	SecurityCodeMinLen = 4
	SecurityCodeMaxLen = 20
)

// ValidateSecurityCode 校验安全码格式，返回清洗后的文本。
func ValidateSecurityCode(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) < SecurityCodeMinLen || len(s) > SecurityCodeMaxLen {
		return "", fmt.Errorf("安全码长度需在 %d-%d 位之间", SecurityCodeMinLen, SecurityCodeMaxLen)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == ' ':
		case r == '_' || r == '-' || r == '.':
		default:
			return "", fmt.Errorf("安全码含非法字符")
		}
	}
	return s, nil
}

// HashSecurityCode 安全码 HMAC 哈希（区分大小写，供改密/解绑校验）。
func HashSecurityCode(code, pepper string) (string, error) {
	if strings.TrimSpace(code) == "" {
		return "", fmt.Errorf("安全码为空")
	}
	rk, err := deriveKey(pepper)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, rk)
	_, _ = m.Write([]byte("security-code-v1"))
	_, _ = m.Write([]byte(code))
	return fmt.Sprintf("%x", m.Sum(nil)), nil
}

// ValidateCodeFormat 对输入做快速清洗（大小写、空格、脱字符）。
func ValidateCodeFormat(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("码为空")
	}
	s = strings.ToUpper(s)
	// 只允许字母数字和连字符。
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return "", fmt.Errorf("卡密含非法字符")
		}
	}
	if len(s) < 6 || len(s) > 64 {
		return "", fmt.Errorf("卡密长度异常")
	}
	return s, nil
}

// deriveKey 从 pepper 派生卡密加密/HMAC 密钥。
// 固定使用 HKDF-SHA256：密钥必须与历史数据保持一致，
// 任何“回退到其他 KDF”的分支都会让已落库的哈希/密文失配，因此不设回退。
func deriveKey(pepper string) ([]byte, error) {
	salt := []byte("mora_bot-pepper-v1")
	hk := hkdf.New(sha256.New, []byte(pepper), salt, nil)
	out := make([]byte, 32)
	if _, err := io.ReadFull(hk, out); err != nil {
		return nil, fmt.Errorf("派生密钥失败: %w", err)
	}
	return out, nil
}
