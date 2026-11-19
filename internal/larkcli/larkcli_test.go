package larkcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lark-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseEnvelopeSkipsWarningsAndReadsPrettyErrors(t *testing.T) {
	raw := "warning: bot_strip_p2p: notice\n{\n  \"ok\": false,\n  \"error\": {\"type\": \"authorization\", \"subtype\": \"missing_scope\", \"code\": 99991679}\n}\n"
	envelope, ok := ParseEnvelope([]byte(raw))
	if !ok || envelope.Error == nil || envelope.Error.Subtype != "missing_scope" || !envelope.Error.Auth() {
		t.Fatalf("%+v %t", envelope, ok)
	}
	if _, ok := ParseEnvelope([]byte("{\"unrelated\":1}\nplain")); ok {
		t.Fatal("non-envelope JSON accepted")
	}
}

func TestTruncateAndIDs(t *testing.T) {
	text := strings.Repeat("汉", 20)
	cut := Truncate(text, 10)
	if len(cut) > 10 || !utf8.ValidString(cut) || !strings.HasSuffix(cut, "…") {
		t.Fatalf("%q", cut)
	}
	if Truncate("short", 10) != "short" {
		t.Fatal("short text changed")
	}
	for id, want := range map[string]bool{"oc_abc-123": true, "oc_": true, "oc_a/../b": false, "ou_a": false, "oc_a b": false} {
		if ValidChatID(id) != want {
			t.Fatalf("%s", id)
		}
	}
	if !ValidMessageID("om_x") || ValidMessageID("om_x?y=1") {
		t.Fatal("message id validation")
	}
}

func TestRunPassesStdinAndIsolatesEnvironment(t *testing.T) {
	t.Setenv("AGENTFLOW_HOST_SECRET", "leak")
	out := filepath.Join(t.TempDir(), "seen")
	cli := Client{Binary: script(t, `{ printf '%s\n' "$*"; env; cat; } > '`+out+`'; echo '{"ok":true,"data":{"x":1}}'`), Brand: BrandLark}
	data, err := cli.Run(context.Background(), "cli_app", "s3cret", []byte("@/etc/passwd"), "im", "+messages-send")
	if err != nil || string(data) != `{"x":1}` {
		t.Fatalf("%s %v", data, err)
	}
	seen, _ := os.ReadFile(out)
	for _, want := range []string{"im +messages-send --as bot --format json", "LARKSUITE_CLI_BRAND=lark", "LARKSUITE_CLI_APP_SECRET=s3cret", "@/etc/passwd"} {
		if !strings.Contains(string(seen), want) {
			t.Fatalf("missing %q:\n%s", want, seen)
		}
	}
	if strings.Contains(string(seen), "leak") {
		t.Fatal("host environment leaked")
	}
	failing := Client{Binary: script(t, `echo '{"ok":false,"error":{"type":"network","code":0}}' >&2; exit 4`)}
	var failure *Failure
	if _, err := failing.Run(context.Background(), "a", "b", nil); !errors.As(err, &failure) || failure.Type != "network" {
		t.Fatalf("%v", err)
	}
}

func TestConsumeStreamsUntilStopped(t *testing.T) {
	dir := t.TempDir()
	cli := Client{Binary: script(t, `echo "[event] ready event_key=$3" >&2; echo '{"n":1}'; echo; echo '{"n":2}'; cat > /dev/null`)}
	var mu sync.Mutex
	var events []string
	consumer, err := cli.Consume(context.Background(), "a", "b", dir, "im.message.receive_v1", func(line []byte) {
		mu.Lock()
		events = append(events, string(line))
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	consumer.Stop()
	select {
	case <-consumer.Done():
	default:
		t.Fatal("consumer still running after Stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(events, ",") != `{"n":1},{"n":2}` {
		t.Fatalf("%v", events)
	}
}

func TestConsumeReportsStartupFailure(t *testing.T) {
	cli := Client{Binary: script(t, `printf '{\n "ok": false,\n "error": {"type": "validation", "subtype": "failed_precondition"}\n}\n' >&2; exit 2`)}
	var failure *Failure
	if _, err := cli.Consume(context.Background(), "a", "b", t.TempDir(), "k", func([]byte) {}); !errors.As(err, &failure) || failure.Subtype != "failed_precondition" {
		t.Fatalf("%v", err)
	}
	missing := Client{Binary: filepath.Join(t.TempDir(), "absent")}
	if _, err := missing.Consume(context.Background(), "a", "b", t.TempDir(), "k", func([]byte) {}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("%v", err)
	}
}
