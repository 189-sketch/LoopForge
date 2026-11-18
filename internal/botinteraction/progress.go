package botinteraction

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"agentflow/internal/domain"
)

type progressKey struct{}

// ReportProgress publishes a public status only; never pass model thoughts,
// tool arguments, credentials or uncommitted answer text.
func ReportProgress(ctx context.Context, status string) {
	if publish, ok := ctx.Value(progressKey{}).(func(string)); ok {
		publish(status)
	}
}

type responseStream struct {
	mu       sync.Mutex
	started  bool
	disabled bool
	finished bool
}

func (s *socket) stream(ctx context.Context, item domain.BotInteraction, content string, finish bool) error {
	return s.streamWithState(ctx, item, content, finish, s.streamState(item.ID))
}

func (s *socket) streamState(id string) *responseStream {
	value, _ := s.streams.LoadOrStore(id, &responseStream{})
	return value.(*responseStream)
}

func (s *socket) streamWithState(ctx context.Context, item domain.BotInteraction, content string, finish bool, state *responseStream) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.finished {
		return nil
	}
	if state.disabled || item.RequestID == "" {
		if finish {
			return errorsStreamUnavailable
		}
		return nil
	}
	if state.started && content == "已收到，正在处理…" {
		return nil
	}
	err := s.request(ctx, "aibot_respond_msg", item.RequestID, map[string]any{
		"msgtype": "stream", "stream": map[string]any{"id": item.ID, "content": content, "finish": finish},
	})
	if err != nil {
		var business *ReplyError
		state.disabled = errors.As(err, &business)
		return err
	}
	state.started = true
	state.finished = finish
	return nil
}

var errorsStreamUnavailable = errors.New("流式回复不可用")

// progressTurn keeps slow-model and queued work visible in one reply bubble.
func (s *socket) progressTurn(ctx context.Context, item domain.BotInteraction) (context.Context, func()) {
	// Initial receipt may already have been sent by the callback reader.
	go func() { _ = s.stream(ctx, item, "已收到，正在处理…", false) }()
	return runProgress(ctx, 8*time.Second, func(text string) { _ = s.stream(ctx, item, text, false) })
}

func progressText(phase string, waited time.Duration) string {
	return fmt.Sprintf("%s…（已等待 %d 秒）", phase, int(waited.Seconds()))
}
