package botinteraction

import (
	"agentflow/internal/domain"
	"agentflow/internal/larkcli"
	"agentflow/internal/repo"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLarkCLI writes a lark-cli stand-in. `event consume <key>` reports ready and
// streams <dir>/<key>.ndjson until stdin closes; every other command is recorded
// as <dir>/call-<pid> (argv, then stdin) and answered with a success envelope.
func fakeLarkCLI(t *testing.T) (binary, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := `#!/bin/sh
dir='` + dir + `'
if [ "$1" = event ] && [ "$2" = consume ]; then
  f="$dir/$3.ndjson"; touch "$f"
  echo "[event] ready event_key=$3" >&2
  tail -n +1 -f "$f" & t=$!
  cat > /dev/null
  kill $t; exit 0
fi
if [ "$1" = event ] && [ "$2" = stop ]; then : > "$dir/bus-stopped"; exit 0; fi
{ printf '%s\n' "$*"; cat; } > "$dir/call-$$.tmp" && mv "$dir/call-$$.tmp" "$dir/call-$$"
echo '{"ok":true,"identity":"bot","data":{"message_id":"om_reply'$$'"}}'
`
	binary = filepath.Join(dir, "lark-cli")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, dir
}
func larkClientFor(binary string) larkcli.Client {
	cli := larkcli.New(larkcli.BrandFeishu)
	cli.Binary = binary
	return cli
}
func larkCalls(dir string) []string {
	paths, _ := filepath.Glob(filepath.Join(dir, "call-*"))
	calls := []string{}
	for _, path := range paths {
		if strings.HasSuffix(path, ".tmp") {
			continue
		}
		if raw, err := os.ReadFile(path); err == nil {
			calls = append(calls, string(raw))
		}
	}
	return calls
}
func larkCallMatching(dir string, parts ...string) string {
	for _, call := range larkCalls(dir) {
		matched := true
		for _, part := range parts {
			matched = matched && strings.Contains(call, part)
		}
		if matched {
			return call
		}
	}
	return ""
}
func emitLark(t *testing.T, dir, key string, event map[string]any) {
	t.Helper()
	line, _ := json.Marshal(event)
	f, err := os.OpenFile(filepath.Join(dir, key+".ndjson"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for " + what)
}

func TestLarkConversationRepliesAndCardActions(t *testing.T) {
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	bot := domain.NotificationBot{ID: "bot-lark", Provider: domain.BotProviderFeishu, BotID: "cli_test", ProjectID: "p", OwnerID: 42, Enabled: true, InteractionEnabled: true}
	if err = store.SaveNotificationBot(bot, "app-secret"); err != nil {
		t.Fatal(err)
	}
	binary, dir := fakeLarkCLI(t)
	service := New(store)
	service.LarkBinary = binary
	var mu sync.Mutex
	var prompts []string
	var chats []string
	service.Generate = func(_ context.Context, _ domain.NotificationBot, conv domain.BotConversation, _ []domain.BotInteraction, item domain.BotInteraction) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		prompts = append(prompts, item.Content)
		chats = append(chats, conv.ChatType+":"+conv.ChatID)
		return "答复：" + item.Content, nil
	}
	actions := make(chan domain.BotInteraction, 4)
	service.Action = func(_ context.Context, _ domain.NotificationBot, item domain.BotInteraction, _ domain.BotConversation) (string, error) {
		actions <- item
		return "「开发」操作已完成：确认通过，继续流程。", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	service.Start(ctx)
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			<-service.Done
		}
	}
	defer stop()
	waitFor(t, "both consumers", func() bool {
		_, a := os.Stat(filepath.Join(dir, larkMessageEvent+".ndjson"))
		_, b := os.Stat(filepath.Join(dir, larkCardEvent+".ndjson"))
		return a == nil && b == nil
	})

	message := map[string]any{"type": larkMessageEvent, "message_id": "om_question", "chat_id": "oc_group", "chat_type": "group", "message_type": "text",
		"sender_id": "ou_user", "sender_type": "user", "content": "@Release Bot 帮我看看进度", "mentions": []map[string]string{{"key": "@_user_1", "name": "Release Bot"}}}
	emitLark(t, dir, larkMessageEvent, message)
	emitLark(t, dir, larkMessageEvent, message) // a redelivered event must not run the model twice
	waitFor(t, "final reply", func() bool {
		return larkCallMatching(dir, "+messages-edit", "答复：帮我看看进度") != "" || larkCallMatching(dir, "+messages-reply", "答复：帮我看看进度") != ""
	})
	receipt := larkCallMatching(dir, "+messages-reply --message-id om_question", "已收到")
	if receipt == "" || !strings.Contains(receipt, "--content -") || !strings.Contains(receipt, "--as bot") {
		t.Fatalf("receipt reply missing: %v", larkCalls(dir))
	}
	waitFor(t, "replied record", func() bool {
		records, _ := store.BotInteractionRecords(bot.ID)
		return len(records) == 1 && records[0].Status == "replied"
	})
	mu.Lock()
	if len(prompts) != 1 || prompts[0] != "帮我看看进度" || chats[0] != "group:oc_group" {
		t.Fatalf("model calls %v %v", prompts, chats)
	}
	mu.Unlock()

	click := map[string]any{"type": larkCardEvent, "event_id": "ev-1", "operator_id": "ou_user", "message_id": "om_card", "chat_id": "oc_group",
		"action_tag": "button", "action_value": `{"key":"approve","task_id":"delivery-1_abc123"}`}
	emitLark(t, dir, larkCardEvent, click)
	emitLark(t, dir, larkCardEvent, click)
	select {
	case item := <-actions:
		if item.Action != "approve" || item.DeliveryID != "delivery-1" || item.CardTaskID != "delivery-1_abc123" || item.SenderID != "ou_user" {
			t.Fatalf("card action %+v", item)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("card action not processed")
	}
	waitFor(t, "card patched", func() bool {
		return larkCallMatching(dir, "api PATCH /open-apis/im/v1/messages/om_card", "操作已完成") != ""
	})
	waitFor(t, "action result message", func() bool {
		return larkCallMatching(dir, "+messages-send --chat-id oc_group", "操作已完成") != ""
	})
	select {
	case <-actions:
		t.Fatal("duplicate card callback executed twice")
	case <-time.After(300 * time.Millisecond):
	}
	stop()
	if _, err := os.Stat(filepath.Join(dir, "bus-stopped")); err != nil {
		t.Fatal("bus was not stopped on shutdown")
	}
}

func TestNormalizeLarkEvents(t *testing.T) {
	bot := domain.NotificationBot{ID: "b", BotID: "cli_x", Provider: domain.BotProviderLark}
	raw := func(v map[string]any) []byte { b, _ := json.Marshal(v); return b }
	p2p := raw(map[string]any{"message_id": "om_1", "chat_id": "oc_dm", "chat_type": "p2p", "message_type": "post", "sender_id": "ou_a", "sender_type": "user", "content": "/new"})
	item, kind, chat, ok := normalizeLarkMessage(bot, p2p)
	if !ok || kind != "single" || chat != "oc_dm" || item.Content != "/new" || item.RequestID != "om_1" || item.ID != repo.BotMessageID("cli_x", "om_1") {
		t.Fatalf("p2p %+v %s %s %t", item, kind, chat, ok)
	}
	image := raw(map[string]any{"message_id": "om_2", "chat_id": "oc_dm", "chat_type": "p2p", "message_type": "image", "sender_id": "ou_a", "content": "[Image]"})
	if item, _, _, _ := normalizeLarkMessage(bot, image); item.Content != "[不支持的消息类型，请发送文字]" {
		t.Fatalf("image %+v", item)
	}
	for _, bad := range []map[string]any{
		{"message_id": "om_3", "chat_id": "oc_g", "message_type": "text", "sender_id": "ou_bot", "sender_type": "bot", "content": "loop"},
		{"message_id": "om_4", "chat_id": "../x", "message_type": "text", "sender_id": "ou_a", "content": "hi"},
		{"message_id": "bad id", "chat_id": "oc_g", "message_type": "text", "sender_id": "ou_a", "content": "hi"},
		{"message_id": "om_5", "chat_id": "oc_g", "message_type": "text", "sender_id": "ou_a", "content": "@Bot", "mentions": []map[string]string{{"name": "Bot"}}},
	} {
		if _, _, _, ok := normalizeLarkMessage(bot, raw(bad)); ok {
			t.Fatalf("accepted %v", bad)
		}
	}
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := New(store)
	object := raw(map[string]any{"event_id": "e", "operator_id": "ou_a", "message_id": "om_c", "chat_id": "oc_g", "action_value": map[string]string{"key": "retry", "task_id": "d1_x"}})
	if item, kind, _, ok := s.normalizeLarkCard(bot, object); !ok || item.Action != "retry" || item.DeliveryID != "d1" || kind != "group" {
		t.Fatalf("object action value %+v %s %t", item, kind, ok)
	}
	handled := raw(map[string]any{"event_id": "e2", "operator_id": "ou_a", "chat_id": "oc_g", "action_value": `{"key":"handled","task_id":"d1_x"}`})
	if _, _, _, ok := s.normalizeLarkCard(bot, handled); ok {
		t.Fatal("handled button accepted")
	}
}

func TestLarkActionCardKeepsActionsAndLinks(t *testing.T) {
	card := map[string]any{"task_id": "d1_abc", "source": map[string]any{"desc": "执行失败"}, "main_title": map[string]string{"title": "登录优化", "desc": "节点：开发"},
		"sub_title_text": "原因：测试*失败*", "horizontal_content_list": []map[string]string{{"keyname": "SOP", "value": "交付"}},
		"button_list": []map[string]any{{"text": "重试", "key": "retry"}, {"text": "放行", "key": "pass"}}, "jump_list": []map[string]any{{"title": "查看详情", "url": "https://platform.example/#/runs/r"}, {"title": "坏链接", "url": "javascript:alert(1)"}}}
	out, _ := json.Marshal(larkActionCard(card))
	text := string(out)
	for _, want := range []string{`"schema":"2.0"`, `"template":"red"`, `"key":"retry","task_id":"d1_abc"`, `"key":"pass","task_id":"d1_abc"`, `"default_url":"https://platform.example/#/runs/r"`, `原因：测试\\*失败\\*`, `"primary_filled"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
	if strings.Contains(text, "javascript:") {
		t.Fatal("unsafe link kept")
	}
	result, _ := json.Marshal(larkResultCard("这条提醒已经过期或已处理"))
	if !strings.Contains(string(result), "提醒已失效") || strings.Contains(string(result), "behaviors") {
		t.Fatalf("result card %s", result)
	}
}

func TestLarkLateReceiptAfterFinalReplyIsDropped(t *testing.T) {
	binary, dir := fakeLarkCLI(t)
	c := &larkChannel{cli: larkClientFor(binary), appID: "cli_x", secret: "s", platform: domain.BotProviderFeishu, done: make(chan struct{})}
	item := domain.BotInteraction{ID: "m", RequestID: "om_q"}
	late := c.receipt(item.ID) // what acknowledge captures before its goroutine runs
	if err := c.reply(context.Background(), item, domain.BotConversation{ChatID: "oc_g"}, "最终答复"); err != nil {
		t.Fatal(err)
	}
	c.forget(item.ID)
	if err := c.update(context.Background(), late, item, "已收到，正在处理…", false); err != nil {
		t.Fatal(err)
	}
	if calls := larkCalls(dir); len(calls) != 1 || !strings.Contains(calls[0], "最终答复") {
		t.Fatalf("calls %v", calls)
	}
}
