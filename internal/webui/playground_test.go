package webui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llm-router/internal/inference"
)

func playTestHandler(protocol string, tools bool, run inference.Run) *Handler {
	return &Handler{deps: Dependencies{Snapshot: func() Status {
		return Status{Aliases: []Alias{{Name: "chat", Protocol: protocol, ClientTools: tools}}}
	}, Run: run}}
}

func playPost(h *Handler, s *browserSession, path string, values url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.servePlayground(rec, req, s)
	return rec
}

func playSendTest(h *Handler, s *browserSession, prompt string) {
	view := h.playView(s)
	playPost(h, s, "/ui/playground/send", url.Values{"conversation_id": {view.ConversationID}, "alias": {"chat"}, "prompt": {prompt}})
}

func playWait(t *testing.T, h *Handler, s *browserSession) PlayView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v := h.playView(s)
		if !v.IsRunning() {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("inference did not finish")
	return PlayView{}
}

func TestPlaygroundTwoTurns(t *testing.T) {
	requests := make(chan inference.Request, 2)
	h := playTestHandler("openai-completions", false, func(ctx context.Context, r inference.Request, emit inference.Emit) (inference.Result, error) {
		requests <- r
		_ = emit(inference.Event{Kind: inference.TextDelta, Text: "reply"})
		return inference.Result{Text: "reply"}, nil
	})
	s := &browserSession{}
	playSendTest(h, s, "first")
	first := playWait(t, h, s)
	playSendTest(h, s, "second")
	second := playWait(t, h, s)
	a, b := <-requests, <-requests
	if a.ConversationID == "" || a.ConversationID != b.ConversationID || len(b.Messages) != 3 || b.Messages[1].Content != "reply" {
		t.Fatalf("lost typed conversation: %#v %#v", a, b)
	}
	if len(first.Messages) != 2 || len(second.Messages) != 4 || second.Messages[1].Text != "reply" || second.Messages[3].Text != "reply" {
		t.Fatalf("duplicated result text: %#v", second.Messages)
	}
	if len(second.HistoryMessages()) != 2 || len(second.ActiveMessages()) != 2 {
		t.Fatal("completed turn not separated from active turn")
	}
}

func TestPlaygroundCancellationStaleCallbacksAndIsolation(t *testing.T) {
	started := make(chan inference.Emit, 2)
	release := make(chan struct{})
	canceled := make(chan struct{}, 2)
	h := playTestHandler("openai-completions", false, func(ctx context.Context, r inference.Request, emit inference.Emit) (inference.Result, error) {
		started <- emit
		<-ctx.Done()
		canceled <- struct{}{}
		<-release
		_ = emit(inference.Event{Kind: inference.TextDelta, Text: "stale"})
		return inference.Result{Text: "stale"}, nil
	})
	a, b := &browserSession{}, &browserSession{}
	playSendTest(h, a, "a")
	emit := <-started
	playSendTest(h, b, "b")
	<-started
	av, bv := h.playView(a), h.playView(b)
	if av.ConversationID == bv.ConversationID {
		t.Fatal("shared conversation ID")
	}
	playPost(h, a, "/ui/playground/stop", url.Values{"conversation_id": {bv.ConversationID}})
	if !h.playView(a).IsRunning() {
		t.Fatal("cross-session stop accepted")
	}
	playPost(h, a, "/ui/playground/stop", url.Values{"conversation_id": {av.ConversationID}})
	<-canceled
	if err := emit(inference.Event{Kind: inference.TextDelta, Text: "stale"}); err == nil {
		t.Fatal("stopped callback accepted")
	}
	if h.playView(a).State != Stopped || !h.playView(b).IsRunning() {
		t.Fatal("cancellation leaked")
	}
	playPost(h, a, "/ui/playground/new", url.Values{"conversation_id": {av.ConversationID}})
	if err := emit(inference.Event{Kind: inference.TextDelta, Text: "stale"}); err == nil {
		t.Fatal("superseded callback accepted")
	}
	if len(h.playView(a).Messages) != 0 {
		t.Fatal("new conversation contaminated")
	}
	h.closePlayground(b)
	<-canceled
	close(release)
}

type disconnectAfterFlush struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *disconnectAfterFlush) Flush() {
	w.ResponseRecorder.Flush()
	w.cancel()
}

func TestPlaygroundObserverReconnectDoesNotRun(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	h := playTestHandler("openai-completions", false, func(ctx context.Context, r inference.Request, emit inference.Emit) (inference.Result, error) {
		calls.Add(1)
		_ = emit(inference.Event{Kind: inference.TextDelta, Text: "partial"})
		close(started)
		<-ctx.Done()
		return inference.Result{}, ctx.Err()
	})
	s := &browserSession{}
	playSendTest(h, s, "hello")
	<-started
	id := h.playView(s).ConversationID
	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "/ui/playground/stream?conversation_id="+id, nil).WithContext(ctx)
		rec := &disconnectAfterFlush{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
		h.playStream(rec, req, s)
		cancel()
		if !strings.Contains(rec.Body.String(), "event: history") || !strings.Contains(rec.Body.String(), "partial") {
			t.Fatal("reconnect did not restore transcript")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("observer resubmitted inference")
	}
	h.closePlayground(s)
}

func TestPlaygroundTerminalResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result inference.Result
		err    error
		state  ConversationState
	}{
		{name: "tools", result: inference.Result{ToolCalls: []inference.ToolCall{{ID: "call", Name: "external"}}}, state: AwaitingTools},
		{name: "failure", err: errors.New("provider failed"), state: Failed},
		{name: "oversize", result: inference.Result{Text: strings.Repeat("x", inference.MaxResponseBytes+1)}, state: Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			h := playTestHandler("anthropic-messages", true, func(ctx context.Context, r inference.Request, e inference.Emit) (inference.Result, error) {
				calls.Add(1)
				if r.Stream {
					t.Error("client tools request streamed")
				}
				return tc.result, tc.err
			})
			s := &browserSession{}
			playSendTest(h, s, "hello")
			if v := playWait(t, h, s); v.State != tc.state {
				t.Fatalf("state %v, want %v", v.State, tc.state)
			}
			playSendTest(h, s, "again")
			if calls.Load() != 1 {
				t.Fatal("terminal conversation resumed")
			}
		})
	}
}

func TestPlaygroundPreviousTurnCallbackRejected(t *testing.T) {
	callbacks := make(chan inference.Emit, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	h := playTestHandler("openai-completions", false, func(ctx context.Context, r inference.Request, emit inference.Emit) (inference.Result, error) {
		callbacks <- emit
		if calls.Add(1) == 2 {
			<-release
		}
		return inference.Result{Text: "reply", HasUsage: true}, nil
	})
	s := &browserSession{}
	playSendTest(h, s, "first")
	old := <-callbacks
	v := playWait(t, h, s)
	if len(v.Messages) != 3 || v.Messages[2].Role != "usage" {
		t.Fatal("normalized usage was not displayed")
	}
	playSendTest(h, s, "second")
	<-callbacks
	if err := old(inference.Event{Kind: inference.TextDelta, Text: "stale"}); err == nil {
		t.Fatal("previous turn callback modified the current turn")
	}
	close(release)
	playWait(t, h, s)
}
