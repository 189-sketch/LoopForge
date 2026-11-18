package larkcli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Consumer streams one EventKey from `lark-cli event consume` as NDJSON.
//
// lark-cli forks a per-app bus daemon (its own session) that holds the long
// connection, and every consumer of the same config dir shares it. A consumer
// exits gracefully when its stdin closes, which is how Stop ends it.
type Consumer struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}
	stop  sync.Once
}

// ReadyTimeout bounds how long Consume waits for the bus handshake.
var ReadyTimeout = 60 * time.Second

// Consume starts a consumer in dir, which must outlive it (the bus keeps its
// socket there), and returns once lark-cli reports the subscription is ready.
// onEvent receives each event line; it runs on the reader goroutine.
func (c Client) Consume(ctx context.Context, appID, secret, dir, key string, onEvent func([]byte)) (*Consumer, error) {
	cmd := exec.Command(c.Binary, "event", "consume", key, "--as", "bot")
	cmd.Dir = dir
	cmd.Env = Env(appID, secret, c.Brand, dir)
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotInstalled
		}
		return nil, err
	}
	consumer := &Consumer{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	ready := make(chan struct{})
	var readyOnce sync.Once
	var tail boundedTail
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "[event] ready") {
				readyOnce.Do(func() { close(ready) })
			}
			tail.add(line)
		}
	}()
	go func() {
		defer readers.Done()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		for scanner.Scan() {
			if line := bytes.TrimSpace(scanner.Bytes()); len(line) > 0 {
				onEvent(append([]byte(nil), line...))
			}
		}
		// Drain anything past an oversized line so the process never blocks on a full pipe.
		_, _ = io.Copy(io.Discard, stdout)
	}()
	go func() {
		readers.Wait()
		_ = cmd.Wait()
		close(consumer.done)
	}()
	timer := time.NewTimer(ReadyTimeout)
	defer timer.Stop()
	select {
	case <-ready:
	case <-consumer.done:
		if envelope, ok := ParseEnvelope(tail.bytes()); ok && envelope.Error != nil {
			return nil, envelope.Error
		}
		return nil, ErrUnreadable
	case <-timer.C:
		consumer.Stop()
		return nil, ErrTimeout
	case <-ctx.Done():
		consumer.Stop()
		return nil, ctx.Err()
	}
	go func() {
		select {
		case <-ctx.Done():
			consumer.Stop()
		case <-consumer.done:
		}
	}()
	return consumer, nil
}

// Done is closed once the consumer process has exited.
func (c *Consumer) Done() <-chan struct{} { return c.done }

// Stop closes stdin for a graceful exit, then escalates to the process group.
// The bus daemon runs in its own session and is stopped separately (StopBus).
func (c *Consumer) Stop() {
	c.stop.Do(func() {
		_ = c.stdin.Close()
		for _, step := range []func(){func() { signalGroup(c.cmd, false) }, func() { signalGroup(c.cmd, true) }} {
			select {
			case <-c.done:
				return
			case <-time.After(5 * time.Second):
				step()
			}
		}
		<-c.done
	})
}

// StopBus asks the bus daemon of dir to exit, releasing the app's long connection
// now instead of after its idle timeout. Errors are ignored: the bus also exits on its own.
func (c Client) StopBus(appID, secret, dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Binary, "event", "stop", "--app-id", appID, "--json")
	cmd.Dir = dir
	cmd.Env = Env(appID, secret, c.Brand, dir)
	_ = cmd.Run()
}

// boundedTail keeps the last 64 KB of stderr for error envelopes.
type boundedTail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *boundedTail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(append(t.buf, line...), '\n')
	if len(t.buf) > 64<<10 {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-(64<<10):]...)
	}
}
func (t *boundedTail) bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}
