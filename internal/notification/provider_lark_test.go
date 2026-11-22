package notification

import (
	"agentflow/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLarkCLI installs a shell stand-in for lark-cli that records its argv,
// environment and stdin, then prints the given stdout/stderr and exit code.
func fakeLarkCLI(t *testing.T, stdout, stderr string, code int) (*LarkCLIProvider, string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	for name, body := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\n" +
		"{ printf 'ARGS:'; printf ' %s' \"$@\"; printf '\\n'; env; printf 'STDIN:'; cat; } > '" + record + "'\n" +
		"cat '" + filepath.Join(dir, "stdout") + "'\ncat '" + filepath.Join(dir, "stderr") + "' >&2\nexit " + string(rune('0'+code)) + "\n"
	binary := filepath.Join(dir, "lark-cli")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	provider := NewLarkCLIProvider(domain.BotProviderFeishu)
	provider.CLI.Binary = binary
	return provider, record
}
func readRecord(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestLarkSessionsListsLiveGroupsWithBotIdentity(t *testing.T) {
	t.Setenv("AGENTFLOW_UNRELATED_SECRET", "must-not-leak")
	stdout := `{"ok":true,"identity":"bot","data":{"chats":[{"chat_id":"oc_live","name":"研发群","chat_status":"normal"},{"chat_id":"oc_gone","name":"旧群","chat_status":"dissolved"},{"chat_id":"bad","name":"异常"}],"has_more":false}}`
	provider, record := fakeLarkCLI(t, stdout, "", 0)
	chats, err := provider.Sessions(context.Background(), "cli_app", "app-secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 || chats[0] != (domain.BotChat{ID: "oc_live", Name: "研发群", Type: "group"}) {
		t.Fatalf("chats %+v", chats)
	}
	got := readRecord(t, record)
	for _, want := range []string{"ARGS: im +chat-list", "--as bot", "LARKSUITE_CLI_APP_ID=cli_app", "LARKSUITE_CLI_APP_SECRET=app-secret", "LARKSUITE_CLI_BRAND=feishu", "LARKSUITE_CLI_STRICT_MODE=bot", "LARKSUITE_CLI_CONFIG_DIR="} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "must-not-leak") || strings.Contains(got, "HOME="+os.Getenv("HOME")+"\n") {
		t.Fatalf("host environment leaked into lark-cli:\n%s", got)
	}
}

func TestLarkSendPassesPostThroughStdin(t *testing.T) {
	provider, record := fakeLarkCLI(t, `{"ok":true,"identity":"bot","data":{"message_id":"om_1","chat_id":"oc_live"}}`, "", 0)
	content := "@/etc/passwd\n### 完成\n- 已通过测试"
	if err := provider.Send(context.Background(), "cli_app", "s", "oc_live", content); err != nil {
		t.Fatal(err)
	}
	got := readRecord(t, record)
	if !strings.Contains(got, "--chat-id oc_live --msg-type post --content -") {
		t.Fatalf("args %s", got)
	}
	payload := got[strings.Index(got, "STDIN:")+len("STDIN:"):]
	var post struct {
		ZhCN struct {
			Content [][]map[string]string `json:"content"`
		} `json:"zh_cn"`
	}
	if err := json.Unmarshal([]byte(payload), &post); err != nil || post.ZhCN.Content[0][0]["tag"] != "md" || post.ZhCN.Content[0][0]["text"] != content {
		t.Fatalf("post payload %q %v", payload, err)
	}
	if err := provider.Send(context.Background(), "cli_app", "s", "chat-without-prefix", "hi"); err == nil {
		t.Fatal("non-chat target accepted")
	}
}

func TestLarkFailuresMapToPlatformErrors(t *testing.T) {
	for _, tc := range []struct {
		name, stderr string
		auth, gone   bool
	}{
		{"credentials", `warning: something` + "\n" + `{"ok":false,"identity":"bot","error":{"type":"authentication","subtype":"token_invalid","code":10014,"message":"app secret invalid"}}`, true, false},
		{"scope", "{\n  \"ok\": false,\n  \"error\": {\"type\": \"authorization\", \"subtype\": \"app_scope_not_applied\", \"code\": 99991672}\n}", true, false},
		{"bot capability", `{"ok":false,"error":{"type":"api","code":232025}}`, true, false},
		{"not in chat", `{"ok":false,"error":{"type":"api","code":230002}}`, false, true},
		{"rate limit", `{"ok":false,"error":{"type":"api","subtype":"rate_limit","code":99991400}}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, _ := fakeLarkCLI(t, "", tc.stderr, 1)
			_, err := provider.Sessions(context.Background(), "cli_app", "s")
			var pe *ProviderError
			if !errors.As(err, &pe) || pe.NeedsAuthorization() != tc.auth || pe.TargetUnavailable() != tc.gone || !strings.HasPrefix(pe.Error(), "飞书错误") {
				t.Fatalf("err %#v", err)
			}
			if strings.Contains(pe.Error(), "app secret invalid") {
				t.Fatal("upstream message echoed")
			}
		})
	}
	provider, _ := fakeLarkCLI(t, "not json", "boom", 1)
	if _, err := provider.Sessions(context.Background(), "cli_app", "s"); err == nil || !strings.Contains(err.Error(), "lark-cli 调用失败") {
		t.Fatalf("unparseable output: %v", err)
	}
	provider.CLI.Binary = filepath.Join(t.TempDir(), "missing")
	if _, err := provider.Sessions(context.Background(), "cli_app", "s"); err == nil || !strings.Contains(err.Error(), "未安装 lark-cli") {
		t.Fatalf("missing binary: %v", err)
	}
}

func TestProviderErrorPlatformNames(t *testing.T) {
	if got := (&ProviderError{Code: 850003, Message: "x"}).Error(); got != "企业微信错误 850003：x" {
		t.Fatal(got)
	}
	if !(&ProviderError{Code: 850003}).NeedsAuthorization() || (&ProviderError{Code: 850003, Platform: domain.BotProviderLark}).NeedsAuthorization() {
		t.Fatal("WeCom codes must not apply to Lark")
	}
	if got := (&ProviderError{Platform: domain.BotProviderLark, Message: "x"}).Error(); got != "Lark 错误：x" {
		t.Fatal(got)
	}
}
