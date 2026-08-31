package bot

import (
	"context"
	"strings"

	tgbotapi "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Router 消息/回调总路由。
type Router struct {
	deps *HandlerDeps
}

// NewRouter 创建路由器。
func NewRouter(deps *HandlerDeps) *Router {
	return &Router{deps: deps}
}

// RegisterTG 把本路由接到 go-telegram/bot 的 update 流（全量兜底 handler）。
func (r *Router) RegisterTG(tg *tgbotapi.Bot) {
	tg.RegisterHandlerMatchFunc(
		func(*models.Update) bool { return true },
		func(ctx context.Context, _ *tgbotapi.Bot, upd *models.Update) {
			r.HandleUpdate(ctx, upd)
		},
	)
}

// HandleUpdate 主入口。
func (r *Router) HandleUpdate(ctx context.Context, upd *models.Update) {
	if upd == nil {
		return
	}
	switch {
	case upd.Message != nil:
		r.handleMessage(ctx, upd.Message)
	case upd.CallbackQuery != nil:
		r.handleCallback(ctx, upd.CallbackQuery)
	}
}

// handleMessage 文本/命令消息。
func (r *Router) handleMessage(ctx context.Context, m *models.Message) {
	if m == nil || m.From == nil || m.Chat.ID == 0 {
		return
	}
	from := TGUser{
		ID:        m.From.ID,
		Username:  m.From.Username,
		FirstName: m.From.FirstName,
		LastName:  m.From.LastName,
	}
	msg := &Message{
		From:      from,
		ChatID:    m.Chat.ID,
		Text:      m.Text,
		MessageID: m.ID,
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}
	if strings.HasPrefix(text, "/") {
		cmd, args := splitCmd(text)
		r.dispatchCommand(ctx, cmd, args, msg)
		return
	}
	// 非命令：若在会话中，推进下一步；否则显示主面板（纯按钮交互优先）。
	if r.continueSession(ctx, msg) {
		return
	}
	u, err := getLocal(ctx, r.deps, msg.From)
	if err != nil {
		sendText(ctx, r.deps, msg.ChatID, "系统繁忙，请稍后再试。")
		return
	}
	text, rows := mainPanel(r.deps, u)
	sendPanel(ctx, r.deps, msg.ChatID, 0, text, rows)
}

// handleCallback inline 回调。
func (r *Router) handleCallback(ctx context.Context, cq *models.CallbackQuery) {
	if cq == nil || cq.From.ID == 0 {
		return
	}
	from := TGUser{
		ID:        cq.From.ID,
		Username:  cq.From.Username,
		FirstName: cq.From.FirstName,
		LastName:  cq.From.LastName,
	}
	var chatID int64
	var msgID int
	if cq.Message.Message != nil && cq.Message.Message.Chat.ID != 0 {
		chatID = cq.Message.Message.Chat.ID
		msgID = cq.Message.Message.ID
	}
	local := &CallbackQuery{
		ID:        cq.ID,
		Data:      cq.Data,
		From:      from,
		ChatID:    chatID,
		MessageID: msgID,
	}
	dispatchCallback(ctx, r.deps, local)
}

// dispatchCommand 命令分发。
func (r *Router) dispatchCommand(ctx context.Context, cmd string, args []string, msg *Message) {
	switch cmd {
	case "/start", "/menu":
		r.cmdStart(ctx, msg, args)
	case "/help":
		r.cmdHelp(ctx, msg)
	case "/signin":
		r.cmdSignin(ctx, msg, args)
	case "/profile":
		r.cmdProfile(ctx, msg, args)
	case "/shop":
		r.cmdShop(ctx, msg, args)
	case "/bind":
		r.cmdBind(ctx, msg, args)
	case "/register":
		r.cmdRegister(ctx, msg, args)
	case "/account":
		r.cmdAccount(ctx, msg, args)
	case "/drama":
		r.cmdDrama(ctx, msg, args)
	case "/redeem":
		r.cmdRedeem(ctx, msg, args)
	case "/admin":
		r.cmdAdmin(ctx, msg, args)
	default:
		sendText(ctx, r.deps, msg.ChatID, "未知命令，发 /help 查看可用指令。")
	}
}

// cmdStart /start 欢迎 + 主面板。
func (r *Router) cmdStart(ctx context.Context, msg *Message, _ []string) {
	u, err := getLocal(ctx, r.deps, msg.From)
	if err != nil {
		sendText(ctx, r.deps, msg.ChatID, "系统繁忙，请稍后再试。")
		return
	}
	text, rows := mainPanel(r.deps, u)
	sendPanel(ctx, r.deps, msg.ChatID, 0, text, rows)
}

// splitCmd 拆分 "/cmd@botname arg1 arg2" → ("/cmd", [arg1, arg2])。
func splitCmd(text string) (string, []string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", nil
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", nil
	}
	cmd := fields[0]
	if at := strings.IndexByte(cmd, '@'); at > 0 {
		cmd = cmd[:at]
	}
	return cmd, fields[1:]
}