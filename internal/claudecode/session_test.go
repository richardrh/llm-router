package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-router/internal/config"
)

const sessionTestTimeout = 3 * time.Second

func sessionTestManager(t *testing.T, script string, idle time.Duration, args ...string) *SessionManager {
	t.Helper()
	manager := NewSessionManager(Upstream{Command: append([]string{script}, args...)}, 1, idle)
	t.Cleanup(manager.CloseAll)
	return manager
}

func sessionTestRun(t *testing.T, manager *SessionManager, key, first, next string) Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sessionTestTimeout)
	defer cancel()
	run, err := manager.Run(ctx, key, "model", first, next, nil)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func sessionTestWaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(sessionTestTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("CLI subprocess did not become ready")
		}
	}
}

func TestSessionManagerReclaimsExpiredCapacity(t *testing.T) {
	script := cliTestScript(t, `
while IFS= read -r input; do
  printf '%s\n' '{"type":"result","session_id":"saved-session","result":"ok"}'
done`)
	const idle = 100 * time.Millisecond
	manager := sessionTestManager(t, script, idle)
	sessionTestRun(t, manager, "first-key", "first", "first")
	time.Sleep(2 * idle)
	if got := sessionTestRun(t, manager, "different-key", "second", "second").Text; got != "ok" {
		t.Fatalf("new session result = %q", got)
	}
}

func TestSessionManagerProtectsActiveTurnsAndCancelsQueuedTurns(t *testing.T) {
	script := cliTestScript(t, `
while IFS= read -r input; do
  printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"ready"}}}'
  case "$input" in
    *continuation*) result=continued ;;
    *) result=initial ;;
  esac
  printf '{"type":"result","session_id":"saved-session","result":"%s"}\n' "$result"
done`)
	const idle = 100 * time.Millisecond
	manager := sessionTestManager(t, script, idle)
	ready := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	finished := make(chan error, 1)
	go func() {
		_, err := manager.Run(context.Background(), "same-key", "model", "initial", "initial", func(string) error {
			close(ready)
			<-release
			return nil
		})
		finished <- err
	}()
	select {
	case <-ready:
	case <-time.After(sessionTestTimeout):
		t.Fatal("active turn did not stream")
	}
	time.Sleep(2 * idle)
	if _, err := manager.Run(context.Background(), "different-key", "model", "other", "other", nil); err == nil {
		t.Fatal("active session was evicted to admit a different key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	queued := make(chan error, 1)
	go func() {
		_, err := manager.Run(ctx, "same-key", "model", "history", "queued", nil)
		queued <- err
	}()
	select {
	case err := <-queued:
		if err != context.DeadlineExceeded {
			t.Fatalf("queued cancellation = %v", err)
		}
	case <-time.After(sessionTestTimeout):
		t.Fatal("queued cancellation waited for the active turn")
	}
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(sessionTestTimeout):
		t.Fatal("active turn did not finish")
	}
	if got := sessionTestRun(t, manager, "same-key", "replayed-history", "continuation").Text; got != "continued" {
		t.Fatalf("completed active session was expired or canceled: %q", got)
	}
}

func TestSessionManagerCancellationRecoversCapacity(t *testing.T) {
	for _, nextKey := range []string{"same-key", "different-key"} {
		t.Run(nextKey, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "started")
			script := cliTestScript(t, `
if [ ! -e "$1" ]; then
  : > "$1"
  IFS= read -r input
  printf '%s\n' '{"type":"stream_event","session_id":"saved-session","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"ready"}}}'
  while :; do sleep 1; done
fi
while IFS= read -r input; do
  printf '%s\n' '{"type":"result","result":"recovered"}'
done`)
			manager := sessionTestManager(t, script, 0, state)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := manager.Run(ctx, "same-key", "model", "first", "first", func(string) error {
				cancel()
				return nil
			})
			if err != context.Canceled {
				t.Fatalf("cancellation = %v", err)
			}
			if got := sessionTestRun(t, manager, nextKey, "history", "next").Text; got != "recovered" {
				t.Fatalf("recovery result = %q", got)
			}
		})
	}
}

func TestSessionManagerCancellationInterruptsInputWrite(t *testing.T) {
	state := filepath.Join(t.TempDir(), "not-reading")
	script := cliTestScript(t, `
if [ ! -e "$1" ]; then
  : > "$1"
  while :; do sleep 1; done
fi
while IFS= read -r input; do
  printf '%s\n' '{"type":"result","result":"recovered"}'
done`)
	manager := sessionTestManager(t, script, 0, state)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := manager.Run(ctx, "same-key", "model", strings.Repeat("x", 1<<20), "next", nil)
		finished <- err
	}()
	sessionTestWaitFile(t, state)
	cancel()
	select {
	case err := <-finished:
		if err != context.Canceled {
			t.Fatalf("input cancellation = %v", err)
		}
	case <-time.After(sessionTestTimeout):
		t.Fatal("cancellation did not interrupt the blocked CLI stdin write")
	}
	if got := sessionTestRun(t, manager, "same-key", "history", "next").Text; got != "recovered" {
		t.Fatalf("recovery result = %q", got)
	}
}

func TestSessionManagerQueuedTurnRestartsAfterCancellation(t *testing.T) {
	state := filepath.Join(t.TempDir(), "started")
	script := cliTestScript(t, `
if [ ! -e "$1" ]; then
  : > "$1"
  IFS= read -r input
  printf '%s\n' '{"type":"stream_event","session_id":"saved-session","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"ready"}}}'
  while :; do sleep 1; done
fi
while IFS= read -r input; do
  printf '%s\n' '{"type":"result","result":"recovered"}'
done`)
	manager := sessionTestManager(t, script, 0, state)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := manager.Run(ctx, "same-key", "model", "first", "first", func(string) error {
			close(ready)
			return nil
		})
		first <- err
	}()
	select {
	case <-ready:
	case <-time.After(sessionTestTimeout):
		t.Fatal("first turn did not stream")
	}
	nextCtx, nextCancel := context.WithTimeout(context.Background(), sessionTestTimeout)
	defer nextCancel()
	type outcome struct {
		run Run
		err error
	}
	second := make(chan outcome, 1)
	go func() {
		run, err := manager.Run(nextCtx, "same-key", "model", "history", "next", nil)
		second <- outcome{run, err}
	}()
	cancel()
	select {
	case err := <-first:
		if err != context.Canceled {
			t.Fatalf("first cancellation = %v", err)
		}
	case <-time.After(sessionTestTimeout):
		t.Fatal("first cancellation did not finish")
	}
	select {
	case got := <-second:
		if got.err != nil || got.run.Text != "recovered" {
			t.Fatalf("queued recovery = %#v, error = %v", got.run, got.err)
		}
	case <-time.After(sessionTestTimeout):
		t.Fatal("queued turn did not recover")
	}
}

func TestSessionManagerBrokenPipeRecoversCapacity(t *testing.T) {
	for _, nextKey := range []string{"same-key", "different-key"} {
		t.Run(nextKey, func(t *testing.T) {
			script := cliTestScript(t, `
IFS= read -r input
exec 0<&-
printf '%s\n' '{"type":"result","session_id":"saved-session","result":"ok"}'
while :; do sleep 1; done`)
			manager := sessionTestManager(t, script, 0)
			sessionTestRun(t, manager, "same-key", "first", "first")
			ctx, cancel := context.WithTimeout(context.Background(), sessionTestTimeout)
			defer cancel()
			if _, err := manager.Run(ctx, "same-key", "model", "history", "second", nil); err == nil {
				t.Fatal("closed CLI stdin did not fail")
			}
			if got := sessionTestRun(t, manager, nextKey, "history", "third").Text; got != "ok" {
				t.Fatalf("broken-pipe recovery result = %q", got)
			}
		})
	}
}

func TestSessionManagerCallbackFailureDoesNotPoisonNextTurn(t *testing.T) {
	state := filepath.Join(t.TempDir(), "started")
	script := cliTestScript(t, `
if [ ! -e "$1" ]; then
  : > "$1"
  IFS= read -r input
  printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"reject"}}}'
  printf '%s\n' '{"type":"result","result":"stale"}'
fi
while IFS= read -r input; do
  printf '%s\n' '{"type":"result","result":"fresh"}'
done`)
	manager := sessionTestManager(t, script, 0, state)
	callbackErr := errors.New("consumer stopped streaming")
	ctx, cancel := context.WithTimeout(context.Background(), sessionTestTimeout)
	defer cancel()
	_, err := manager.Run(ctx, "same-key", "model", "first", "first", func(string) error { return callbackErr })
	if err != callbackErr {
		t.Fatalf("callback error = %v", err)
	}
	if got := sessionTestRun(t, manager, "same-key", "history", "next").Text; got != "fresh" {
		t.Fatalf("next turn consumed abandoned protocol output: %q", got)
	}
}

func TestSessionManagerResumeSendsPendingToolResultAndPreservesID(t *testing.T) {
	state := filepath.Join(t.TempDir(), "started")
	script := cliTestScript(t, `
state=$1
shift
resumed=false
while [ "$#" -gt 0 ]; do
  if [ "$1" = --resume ]; then
    shift
    [ "$1" = saved-session ] || exit 7
    resumed=true
  fi
  shift
done
if [ ! -e "$state" ]; then
  : > "$state"
  [ "$resumed" = false ] || exit 8
  IFS= read -r input
  printf '%s\n' '{"type":"result","session_id":"saved-session","result":"{\"kind\":\"client_tool_request\",\"id\":\"call-1\",\"name\":\"lookup\",\"arguments\":{}}"}'
  while :; do sleep 1; done
fi
[ "$resumed" = true ] || exit 9
IFS= read -r input
case "$input" in
  *'External tool result (call-1): found'*) result=continued ;;
  *'newest-user-turn'*) result=next ;;
  *) result=replayed-history ;;
esac
exec 0<&-
printf '%s\n' '{"type":"result","result":"{\"kind\":\"final_response\",\"text\":\"'"$result"'\"}"}'
while :; do sleep 1; done`)
	const idle = 100 * time.Millisecond
	manager := NewSessionManager(Upstream{Command: []string{script, state}, ToolMode: config.CLIToolsClient}, 1, idle)
	t.Cleanup(manager.CloseAll)
	initial := sessionTestRun(t, manager, "same-key", "full initial history", "first user turn")
	if initial.ToolCall == nil || initial.ToolCall.ID != "call-1" {
		t.Fatalf("initial tool request = %#v", initial.ToolCall)
	}
	time.Sleep(2 * idle)
	if got := sessionTestRun(t, manager, "same-key", "full history omitting pending tool output", "External tool result (call-1): found").Text; got != "continued" {
		t.Fatalf("resumed tool continuation = %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionTestTimeout)
	defer cancel()
	if _, err := manager.Run(ctx, "same-key", "model", "full history", "next", nil); err == nil {
		t.Fatal("closed resumed stdin did not fail")
	}
	if got := sessionTestRun(t, manager, "same-key", "full history", "newest-user-turn").Text; got != "next" {
		t.Fatalf("restarted continuation lost its saved session ID or replayed history: %q", got)
	}
}

func TestSessionManagerErrorResultKeepsContinuationAndRefreshesIdleTime(t *testing.T) {
	script := cliTestScript(t, `
while IFS= read -r input; do
  case "$input" in
    *continuation*)
      printf '%s\n' '{"type":"result","result":"continued"}'
      ;;
    *)
      printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"ready"}}}'
      printf '%s\n' '{"type":"result","session_id":"saved-session","is_error":true,"result":"rejected"}'
      ;;
  esac
done`)
	const idle = 100 * time.Millisecond
	manager := sessionTestManager(t, script, idle)
	ctx, cancel := context.WithTimeout(context.Background(), sessionTestTimeout)
	defer cancel()
	_, err := manager.Run(ctx, "same-key", "model", "initial", "initial", func(string) error {
		time.Sleep(2 * idle)
		return nil
	})
	if err == nil {
		t.Fatal("CLI error result was not reported")
	}
	if got := sessionTestRun(t, manager, "same-key", "replayed-history", "continuation").Text; got != "continued" {
		t.Fatalf("completed error result lost its continuation state: %q", got)
	}
}
