// Package jellyfin 与 Jellyfin 服务器通信。
// 所有请求都通过 https 打向官方 REST API，绝不 mock。
// 项目仅用一个管理 API Key；用户侧密码校验通过 AuthenticateByName 走公开接口。
package jellyfin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client 管理端 HTTP 客户端。
type Client struct {
	Base   string // 如 https://jf.example.com（无尾 /）
	apiKey string
	hc     *http.Client
}

// New 创建客户端。BaseURL 必须是 https://（或显式 http://localhost），否则返回错误。
func New(baseURL, apiKey string) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("JELLYFIN_URL 为空")
	}
	if !strings.HasPrefix(baseURL, "https://") && !strings.HasPrefix(baseURL, "http://localhost") && !strings.HasPrefix(baseURL, "http://127.0.0.1") {
		return nil, fmt.Errorf("JELLYFIN_URL 必须使用 https://（开发可用 localhost）")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("JELLYFIN_API_KEY 为空")
	}
	return &Client{
		Base:   baseURL,
		apiKey: apiKey,
		hc:     &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// apiError 把 HTTP 非 2xx 转成可 errorf 的错。
func (c *Client) apiError(resp *http.Response, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 160 {
		snippet = snippet[:160] + "..."
	}
	return fmt.Errorf("Jellyfin %s %d: %s", resp.Request.Method+" "+resp.Request.URL.Path, resp.StatusCode, snippet)
}

// do 发起一次管理请求。out 传 nil 表示不解析响应体。
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	u := c.Base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	// Jellyfin 管理端 API Key 用 X-Emby-Token（兼容现代版本）。
	req.Header.Set("X-Emby-Token", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接 Jellyfin 失败: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return c.apiError(resp, b)
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("解析 Jellyfin 响应失败: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 用户管理
// ---------------------------------------------------------------------------

// UserDTO Jellyfin 用户对象的关键子集。
type UserDTO struct {
	ID              string     `json:"Id"`
	Name            string     `json:"Name"`
	Policy          UserPolicy `json:"Policy"`
	PrimaryImageTag string     `json:"PrimaryImageTag,omitempty"`
}

// UserPolicy 官方 UserPolicy DTO（摘取与机器人行为直接相关的字段）。
// 其它字段（DeviceAccess、BlockLiveTv 等）以模板为准原样回传。
type UserPolicy struct {
	IsAdministrator            bool     `json:"IsAdministrator"`
	IsHidden                   bool     `json:"IsHidden"`
	IsDisabled                 bool     `json:"IsDisabled"`
	EnableAllFolders           bool     `json:"EnableAllFolders"`
	EnabledFolders             []string `json:"EnabledFolders,omitempty"`
	EnableMediaPlayback        bool     `json:"EnableMediaPlayback"`
	EnableLiveTvAccess         bool     `json:"EnableLiveTvAccess"`
	MaxActiveSessions          int      `json:"MaxActiveSessions"`
	RemoteClientBitrateLimit   int      `json:"RemoteClientBitrateLimit"`
	AuthenticationProviderID   string   `json:"AuthenticationProviderId,omitempty"`
	PasswordResetProviderID    string   `json:"PasswordResetProviderId,omitempty"`
	InvalidLoginAttemptCount   int      `json:"InvalidLoginAttemptCount,omitempty"`
	LoginAttemptsBeforeLockout int      `json:"LoginAttemptsBeforeLockout,omitempty"`
	MaxParentalRating          int      `json:"MaxParentalRating,omitempty"`
	// 其余字段保留透传（用 AdditionalFields 模式或 json.RawMessage 存储）。
	AdditionalFields map[string]any `json:"-"`
}

// ListUsers 拉取所有用户（含策略）。
func (c *Client) ListUsers(ctx context.Context) ([]UserDTO, error) {
	var out []UserDTO
	if err := c.do(ctx, http.MethodGet, "/Users", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetUser 按 ID 拉单个用户；不存在返回 IsNotFound=true。
func (c *Client) GetUser(ctx context.Context, id string) (*UserDTO, bool, error) {
	var users []UserDTO
	if err := c.do(ctx, http.MethodGet, "/Users", nil, nil, &users); err != nil {
		return nil, false, err
	}
	for _, u := range users {
		if strings.EqualFold(u.ID, id) {
			uu := u
			return &uu, true, nil
		}
	}
	return nil, false, nil
}

// FindUserByName 按用户名找。
func (c *Client) FindUserByName(ctx context.Context, name string) (*UserDTO, bool, error) {
	users, err := c.ListUsers(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, u := range users {
		if strings.EqualFold(u.Name, name) {
			uu := u
			return &uu, true, nil
		}
	}
	return nil, false, nil
}

// UserExists 快速判断用户名是否已被占用（独立于 CreateUser，用于前端即时提示）。
func (c *Client) UserExists(ctx context.Context, name string) (bool, error) {
	_, ok, err := c.FindUserByName(ctx, name)
	return ok, err
}

// CreateUser 调官方 API：POST /Users/New {Name, Password}
// 返回新建用户的 Id。
func (c *Client) CreateUser(ctx context.Context, name, password string) (*UserDTO, error) {
	body := map[string]any{"Name": name}
	if password != "" {
		body["Password"] = password
	}
	// 返回整个 UserDTO
	var out UserDTO
	if err := c.do(ctx, http.MethodPost, "/Users/New", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteUser 调官方 API：DELETE /Users/{id}
func (c *Client) DeleteUser(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/Users/"+url.PathEscape(id), nil, nil, nil)
}

// UpdateUserPolicy 替换用户策略（POST /Users/{id}/Policy）。
func (c *Client) UpdateUserPolicy(ctx context.Context, id string, p UserPolicy) error {
	return c.do(ctx, http.MethodPost, "/Users/"+url.PathEscape(id)+"/Policy", nil, p, nil)
}

// SetUserDisabled 单独翻 IsDisabled 字段。
func (c *Client) SetUserDisabled(ctx context.Context, id string, disabled bool) error {
	u, ok, err := c.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Jellyfin 用户不存在 id=%s", id)
	}
	// 基于当前策略重建，只改 IsDisabled，避免其它字段被默认覆盖。
	p := u.Policy
	p.IsDisabled = disabled
	return c.UpdateUserPolicy(ctx, id, p)
}

// SetUserHidden 单独翻 IsHidden。
func (c *Client) SetUserHidden(ctx context.Context, id string, hidden bool) error {
	u, ok, err := c.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Jellyfin 用户不存在 id=%s", id)
	}
	p := u.Policy
	p.IsHidden = hidden
	return c.UpdateUserPolicy(ctx, id, p)
}

// ---------------------------------------------------------------------------
// 会话 / 设备
// ---------------------------------------------------------------------------

// UserSession 用户的一个在线会话（用于「我的登录设备」展示与清理）。
type UserSession struct {
	DeviceID     string    `json:"DeviceId"`
	DeviceName   string    `json:"DeviceName"`
	Client       string    `json:"Client"`
	UserID       string    `json:"UserId"`
	UserName     string    `json:"UserName"`
	LastActivity time.Time `json:"LastActivityDate"`
}

// ListUserSessions 列出某用户当前占用的会话。
// 这些会话计入 Policy.MaxActiveSessions；超限后即使密码正确也会被拒登（403）。
func (c *Client) ListUserSessions(ctx context.Context, userID string) ([]UserSession, error) {
	var all []UserSession
	if err := c.do(ctx, http.MethodGet, "/Sessions", nil, nil, &all); err != nil {
		return nil, err
	}
	out := make([]UserSession, 0, len(all))
	for _, s := range all {
		if strings.EqualFold(s.UserID, userID) {
			out = append(out, s)
		}
	}
	return out, nil
}

// DeleteDevice 删除一个设备及其会话（DELETE /Devices?id=），释放在线名额。
func (c *Client) DeleteDevice(ctx context.Context, deviceID string) error {
	if deviceID == "" {
		return nil
	}
	return c.do(ctx, http.MethodDelete, "/Devices", url.Values{"id": {deviceID}}, nil, nil)
}

// LogoutAllDevices 踢掉该用户的全部在线会话，返回成功清理的数量。
// 用于用户自助「清理登录设备」——这是从 MaxActiveSessions 上限中恢复的唯一手段。
func (c *Client) LogoutAllDevices(ctx context.Context, userID string) (int, error) {
	sessions, err := c.ListUserSessions(ctx, userID)
	if err != nil {
		return 0, err
	}
	n := 0
	var lastErr error
	for _, s := range sessions {
		if err := c.DeleteDevice(ctx, s.DeviceID); err != nil {
			lastErr = err
			continue
		}
		n++
	}
	if n == 0 && lastErr != nil {
		return 0, lastErr
	}
	return n, nil
}

// MaxActiveSessions 读取用户策略里的同时在线上限（0 表示不限）。
func (c *Client) MaxActiveSessions(ctx context.Context, userID string) (int, error) {
	u, ok, err := c.GetUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("Jellyfin 用户不存在 id=%s", userID)
	}
	return u.Policy.MaxActiveSessions, nil
}

// ---------------------------------------------------------------------------
// 密码
// ---------------------------------------------------------------------------

// AuthResult 用户侧凭证校验结果。
// 必须把「密码错」与「被拦截」分开：Jellyfin 在账号被禁用、或同时在线会话数
// 达到 Policy.MaxActiveSessions 时，对**正确密码**同样返回 403，
// 若一律当成"密码错误"会让用户以为自己密码丢了（本项目历史 bug）。
type AuthResult int

const (
	// AuthOK 凭证正确且允许登录。
	AuthOK AuthResult = iota
	// AuthBadCredentials 用户名或密码确实不对（HTTP 401）。
	AuthBadCredentials
	// AuthBlocked 凭证可能是对的，但服务器拒绝建立会话：
	// 账号被禁用，或同时在线设备数已达上限（HTTP 403）。
	AuthBlocked
)

// authDeviceID 按用户名派生一个稳定的探测 DeviceId。
// 两条约束缺一不可：
//   - 不能所有用户共用一个固定 DeviceId（历史写法 "mora_bot-001"），否则不同用户的
//     校验会互相顶掉会话，并在 Jellyfin 设备列表里留下一条长期占用名额的记录；
//   - 也不必每次随机，否则每校验一次就多一条设备记录。按用户名派生 + 用后即删
//     （见 releaseProbeSession），同一用户反复校验也只会复用同一条，自限不堆积。
func authDeviceID(username string) string {
	sum := sha256.Sum256([]byte("mora_bot-auth-probe|" + username))
	return "mora_bot-" + hex.EncodeToString(sum[:8])
}

// AuthenticateByName 用户侧自证（公共接口，走 https）。
// 官方语义：POST /Users/AuthenticateByName {Username, Pw}
// 需要客户端 header（官方客户端规范）：X-Emby-Authorization
//
// 重要：这个接口会真的建立一个会话并占用用户的 MaxActiveSessions 名额。
// 我们只是借它验证密码，所以拿到 token 后必须立刻登出，否则会长期占用
// 用户的在线设备名额，导致用户后续用正确密码也登不上（403）。
func (c *Client) AuthenticateByName(ctx context.Context, username, pw string) (AuthResult, error) {
	body := map[string]any{
		"Username": username,
		"Pw":       pw,
	}
	u := c.Base + "/Users/AuthenticateByName"
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return AuthBadCredentials, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Emby-Authorization", fmt.Sprintf(
		`MediaBrowser Client="mora_bot", Device="mora_bot", DeviceId=%q, Version="1.0.0"`,
		authDeviceID(username)))
	resp, err := c.hc.Do(req)
	if err != nil {
		return AuthBadCredentials, fmt.Errorf("连接 Jellyfin 失败: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		// 立刻归还会话名额，并清掉刚建出来的探测设备。
		var out struct {
			AccessToken string `json:"AccessToken"`
			SessionInfo struct {
				DeviceID string `json:"DeviceId"`
			} `json:"SessionInfo"`
		}
		if json.Unmarshal(rb, &out) == nil {
			c.releaseProbeSession(ctx, out.AccessToken, out.SessionInfo.DeviceID)
		} else {
			// 即使响应解析失败，也按已知 DeviceId 尽力清理探测设备。
			c.releaseProbeSession(ctx, "", authDeviceID(username))
		}
		return AuthOK, nil
	case http.StatusUnauthorized, http.StatusBadRequest, http.StatusNotFound:
		return AuthBadCredentials, nil
	case http.StatusForbidden:
		return AuthBlocked, nil
	default:
		return AuthBadCredentials, fmt.Errorf("凭证校验响应异常: %d", resp.StatusCode)
	}
}

// releaseProbeSession 撤销一次密码校验所建立的会话与设备记录（尽力而为，不阻断主流程）。
// 使用独立超时上下文，避免原 handler context 已取消导致探测会话残留。
func (c *Client) releaseProbeSession(ctx context.Context, token, deviceID string) {
	releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if token != "" {
		req, err := http.NewRequestWithContext(releaseCtx, http.MethodPost, c.Base+"/Sessions/Logout", nil)
		if err == nil {
			req.Header.Set("X-Emby-Token", token)
			if resp, err := c.hc.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				resp.Body.Close()
			}
		}
	}
	if deviceID != "" {
		_ = c.DeleteDevice(releaseCtx, deviceID)
	}
	_ = ctx // 保留原 ctx 参数签名兼容；释放动作不依赖调用方生命周期。
}

// ResetPassword 调官方 API：POST /Users/{id}/Password（body ResetPassword=true）。
// 旧版 /Users/{id}/Password/Reset 在 Jellyfin 10.10+ 已移除（会返回 404），
// 10.11.11 实测：POST /Users/{id}/Password {"ResetPassword":true} 返回 204。
func (c *Client) ResetPassword(ctx context.Context, id string) error {
	body := map[string]any{"ResetPassword": true}
	return c.do(ctx, http.MethodPost, "/Users/"+url.PathEscape(id)+"/Password", nil, body, nil)
}

// AdminSetPassword 管理员直接改密码（单请求，原子设置新密码）。
// 实测 Jellyfin 10.11.11 的 POST /Users/{id}/Password 仅需 NewPw 即可完成设置；
// 不再先 Reset 再 Set，避免两步之间失败导致账号短暂变成“无密码”状态。
func (c *Client) AdminSetPassword(ctx context.Context, id, newPw string) error {
	body := map[string]any{
		"NewPw":         newPw,
		"ResetPassword": false,
	}
	return c.do(ctx, http.MethodPost, "/Users/"+url.PathEscape(id)+"/Password", nil, body, nil)
}
