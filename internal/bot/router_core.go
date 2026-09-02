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
// 仅处理私聊消息：本 Bot 的全部功能都面向私聊设计（面板/会话/卡密/密码），
// 群里误发的密码或卡密不应被处理并回显流程提示，也避免面板刷群。
func (r *Router) handleMessage(ctx context.Context, m *models.Message) {
	if m == nil || m.From == nil || m.Chat.ID == 0 {
		return
	}
	if m.Chat.Type != models.ChatTypePrivate {
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
	// 用独立的 SessionLocks 串行化同一用户的消息/会话推进，避免多 worker 下
	// 两个消息同时读写同一 Session 导致步骤错乱。Lockers 用于资金等业务互斥，
	// 这里不能复用，否则会与内部 Lockers.WithUser 形成非重入死锁。
	if r.deps.SessionLocks != nil {
		r.deps.SessionLocks.WithUser(msg.From.ID, func() {
			r.handleMessageInner(ctx, msg, text)
		})
		return
	}
	r.handleMessageInner(ctx, msg, text)
}

// handleMessageInner 实际处理一条文本/命令消息（调用方已持有 SessionLocks 时执行）。
func (r *Router) handleMessageInner(ctx context.Context, msg *Message, text string) {
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
// 安全面板只在私聊里交互：面板消息被转发进群后按钮仍然有效，
// 若不校验聊天类型，群成员点击会把点击者自己的设备列表/购买到的卡密明文等
// 敏感输出直接刷进群聊。除 private 外的任何环境（含群/频道/无法确认的
// 内联回调）一律拒绝。
func (r *Router) handleCallback(ctx context.Context, cq *models.CallbackQuery) {
	if cq == nil || cq.From.ID == 0 || cq.Data == "" {
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
	chatPrivate := false
	switch {
	case cq.Message.Message != nil:
		chatID = cq.Message.Message.Chat.ID
		msgID = cq.Message.Message.ID
		chatPrivate = cq.Message.Message.Chat.Type == models.ChatTypePrivate
	case cq.Message.InaccessibleMessage != nil:
		chatID = cq.Message.InaccessibleMessage.Chat.ID
		msgID = cq.Message.InaccessibleMessage.MessageID
		chatPrivate = cq.Message.InaccessibleMessage.Chat.Type == models.ChatTypePrivate
	}
	if !chatPrivate || chatID == 0 {
		if cq.ID != "" && r.deps != nil && r.deps.Snd != nil {
			_ = r.deps.Snd.AnswerCallback(ctx, cq.ID, "请在与机器人的私聊中使用面板。", true)
		}
		return
	}
	local := &CallbackQuery{
		ID:        cq.ID,
		Data:      cq.Data,
		From:      from,
		ChatID:    chatID,
		MessageID: msgID,
	}
	if r.deps.SessionLocks != nil {
		r.deps.SessionLocks.WithUser(from.ID, func() {
			dispatchCallback(ctx, r.deps, local)
		})
		return
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