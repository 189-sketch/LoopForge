// Package larkcli runs the official larksuite/cli (`lark-cli`) with bot identity
// for Feishu and Lark bots. Every invocation gets app credentials through the
// environment and an isolated config directory, so a shared profile, keychain or
// an operator's own `lark-cli auth login` session is never visible to it.
package larkcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Brands accepted by LARKSUITE_CLI_BRAND.
const (
	BrandFeishu = "feishu"
	BrandLark   = "lark"
)

var (
	ErrNotInstalled = errors.New("服务器未安装 lark-cli，请联系管理员")
	ErrTimeout      = errors.New("lark-cli 调用超时")
	ErrUnreadable   = errors.New("lark-cli 调用失败")
)

// Failure is the typed error envelope lark-cli writes to stderr.
type Failure struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Code    int    `json:"code"`
}

func (f *Failure) Error() string {
	return fmt.Sprintf("lark-cli %s/%s %d", f.Type, f.Subtype, f.Code)
}

// Auth reports failures the app owner must fix: credentials, missing scopes or a disabled bot capability.
func (f *Failure) Auth() bool {
	return f.Type == "authentication" || f.Type == "config" || f.Type == "authorization" || f.Code == 99991672 || f.Code == 232025
}

// Envelope is the lark-cli JSON contract: success on stdout, a typed error on stderr.
type Envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *Failure        `json:"error"`
}

type Client struct {
	Binary string
	Brand  string
}

func New(brand string) Client {
	if brand != BrandLark {
		brand = BrandFeishu
	}
	return Client{Binary: "lark-cli", Brand: brand}
}

// Env is a whitelist of host variables plus the per-call app credentials. HOME
// and every lark-cli directory point into dir.
func Env(appID, secret, brand, dir string) []string {
	var env []string
	for _, name := range []string{"PATH", "TMPDIR", "LANG", "LC_ALL", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env,
		"HOME="+dir,
		"LARKSUITE_CLI_CONFIG_DIR="+filepath.Join(dir, "config"),
		"LARKSUITE_CLI_DATA_DIR="+filepath.Join(dir, "data"),
		"LARKSUITE_CLI_LOG_DIR="+filepath.Join(dir, "log"),
		"LARKSUITE_CLI_APP_ID="+appID,
		"LARKSUITE_CLI_APP_SECRET="+secret,
		"LARKSUITE_CLI_BRAND="+brand,
		"LARKSUITE_CLI_DEFAULT_AS=bot",
		"LARKSUITE_CLI_STRICT_MODE=bot",
		"LARKSUITE_CLI_NO_UPDATE_NOTIFIER=1",
		"LARKSUITE_CLI_NO_SKILLS_NOTIFIER=1",
	)
}

// ParseEnvelope finds the JSON envelope in lark-cli output. Warnings such as
// "warning: bot_strip_p2p: ..." may precede it, and errors are pretty-printed
// over several lines, so every line that opens a JSON object is tried in turn.
func ParseEnvelope(raw []byte) (Envelope, bool) {
	for offset := 0; offset < len(raw); {
		line := raw[offset:]
		if trimmed := bytes.TrimLeft(line, " \t"); len(trimmed) > 0 && trimmed[0] == '{' {
			var envelope Envelope
			if json.NewDecoder(bytes.NewReader(line)).Decode(&envelope) == nil && (envelope.OK || envelope.Error != nil) {
				return envelope, true
			}
		}
		next := bytes.IndexByte(line, '\n')
		if next < 0 {
			break
		}
		offset += next + 1
	}
	return Envelope{}, false
}

// Run executes one bot-identity command in a throwaway directory and returns the
// envelope's data. It returns *Failure for typed CLI errors.
func (c Client) Run(ctx context.Context, appID, secret string, stdin []byte, args ...string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "agentflow-lark-")
	if err != nil {
		return nil, errors.New("lark-cli 临时目录不可用")
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, c.Binary, append(args, "--as", "bot", "--format", "json")...)
	cmd.Dir = dir
	cmd.Env = Env(appID, secret, c.Brand, dir)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, ErrTimeout
	}
	if errors.Is(runErr, exec.ErrNotFound) || errors.Is(runErr, fs.ErrNotExist) {
		return nil, ErrNotInstalled
	}
	if envelope, ok := ParseEnvelope(out.Bytes()); ok && envelope.OK && runErr == nil {
		return envelope.Data, nil
	}
	if envelope, ok := ParseEnvelope(errOut.Bytes()); ok && envelope.Error != nil {
		return nil, envelope.Error
	}
	return nil, ErrUnreadable
}

// MaxPostBytes keeps a rich-text message under Lark's 30 KB request limit with room for the envelope.
const MaxPostBytes = 28 << 10

// Post wraps Markdown as a one-element `post` message. Callers pass it through
// stdin (`--content -`) so text is never read as an `@file` path, and remote
// image links stay links instead of being downloaded server-side.
func Post(markdown string) []byte {
	markdown = Truncate(markdown, MaxPostBytes)
	post, _ := json.Marshal(map[string]any{"zh_cn": map[string]any{"content": [][]map[string]string{{{"tag": "md", "text": markdown}}}}})
	return post
}

// Truncate shortens text to at most limit bytes on a rune boundary, marking the cut.
func Truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit - len("…")
	for cut > 0 && !utf8RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ValidChatID and ValidMessageID guard IDs that end up in argv or a request path.
func ValidChatID(id string) bool    { return strings.HasPrefix(id, "oc_") && validIDRunes(id) }
func ValidMessageID(id string) bool { return strings.HasPrefix(id, "om_") && validIDRunes(id) }
func validIDRunes(id string) bool {
	if len(id) > 191 {
		return false
	}
	for _, r := range id {
		if !(r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 2<<20 {
		return 0, errors.New("lark-cli output too large")
	}
	return b.Buffer.Write(p)
}
