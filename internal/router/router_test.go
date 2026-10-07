package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/richardrh/llm-router/internal/usage"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUpstream is a controllable OpenAI-compatible endpoint. It records the
// model ids it was asked for so tests can assert on routing decisions rather
// than on log output.
type fakeUpstream struct {
	name string
	srv  *httptest.Server

	mu       sync.Mutex
	requests []seenRequest
	status   atomic.Int64
	// stream, when true, replies with an SSE body.
	stream atomic.Bool
	// chunks is the number of SSE events emitted before the terminator.
	chunks atomic.Int64
	// streamUsage, when true, appends a usage-bearing chunk before the
	// terminator, as an OpenAI-wire upstream does when stream_options asks for
	// one. Left false, the upstream reports nothing, which is the case the
	// router must not invent a cost for.
	streamUsage atomic.Bool
	// anthropic, when true, replies in the native Messages shape — and, when
	// streaming, with Anthropic SSE events — rather than the OpenAI shape, so
	// translation can be exercised against a faithful upstream rather than a
	// double-translated one.
	anthropic atomic.Bool
	// gate, when set, blocks the handler until closed.
	gate chan struct{}
}

type seenRequest struct {
	Model string
	Body  map[string]any
	Auth  string
	// Path is the upstream endpoint that was called, which tells a translated
	// request from a passed-through one.
	Path string
	// APIKey and Version record the Anthropic credential headers, which the
	// router must set only for an anthropic-style upstream.
	APIKey  string
	Version string
	HasSess bool
}

func newFakeUpstream(t *testing.T, name string) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{name: name}
	u.status.Store(http.StatusOK)
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.requests = append(u.requests, seenRequest{
			Model:   asString(body["model"]),
			Body:    body,
			Auth:    r.Header.Get("Authorization"),
			Path:    r.URL.Path,
			APIKey:  r.Header.Get("x-api-key"),
			Version: r.Header.Get("anthropic-version"),
			HasSess: r.Header.Get("X-Client-Session") != "",
		})
		u.mu.Unlock()

		if u.gate != nil {
			<-u.gate
		}

		if code := u.status.Load(); code != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(code))
			_, _ = w.Write([]byte(`{"error":{"message":"upstream said no"}}`))
			return
		}

		if u.stream.Load() {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			if u.anthropic.Load() {
				for _, event := range []string{
					"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-opus-5-5\",\"usage\":{\"input_tokens\":7,\"output_tokens\":1}}}\n\n",
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"toka\"}}\n\n",
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n",
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
				} {
					_, _ = io.WriteString(w, event)
					if flusher != nil {
						flusher.Flush()
					}
				}
				return
			}
			n := u.chunks.Load()
			if n == 0 {
				n = 3
			}
			for i := range n {
				_, _ = io.WriteString(w, `data: {"id":"1","choices":[{"delta":{"content":"tok`+string(rune('a'+i))+`"}}]}`+"\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
			if u.streamUsage.Load() {
				_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}

		if u.anthropic.Load() {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          "msg_1",
				"type":        "message",
				"role":        "assistant",
				"model":       "claude-opus-5-5",
				"content":     []any{map[string]any{"type": "text", "text": "hi"}},
				"stop_reason": "end_turn",
				"usage":       map[string]any{"input_tokens": 7, "output_tokens": 3},
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-" + u.name,
			"model":   asString(body["model"]),
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "hi"}}},
			"usage":   map[string]any{"prompt_tokens": 7, "completion_tokens": 3},
		})
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func (u *fakeUpstream) seen() []seenRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]seenRequest, len(u.requests))
	copy(out, u.requests)
	return out
}

func (u *fakeUpstream) hits() int { return len(u.seen()) }

// newTestServer builds a router over the given upstreams with one alias whose
// targets mirror the supplied weights.
func newTestServer(t *testing.T, ups map[string]*fakeUpstream, weights map[string]int, mutate func(*Config, *Alias)) *Server {
	t.Helper()
	srv, _ := newTestServerLogging(t, ups, weights, mutate)
	return srv
}

// newTestServerLogging is newTestServer with the log captured, for tests that
// assert on the accounting line rather than on routing.
func newTestServerLogging(t *testing.T, ups map[string]*fakeUpstream, weights map[string]int, mutate func(*Config, *Alias)) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Defaults: Defaults{
			MaxAttempts:       3,
			Sticky:            true,
			FirstByteTimeout:  Duration(5 * time.Second),
			StreamIdleTimeout: Duration(5 * time.Second),
			MaxStreamDuration: Duration(30 * time.Second),
			MaxBodyBytes:      1 << 20,
			BreakerFailures:   100, // keep the breaker out of the way by default
			BreakerCooldown:   Duration(10 * time.Millisecond),
			RateLimitCooldown: Duration(50 * time.Millisecond),
			// Mirrors what applyDefaults would set, since this harness builds a
			// Config directly rather than loading one.
			CacheAffinityTTL: Duration(5 * time.Minute),
		},
		Upstreams: map[string]Upstream{},
		Models:    map[string]Alias{},
	}
	alias := Alias{API: APIOpenAICompletions, ContextWindow: 128000, MaxOutputTokens: 8192}
	for name, up := range ups {
		cfg.Upstreams[name] = Upstream{BaseURL: up.srv.URL + "/v1", APIKey: "sk-" + name, MaxConcurrency: 8}
		alias.Targets = append(alias.Targets, Target{Upstream: name, Model: "real/" + name, Weight: weights[name]})
	}
	if mutate != nil {
		mutate(cfg, &alias)
	}
	cfg.Models["test-alias"] = alias

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	r, err := NewRouter(cfg, log)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return NewServer(cfg, r, log, nil), &logs
}

func post(t *testing.T, srv *Server, path string, body map[string]any, headers map[string]string) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

// TestAliasRewrittenPerTarget is the core promise: one client-visible alias,
// different real model ids per upstream.
func TestAliasRewrittenPerTarget(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 0}, nil)

	// Weight 0 on b forces the deterministic path: a is picked.
	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	got := a.seen()
	if len(got) != 1 {
		t.Fatalf("upstream a hits = %d, want 1", len(got))
	}
	if got[0].Model != "real/a" {
		t.Errorf("upstream model = %q, want real/a", got[0].Model)
	}
	if got[0].Auth != "Bearer sk-a" {
		t.Errorf("auth = %q, want Bearer sk-a", got[0].Auth)
	}
	if b.hits() != 0 {
		t.Errorf("upstream b hits = %d, want 0", b.hits())
	}
}

// TestFailoverOnRetryableStatus is the availability promise: a provider saying
// "not me" moves the request to the next target before anything is committed.
func TestFailoverOnRetryableStatus(t *testing.T) {
	for _, code := range []int64{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusUnauthorized} {
		a := newFakeUpstream(t, "a")
		b := newFakeUpstream(t, "b")
		a.status.Store(code)
		a.gate = nil

		srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 0}, nil)
		res := post(t, srv, "/v1/chat/completions", map[string]any{
			"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)

		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d for upstream %d, want 200 (failover failed)", res.StatusCode, code)
		}
		if a.hits() != 1 {
			t.Errorf("upstream a hits = %d, want 1", a.hits())
		}
		if b.hits() != 1 {
			t.Errorf("upstream b hits = %d, want 1", b.hits())
		}
	}
}

// TestNoFailoverOnClientError: a 400 is the client's bug and would fail
// everywhere, so it must be returned rather than replayed.
func TestNoFailoverOnClientError(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	a.status.Store(http.StatusBadRequest)

	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 0}, nil)
	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if b.hits() != 0 {
		t.Errorf("upstream b hits = %d, want 0 (400 must not fail over)", b.hits())
	}
}

// TestAllTargetsFail surfaces the failure rather than hanging or reporting a
// misleading success.
func TestAllTargetsFail(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	a.status.Store(http.StatusInternalServerError)
	b.status.Store(http.StatusServiceUnavailable)

	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 1}, nil)
	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)

	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.StatusCode)
	}
}

// TestStickySessionAffinity: successive turns of one session must stay on one
// upstream so provider-side prompt caches keep hitting.
func TestStickySessionAffinity(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	// Equal weights would otherwise scatter turns across both.
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 1}, nil)

	hdr := map[string]string{"X-OMP-Session": "session-42"}
	for range 12 {
		res := post(t, srv, "/v1/chat/completions", map[string]any{
			"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "next turn"}},
		}, hdr)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
	}

	aHits, bHits := a.hits(), b.hits()
	if aHits+bHits != 12 {
		t.Fatalf("total hits = %d, want 12", aHits+bHits)
	}
	if aHits != 12 && bHits != 12 {
		t.Errorf("session was split across upstreams: a=%d b=%d, want all 12 on one", aHits, bHits)
	}
}

// TestDifferentSessionsSpread proves stickiness is not just "always pick the
// first": separate sessions must be free to land on different upstreams.
func TestDifferentSessionsSpread(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 1}, nil)

	for i := range 60 {
		hdr := map[string]string{"X-OMP-Session": "session-" + string(rune('a'+i%26)) + string(rune('0'+i/26))}
		res := post(t, srv, "/v1/chat/completions", map[string]any{
			"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, hdr)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
	}
	if a.hits() == 0 || b.hits() == 0 {
		t.Errorf("all sessions landed on one upstream: a=%d b=%d", a.hits(), b.hits())
	}
}

func get(t *testing.T, srv *Server, path string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

// TestStickyFailsOverWhenPinnedTargetDies: a session pinned to a dead upstream
// must be rescued, not fail forever.
func TestStickyFailsOverWhenPinnedTargetDies(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	a.status.Store(http.StatusInternalServerError)

	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 1}, nil)
	hdr := map[string]string{"X-OMP-Session": "session-doomed"}
	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, hdr)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if b.hits() != 1 {
		t.Errorf("healthy upstream b hits = %d, want 1", b.hits())
	}
}

// TestWeightsAffectDistribution: with a 3:1 split and no stickiness, traffic
// must follow the weights rather than the config order.
func TestWeightsAffectDistribution(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 3, "b": 1}, func(_ *Config, al *Alias) {
		no := false
		al.Sticky = &no
	})

	const n = 400
	for range n {
		post(t, srv, "/v1/chat/completions", map[string]any{
			"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
	}
	aHits, bHits := a.hits(), b.hits()
	if aHits+bHits != n {
		t.Fatalf("total hits = %d, want %d", aHits+bHits, n)
	}
	// Expect ~75/25. Wide bounds: this asserts weighting works, not the RNG.
	if aHits < 240 || aHits > 360 {
		t.Errorf("3:1 weights produced a=%d b=%d, want a within 240..360", aHits, bHits)
	}
}

// TestWeightZeroIsFailoverOnly: a zero-weight target must never be picked while
// a weighted target is healthy.
func TestWeightZeroIsFailoverOnly(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 0}, func(_ *Config, al *Alias) {
		no := false
		al.Sticky = &no
	})
	for range 50 {
		post(t, srv, "/v1/chat/completions", map[string]any{
			"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
	}
	if b.hits() != 0 {
		t.Errorf("zero-weight upstream b was selected %d times", b.hits())
	}
	if a.hits() != 50 {
		t.Errorf("upstream a hits = %d, want 50", a.hits())
	}
}

// TestStreamIsPassedThroughIncrementally: SSE must reach the client as it is
// produced, not buffered to end of turn.
func TestStreamIsPassedThroughIncrementally(t *testing.T) {
	a := newFakeUpstream(t, "a")
	a.stream.Store(true)
	a.chunks.Store(4)
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	body, _ := io.ReadAll(res.Body)
	got := string(body)
	if !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Errorf("stream missing terminator: %q", got)
	}
	if n := strings.Count(got, "data: {"); n != 4 {
		t.Errorf("stream chunks = %d, want 4", n)
	}
}

// TestStreamUsageInjection: clients that want usage on a stream must actually
// get it asked for.
func TestStreamUsageInjection(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, func(c *Config, _ *Alias) {
		c.Defaults.InjectStreamUsage = true
	})

	post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)

	seen := a.seen()
	if len(seen) != 1 {
		t.Fatalf("hits = %d, want 1", len(seen))
	}
	opts, ok := seen[0].Body["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Errorf("stream_options = %v, want include_usage=true", seen[0].Body["stream_options"])
	}
}

// TestStreamUsageNotInjectedOnAnthropicWire: stream_options belongs to the
// OpenAI wire. The Messages API rejects unknown parameters, so injecting it
// into a streaming request would turn a working stream into a 400.
func TestStreamUsageNotInjectedOnAnthropicWire(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(c *Config, al *Alias) {
			c.Defaults.InjectStreamUsage = true
			al.API = APIAnthropicMessages
		})

	res := post(t, srv, "/v1/messages", map[string]any{
		"model": "test-alias", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	seen := a.seen()
	if len(seen) != 1 {
		t.Fatalf("hits = %d, want 1", len(seen))
	}
	if _, ok := seen[0].Body["stream_options"]; ok {
		t.Errorf("stream_options was injected into an Anthropic-wire request: %v", seen[0].Body["stream_options"])
	}
}

// TestUnknownModelIsRejected: an unrouted name must not silently hit a default.
func TestUnknownModelIsRejected(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "no-such-model", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	if a.hits() != 0 {
		t.Errorf("unknown model still reached an upstream (%d hits)", a.hits())
	}
}

// TestProtocolMismatchIsRejected: an alias declared for one wire protocol must
// not answer on another, rather than returning a half-correct response.
func TestProtocolMismatchIsRejected(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)

	res := post(t, srv, "/v1/messages", map[string]any{"model": "test-alias"}, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// TestModelRewriteReturnsAlias: clients should see the alias they asked for,
// not the upstream's internal id.
func TestModelRewriteReturnsAlias(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)
	srv.cfg.Defaults.RewriteModel = true

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if out["model"] != "test-alias" {
		t.Errorf("model = %v, want test-alias", out["model"])
	}
}

// TestUpstreamHeadersAndScrubbing: upstream credentials are added, and the
// client's own Authorization never leaks through.
func TestUpstreamHeadersAndScrubbing(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)
	srv.router.upstreams["a"].cfg.Headers = map[string]string{"X-Title": "omp-router"}

	post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, map[string]string{"Authorization": "Bearer client-secret"})

	seen := a.seen()
	if len(seen) != 1 {
		t.Fatalf("hits = %d, want 1", len(seen))
	}
	if seen[0].Auth != "Bearer sk-a" {
		t.Errorf("upstream auth = %q, want the router's key, not the client's", seen[0].Auth)
	}
}

// TestBodyDropAndPatch covers the escape hatch for providers that reject or
// require specific fields.
func TestBodyDropAndPatch(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)
	srv.router.upstreams["a"].cfg.BodyDrop = []string{"provider", "reasoning"}
	srv.router.upstreams["a"].cfg.BodyPatch = map[string]any{"top_k": 40}

	post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "reasoning": map[string]any{"effort": "high"}, "provider": map[string]any{"only": []string{"x"}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)

	seen := a.seen()
	if len(seen) != 1 {
		t.Fatalf("hits = %d, want 1", len(seen))
	}
	if _, ok := seen[0].Body["provider"]; ok {
		t.Error("bodyDrop did not remove provider")
	}
	if _, ok := seen[0].Body["reasoning"]; ok {
		t.Error("bodyDrop did not remove reasoning")
	}
	if seen[0].Body["top_k"] != float64(40) {
		t.Errorf("bodyPatch top_k = %v, want 40", seen[0].Body["top_k"])
	}
}

// TestClientAuthorizationNotRequiredWhenNoGatewayKey: a loopback router with no
// configured key accepts unauthenticated clients.
func TestClientAuthorizationNotRequiredWhenNoGatewayKey(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}

// TestGatewayKeyEnforced: when a key is configured, a wrong one is refused
// before any upstream work happens.
func TestGatewayKeyEnforced(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)
	srv.cfg.APIKey = "sk-gateway"

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, map[string]string{"Authorization": "Bearer wrong"})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if a.hits() != 0 {
		t.Error("unauthorized request reached an upstream")
	}

	res = post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, map[string]string{"Authorization": "Bearer sk-gateway"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("authorized status = %d, want 200", res.StatusCode)
	}
}

// TestBodyLimitRejected: an oversized prompt is refused, not buffered forever.
func TestBodyLimitRejected(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)
	srv.cfg.Defaults.MaxBodyBytes = 512

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model":    "test-alias",
		"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", 4096)}},
	}, nil)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", res.StatusCode)
	}
}

// TestUnsupportedEndpointIsExplicit: the router does not pass through endpoints
// it does not implement.
func TestUnsupportedEndpointIsExplicit(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)

	res := post(t, srv, "/v1/embeddings", map[string]any{"model": "test-alias"}, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

// TestModelsEndpointAdvertisesAliases is what lets omp/pi discover the router:
// the list must be the aliases, carrying real context limits.
func TestModelsEndpointAdvertisesAliases(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)

	res := get(t, srv, "/v1/models", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Context int    `json:"context_length"`
			Limits  struct {
				MaxOut int `json:"max_output_tokens"`
			} `json:"limits"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "list" || len(out.Data) != 1 {
		t.Fatalf("unexpected list: %+v", out)
	}
	if out.Data[0].ID != "test-alias" {
		t.Errorf("id = %q, want test-alias", out.Data[0].ID)
	}
	if out.Data[0].Context != 128000 || out.Data[0].Limits.MaxOut != 8192 {
		t.Errorf("limits = ctx %d maxOut %d, want 128000/8192", out.Data[0].Context, out.Data[0].Limits.MaxOut)
	}
}

// TestModelGroupInfoForRichDiscovery covers the LiteLLM-shaped metadata route
// used by clients that read context and pricing from management endpoints.
func TestModelGroupInfoForRichDiscovery(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)
	srv.cfg.Models["test-alias"] = Alias{
		API: APIOpenAICompletions, ContextWindow: 200000, MaxOutputTokens: 16000,
		Cost:    Cost{Input: 3.0, Output: 15.0},
		Targets: []Target{{Upstream: "a", Model: "real/a", Weight: 1}},
	}

	res := get(t, srv, "/model_group/info", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var out struct {
		Data []struct {
			ModelName string `json:"model_name"`
			Info      struct {
				MaxInputTokens int     `json:"max_input_tokens"`
				InputCostPerTk float64 `json:"input_cost_per_token"`
			} `json:"model_info"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].ModelName != "test-alias" {
		t.Fatalf("unexpected payload: %+v", out.Data)
	}
	if out.Data[0].Info.MaxInputTokens != 200000 {
		t.Errorf("max_input_tokens = %d, want 200000", out.Data[0].Info.MaxInputTokens)
	}
	// Cost is configured per million tokens; the wire format is per token.
	if got := out.Data[0].Info.InputCostPerTk; got != 0.000003 {
		t.Errorf("input_cost_per_token = %v, want 0.000003", got)
	}
}

// TestSaturatedUpstreamIsSkipped: a target at its concurrency limit must be
// passed over rather than queued behind.
func TestSaturatedUpstreamIsSkipped(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	a.gate = make(chan struct{})
	defer close(a.gate)

	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 1}, nil)
	srv.router.upstreams["a"].cfg.MaxConcurrency = 1
	srv.router.upstreams["a"].sem = make(chan struct{}, 1)
	srv.router.upstreams["a"].sem <- struct{}{} // occupy the only slot

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if b.hits() != 1 {
		t.Errorf("healthy upstream b hits = %d, want 1 (saturated a must be skipped)", b.hits())
	}
}

// TestLoadedConfigServesRequests covers the path a real deployment takes: YAML
// on disk -> LoadConfig -> defaults -> router -> client request. Earlier tests
// built Config structs directly and so never exercised defaulting.
func TestLoadedConfigServesRequests(t *testing.T) {
	a := newFakeUpstream(t, "a")
	path := filepath.Join(t.TempDir(), "router.yaml")
	yaml := fmt.Sprintf("listen: 127.0.0.1:0\n"+
		"upstreams:\n"+
		"  a:\n"+
		"    baseUrl: %s/v1\n"+
		"    apiKey: sk-a\n"+
		"models:\n"+
		"  alias-one:\n"+
		"    api: openai-completions\n"+
		"    contextWindow: 100000\n"+
		"    maxOutputTokens: 4096\n"+
		"    targets: [{upstream: a, model: real/a, weight: 1}]\n", a.srv.URL)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Upstreams["a"].MaxConcurrency; got < 1 {
		t.Fatalf("MaxConcurrency = %d, want a usable default; zero makes every upstream unselectable", got)
	}
	if cfg.Defaults.MaxAttempts < 1 {
		t.Errorf("MaxAttempts = %d, want a positive default", cfg.Defaults.MaxAttempts)
	}
	if cfg.Defaults.FirstByteTimeout <= 0 {
		t.Errorf("FirstByteTimeout = %v, want a positive default", cfg.Defaults.FirstByteTimeout)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := NewRouter(cfg, log)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	srv := NewServer(cfg, r, log, nil)

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "alias-one", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200; body: %s", res.StatusCode, body)
	}
	if a.hits() != 1 {
		t.Errorf("upstream hits = %d, want 1", a.hits())
	}
}

// TestAllTargetsSaturatedReportsBusy: when nothing could even be attempted, the
// router must say so instead of blaming the upstreams with a 502.
func TestAllTargetsSaturatedReportsBusy(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)
	srv.router.upstreams["a"].sem = make(chan struct{}, 1)
	srv.router.upstreams["a"].sem <- struct{}{}

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
	if a.hits() != 0 {
		t.Errorf("saturated upstream was contacted %d times", a.hits())
	}
}

// TestTrippedWeightedTargetYieldsToLiveFailoverBackup covers the ordering the
// breaker depends on: once the weighted target is known dead, a weight-0
// backup must be preferred over re-dispatching into the dead one.
func TestTrippedWeightedTargetYieldsToLiveFailoverBackup(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	a.status.Store(http.StatusInternalServerError)

	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 0},
		func(c *Config, al *Alias) {
			no := false
			al.Sticky = &no
			c.Defaults.BreakerFailures = 1
			c.Defaults.BreakerCooldown = Duration(time.Hour)
		})

	post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if a.hits() != 1 {
		t.Fatalf("first request hits on a = %d, want 1", a.hits())
	}

	for range 5 {
		res := post(t, srv, "/v1/chat/completions", map[string]any{
			"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
	}
	if a.hits() != 1 {
		t.Errorf("tripped weighted upstream was retried: hits went 1 -> %d, want 1", a.hits())
	}
	if b.hits() != 6 {
		t.Errorf("backup upstream hits = %d, want 6 (all requests after the first)", b.hits())
	}
}

// TestSkippedTargetsDoNotConsumeAttemptBudget: skipping a target for
// saturation must not eat the request's failover allowance.
func TestSkippedTargetsDoNotConsumeAttemptBudget(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")

	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 0},
		func(c *Config, al *Alias) {
			no := false
			al.Sticky = &no
			// Room for a single dispatch: anything beyond the first must fail.
			c.Defaults.MaxAttempts = 1
		})
	// a is saturated, so the budget must be spent on b rather than exhausted.
	srv.router.upstreams["a"].sem = make(chan struct{}, 1)
	srv.router.upstreams["a"].sem <- struct{}{}

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if b.hits() != 1 {
		t.Errorf("backup hits = %d, want 1", b.hits())
	}
}

// TestRecoveredUpstreamAdmitsExactlyOneProbe: after a cooldown, only one request
// may test the upstream, so the herd cannot re-trip it the instant it returns.
func TestRecoveredUpstreamAdmitsExactlyOneProbe(t *testing.T) {
	up := newUpstreamState("u", Upstream{MaxConcurrency: 4}, "", 1)
	up.recordFailure(time.Hour, time.Time{}) // trips immediately
	if up.available(time.Now()) {
		t.Fatal("tripped upstream reports available")
	}
	later := time.Now().Add(2 * time.Hour)
	if !up.available(later) {
		t.Fatal("upstream did not recover after its cooldown")
	}
	// Repeated evaluation must stay true: selection inspects a target many times.
	if !up.available(later) {
		t.Fatal("available is not idempotent across evaluations")
	}

	if !up.claimProbe(later) {
		t.Fatal("first recovery probe was refused")
	}
	if up.claimProbe(later) {
		t.Error("a second recovery probe was admitted while one was in flight")
	}

	up.recordSuccess()
	if !up.claimProbe(later) || !up.claimProbe(later) {
		t.Error("a healthy upstream must admit every request")
	}
}

// TestPrefixStickyKeyIsStableAsTranscriptGrows: the whole point of prefix
// affinity is that a conversation keeps its key as later turns are appended.
func TestPrefixStickyKeyIsStableAsTranscriptGrows(t *testing.T) {
	plan, err := newRequestPlan([]byte(`{"model":"a","messages":[
		{"role":"system","content":"you are a coding agent"},
		{"role":"user","content":"fix the failing test"}]}`), 1<<20)
	if err != nil {
		t.Fatalf("newRequestPlan: %v", err)
	}
	plan.alias = "a"
	first := plan.prefixStickyKey()
	if first == "" {
		t.Fatal("prefixStickyKey returned empty for a two-message conversation")
	}

	plan.fields["messages"] = json.RawMessage(`[
		{"role":"system","content":"you are a coding agent"},
		{"role":"user","content":"fix the failing test"},
		{"role":"assistant","content":"on it"},
		{"role":"user","content":"still failing"}]`)
	if got := plan.prefixStickyKey(); got != first {
		t.Errorf("key changed as the transcript grew:\n got %s\nwant %s", got, first)
	}

	plan.fields["messages"] = json.RawMessage(`[
		{"role":"system","content":"you are a coding agent"},
		{"role":"user","content":"a completely different task"}]`)
	if got := plan.prefixStickyKey(); got == first {
		t.Error("a different conversation reused the same key")
	}
}

// TestPrefixStickyPinsConversationWithoutClientHeader is the omp/pi case: no
// session header is sent, so affinity must come from the prompt itself.
func TestPrefixStickyPinsConversationWithoutClientHeader(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 1},
		func(c *Config, _ *Alias) { c.Defaults.StickyByPrefix = true })

	turn := func(n int) {
		msgs := []any{
			map[string]any{"role": "system", "content": "you are a coding agent"},
			map[string]any{"role": "user", "content": "fix the failing test"},
		}
		for range n {
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": "working"},
				map[string]any{"role": "user", "content": "keep going"})
		}
		res := post(t, srv, "/v1/chat/completions", map[string]any{
			"model": "test-alias", "messages": msgs,
		}, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
	}

	for range 10 {
		turn(3)
	}
	aHits, bHits := a.hits(), b.hits()
	if aHits != 10 && bHits != 10 {
		t.Errorf("conversation scattered across upstreams: a=%d b=%d, want all 10 on one", aHits, bHits)
	}
}

func TestPrefixStickyKeyEmptyWithoutMessages(t *testing.T) {
	plan, err := newRequestPlan([]byte(`{"model":"a"}`), 1<<20)
	if err != nil {
		t.Fatalf("newRequestPlan: %v", err)
	}
	if got := plan.prefixStickyKey(); got != "" {
		t.Errorf("prefixStickyKey = %q, want empty", got)
	}
}

// TestAnthropicAuthStyleSendsClaudeCredentialHeaders: the Claude API reads
// x-api-key on /v1/messages and Authorization on /v1/chat/completions, so both
// must be present, along with a version header Claude rejects requests without.
func TestAnthropicAuthStyleSendsClaudeCredentialHeaders(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(c *Config, _ *Alias) {
			u := c.Upstreams["a"]
			u.AuthStyle = AuthAnthropic
			c.Upstreams["a"] = u
		})

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	got := a.seen()
	if len(got) != 1 {
		t.Fatalf("upstream hits = %d, want 1", len(got))
	}
	if got[0].APIKey != "sk-a" {
		t.Errorf("x-api-key = %q, want sk-a", got[0].APIKey)
	}
	if got[0].Auth != "Bearer sk-a" {
		t.Errorf("Authorization = %q, want Bearer sk-a", got[0].Auth)
	}
	if got[0].Version != defaultAnthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", got[0].Version, defaultAnthropicVersion)
	}
}

// TestAnthropicAuthStyleKeepsConfiguredVersion: an upstream pinning its own
// anthropic-version must not have it clobbered by the built-in default.
func TestAnthropicAuthStyleKeepsConfiguredVersion(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(c *Config, _ *Alias) {
			u := c.Upstreams["a"]
			u.AuthStyle = AuthAnthropic
			u.Headers = map[string]string{"anthropic-version": "2025-01-01"}
			c.Upstreams["a"] = u
		})

	post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)

	got := a.seen()
	if len(got) != 1 {
		t.Fatalf("upstream hits = %d, want 1", len(got))
	}
	if got[0].Version != "2025-01-01" {
		t.Errorf("anthropic-version = %q, want the configured 2025-01-01", got[0].Version)
	}
}

// TestClientCredentialsNeverReachAnotherUpstream: an Anthropic-wire client
// authenticates to the router with x-api-key. Relaying that header would hand
// the client's Claude key to whichever provider the alias happens to pick.
func TestClientCredentialsNeverReachAnotherUpstream(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, map[string]string{
		"Authorization": "Bearer client-token",
		"x-api-key":     "sk-client-claude-key",
		"api-key":       "sk-client-azure-key",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	got := a.seen()
	if len(got) != 1 {
		t.Fatalf("upstream hits = %d, want 1", len(got))
	}
	if got[0].Auth != "Bearer sk-a" {
		t.Errorf("Authorization = %q, want the router's own key", got[0].Auth)
	}
	if got[0].APIKey != "" {
		t.Errorf("client x-api-key leaked to upstream: %q", got[0].APIKey)
	}
}

// TestUnknownAuthStyleIsRejectedAtLoad: a typo in authStyle would otherwise
// silently fall back to bearer and fail against Claude with a confusing 401.
func TestUnknownAuthStyleIsRejectedAtLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.yaml")
	yaml := "upstreams:\n" +
		"  a:\n" +
		"    baseUrl: https://example.test/v1\n" +
		"    authStyle: anthorpic\n" +
		"models:\n" +
		"  alias-one:\n" +
		"    api: openai-completions\n" +
		"    targets: [{upstream: a, model: real/a, weight: 1}]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig accepted an unknown authStyle; want an error")
	}
}

// TestOpenRouterShapedBasePathServesInference: a client configured with the
// OpenRouter base URL and only the host swapped must be able to infer.
func TestOpenRouterShapedBasePathServesInference(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)

	res := post(t, srv, "/api/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200; body: %s", res.StatusCode, body)
	}
	if a.hits() != 1 {
		t.Errorf("upstream hits = %d, want 1", a.hits())
	}
	if got := res.Header.Get("X-Router-Upstream"); got != "a" {
		t.Errorf("X-Router-Upstream = %q, want a", got)
	}
}

// TestOpenRouterShapedBasePathServesDiscovery: /api/v1/models must list aliases
// and resolve a single one, not mistake the prefix for a model id.
func TestOpenRouterShapedBasePathServesDiscovery(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1}, nil)

	res := get(t, srv, "/api/v1/models", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/models status = %d, want 200", res.StatusCode)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "test-alias" {
		t.Errorf("data = %+v, want exactly test-alias", list.Data)
	}

	one := get(t, srv, "/api/v1/models/test-alias", nil)
	if one.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/models/test-alias status = %d, want 200", one.StatusCode)
	}
	var card struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(one.Body).Decode(&card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card.ID != "test-alias" {
		t.Errorf("id = %q, want test-alias", card.ID)
	}

	group := get(t, srv, "/api/v1/model_group/info", nil)
	if group.StatusCode != http.StatusOK {
		t.Errorf("GET /api/v1/model_group/info status = %d, want 200", group.StatusCode)
	}
}

// setTargetShape attaches field shaping to the target served by one upstream.
// Targets are built by ranging a map, so their order is not deterministic and
// they must be found by name rather than by index.
func setTargetShape(al *Alias, upstream string, patch map[string]any, drop []string) {
	for i := range al.Targets {
		if al.Targets[i].Upstream == upstream {
			al.Targets[i].BodyPatch = patch
			al.Targets[i].BodyDrop = drop
		}
	}
}

// TestTargetBodyShapingIsPerTarget is the reason shaping lives on the target
// rather than the upstream: the same alias must address OpenRouter in
// OpenRouter's vocabulary and Claude in Claude's, in one request.
func TestTargetBodyShapingIsPerTarget(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	a.status.Store(http.StatusInternalServerError) // force the second target

	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 1, "b": 0},
		func(_ *Config, al *Alias) {
			setTargetShape(al, "a", map[string]any{
				"provider":  map[string]any{"order": []any{"anthropic"}, "allow_fallbacks": false},
				"reasoning": map[string]any{"effort": "high"},
			}, nil)
			// Claude: thinking is expressed differently, and OpenRouter's
			// reasoning must not be forwarded to a provider that rejects
			// unknown parameters.
			setTargetShape(al, "b", map[string]any{
				"thinking": map[string]any{"type": "adaptive", "display": "summarized"},
			}, []string{"reasoning"})
		})

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model":     "test-alias",
		"reasoning": map[string]any{"effort": "low"},
		"messages":  []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover", res.StatusCode)
	}

	gotA, gotB := a.seen(), b.seen()
	if len(gotA) != 1 || len(gotB) != 1 {
		t.Fatalf("hits = a:%d b:%d, want 1 each", len(gotA), len(gotB))
	}

	provider, _ := gotA[0].Body["provider"].(map[string]any)
	if provider["allow_fallbacks"] != false {
		t.Errorf("a provider.allow_fallbacks = %v, want false", provider["allow_fallbacks"])
	}
	order, _ := provider["order"].([]any)
	if len(order) != 1 || order[0] != "anthropic" {
		t.Errorf("a provider.order = %v, want [anthropic]", provider["order"])
	}
	// The target's patch replaces the client's value for the same key.
	reasoning, _ := gotA[0].Body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Errorf("a reasoning.effort = %v, want high (target patch beats client)", reasoning["effort"])
	}

	thinking, _ := gotB[0].Body["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" || thinking["display"] != "summarized" {
		t.Errorf("b thinking = %v, want adaptive/summarized", gotB[0].Body["thinking"])
	}
	if _, ok := gotB[0].Body["reasoning"]; ok {
		t.Errorf("b received OpenRouter's reasoning field: %v", gotB[0].Body["reasoning"])
	}
	if _, ok := gotB[0].Body["provider"]; ok {
		t.Errorf("b received OpenRouter's provider field: %v", gotB[0].Body["provider"])
	}
}

// TestTargetBodyShapingBeatsUpstreamShaping: the upstream expresses
// provider-wide defaults, the target the per-route override, so the target wins.
func TestTargetBodyShapingBeatsUpstreamShaping(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(c *Config, al *Alias) {
			u := c.Upstreams["a"]
			u.BodyPatch = map[string]any{"top_p": 0.1, "presence_penalty": 0.5}
			c.Upstreams["a"] = u
			setTargetShape(al, "a", map[string]any{"top_p": 0.9}, nil)
		})

	post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)

	got := a.seen()
	if len(got) != 1 {
		t.Fatalf("hits = %d, want 1", len(got))
	}
	if got[0].Body["top_p"] != 0.9 {
		t.Errorf("top_p = %v, want the target's 0.9", got[0].Body["top_p"])
	}
	if got[0].Body["presence_penalty"] != 0.5 {
		t.Errorf("presence_penalty = %v, want the upstream's 0.5 to survive", got[0].Body["presence_penalty"])
	}
}

// TestTargetBodyDropAppliesPerTarget: a target may refuse a field the client
// sends even though the upstream allows it.
func TestTargetBodyDropAppliesPerTarget(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(_ *Config, al *Alias) {
			setTargetShape(al, "a", nil, []string{"reasoning", "provider"})
		})

	post(t, srv, "/v1/chat/completions", map[string]any{
		"model":     "test-alias",
		"reasoning": map[string]any{"effort": "high"},
		"provider":  map[string]any{"sort": "price"},
		"messages":  []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)

	got := a.seen()
	if len(got) != 1 {
		t.Fatalf("hits = %d, want 1", len(got))
	}
	if _, ok := got[0].Body["reasoning"]; ok {
		t.Errorf("target drop did not remove reasoning: %v", got[0].Body["reasoning"])
	}
	if _, ok := got[0].Body["provider"]; ok {
		t.Errorf("target drop did not remove provider: %v", got[0].Body["provider"])
	}
}

// TestBodyPatchMayNotSetModel: the router rewrites "model" from the target, so a
// patch naming it would be silently undone and misreads as model selection.
func TestBodyPatchMayNotSetModel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream string
		target   string
	}{
		{
			name:     "through upstream",
			upstream: "    bodyPatch: {model: sneaky}\n",
		},
		{
			name:   "through target",
			target: "    targets: [{upstream: a, model: real/a, weight: 1, bodyPatch: {model: sneaky}}]\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "router.yaml")
			yaml := "upstreams:\n" +
				"  a:\n" +
				"    baseUrl: https://example.test/v1\n" +
				tc.upstream +
				"models:\n" +
				"  alias-one:\n" +
				"    api: openai-completions\n" +
				tc.target
			if tc.target == "" {
				yaml += "    targets: [{upstream: a, model: real/a, weight: 1}]\n"
			}
			if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("LoadConfig accepted bodyPatch on \"model\"; want an error")
			}
		})
	}
}

// loadYAML writes a config to a temp file and loads it, so tests exercise the
// same path as the operator's router.yaml.
func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return LoadConfig(path)
}

// TestCapabilityTableParses guards the embedded data, which every other
// capability check depends on.
func TestCapabilityTableParses(t *testing.T) {
	table, err := loadCapabilities()
	if err != nil {
		t.Fatalf("loadCapabilities: %v", err)
	}
	opus, ok := table.Lookup("claude-opus-5-5")
	if !ok {
		t.Fatal("claude-opus-5-5 missing from the capability table")
	}
	if opus.MaxInputTokens != 1_000_000 || opus.MaxOutputTokens != 128_000 {
		t.Errorf("opus limits = %d/%d, want 1000000/128000", opus.MaxInputTokens, opus.MaxOutputTokens)
	}
	if opus.Cost.Input != 4 || opus.Cost.Output != 20 || opus.Cost.CacheRead != 0.2 || opus.Cost.CacheWrite != 5 {
		t.Errorf("opus cost = %+v, want 4/20/0.2/5", opus.Cost)
	}
	if !opus.Supports(flagAdaptiveThinking) || !opus.Supports(flagThinkingAlwaysOn) {
		t.Error("opus 5.5 should support adaptive thinking with thinking always on")
	}
	if got := supportedEfforts(opus); len(got) != 5 {
		t.Errorf("supportedEfforts(opus) = %v, want five levels", got)
	}

	haiku, ok := table.Lookup("claude-haiku-4-5")
	if !ok {
		t.Fatal("claude-haiku-4-5 missing from the capability table")
	}
	if haiku.Supports(flagAdaptiveThinking) {
		t.Error("haiku 4.5 rejects adaptive thinking, so the flag must be absent")
	}
	if haiku.Supports(flagThinkingAlwaysOn) {
		t.Error("haiku 4.5 thinking is off by default, so the flag must be absent")
	}
	if got := supportedEfforts(haiku); got != nil {
		t.Errorf("supportedEfforts(haiku) = %v, want nil (no output_config)", got)
	}
}

// TestNormalizeModelIDSplitsSpellings: one model, several names. If these do not
// collapse to a single key the capability checks silently stop working for
// whichever spelling misses.
func TestNormalizeModelIDSplitsSpellings(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"claude-opus-5-5", "claude-opus-5-5"},
		{"anthropic/claude-opus-5.5", "claude-opus-5-5"},
		{"anthropic/claude-opus-5.5:nitro", "claude-opus-5-5"},
		{"anthropic/claude-opus-5.5:floor", "claude-opus-5-5"},
		{"~anthropic/claude-opus-latest", "claude-opus-latest"},
		{"anthropic/claude-haiku-4.5", "claude-haiku-4-5"},
	} {
		if got := normalizeModelID(tc.in); got != tc.want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	table, err := loadCapabilities()
	if err != nil {
		t.Fatalf("loadCapabilities: %v", err)
	}
	slug, okSlug := table.Lookup("anthropic/claude-opus-5.5")
	native, okNative := table.Lookup("claude-opus-5-5")
	if !okSlug || !okNative {
		t.Fatal("the OpenRouter slug and Claude's own id must both resolve")
	}
	if slug.MaxInputTokens != native.MaxInputTokens || slug.Cost.Input != native.Cost.Input {
		t.Error("both spellings should describe the same model")
	}
}

// TestCapabilityMismatchIsRejectedAtLoad turns what would be an opaque provider
// 400 into a startup error.
func TestCapabilityMismatchIsRejectedAtLoad(t *testing.T) {
	const base = "upstreams:\n" +
		"  anthropic:\n" +
		"    baseUrl: https://api.anthropic.com/v1\n" +
		"    apiKeyEnv: K\n" +
		"    authStyle: anthropic\n" +
		"models:\n" +
		"  alias-one:\n" +
		"    api: openai-completions\n" +
		"    targets:\n" +
		"      - upstream: anthropic\n"

	for _, tc := range []struct {
		name   string
		model  string
		patch  string
		reject bool
	}{
		{"adaptive thinking on haiku", "claude-haiku-4-5", "thinking: {type: adaptive}", true},
		{"explicit thinking on opus 5.5", "claude-opus-5-5", "thinking: {type: enabled, budget_tokens: 2048}", true},
		{"disabled thinking on opus 5.5", "claude-opus-5-5", "thinking: {type: disabled}", true},
		{"effort on haiku", "claude-haiku-4-5", "output_config: {effort: high}", true},
		{"adaptive thinking on opus 5.5", "claude-opus-5-5", "thinking: {type: adaptive}", false},
		{"explicit thinking on haiku", "claude-haiku-4-5", "thinking: {type: enabled, budget_tokens: 2048}", false},
		{"max effort on opus 5.5", "claude-opus-5-5", "output_config: {effort: max}", false},
		{"xhigh effort on opus 5.5", "claude-opus-5-5", "output_config: {effort: xhigh}", false},
		{"max effort on sonnet 4.5", "claude-sonnet-4-5", "output_config: {effort: max}", true},
		{"unknown model is not checked", "private-finetune-v3", "thinking: {type: adaptive}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := base + "        model: " + tc.model + "\n" +
				"        weight: 1\n" +
				"        bodyPatch: {" + tc.patch + "}\n"
			_, err := loadYAML(t, yaml)
			if tc.reject && err == nil {
				t.Fatal("config was accepted, but the model rejects that bodyPatch")
			}
			if !tc.reject && err != nil {
				t.Fatalf("a valid config was rejected: %v", err)
			}
		})
	}
}

// TestCapabilityDriftIsWarnedNotFatal: the table is a snapshot. Disagreeing with
// it is worth reporting, but it must never stop a setup that works.
func TestCapabilityDriftIsWarnedNotFatal(t *testing.T) {
	cfg, err := loadYAML(t, `upstreams:
  anthropic:
    baseUrl: https://api.anthropic.com/v1
    apiKeyEnv: K
    authStyle: anthropic
models:
  anthropic/claude-opus-5.5:
    api: openai-completions
    contextWindow: 200000
    cost: {input: 5}
    targets:
      - upstream: anthropic
        model: claude-opus-5-5
        weight: 1
`)
	if err != nil {
		t.Fatalf("drift must not be fatal: %v", err)
	}
	joined := strings.Join(cfg.Warnings, "\n")
	for _, want := range []string{"contextWindow", "cost.input"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings do not mention %s:\n%s", want, joined)
		}
	}

	// The same alias with accurate metadata produces no warnings at all.
	clean, err := loadYAML(t, `upstreams:
  anthropic:
    baseUrl: https://api.anthropic.com/v1
    apiKeyEnv: K
    authStyle: anthropic
models:
  anthropic/claude-opus-5.5:
    api: openai-completions
    contextWindow: 1000000
    maxOutputTokens: 128000
    cost: {input: 4, output: 20, cacheRead: 0.20, cacheWrite: 5}
    targets:
      - upstream: anthropic
        model: claude-opus-5-5
        weight: 1
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(clean.Warnings) != 0 {
		t.Errorf("accurate metadata produced warnings: %v", clean.Warnings)
	}
}

// TestModelGroupInfoReportsCapabilities: a client should learn what it may ask
// for before sending a request rather than from a 400.
func TestModelGroupInfoReportsCapabilities(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(_ *Config, al *Alias) {
			// Omit declared metadata so the table has to supply it.
			al.ContextWindow = 0
			al.MaxOutputTokens = 0
			al.Targets[0].Model = "claude-opus-5-5"
		})

	res := get(t, srv, "/model_group/info", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body struct {
		Data []struct {
			ModelName string `json:"model_name"`
			ModelInfo struct {
				MaxInputTokens            int      `json:"max_input_tokens"`
				MaxTokens                 int      `json:"max_tokens"`
				Providers                 []string `json:"providers"`
				SupportsAdaptiveThinking  bool     `json:"supports_adaptive_thinking"`
				SupportsPromptCaching     bool     `json:"supports_prompt_caching"`
				SupportedReasoningEfforts []string `json:"supported_reasoning_efforts"`
			} `json:"model_info"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("entries = %d, want 1", len(body.Data))
	}
	mi := body.Data[0].ModelInfo
	if mi.MaxInputTokens != 1_000_000 || mi.MaxTokens != 128_000 {
		t.Errorf("limits = %d/%d, want the table's 1000000/128000", mi.MaxInputTokens, mi.MaxTokens)
	}
	if !mi.SupportsAdaptiveThinking || !mi.SupportsPromptCaching {
		t.Errorf("capability flags missing: %+v", mi)
	}
	if len(mi.SupportedReasoningEfforts) != 5 {
		t.Errorf("supported_reasoning_efforts = %v, want five levels", mi.SupportedReasoningEfforts)
	}
	if len(mi.Providers) != 1 || mi.Providers[0] != "a" {
		t.Errorf("providers = %v, want [a]", mi.Providers)
	}
}

func absDiff(a, b float64) float64 {
	d := a - b
	if d < 0 {
		return -d
	}
	return d
}

// loggedCost pulls cost_usd out of a captured log line. Asserting on the parsed
// value rather than its text keeps the test independent of float formatting.
func loggedCost(t *testing.T, logs string) float64 {
	t.Helper()
	i := strings.Index(logs, "cost_usd=")
	if i < 0 {
		t.Fatalf("no cost_usd in log:\n%s", logs)
	}
	rest := logs[i+len("cost_usd="):]
	end := strings.IndexAny(rest, " \n")
	if end < 0 {
		end = len(rest)
	}
	v, err := strconv.ParseFloat(rest[:end], 64)
	if err != nil {
		t.Fatalf("parse cost %q: %v", rest[:end], err)
	}
	return v
}

// TestUsageNormalisation: the two wires report tokens differently. Getting this
// wrong double-bills cache reads, which is precisely the traffic this router
// exists to make cheap.
func TestUsageNormalisation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		want  Usage
		found bool
	}{
		{
			name:  "openai response, cached subset of prompt",
			body:  `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":80}}}`,
			want:  Usage{InputTokens: 20, OutputTokens: 20, CacheReadTokens: 80},
			found: true,
		},
		{
			name:  "openai response without cache detail",
			body:  `{"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
			want:  Usage{InputTokens: 7, OutputTokens: 3},
			found: true,
		},
		{
			name:  "anthropic response, cache fields exclusive of input",
			body:  `{"usage":{"input_tokens":20,"output_tokens":20,"cache_read_input_tokens":80,"cache_creation_input_tokens":10}}`,
			want:  Usage{InputTokens: 20, OutputTokens: 20, CacheReadTokens: 80, CacheWriteTokens: 10},
			found: true,
		},
		{
			name:  "anthropic stream start nests usage under the message",
			body:  `{"type":"message_start","message":{"usage":{"input_tokens":20,"output_tokens":1,"cache_read_input_tokens":80}}}`,
			want:  Usage{InputTokens: 20, OutputTokens: 1, CacheReadTokens: 80},
			found: true,
		},
		{
			name: "no usage reported",
			body: `{"choices":[{"message":{"content":"hi"}}]}`,
			want: Usage{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := usageFromBody([]byte(tc.body))
			if ok != tc.found {
				t.Fatalf("found = %v, want %v", ok, tc.found)
			}
			if ok && got != tc.want {
				t.Errorf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestUsageObserverMergesAnthropicStream: the Anthropic wire reports input
// counts in message_start and the final output count in message_delta, so the
// observer must accumulate rather than take the last event it saw.
func TestUsageObserverMergesAnthropicStream(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":20,"output_tokens":1,"cache_read_input_tokens":80,"cache_creation_input_tokens":10}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","usage":{"output_tokens":25}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")

	var obs usageObserver
	// Feed awkward fragments so the line buffering is exercised, not just the
	// happy path of one clean read per event.
	for i := 0; i < len(stream); i += 7 {
		obs.Write([]byte(stream[i:min(i+7, len(stream))]))
	}

	got, found := obs.Result()
	if !found {
		t.Fatal("no usage observed")
	}
	want := Usage{InputTokens: 20, OutputTokens: 25, CacheReadTokens: 80, CacheWriteTokens: 10}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// TestUsageObserverRecoversFromAnOversizedLine: a misbehaving upstream must not
// be able to grow the buffer without limit, and must not stop accounting for the
// rest of the stream either.
func TestUsageObserverRecoversFromAnOversizedLine(t *testing.T) {
	var obs usageObserver
	obs.Write([]byte("data: {\"pad\":\"" + strings.Repeat("x", usage.MaxSSELine+10)))
	obs.Write([]byte("\n"))
	obs.Write([]byte("data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n"))

	got, found := obs.Result()
	if !found {
		t.Fatal("the observer gave up after an oversized line instead of resuming")
	}
	if got.InputTokens != 5 || got.OutputTokens != 2 {
		t.Errorf("usage = %+v, want 5 input / 2 output", got)
	}
	if obs.BufferLen() > usage.MaxSSELine {
		t.Errorf("line buffer grew past the cap: %d bytes", obs.BufferLen())
	}
}

// TestCostEstimate checks the arithmetic directly, so the end-to-end tests only
// have to prove the value reaches the log.
func TestCostEstimate(t *testing.T) {
	c := Cost{Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5}
	u := Usage{InputTokens: 20, OutputTokens: 20, CacheReadTokens: 80, CacheWriteTokens: 10}
	// 20*4 + 20*20 + 80*0.2 + 10*5 = 80 + 400 + 16 + 50 = 546 per million.
	if got := estimateCost(c, u); absDiff(got, 546e-6) > 1e-12 {
		t.Errorf("Estimate = %v, want 546e-6", got)
	}
	if got := estimateCost(Cost{}, u); got != 0 {
		t.Errorf("zero rates should estimate zero, got %v", got)
	}
	if u.TotalInput() != 110 || u.Total() != 130 {
		t.Errorf("totals = %d/%d, want 110/130", u.TotalInput(), u.Total())
	}
}

// TestStreamStaysByteTransparentWhileAccounting is the whole point of observing
// rather than parsing: the client must receive exactly the upstream's bytes,
// including its terminator, while the router still learns the token counts.
func TestStreamStaysByteTransparentWhileAccounting(t *testing.T) {
	a := newFakeUpstream(t, "a")
	a.stream.Store(true)
	a.chunks.Store(3)
	a.streamUsage.Store(true)
	srv, logs := newTestServerLogging(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(_ *Config, al *Alias) {
			al.Cost = Cost{Input: 4, Output: 20, CacheRead: 0.2}
		})

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body := string(raw)
	for i := range 3 {
		if want := `"tok` + string(rune('a'+i)) + `"`; !strings.Contains(body, want) {
			t.Errorf("client body is missing %s:\n%q", want, body)
		}
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("client body should end with the upstream's own terminator, got %q", body)
	}

	line := logs.String()
	for _, want := range []string{"input_tokens=20", "output_tokens=20", "cache_read_tokens=80"} {
		if !strings.Contains(line, want) {
			t.Errorf("accounting line is missing %s:\n%s", want, line)
		}
	}
	// 20*4 + 20*20 + 80*0.2 = 496 per million.
	if got := loggedCost(t, line); absDiff(got, 496e-6) > 1e-12 {
		t.Errorf("cost = %v, want 496e-6", got)
	}
}

// TestNonStreamingAccounting: a buffered response already passes through the
// router, so its usage costs nothing extra to read.
func TestNonStreamingAccounting(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv, logs := newTestServerLogging(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(_ *Config, al *Alias) {
			al.Cost = Cost{Input: 1, Output: 5}
		})

	post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)

	line := logs.String()
	// The fake reports prompt_tokens 7 and completion_tokens 3.
	for _, want := range []string{"input_tokens=7", "output_tokens=3"} {
		if !strings.Contains(line, want) {
			t.Errorf("accounting line is missing %s:\n%s", want, line)
		}
	}
	// 7*1 + 3*5 = 22 per million.
	if got := loggedCost(t, line); absDiff(got, 22e-6) > 1e-12 {
		t.Errorf("cost = %v, want 22e-6", got)
	}
}

// TestUnreportedUsageIsNotInvented: when the provider reports nothing, the
// router says so rather than claiming a precise spend it never measured.
func TestUnreportedUsageIsNotInvented(t *testing.T) {
	a := newFakeUpstream(t, "a")
	a.stream.Store(true)
	// streamUsage left false: the upstream sends no usage chunk.
	srv, logs := newTestServerLogging(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(_ *Config, al *Alias) {
			al.Cost = Cost{Input: 4, Output: 20}
		})

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	line := logs.String()
	if !strings.Contains(line, "usage=unreported") {
		t.Errorf("expected an explicit unreported marker:\n%s", line)
	}
	if strings.Contains(line, "cost_usd") {
		t.Errorf("a cost was claimed without any counts:\n%s", line)
	}
}

// TestCacheAffinityTracksOnlyCacheEvidence: a target becomes warm because a
// provider said so, never because it merely served a request. Otherwise an
// upstream that does not cache at all would attract traffic on the strength of a
// cache it does not have.
func TestCacheAffinityTracksOnlyCacheEvidence(t *testing.T) {
	aff := newCacheAffinity(time.Minute, 100)
	now := time.Now()

	aff.observe("fp", "cold", Usage{InputTokens: 10, OutputTokens: 2}, now)
	if got := aff.warm("fp", now); len(got) != 0 {
		t.Errorf("warm = %v, want nothing for a target that reported no caching", got)
	}

	// A read proves the prefix was already there; a write proves it is there
	// now, for the next turn. Both count.
	aff.observe("fp", "read", Usage{CacheReadTokens: 5}, now)
	aff.observe("fp", "write", Usage{CacheWriteTokens: 5}, now)
	warm := aff.warm("fp", now)
	if !warm["read"] || !warm["write"] {
		t.Errorf("warm = %v, want both read and write evidence to count", warm)
	}
	if warm["cold"] {
		t.Error("a target that never cached must not be treated as warm")
	}

	// Without a fingerprint there is no conversation to attribute this to.
	aff.observe("", "read", Usage{CacheReadTokens: 5}, now)
	if aff.tracked() != 1 {
		t.Errorf("tracked = %d, want 1", aff.tracked())
	}
}

// TestCacheAffinityExpiresWithTheProvidersCache: routing to a target whose cache
// has since expired buys a cache write rather than saving one.
func TestCacheAffinityExpiresWithTheProvidersCache(t *testing.T) {
	aff := newCacheAffinity(time.Minute, 100)
	now := time.Now()
	aff.observe("fp", "up", Usage{CacheReadTokens: 5}, now)

	if got := aff.warm("fp", now.Add(30*time.Second)); !got["up"] {
		t.Error("an observation inside the TTL should still be warm")
	}
	if got := aff.warm("fp", now.Add(2*time.Minute)); len(got) != 0 {
		t.Errorf("warm = %v, want nothing once the provider's cache expired", got)
	}
	if aff.tracked() != 0 {
		t.Errorf("tracked = %d, want the expired prefix to be dropped", aff.tracked())
	}
}

// TestCacheAffinityPrefersTheWarmTarget: after a failover leaves a conversation's
// prefix cached on the backup, later turns should return to that cache instead of
// going back to a target whose copy is cold.
func TestCacheAffinityPrefersTheWarmTarget(t *testing.T) {
	a := newFakeUpstream(t, "a")
	b := newFakeUpstream(t, "b")
	a.status.Store(http.StatusInternalServerError)
	a.stream.Store(true)
	b.stream.Store(true)
	b.streamUsage.Store(true)

	// a outweighs b a hundred to one, so every turn that lands on b is the
	// affinity decision rather than the weighted one.
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a, "b": b}, map[string]int{"a": 100, "b": 1},
		func(_ *Config, al *Alias) {
			off := false
			al.Sticky = &off
		})

	turn := func() {
		t.Helper()
		res := post(t, srv, "/v1/chat/completions", map[string]any{
			"model": "test-alias", "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
	}

	// The first turn fails over to b, which reports a cache read: b now holds
	// this prefix.
	turn()
	if b.hits() != 1 {
		t.Fatalf("b hits = %d, want 1 after failover", b.hits())
	}

	for i := range 5 {
		before := a.hits()
		turn()
		if a.hits() != before {
			t.Fatalf("turn %d went to a although b holds a warm cache", i+2)
		}
	}
	if b.hits() != 6 {
		t.Errorf("b hits = %d, want 6 (every turn after the first)", b.hits())
	}
}

// TestCacheAffinityDisabledLeavesWeightedSelection: the store is absent when the
// feature is off, and a nil store must stay inert because the selection path
// consults it unconditionally.
func TestCacheAffinityDisabledLeavesWeightedSelection(t *testing.T) {
	if !(&Config{}).CacheAffinityOn() {
		t.Error("cache affinity should default to on")
	}

	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(c *Config, _ *Alias) {
			off := false
			c.Defaults.CacheAffinity = &off
		})

	if srv.router.affinity != nil {
		t.Error("affinity store should not be built when the feature is off")
	}
	if got := srv.router.affinityIndex(nil, "fp", time.Now()); got != -1 {
		t.Errorf("affinityIndex = %d, want -1 with the feature off", got)
	}
	srv.router.affinity.observe("fp", "a", Usage{CacheReadTokens: 1}, time.Now())
	if got := srv.router.affinity.warm("fp", time.Now()); got != nil {
		t.Errorf("warm = %v, want nil with the feature off", got)
	}
}

// TestTranslatedTargetOpenAIClientToAnthropicUpstream: an OpenAI-shaped client
// reaching Claude's native endpoint. The client must never learn that the wire
// changed underneath it, and the upstream must receive a request it can accept.
func TestTranslatedTargetOpenAIClientToAnthropicUpstream(t *testing.T) {
	a := newFakeUpstream(t, "a")
	a.anthropic.Store(true)
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(_ *Config, al *Alias) {
			al.Targets[0].API = APIAnthropicMessages
			al.Targets[0].Model = "claude-opus-5-5"
		})

	res := post(t, srv, "/v1/chat/completions", map[string]any{
		"model": "test-alias",
		"messages": []any{
			map[string]any{"role": "system", "content": "be terse"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}, nil)
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200; body: %s", res.StatusCode, body)
	}

	got := a.seen()
	if len(got) != 1 {
		t.Fatalf("upstream hits = %d, want 1", len(got))
	}
	if got[0].Path != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", got[0].Path)
	}
	if _, ok := got[0].Body["max_tokens"]; !ok {
		t.Error("translated request lacks max_tokens, which the Messages API requires")
	}
	if _, ok := got[0].Body["system"]; !ok {
		t.Error("the system message was not hoisted to the top level")
	}
	msgs, _ := got[0].Body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("translated messages = %d, want 1 (the system turn is hoisted)", len(msgs))
	}
	if first, _ := msgs[0].(map[string]any); first["role"] != "user" {
		t.Errorf("first translated role = %v, want user", first["role"])
	}

	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", out.Object)
	}
	if out.Model != "test-alias" {
		t.Errorf("model = %q, want the alias", out.Model)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "hi" {
		t.Errorf("choices = %+v, want the upstream's text", out.Choices)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop (mapped from end_turn)", out.Choices[0].FinishReason)
	}
	if out.Usage.PromptTokens != 7 {
		t.Errorf("prompt_tokens = %d, want 7", out.Usage.PromptTokens)
	}
}

// TestTranslatedTargetAnthropicClientToOpenAIUpstream: the other direction, which
// is what lets an Anthropic-wire client use an OpenAI-shaped provider.
func TestTranslatedTargetAnthropicClientToOpenAIUpstream(t *testing.T) {
	a := newFakeUpstream(t, "a")
	srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
		func(_ *Config, al *Alias) {
			al.API = APIAnthropicMessages
			al.Targets[0].API = APIOpenAICompletions
			al.Targets[0].Model = "anthropic/claude-opus-5.5"
		})

	res := post(t, srv, "/v1/messages", map[string]any{
		"model":      "test-alias",
		"system":     "be terse",
		"max_tokens": 128,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200; body: %s", res.StatusCode, body)
	}

	got := a.seen()
	if len(got) != 1 {
		t.Fatalf("upstream hits = %d, want 1", len(got))
	}
	if got[0].Path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", got[0].Path)
	}
	msgs, _ := got[0].Body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("translated messages = %d, want 2 (system + user)", len(msgs))
	}
	if first, _ := msgs[0].(map[string]any); first["role"] != "system" {
		t.Errorf("first translated role = %v, want system", first["role"])
	}
	if _, ok := got[0].Body["max_tokens"]; !ok {
		t.Error("max_tokens was dropped in translation")
	}

	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Type != "message" || out.Role != "assistant" {
		t.Errorf("envelope = %+v, want an assistant message", out)
	}
	if out.Model != "test-alias" {
		t.Errorf("model = %q, want the alias", out.Model)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "hi" {
		t.Errorf("content = %+v, want the upstream's text", out.Content)
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn (mapped from stop)", out.StopReason)
	}
}

// TestTranslatedStreamsBothDirections: the streaming path is where translation is
// hardest, and where a client would notice immediately if it were wrong.
func TestTranslatedStreamsBothDirections(t *testing.T) {
	t.Run("openai client, anthropic upstream", func(t *testing.T) {
		a := newFakeUpstream(t, "a")
		a.anthropic.Store(true)
		a.stream.Store(true)
		srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
			func(_ *Config, al *Alias) {
				al.Targets[0].API = APIAnthropicMessages
				al.Targets[0].Model = "claude-opus-5-5"
			})

		res := post(t, srv, "/v1/chat/completions", map[string]any{
			"model": "test-alias", "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
		body, _ := io.ReadAll(res.Body)
		got := string(body)

		if !strings.Contains(got, `"object":"chat.completion.chunk"`) {
			t.Errorf("stream is not OpenAI-shaped:\n%s", got)
		}
		if !strings.Contains(got, `"content":"toka"`) {
			t.Errorf("stream lost the upstream's text:\n%s", got)
		}
		if !strings.Contains(got, `"finish_reason":"stop"`) {
			t.Errorf("stream never reported a finish reason:\n%s", got)
		}
		if !strings.HasSuffix(got, "data: [DONE]\n\n") {
			t.Errorf("stream is missing its terminator:\n%q", got)
		}
		if strings.Contains(got, "message_start") {
			t.Errorf("Anthropic events leaked to an OpenAI client:\n%s", got)
		}
	})

	t.Run("anthropic client, openai upstream", func(t *testing.T) {
		a := newFakeUpstream(t, "a")
		a.stream.Store(true)
		a.chunks.Store(2)
		srv := newTestServer(t, map[string]*fakeUpstream{"a": a}, map[string]int{"a": 1},
			func(_ *Config, al *Alias) {
				al.API = APIAnthropicMessages
				al.Targets[0].API = APIOpenAICompletions
			})

		res := post(t, srv, "/v1/messages", map[string]any{
			"model": "test-alias", "stream": true, "max_tokens": 64,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
		body, _ := io.ReadAll(res.Body)
		got := string(body)

		for _, want := range []string{
			"event: message_start",
			"event: content_block_delta",
			"event: message_delta",
			"event: message_stop",
			`"text":"toka"`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("Anthropic stream is missing %s:\n%s", want, got)
			}
		}
		if strings.Contains(got, "chat.completion.chunk") {
			t.Errorf("OpenAI chunks leaked to an Anthropic client:\n%s", got)
		}
	})
}

// TestUntranslatablePairRejectedAtLoad: a pair the router cannot bridge must fail
// at startup rather than as a mangled request at runtime.
func TestUntranslatablePairRejectedAtLoad(t *testing.T) {
	_, err := loadYAML(t, `upstreams:
  a:
    baseUrl: https://example.test/v1
    apiKey: k
models:
  alias-one:
    api: openai-responses
    targets:
      - {upstream: a, model: m, weight: 1, api: anthropic-messages}
`)
	if err == nil {
		t.Fatal("accepted a client/upstream pair the router cannot translate")
	}
	if !strings.Contains(err.Error(), "no translation") {
		t.Errorf("error does not name the missing translation: %v", err)
	}

	// The supported pair loads, so the check is not simply rejecting everything.
	if _, err := loadYAML(t, `upstreams:
  a:
    baseUrl: https://example.test/v1
    apiKey: k
models:
  alias-one:
    api: openai-completions
    targets:
      - {upstream: a, model: m, weight: 1, api: anthropic-messages}
`); err != nil {
		t.Fatalf("a supported translation pair was rejected: %v", err)
	}
}

// TestShippedConfigResolves keeps router.yaml under test. It is the file an
// operator actually runs, so a change that breaks it should fail here rather
// than at startup on someone else's machine. Loading needs no credentials: keys
// are resolved when the router is built, not when the config is read.
func TestShippedConfigResolves(t *testing.T) {
	cfg, err := LoadConfig("../../router.yaml")
	if err != nil {
		t.Fatalf("the shipped router.yaml does not load: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("the shipped config produces warnings: %v", cfg.Warnings)
	}

	ups := cfg.UpstreamsInOrder()
	if len(ups) != 2 {
		t.Errorf("upstreams = %v, want exactly openrouter and anthropic", ups)
	}
	for _, want := range []string{"openrouter", "anthropic"} {
		if !slices.Contains(ups, want) {
			t.Errorf("upstream %q is missing from the shipped config", want)
		}
	}

	for _, name := range []string{
		"anthropic/claude-opus-5.5",
		"anthropic/claude-sonnet-5.5",
		"anthropic/claude-haiku-4.5",
		"claude-opus-5-5",
		"claude-sonnet-5-5",
	} {
		if _, ok := cfg.Models[name]; !ok {
			t.Errorf("alias %q is missing from the shipped config", name)
		}
	}

	// The OpenAI-wire aliases must name each provider's own model id, and keep
	// Claude direct as failover-only.
	opus := cfg.Models["anthropic/claude-opus-5.5"]
	if opus.API != APIOpenAICompletions {
		t.Errorf("opus alias api = %q, want openai-completions", opus.API)
	}
	if len(opus.Targets) != 2 {
		t.Fatalf("opus alias has %d targets, want 2", len(opus.Targets))
	}
	if opus.Targets[0].Upstream != "openrouter" || opus.Targets[0].Model != "anthropic/claude-opus-5.5" {
		t.Errorf("first opus target = %s/%s, want openrouter/anthropic/claude-opus-5.5",
			opus.Targets[0].Upstream, opus.Targets[0].Model)
	}
	if opus.Targets[1].Upstream != "anthropic" || opus.Targets[1].Model != "claude-opus-5-5" {
		t.Errorf("second opus target = %s/%s, want anthropic/claude-opus-5-5",
			opus.Targets[1].Upstream, opus.Targets[1].Model)
	}
	if got := []int{opus.Targets[0].WeightValue(), opus.Targets[1].WeightValue()}; !slices.Equal(got, []int{1, 0}) {
		t.Errorf("opus weights = %v, want [1 0] (OpenRouter carries, Claude direct is failover)", got)
	}

	// Every translating target must be a pair the router can actually bridge,
	// which LoadConfig already enforced — assert it here so the intent is
	// visible rather than implied.
	for name, alias := range cfg.Models {
		for i, target := range alias.Targets {
			if target.API != "" && !translatable(alias.API, target.API) {
				t.Errorf("model %q target %d: %s to %s is not translatable", name, i, alias.API, target.API)
			}
		}
	}
}
