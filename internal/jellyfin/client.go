// Package jellyfin 与 Jellyfin 服务器通信。
// 所有请求都通过 https 打向官方 REST API，绝不 mock。
// 项目仅用一个管理 API Key；用户侧密码校验通过 AuthenticateByName 走公开接口。
package jellyfin

import (
	"bytes"
	"context"
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
// 密码
// ---------------------------------------------------------------------------

// AuthenticateByName 用户侧自证（公共接口，通行密钥 https）。
// 官方语义：POST /Users/AuthenticateByName {Username, Pw}
// 需要客户端 header（官方客户端规范）：X-Emby-Authorization
// 我们发完后即可丢弃 token，不传后续。
func (c *Client) AuthenticateByName(ctx context.Context, username, pw string) (bool, error) {
	body := map[string]any{
		"Username": username,
		"Pw":       pw,
	}
	u := c.Base + "/Users/AuthenticateByName"
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Emby-Authorization",
		`MediaBrowser Client="mora_bot", Device="mora_bot", DeviceId="mora_bot-001", Version="1.0.0"`)
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("连接 Jellyfin 失败: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest, http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("凭证校验响应异常: %d", resp.StatusCode)
	}
}

// ResetPassword 调官方 API：POST /Users/{id}/Password（body ResetPassword=true）。
// 旧版 /Users/{id}/Password/Reset 在 Jellyfin 10.10+ 已移除（会返回 404），
// 10.11.11 实测：POST /Users/{id}/Password {"ResetPassword":true} 返回 204。
func (c *Client) ResetPassword(ctx context.Context, id string) error {
	body := map[string]any{"ResetPassword": true}
	return c.do(ctx, http.MethodPost, "/Users/"+url.PathEscape(id)+"/Password", nil, body, nil)
}

// AdminSetPassword 管理员直接改密码（先 reset 再 set）。
// 参考 r/jellyfin 与官方文档：不改密码的 reset+set 流程更稳。
func (c *Client) AdminSetPassword(ctx context.Context, id, newPw string) error {
	// 1) 重置为 "无密码"
	if err := c.ResetPassword(ctx, id); err != nil {
		return fmt.Errorf("重置密码失败: %w", err)
	}
	// 2) 用 UpdateUserPassword（管理员身份，无旧密码）
	body := map[string]any{
		"NewPw":         newPw,
		"ResetPassword": false,
	}
	return c.do(ctx, http.MethodPost, "/Users/"+url.PathEscape(id)+"/Password", nil, body, nil)
}
