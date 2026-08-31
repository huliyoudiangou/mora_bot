package jellyfin

import (
	"context"
	"fmt"
	"net/url"
)

// rawUserDTO 全字段透传版（只为克隆策略，避免结构体裁剪掉 Jellyfin 未来新增字段）。
type rawUserDTO struct {
	ID            string         `json:"Id"`
	Name          string         `json:"Name"`
	Policy        map[string]any `json:"Policy"`
	Configuration map[string]any `json:"Configuration"`
}

func (c *Client) getRawUser(ctx context.Context, id string) (*rawUserDTO, bool, error) {
	var users []rawUserDTO
	if err := c.do(ctx, "GET", "/Users", nil, nil, &users); err != nil {
		return nil, false, err
	}
	for _, u := range users {
		if u.ID == id || u.Name == id {
			uu := u
			return &uu, true, nil
		}
	}
	return nil, false, nil
}

// ClonePolicyFromTemplate 1:1 复刻模板用户的设置权限到目标用户：
//   - Policy（权限：目录访问、播放、直播、并发等）整体原样套用，
//   - Configuration（个人设置：播放/字幕/显示偏好等）整体原样套用，
//   - 仅强制清空管理权限、禁用、隐藏标记——机器人注册的账号永远是“普通可用”。
func (c *Client) ClonePolicyFromTemplate(ctx context.Context, templateUserID, targetUserID string) error {
	if templateUserID == "" {
		return nil // 未配置模板：用 Jellyfin 默认策略
	}
	tpl, ok, err := c.getRawUser(ctx, templateUserID)
	if err != nil {
		return fmt.Errorf("读取模板用户策略失败: %w", err)
	}
	if !ok || tpl.Policy == nil {
		return fmt.Errorf("模板用户不存在或 Policy 为空: %s", templateUserID)
	}
	escaped := url.PathEscape(targetUserID)

	// 1) 套用 Policy（权限）
	p := tpl.Policy
	p["IsAdministrator"] = false
	p["IsDisabled"] = false
	p["IsHidden"] = false
	if err := c.do(ctx, "POST", "/Users/"+escaped+"/Policy", nil, p, nil); err != nil {
		return fmt.Errorf("套用模板 Policy 失败: %w", err)
	}

	// 2) 套用 Configuration（其它全部用户设置）
	if tpl.Configuration == nil {
		return nil
	}
	if err := c.do(ctx, "POST", "/Users/"+escaped+"/Configuration", nil, tpl.Configuration, nil); err != nil {
		return fmt.Errorf("套用模板 Configuration 失败: %w", err)
	}
	return nil
}
