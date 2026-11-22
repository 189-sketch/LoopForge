package botinteraction

import (
	"agentflow/internal/domain"
	"context"
	"log"
	"time"
)

// channel is one live platform connection that Service.process drives: the
// WeCom AI Bot WebSocket, or a Feishu/Lark `lark-cli event consume` stream.
// A platform rejection of a reply is returned as *ReplyError so the message is
// marked failed; any other error drops the connection and reconnects.
type channel interface {
	closed() <-chan struct{}
	close()
	// acknowledge shows a "received" receipt for a new chat message.
	acknowledge(ctx context.Context, item domain.BotInteraction)
	// progressTurn keeps a running turn visible; the returned func stops updates.
	progressTurn(ctx context.Context, item domain.BotInteraction) (context.Context, func())
	// reply delivers the final answer to a chat message, replacing its receipt where the platform allows.
	reply(ctx context.Context, item domain.BotInteraction, conv domain.BotConversation, text string) error
	send(ctx context.Context, chatID, text string) error
	// sendCard sends an action card; card uses the WeCom template_card shape, including task_id.
	sendCard(ctx context.Context, chatID string, card map[string]any) error
	// finishActionCard marks a clicked action card as handled with the result.
	finishActionCard(ctx context.Context, item domain.BotInteraction, result string) error
	// forget releases per-message reply state.
	forget(id string)
}

// accept queues a normalized message and acknowledges plain chat messages.
func (s *Service) accept(ctx context.Context, item domain.BotInteraction, kind, chat string, ch channel) {
	inserted, err := s.Store.EnqueueBotInteraction(item, kind, chat)
	if err != nil || !inserted {
		if err != nil {
			log.Printf("bot event enqueue failed action=%s", item.Action)
		}
		return
	}
	// Card operations are direct engine controls. Reply with the actual result
	// rather than consuming the callback with a receipt-only update.
	if item.Action == "" {
		ch.acknowledge(ctx, item)
	}
}

// runProgress publishes "phase…(waited N s)" every interval until stopped.
// Status publication is non-blocking so network delays cannot stall model tools.
func runProgress(ctx context.Context, every time.Duration, update func(string)) (context.Context, func()) {
	updates := make(chan string, 1)
	publish := func(status string) {
		select {
		case updates <- status:
		default:
		}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(stopped)
		phase := "正在理解你的请求"
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case phase = <-updates:
				// The next tick coalesces rapid tool events and bounds update frequency.
			case <-ticker.C:
				update(progressText(phase, time.Since(started)))
			}
		}
	}()
	return context.WithValue(ctx, progressKey{}, publish), func() { close(done); <-stopped }
}
