package httpapi

import (
	"agentflow/internal/domain"
	"errors"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/url"
	"strings"
	"time"
)

type botIdentityHandlers struct {
	*deps
	publicURL string
}

func (h *botIdentityHandlers) currentUser(c *gin.Context) (domain.User, bool) {
	// Binding is an interactive login operation, never a Bearer-token operation.
	if _, ok := bearerToken(c.Request); ok {
		c.JSON(401, gin.H{"error": "请使用平台网页登录确认身份"})
		return domain.User{}, false
	}
	current, ok := h.requestUser(c.Request, false)
	if !ok || current == nil || current.IsGuest || current.ID <= 0 {
		c.JSON(401, gin.H{"error": "请先登录平台"})
		return domain.User{}, false
	}
	user, ok := h.repo.User(current.ID)
	if !ok {
		c.JSON(401, gin.H{"error": "平台账号不存在"})
		return user, false
	}
	return user, true
}
func (h *botIdentityHandlers) binding(c *gin.Context) {
	user, ok := h.currentUser(c)
	if !ok {
		return
	}
	c.Header("Cache-Control", "no-store")
	var input struct {
		Token string `json:"token"`
	}
	if c.ShouldBindJSON(&input) != nil || len(input.Token) != 64 {
		c.JSON(400, gin.H{"error": "绑定链接无效"})
		return
	}
	status, err := h.repo.BotBindingChallengeStatus(input.Token, user.ID, time.Now().UTC())
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if strings.HasSuffix(c.Request.URL.Path, "/claim") && status != "confirmed" {
		if !botBindingSameOrigin(c) {
			c.JSON(403, gin.H{"error": "请从平台绑定页面操作"})
			return
		}
		code, err := h.repo.ClaimBotBindingChallenge(input.Token, user.ID, time.Now().UTC())
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "claimed", "loginName": user.LoginName, "command": "/bind " + code, "platform": h.botBindingPlatform(input.Token)})
		return
	}
	c.JSON(200, gin.H{"status": status, "loginName": user.LoginName, "platform": h.botBindingPlatform(input.Token)})
}
func botBindingSameOrigin(c *gin.Context) bool {
	origin, err := url.Parse(c.GetHeader("Origin"))
	return err == nil && (origin.Scheme == "http" || origin.Scheme == "https") && origin.Host == c.Request.Host
}
func (h *botIdentityHandlers) list(c *gin.Context) {
	user, ok := h.currentUser(c)
	if !ok {
		return
	}
	bindings, err := h.repo.BotUserBindings(user.ID)
	if err != nil {
		respond(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, bindings)
}
func (h *botIdentityHandlers) remove(c *gin.Context) {
	user, ok := h.currentUser(c)
	if !ok {
		return
	}
	if !botBindingSameOrigin(c) {
		c.JSON(403, gin.H{"error": "请从个人配置解除绑定"})
		return
	}
	if err := h.repo.RevokeBotUserBinding(c.Param("id"), user.ID, time.Now().UTC()); err != nil {
		c.JSON(404, gin.H{"error": "绑定不存在"})
		return
	}
	c.Status(204)
}

// botSenderAccount names the chat account a binding is for, e.g. "企微账号".
func botSenderAccount(bot domain.NotificationBot) string {
	if bot.Platform() == domain.BotProviderWeCom {
		return "企微账号"
	}
	return domain.BotPlatformLabel(bot.Platform()) + "账号"
}

// botBindingPlatform names the platform of the bot a binding link was issued by.
func (h *botIdentityHandlers) botBindingPlatform(token string) string {
	botID, err := h.repo.BotBindingChallengeBot(token)
	if err != nil {
		return ""
	}
	bots, err := h.repo.InteractiveNotificationBots()
	if err != nil {
		return ""
	}
	for _, bot := range bots {
		if bot.BotID == botID {
			return domain.BotPlatformLabel(bot.Platform())
		}
	}
	return ""
}
func botIdentityCommand(content string) string {
	text := strings.TrimSpace(content)
	if strings.HasPrefix(text, "@") {
		if _, rest, ok := strings.Cut(text, " "); ok {
			text = strings.TrimSpace(rest)
		}
	}
	return text
}

// Only platform login nominates an account. The trusted callback from the
// original WeCom sender must submit the browser-only code before it takes effect.
func (h *botIdentityHandlers) message(bot domain.NotificationBot, item domain.BotInteraction) (bool, string, error) {
	text := botIdentityCommand(item.Content)
	if text == "/unbind" || text == "解除绑定" {
		binding, err := h.repo.BotUserBinding(bot.BotID, item.SenderID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return true, "当前" + botSenderAccount(bot) + "尚未绑定平台账号。发送“绑定账号”可进行关联。", nil
		}
		if err != nil {
			return true, "", err
		}
		if err := h.repo.RevokeBotUserBinding(binding.ID, binding.UserID, time.Now().UTC()); err != nil {
			return true, "", err
		}
		return true, "已解除当前" + botSenderAccount(bot) + "与平台账号的绑定，旧确认码已失效。需要重新关联时，请发送“绑定账号”。", nil
	}

	if strings.HasPrefix(text, "/bind ") {
		binding, err := h.repo.ConfirmBotUserBinding(bot.BotID, item.SenderID, strings.TrimSpace(strings.TrimPrefix(text, "/bind ")), time.Now().UTC())
		if err != nil {
			return true, err.Error(), nil
		}
		user, ok := h.repo.User(binding.UserID)
		if !ok {
			return true, "平台账号不存在，请在个人配置解除后重新绑定。", nil
		}
		return true, "已绑定平台账号「" + user.LoginName + "」。后续操作将使用你的身份、个人凭据和项目权限。请重新发送刚才的请求。", nil
	}
	if text == "绑定账号" || text == "/bind" {
		if binding, err := h.repo.BotUserBinding(bot.BotID, item.SenderID); err == nil {
			if user, ok := h.repo.User(binding.UserID); ok {
				return true, "当前已绑定「" + user.LoginName + "」。发送 /unbind 或在平台个人配置中解除绑定。", nil
			}
		}
	}
	_, err := h.repo.BotUserBinding(bot.BotID, item.SenderID)
	if err == nil {
		return false, "", nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return true, "", err
	}
	base, err := url.Parse(strings.TrimRight(h.publicURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return true, "请先关联平台账号；平台外部访问地址尚未配置，请联系管理员。", nil
	}
	token, err := h.repo.NewBotBindingChallenge(bot.BotID, item.SenderID, time.Now().UTC())
	if err != nil {
		return true, "", err
	}
	link := strings.TrimRight(h.publicURL, "/") + "/bot-bind.html#request=" + url.QueryEscape(token)
	return true, "请先[关联你的平台账号](" + link + ")（15 分钟内有效）。登录确认后，将页面上的 /bind 确认码发回当前 Bot。群里其他人点击链接不会直接完成绑定。", nil
}
