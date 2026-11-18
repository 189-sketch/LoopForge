package notification

import (
	"agentflow/internal/domain"
	"agentflow/internal/larkcli"
	"context"
	"encoding/json"
	"errors"
)

// LarkCLIProvider sends Feishu/Lark bot notifications through the official
// larksuite/cli (`lark-cli`); see package larkcli for the isolation rules.
type LarkCLIProvider struct {
	CLI larkcli.Client
}

func NewLarkCLIProvider(brand string) *LarkCLIProvider {
	return &LarkCLIProvider{CLI: larkcli.New(brand)}
}

func (p *LarkCLIProvider) platform() string {
	if p.CLI.Brand == larkcli.BrandLark {
		return domain.BotProviderLark
	}
	return domain.BotProviderFeishu
}
func (p *LarkCLIProvider) call(ctx context.Context, appID, secret string, stdin []byte, args ...string) (json.RawMessage, error) {
	data, err := p.CLI.Run(ctx, appID, secret, stdin, args...)
	var failure *larkcli.Failure
	switch {
	case err == nil:
		return data, nil
	case errors.As(err, &failure):
		return nil, p.failure(failure)
	case errors.Is(err, larkcli.ErrNotInstalled):
		return nil, errors.New("通知服务未安装 lark-cli，请联系管理员")
	case errors.Is(err, larkcli.ErrTimeout):
		return nil, &ProviderError{Platform: p.platform(), Message: "通知请求超时"}
	}
	return nil, &ProviderError{Platform: p.platform(), Message: "lark-cli 调用失败"}
}

// failure turns a lark-cli error category into a platform message. Upstream
// messages are not echoed: they can carry app identifiers and are not localized.
func (p *LarkCLIProvider) failure(f *larkcli.Failure) *ProviderError {
	e := &ProviderError{Code: f.Code, Platform: p.platform(), Message: "请求失败，请稍后重试", Auth: f.Auth()}
	switch {
	case f.Type == "authentication" || f.Type == "config":
		e.Message = "应用凭据无效，请检查 App ID 与 App Secret"
	case f.Code == 232025:
		e.Message = "应用未启用机器人能力，请在开发者后台添加机器人并发布版本"
	case f.Auth():
		e.Message = "应用缺少权限，请在开发者后台开通 im:chat:read 与 im:message:send_as_bot 并发布版本"
	case f.Subtype == "not_found" || f.Code == 230002:
		e.Code, e.Message = 404, "机器人不在接收群中，请将机器人加入群聊后刷新列表"
	case f.Type == "network":
		e.Message = "网络请求失败，请稍后重试"
	case f.Subtype == "rate_limit":
		e.Message = "请求过于频繁，请稍后重试"
	}
	return e
}

// Sessions lists the group chats the bot belongs to. Bot identity cannot enumerate
// p2p chats (a Lark privacy rule), so Feishu/Lark notifications go to groups only.
func (p *LarkCLIProvider) Sessions(ctx context.Context, appID, secret string) ([]domain.BotChat, error) {
	raw, err := p.call(ctx, appID, secret, nil, "im", "+chat-list", "--sort", "active_time", "--page-size", "100", "--page-all", "--page-limit", "5")
	if err != nil {
		return nil, err
	}
	var rsp struct {
		Chats []struct {
			ID     string `json:"chat_id"`
			Name   string `json:"name"`
			Status string `json:"chat_status"`
		} `json:"chats"`
	}
	if json.Unmarshal(raw, &rsp) != nil {
		return nil, &ProviderError{Platform: p.platform(), Message: "群列表格式异常"}
	}
	chats := make([]domain.BotChat, 0, len(rsp.Chats))
	for _, chat := range rsp.Chats {
		if larkcli.ValidChatID(chat.ID) && (chat.Status == "" || chat.Status == "normal") {
			chats = append(chats, domain.BotChat{ID: chat.ID, Name: chat.Name, Type: "group"})
		}
	}
	return chats, nil
}

// Send posts Markdown as a rich-text `md` element through stdin (see larkcli.Post).
func (p *LarkCLIProvider) Send(ctx context.Context, appID, secret, target, content string) error {
	if len([]byte(content)) == 0 || len([]byte(content)) > 20480 {
		return errors.New("通知正文必须为 1 至 20480 字节")
	}
	if !larkcli.ValidChatID(target) {
		return &ProviderError{Code: 404, Platform: p.platform(), Message: "接收群无效，请刷新列表后重新选择"}
	}
	raw, err := p.call(ctx, appID, secret, larkcli.Post(content), "im", "+messages-send", "--chat-id", target, "--msg-type", "post", "--content", "-")
	if err != nil {
		return err
	}
	var result struct {
		MessageID string `json:"message_id"`
	}
	if json.Unmarshal(raw, &result) != nil || result.MessageID == "" {
		return &ProviderError{Platform: p.platform(), Message: "未确认发送成功"}
	}
	return nil
}
