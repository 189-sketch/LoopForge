package botinteraction

import (
	"agentflow/internal/domain"
	"agentflow/internal/larkcli"
	"agentflow/internal/repo"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"
)

// Feishu/Lark conversations run on larksuite/cli: two `event consume`
// subprocesses (chat messages and card callbacks) share one bus daemon that
// holds the app's long connection, and replies are one-shot lark-cli commands.
// Feishu delivers card callbacks over the same connection once the app enables
// 回调配置 → 使用长连接接收回调.
const (
	larkMessageEvent = "im.message.receive_v1"
	larkCardEvent    = "card.action.trigger"
	// Feishu caps edits per message, so progress refreshes less often than on WeCom.
	larkProgressEvery = 15 * time.Second
)

type larkChannel struct {
	cli           larkcli.Client
	appID, secret string
	platform      string
	dir           string
	cancel        context.CancelFunc
	done          chan struct{}
	shutdown      sync.Once
	consumers     []*larkcli.Consumer
	receipts      sync.Map // item ID → *larkReceipt
}

// larkReceipt is the "received" reply that later becomes the answer.
type larkReceipt struct {
	mu        sync.Mutex
	messageID string
	finished  bool
}

func (s *Service) dialLark(ctx context.Context, bot domain.NotificationBot, secret string) (channel, error) {
	cli := larkcli.New(bot.Platform())
	if s.LarkBinary != "" {
		cli.Binary = s.LarkBinary
	}
	// The bus daemon keeps its socket in the config dir, so the dir lives as long as the connection.
	dir, err := os.MkdirTemp("", "agentflow-lark-bot-")
	if err != nil {
		return nil, errors.New("对话临时目录不可用")
	}
	chCtx, cancel := context.WithCancel(ctx)
	ch := &larkChannel{cli: cli, appID: bot.BotID, secret: secret, platform: bot.Platform(), dir: dir, cancel: cancel, done: make(chan struct{})}
	// Start sequentially: the first consumer forks the bus, the second attaches to it.
	for _, key := range []string{larkMessageEvent, larkCardEvent} {
		consumer, err := cli.Consume(chCtx, bot.BotID, secret, dir, key, func(line []byte) { s.receiveLark(chCtx, bot, key, line, ch) })
		if err != nil {
			ch.close()
			return nil, err
		}
		ch.consumers = append(ch.consumers, consumer)
		go func() {
			select {
			case <-consumer.Done():
				ch.close()
			case <-ch.done:
			}
		}()
	}
	return ch, nil
}

// connectHint turns a connection failure into the state shown in Bot management.
func connectHint(err error) string {
	var failure *larkcli.Failure
	switch {
	case errors.Is(err, larkcli.ErrNotInstalled):
		return "服务器未安装 lark-cli，请联系管理员"
	case errors.As(err, &failure) && failure.Auth():
		return "应用凭据或权限无效，请检查 App Secret 与事件订阅权限"
	case errors.As(err, &failure) && failure.Subtype == "failed_precondition":
		return "该应用的事件长连接已被其他进程占用，断开后会自动重连"
	}
	return ""
}

func (s *Service) receiveLark(ctx context.Context, bot domain.NotificationBot, key string, line []byte, ch *larkChannel) {
	var (
		item       domain.BotInteraction
		kind, chat string
		ok         bool
	)
	if key == larkCardEvent {
		item, kind, chat, ok = s.normalizeLarkCard(bot, line)
	} else {
		item, kind, chat, ok = normalizeLarkMessage(bot, line)
	}
	if ok {
		s.accept(ctx, item, kind, chat, ch)
	}
}

// normalizeLarkMessage maps a flattened im.message.receive_v1 event. Conversations
// are keyed by chat_id for both p2p and group chats, which Lark accepts as a send target.
func normalizeLarkMessage(bot domain.NotificationBot, line []byte) (domain.BotInteraction, string, string, bool) {
	var event struct {
		MessageID   string `json:"message_id"`
		ChatID      string `json:"chat_id"`
		ChatType    string `json:"chat_type"`
		MessageType string `json:"message_type"`
		SenderID    string `json:"sender_id"`
		SenderType  string `json:"sender_type"`
		Content     string `json:"content"`
		Mentions    []struct {
			Name string `json:"name"`
		} `json:"mentions"`
	}
	if json.Unmarshal(line, &event) != nil || !larkcli.ValidMessageID(event.MessageID) || !larkcli.ValidChatID(event.ChatID) || event.SenderID == "" || len(event.SenderID) > 191 {
		return domain.BotInteraction{}, "", "", false
	}
	if event.SenderType != "" && event.SenderType != "user" {
		return domain.BotInteraction{}, "", "", false
	}
	kind := "group"
	if event.ChatType == "p2p" {
		kind = "single"
	}
	text := "[不支持的消息类型，请发送文字]"
	if event.MessageType == "text" || event.MessageType == "post" {
		text = stripLeadingMentions(event.Content, event.Mentions)
	}
	text = domain.NotificationText(text, 16000)
	if strings.TrimSpace(text) == "" {
		return domain.BotInteraction{}, "", "", false
	}
	return domain.BotInteraction{ID: repo.BotMessageID(bot.BotID, event.MessageID), BotID: bot.ID, SenderID: event.SenderID, RequestID: event.MessageID, Content: text}, kind, event.ChatID, true
}

// stripLeadingMentions drops the "@Bot" a group message starts with, so commands
// such as /new or 绑定账号 match even when the bot's display name has spaces.
func stripLeadingMentions(text string, mentions []struct {
	Name string `json:"name"`
}) string {
	text = strings.TrimSpace(text)
	for changed := true; changed; {
		changed = false
		for _, mention := range mentions {
			if name := "@" + mention.Name; mention.Name != "" && strings.HasPrefix(text, name) {
				text, changed = strings.TrimSpace(strings.TrimPrefix(text, name)), true
			}
		}
	}
	return text
}

// normalizeLarkCard maps a card.action.trigger callback from a notification card.
// The button value carries the same key/task_id pair the WeCom card uses.
func (s *Service) normalizeLarkCard(bot domain.NotificationBot, line []byte) (domain.BotInteraction, string, string, bool) {
	var event struct {
		EventID     string          `json:"event_id"`
		OperatorID  string          `json:"operator_id"`
		MessageID   string          `json:"message_id"`
		ChatID      string          `json:"chat_id"`
		ActionValue json.RawMessage `json:"action_value"`
	}
	if json.Unmarshal(line, &event) != nil || event.OperatorID == "" || len(event.OperatorID) > 191 || !larkcli.ValidChatID(event.ChatID) {
		return domain.BotInteraction{}, "", "", false
	}
	var value struct {
		Key    string `json:"key"`
		TaskID string `json:"task_id"`
	}
	raw := []byte(event.ActionValue)
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		raw = []byte(encoded)
	}
	if json.Unmarshal(raw, &value) != nil || value.Key == "" || value.Key == "handled" || value.TaskID == "" || len(value.TaskID) > 191 {
		return domain.BotInteraction{}, "", "", false
	}
	delivery, _, _ := strings.Cut(value.TaskID, "_")
	kind := "group"
	if d, err := s.Store.NotificationDelivery(delivery); err == nil && d.BotID == bot.ID && d.TargetID == event.ChatID && d.TargetType != "" {
		kind = d.TargetType
	}
	eventKey := event.EventID
	if eventKey == "" {
		eventKey = event.MessageID + ":" + event.OperatorID + ":" + value.TaskID + ":" + value.Key
	}
	item := domain.BotInteraction{ID: repo.BotMessageID(bot.BotID, "card:"+eventKey), BotID: bot.ID, SenderID: event.OperatorID, RequestID: event.MessageID, Action: value.Key, DeliveryID: delivery, CardTaskID: value.TaskID}
	return item, kind, event.ChatID, true
}

func (c *larkChannel) closed() <-chan struct{} { return c.done }

// close stops both consumers and the bus so the app's long connection is free
// before run reconnects; concurrent callers wait for the cleanup to finish.
func (c *larkChannel) close() {
	c.shutdown.Do(func() {
		close(c.done)
		c.cancel()
		var wg sync.WaitGroup
		for _, consumer := range c.consumers {
			wg.Add(1)
			go func() { defer wg.Done(); consumer.Stop() }()
		}
		wg.Wait()
		c.cli.StopBus(c.appID, c.secret, c.dir)
		_ = os.RemoveAll(c.dir)
	})
}

// run executes a reply command; platform rejections become *ReplyError so the
// message is marked failed, while network trouble drops the connection.
func (c *larkChannel) run(ctx context.Context, stdin []byte, args ...string) (json.RawMessage, error) {
	data, err := c.cli.Run(ctx, c.appID, c.secret, stdin, args...)
	var failure *larkcli.Failure
	if errors.As(err, &failure) && failure.Type != "network" {
		name := "飞书"
		if c.platform == domain.BotProviderLark {
			name = "Lark "
		}
		return nil, &ReplyError{Code: failure.Code, Message: name + "拒绝了这次回复，请检查会话和应用权限"}
	}
	return data, err
}
func messageIDFrom(data json.RawMessage) string {
	var result struct {
		MessageID string `json:"message_id"`
	}
	_ = json.Unmarshal(data, &result)
	return result.MessageID
}
func (c *larkChannel) replyTo(ctx context.Context, messageID, text string) (string, error) {
	if !larkcli.ValidMessageID(messageID) {
		return "", &ReplyError{Message: "原消息无效，无法回复"}
	}
	data, err := c.run(ctx, larkcli.Post(text), "im", "+messages-reply", "--message-id", messageID, "--msg-type", "post", "--content", "-")
	return messageIDFrom(data), err
}
func (c *larkChannel) send(ctx context.Context, chatID, text string) error {
	if !larkcli.ValidChatID(chatID) {
		return &ReplyError{Message: "接收会话无效"}
	}
	_, err := c.run(ctx, larkcli.Post(text), "im", "+messages-send", "--chat-id", chatID, "--msg-type", "post", "--content", "-")
	return err
}
func (c *larkChannel) receipt(id string) *larkReceipt {
	value, _ := c.receipts.LoadOrStore(id, &larkReceipt{})
	return value.(*larkReceipt)
}

// update writes text into the message's receipt, creating it on first use. A
// final update that cannot edit the receipt replies with a new message instead.
// Callers resolve r before starting a goroutine, so a late receipt never reappears
// after the answer is final and the entry has been forgotten.
func (c *larkChannel) update(ctx context.Context, r *larkReceipt, item domain.BotInteraction, text string, final bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return nil
	}
	if r.messageID != "" {
		_, err := c.run(ctx, larkcli.Post(text), "im", "+messages-edit", "--message-id", r.messageID, "--msg-type", "post", "--content", "-")
		if err == nil || !final {
			r.finished = err == nil && final
			return err
		}
	}
	id, err := c.replyTo(ctx, item.RequestID, text)
	if err != nil {
		return err
	}
	if !final {
		r.messageID = id
	}
	r.finished = final
	return nil
}
func (c *larkChannel) acknowledge(ctx context.Context, item domain.BotInteraction) {
	r := c.receipt(item.ID)
	go func() { _ = c.update(ctx, r, item, "已收到，正在处理…", false) }()
}
func (c *larkChannel) progressTurn(ctx context.Context, item domain.BotInteraction) (context.Context, func()) {
	r := c.receipt(item.ID)
	go func() {
		r.mu.Lock()
		pending := r.messageID == "" && !r.finished
		r.mu.Unlock()
		if pending {
			_ = c.update(ctx, r, item, "已收到，正在处理…", false)
		}
	}()
	return runProgress(ctx, larkProgressEvery, func(text string) { _ = c.update(ctx, r, item, text, false) })
}
func (c *larkChannel) reply(ctx context.Context, item domain.BotInteraction, conv domain.BotConversation, text string) error {
	if err := c.update(ctx, c.receipt(item.ID), item, text, true); err == nil {
		return nil
	}
	return c.send(ctx, conv.ChatID, text)
}
func (c *larkChannel) sendCard(ctx context.Context, chatID string, card map[string]any) error {
	if !larkcli.ValidChatID(chatID) {
		return &ReplyError{Message: "接收会话无效"}
	}
	data, _ := json.Marshal(larkActionCard(card))
	_, err := c.run(ctx, data, "im", "+messages-send", "--chat-id", chatID, "--msg-type", "interactive", "--content", "-")
	return err
}

// finishActionCard replaces the clicked card (shared cards update for every viewer).
func (c *larkChannel) finishActionCard(ctx context.Context, item domain.BotInteraction, result string) error {
	if !larkcli.ValidMessageID(item.RequestID) {
		return nil
	}
	card, _ := json.Marshal(larkResultCard(result))
	body, _ := json.Marshal(map[string]string{"content": string(card)})
	_, err := c.run(ctx, body, "api", "PATCH", "/open-apis/im/v1/messages/"+item.RequestID, "--data", "-")
	return err
}
func (c *larkChannel) forget(id string) { c.receipts.Delete(id) }

// larkActionCard renders the WeCom-shaped notification card as a Feishu card 2.0
// with callback buttons whose value carries the key and the server-issued task_id.
func larkActionCard(card map[string]any) map[string]any {
	var tpl struct {
		TaskID string `json:"task_id"`
		Source struct {
			Desc string `json:"desc"`
		} `json:"source"`
		MainTitle struct {
			Title string `json:"title"`
			Desc  string `json:"desc"`
		} `json:"main_title"`
		SubTitle string `json:"sub_title_text"`
		Rows     []struct {
			Key   string `json:"keyname"`
			Value string `json:"value"`
		} `json:"horizontal_content_list"`
		Buttons []struct {
			Text string `json:"text"`
			Key  string `json:"key"`
		} `json:"button_list"`
		Jumps []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"jump_list"`
	}
	raw, _ := json.Marshal(card)
	_ = json.Unmarshal(raw, &tpl)
	template := "orange"
	if tpl.Source.Desc == "执行失败" {
		template = "red"
	}
	lines := []string{}
	for _, line := range strings.Split(tpl.SubTitle, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, larkMarkdownText(line))
		}
	}
	for _, row := range tpl.Rows {
		lines = append(lines, "**"+larkMarkdownText(row.Key)+"**："+larkMarkdownText(row.Value))
	}
	elements := []map[string]any{{"tag": "markdown", "content": strings.Join(lines, "\n")}}
	columns := []map[string]any{}
	for i, button := range tpl.Buttons {
		kind := "default"
		if i == 0 {
			kind = "primary_filled"
		}
		columns = append(columns, larkButtonColumn(map[string]any{"tag": "button", "text": plainText(button.Text), "type": kind,
			"behaviors": []map[string]any{{"type": "callback", "value": map[string]string{"key": button.Key, "task_id": tpl.TaskID}}}}))
	}
	for _, jump := range tpl.Jumps {
		if strings.HasPrefix(jump.URL, "https://") || strings.HasPrefix(jump.URL, "http://") {
			columns = append(columns, larkButtonColumn(map[string]any{"tag": "button", "text": plainText(jump.Title), "type": "default",
				"behaviors": []map[string]any{{"type": "open_url", "default_url": jump.URL}}}))
		}
	}
	if len(columns) > 0 {
		elements = append(elements, map[string]any{"tag": "column_set", "flex_mode": "flow", "horizontal_spacing": "8px", "columns": columns})
	}
	header := map[string]any{"title": plainText(tpl.MainTitle.Title), "template": template}
	if tpl.MainTitle.Desc != "" {
		header["subtitle"] = plainText(tpl.MainTitle.Desc)
	}
	if tpl.Source.Desc != "" {
		header["text_tag_list"] = []map[string]any{{"tag": "text_tag", "text": plainText(tpl.Source.Desc), "color": template}}
	}
	return map[string]any{"schema": "2.0", "config": map[string]any{"update_multi": true, "summary": map[string]string{"content": tpl.Source.Desc + "：" + tpl.MainTitle.Title}}, "header": header, "body": map[string]any{"elements": elements}}
}

// larkResultCard replaces a handled action card; it has no buttons, so it cannot be clicked twice.
func larkResultCard(result string) map[string]any {
	title := actionResultTitle(result)
	template := "red"
	if title == "操作已完成" {
		template = "green"
	} else if title == "提醒已失效" {
		template = "grey"
	}
	return map[string]any{"schema": "2.0", "config": map[string]any{"update_multi": true},
		"header": map[string]any{"title": plainText(title), "template": template},
		"body":   map[string]any{"elements": []map[string]any{{"tag": "markdown", "content": larkMarkdownText(cardText(result, 200))}}}}
}
func larkButtonColumn(button map[string]any) map[string]any {
	return map[string]any{"tag": "column", "width": "auto", "elements": []map[string]any{button}}
}
func plainText(text string) map[string]string {
	return map[string]string{"tag": "plain_text", "content": text}
}

// larkMarkdownText keeps platform text literal inside a card markdown element.
func larkMarkdownText(text string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;").Replace(text)
}
