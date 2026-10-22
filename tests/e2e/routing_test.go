package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const routingAnswer = `{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"backup answer"},"finish_reason":"stop"}]}`
const routingPrompt = `{"model":"chat","messages":[{"role":"user","content":"opening"}]}`

func routingConfig(a, b string, attempts, capacity int, extra string) string {
	return fmt.Sprintf(`apiKey: %q
defaults:
  maxAttempts: %d
  breakerFailures: 1
  breakerCooldown: 1h
  rateLimitCooldown: 1h
  rewriteModel: false
  injectStreamUsage: false
%s
upstreams:
  primary:
    baseUrl: %s
    maxConcurrency: %d
  backup:
    baseUrl: %s
    maxConcurrency: %d
models:
  chat:
    api: openai-completions
    targets:
      - {upstream: primary, weight: 1}
      - {upstream: backup, weight: 0}
`, gatewayKey, attempts, extra, a, capacity, b, capacity)
}

func routingProvider(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}
func routingInfer(t *testing.T, p *routerProcess, body string, headers map[string]string) httpResult {
	t.Helper()
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Authorization"] = "Bearer " + gatewayKey
	return request(t, http.MethodPost, p.URL+"/v1/chat/completions", body, headers)
}
func routingError(t *testing.T, r httpResult, status int, kind string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status=%d body=%s; want %d", r.Status, r.Body, status)
	}
	obj := decodeJSON(t, r.Body)
	e, ok := obj["error"].(map[string]any)
	if !ok || e["type"] != kind {
		t.Fatalf("error envelope=%s; want type %q", r.Body, kind)
	}
}
func routingOK(t *testing.T, r httpResult) {
	t.Helper()
	if r.Status != 200 || !bytes.Equal(r.Body, []byte(routingAnswer)) {
		t.Fatalf("response=%d %s; want literal fixture", r.Status, r.Body)
	}
}

func TestRoutingH3StatusAndBudget(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 422, 429, 500, 502, 503, 599} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var mu sync.Mutex
			var dispatches []string
			record := func(s string) { mu.Lock(); dispatches = append(dispatches, s); mu.Unlock() }
			a := routingProvider(t, func(w http.ResponseWriter, r *http.Request) {
				record("primary")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"literal provider refusal"}}`)
			})
			b := routingProvider(t, func(w http.ResponseWriter, r *http.Request) { record("backup"); io.WriteString(w, routingAnswer) })
			p := startRouter(t, routingConfig(a.URL, b.URL, 2, 8, ""))
			got := routingInfer(t, p, routingPrompt, nil)
			retry := status == 401 || status == 402 || status == 403 || status == 429 || status >= 500
			if retry {
				routingOK(t, got)
			} else if got.Status != status {
				t.Fatalf("status=%d want %d: %s", got.Status, status, got.Body)
			}
			mu.Lock()
			defer mu.Unlock()
			want := "primary"
			if retry {
				want += ",backup"
			}
			if strings.Join(dispatches, ",") != want {
				t.Fatalf("dispatches=%v want %s", dispatches, want)
			}
		})
	}
	for _, budget := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("all-fail-budget-%d", budget), func(t *testing.T) {
			var aHits, bHits atomic.Int32
			a := routingProvider(t, func(w http.ResponseWriter, r *http.Request) { aHits.Add(1); w.WriteHeader(500) })
			b := routingProvider(t, func(w http.ResponseWriter, r *http.Request) { bHits.Add(1); w.WriteHeader(503) })
			p := startRouter(t, routingConfig(a.URL, b.URL, budget, 8, ""))
			routingError(t, routingInfer(t, p, routingPrompt, nil), 502, "all_targets_failed")
			want := int32(1)
			if budget > 1 {
				want = 2
			}
			if aHits.Load() != 1 || aHits.Load()+bHits.Load() != want {
				t.Fatalf("dispatches primary=%d backup=%d budget=%d", aHits.Load(), bHits.Load(), budget)
			}
		})
	}
}

func TestRoutingH3SaturationAndWeightZero(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var aHits, bHits atomic.Int32
	hold := func(counter *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			entered <- struct{}{}
			select {
			case <-release:
				io.WriteString(w, routingAnswer)
			case <-r.Context().Done():
			}
		}
	}
	a := routingProvider(t, hold(&aHits))
	b := routingProvider(t, hold(&bHits))
	p := startRouter(t, routingConfig(a.URL, b.URL, 1, 1, ""))
	results := make(chan httpResult, 2)
	go func() { results <- routingInfer(t, p, routingPrompt, nil) }()
	awaitSignal(t, entered, "primary occupied")
	if aHits.Load() != 1 || bHits.Load() != 0 {
		t.Fatal("weight-zero backup chosen while primary eligible")
	}
	go func() { results <- routingInfer(t, p, routingPrompt, nil) }()
	awaitSignal(t, entered, "backup occupied despite dispatch budget one")
	routingError(t, routingInfer(t, p, routingPrompt, nil), 503, "targets_busy")
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Fatalf("busy request dispatched: %d/%d", aHits.Load(), bHits.Load())
	}
	once.Do(func() { close(release) })
	for range 2 {
		select {
		case r := <-results:
			routingOK(t, r)
		case <-time.After(10 * time.Second):
			t.Fatal("held request did not complete")
		}
	}
}

func routingStreamRequest(t *testing.T, p *routerProcess, ctx context.Context, stream bool) *http.Response {
	t.Helper()
	body := strings.TrimSuffix(routingPrompt, "}") + fmt.Sprintf(`,"stream":%t}`, stream)
	req, err := http.NewRequestWithContext(ctx, "POST", p.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestRoutingH4IncrementalExactBytes(t *testing.T) {
	const first = ": provider keepalive\n\ndata: {\"id\":\"chunk\",\"unknown_provider_field\":{\"opaque\":true},\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"first\"}}]}\n\n"
	const rest = "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"alpha\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var backup atomic.Int32
	a := routingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			io.WriteString(w, rest)
		case <-r.Context().Done():
		}
	})
	b := routingProvider(t, func(w http.ResponseWriter, r *http.Request) { backup.Add(1); io.WriteString(w, routingAnswer) })
	p := startRouter(t, routingConfig(a.URL, b.URL, 2, 8, ""))
	res := routingStreamRequest(t, p, context.Background(), true)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d", res.StatusCode)
	}
	prefix := make([]byte, len(first))
	if _, err := io.ReadFull(res.Body, prefix); err != nil {
		t.Fatal(err)
	}
	if string(prefix) != first {
		t.Fatalf("first bytes=%q want %q", prefix, first)
	}
	once.Do(func() { close(release) })
	tail, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(tail) != rest {
		t.Fatalf("remaining bytes=%q want %q", tail, rest)
	}
	if backup.Load() != 0 {
		t.Fatal("stream replayed on backup")
	}
}

func TestRoutingH4CommittedTruncationNeverReplays(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var backup atomic.Int32
			partial := "{\"id\":\"partial"
			if stream {
				partial = "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
			}
			a := routingProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(partial)+100))
				w.WriteHeader(200)
				io.WriteString(w, partial)
				w.(http.Flusher).Flush()
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
			})
			b := routingProvider(t, func(w http.ResponseWriter, r *http.Request) { backup.Add(1); io.WriteString(w, routingAnswer) })
			p := startRouter(t, routingConfig(a.URL, b.URL, 2, 8, ""))
			res := routingStreamRequest(t, p, context.Background(), stream)
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != 200 || string(body) != partial {
				t.Fatalf("committed response=%d %q; want 200 %q", res.StatusCode, body, partial)
			}
			if backup.Load() != 0 {
				t.Fatal("committed failure dispatched backup")
			}
		})
	}
}

func TestRoutingH4ActiveCancellationReleasesWithoutReplay(t *testing.T) {
	canceled := make(chan struct{})
	var calls, backup atomic.Int32
	a := routingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			io.WriteString(w, routingAnswer)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	})
	b := routingProvider(t, func(w http.ResponseWriter, r *http.Request) { backup.Add(1); io.WriteString(w, routingAnswer) })
	p := startRouter(t, routingConfig(a.URL, b.URL, 2, 1, ""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := routingStreamRequest(t, p, ctx, true)
	buf := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(res.Body, buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	res.Body.Close()
	awaitSignal(t, canceled, "provider cancellation")
	probes := int32(0)
	eventually(t, "canceled primary capacity released", func() bool {
		probes++
		r := routingInfer(t, p, routingPrompt, nil)
		return r.Status == 200 && calls.Load() > 1
	})
	if calls.Load()+backup.Load() != 1+probes {
		t.Fatalf("cancellation replayed: primary=%d backup=%d client requests=%d", calls.Load(), backup.Load(), 1+probes)
	}
}

func TestRoutingR2StickyAndPrefixContinuity(t *testing.T) {
	for _, prefix := range []bool{false, true} {
		t.Run(fmt.Sprintf("prefix=%t", prefix), func(t *testing.T) {
			var failA, failB atomic.Bool
			handler := func(name string, fail *atomic.Bool) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if fail.Load() {
						w.WriteHeader(503)
						return
					}
					w.Header().Set("X-Fixture-Provider", name)
					io.WriteString(w, routingAnswer)
				}
			}
			a := routingProvider(t, handler("a", &failA))
			b := routingProvider(t, handler("b", &failB))
			cfg := routingConfig(a.URL, b.URL, 2, 8, fmt.Sprintf("  sticky: true\n  stickyByPrefix: %t", prefix))
			cfg = strings.Replace(cfg, "upstream: backup, weight: 0", "upstream: backup, weight: 1", 1)
			p := startRouter(t, cfg)
			headers := map[string]string{}
			if !prefix {
				headers["X-Session-ID"] = "stable-session"
			}
			first := routingInfer(t, p, routingPrompt, headers)
			routingOK(t, first)
			chosen := first.Header.Get("X-Fixture-Provider")
			if chosen != "a" && chosen != "b" {
				t.Fatalf("missing provider marker: %v", first.Header)
			}
			continuation := `{"model":"chat","messages":[{"role":"user","content":"opening"},{"role":"assistant","content":"previous answer"},{"role":"user","content":"next turn"}]}`
			for range 4 {
				next := routingInfer(t, p, continuation, headers)
				routingOK(t, next)
				if next.Header.Get("X-Fixture-Provider") != chosen {
					t.Fatal("conversation lost its provider pin")
				}
			}
			if !prefix {
				different := routingInfer(t, p, `{"model":"chat","messages":[{"role":"user","content":"different opening under explicit identity"}]}`, headers)
				routingOK(t, different)
				if different.Header.Get("X-Fixture-Provider") != chosen {
					t.Fatal("prefix inference overrode explicit session identity")
				}
			}
			if chosen == "a" {
				failA.Store(true)
			} else {
				failB.Store(true)
			}
			fallback := routingInfer(t, p, continuation, headers)
			routingOK(t, fallback)
			if fallback.Header.Get("X-Fixture-Provider") == chosen {
				t.Fatal("unhealthy sticky target prevented fallback")
			}
			again := routingInfer(t, p, continuation, headers)
			routingOK(t, again)
			if again.Header.Get("X-Fixture-Provider") != fallback.Header.Get("X-Fixture-Provider") {
				t.Fatal("fallback continuity lost")
			}
		})
	}
}

func TestRoutingR2CooldownSingleRecoveryProbe(t *testing.T) {
	var primary, backup atomic.Int32
	probe := make(chan struct{}, 32)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	a := routingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		n := primary.Add(1)
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		probe <- struct{}{}
		select {
		case <-release:
			io.WriteString(w, routingAnswer)
		case <-r.Context().Done():
		}
	})
	b := routingProvider(t, func(w http.ResponseWriter, r *http.Request) { backup.Add(1); io.WriteString(w, routingAnswer) })
	cfg := strings.Replace(routingConfig(a.URL, b.URL, 1, 16, ""), "breakerCooldown: 1h", "breakerCooldown: 200ms", 1)
	p := startRouter(t, cfg)
	routingError(t, routingInfer(t, p, routingPrompt, nil), 502, "all_targets_failed")
	routingOK(t, routingInfer(t, p, routingPrompt, nil))
	if primary.Load() != 1 {
		t.Fatal("cooldown did not suppress primary")
	}
	result := make(chan httpResult, 1)
	// The bounded ticker advances wall-clock cooldown, while provider entry is
	// the ordering gate. No request can finish the recovery probe until released.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	searching := true
	for searching {
		select {
		case <-ctx.Done():
			t.Fatal("no recovery probe admitted")
		case <-ticker.C:
			go func() { result <- routingInfer(t, p, routingPrompt, nil) }()
			select {
			case <-probe:
				searching = false
			case r := <-result:
				routingOK(t, r)
			case <-ctx.Done():
				t.Fatal("recovery selection stalled")
			}
		}
	}
	for range 8 {
		routingOK(t, routingInfer(t, p, routingPrompt, nil))
	}
	if primary.Load() != 2 {
		t.Fatalf("half-open admitted %d probes; want one", primary.Load()-1)
	}
	once.Do(func() { close(release) })
	select {
	case r := <-result:
		routingOK(t, r)
	case <-ctx.Done():
		t.Fatal("probe did not finish")
	}
	before := primary.Load()
	routingOK(t, routingInfer(t, p, routingPrompt, nil))
	if primary.Load() != before+1 {
		t.Fatal("successful probe did not restore weighted primary")
	}
}

func TestRoutingR2CacheAffinityRespectsWeightPinAndHealth(t *testing.T) {
	for _, mode := range []string{"weight", "pin", "health"} {
		t.Run(mode, func(t *testing.T) {
			var hold, fail atomic.Bool
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			const warm = `{"id":"warm","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"warm"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101,"prompt_tokens_details":{"cached_tokens":90}}}`
			a := routingProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if fail.Load() {
					w.WriteHeader(503)
					return
				}
				if hold.Load() {
					entered <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("X-Fixture-Provider", "primary")
				io.WriteString(w, warm)
			})
			b := routingProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Fixture-Provider", "backup")
				io.WriteString(w, warm)
			})
			extra := "  sticky: false"
			if mode == "pin" {
				extra = "  sticky: true"
			}
			p := startRouter(t, routingConfig(a.URL, b.URL, 2, 1, extra))
			headers := map[string]string{"X-Session-ID": "pinned-primary"}
			first := routingInfer(t, p, routingPrompt, headers)
			if first.Status != 200 || first.Header.Get("X-Fixture-Provider") != "primary" {
				t.Fatalf("primary warm-up failed: %d %s", first.Status, first.Body)
			}
			if mode == "health" {
				fail.Store(true)
				fallback := routingInfer(t, p, routingPrompt, nil)
				if fallback.Status != 200 || fallback.Header.Get("X-Fixture-Provider") != "backup" {
					t.Fatalf("warm cache prevented healthy fallback: %d %s", fallback.Status, fallback.Body)
				}
				next := routingInfer(t, p, routingPrompt, nil)
				if next.Status != 200 || next.Header.Get("X-Fixture-Provider") != "backup" {
					t.Fatal("warm unhealthy primary attracted traffic")
				}
				return
			}
			// Warm the failover-only cache through an actual capacity fallback, without
			// mutating router state or relying on weighted random selection.
			hold.Store(true)
			result := make(chan httpResult, 1)
			go func() {
				result <- routingInfer(t, p, routingPrompt, map[string]string{"X-Session-ID": "capacity-holder"})
			}()
			awaitSignal(t, entered, "primary held while backup cache is warmed")
			backupWarm := routingInfer(t, p, routingPrompt, map[string]string{"X-Session-ID": "cache-observer"})
			if backupWarm.Status != 200 || backupWarm.Header.Get("X-Fixture-Provider") != "backup" {
				t.Fatalf("backup cache warm-up failed: %d %s", backupWarm.Status, backupWarm.Body)
			}
			hold.Store(false)
			once.Do(func() { close(release) })
			select {
			case r := <-result:
				if r.Status != 200 {
					t.Fatalf("held request=%d", r.Status)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("held warm-up did not finish")
			}
			for range 4 {
				h := map[string]string{}
				if mode == "pin" {
					h["X-Session-ID"] = "pinned-primary"
				}
				next := routingInfer(t, p, routingPrompt, h)
				if next.Status != 200 || next.Header.Get("X-Fixture-Provider") != "primary" {
					t.Fatalf("warm failover-only cache overrode %s: %d %s", mode, next.Status, next.Body)
				}
			}
		})
	}
}

func TestRoutingR2ExplicitPinWinsOverOtherWarmCache(t *testing.T) {
	for _, disableAliasSticky := range []bool{false, true} {
		t.Run(fmt.Sprintf("aliasStickyDisabled=%t", disableAliasSticky), func(t *testing.T) {
			var held atomic.Value
			held.Store("")
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			provider := func(name string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if held.Load().(string) == name && bytes.Contains(body, []byte("hold-marker")) {
						entered <- struct{}{}
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					w.Header().Set("X-Fixture-Provider", name)
					// Only the competing provider reports cache reuse, so normal affinity
					// would prefer it for the shared opening prefix.
					cache := 0
					if held.Load().(string) != "" && held.Load().(string) != name {
						cache = 90
					}
					fmt.Fprintf(w, `{"id":"cache","choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101,"prompt_tokens_details":{"cached_tokens":%d}}}`, cache)
				}
			}
			a := routingProvider(t, provider("a"))
			b := routingProvider(t, provider("b"))
			cfg := routingConfig(a.URL, b.URL, 2, 1, "  sticky: true")
			cfg = strings.Replace(cfg, "upstream: backup, weight: 0", "upstream: backup, weight: 1", 1)
			if disableAliasSticky {
				cfg = strings.Replace(cfg, "    api: openai-completions", "    sticky: false\n    api: openai-completions", 1)
			}
			cfg += `
  hold-a:
    api: openai-completions
    targets:
      - {upstream: primary, weight: 1}
  hold-b:
    api: openai-completions
    targets:
      - {upstream: backup, weight: 1}
`
			p := startRouter(t, cfg)
			headers := map[string]string{"X-Session-ID": "explicit-pin"}
			first := routingInfer(t, p, routingPrompt, headers)
			chosen := first.Header.Get("X-Fixture-Provider")
			if first.Status != 200 || (chosen != "a" && chosen != "b") {
				t.Fatalf("initial pin response=%d %s", first.Status, first.Body)
			}
			held.Store(chosen)
			holdBody := fmt.Sprintf(`{"model":"hold-%s","messages":[{"role":"user","content":"opening"},{"role":"assistant","content":"answer"},{"role":"user","content":"hold-marker"}]}`, chosen)
			result := make(chan httpResult, 1)
			go func() {
				result <- routingInfer(t, p, holdBody, map[string]string{"X-Session-ID": "distinct-capacity-holder"})
			}()
			awaitSignal(t, entered, "pinned provider occupied")
			warm := routingInfer(t, p, routingPrompt, map[string]string{"X-Session-ID": "warm-other"})
			if warm.Status != 200 || warm.Header.Get("X-Fixture-Provider") == chosen {
				t.Fatalf("other cache was not warmed: %d %s", warm.Status, warm.Body)
			}
			once.Do(func() { close(release) })
			select {
			case r := <-result:
				if r.Status != 200 {
					t.Fatalf("held pin response=%d", r.Status)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("held pin did not finish")
			}
			for range 4 {
				next := routingInfer(t, p, routingPrompt, headers)
				if next.Status != 200 {
					t.Fatalf("response=%d %s", next.Status, next.Body)
				}
				got := next.Header.Get("X-Fixture-Provider")
				if disableAliasSticky {
					if got == chosen {
						t.Fatal("alias sticky:false failed to override global sticky:true and follow competing warm cache")
					}
				} else if got != chosen {
					t.Fatal("cache affinity overrode explicit session pin")
				}
			}
		})
	}
}
