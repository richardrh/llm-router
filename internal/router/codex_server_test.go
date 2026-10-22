package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// A separate test process speaks the real stdio JSON-RPC boundary, not a mocked
// Manager. No installed CLI, login, account, or inference is involved.
func TestCodexHTTPFixture(t *testing.T) {
	mode := ""
	for i, arg := range os.Args {
		if arg == "--codex-http-fixture" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
		}
	}
	if mode == "" {
		return
	}
	send := func(v any) { b, _ := json.Marshal(v); fmt.Println(string(b)) }
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scan.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		if string(req.ID) == "99" {
			send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thr", "turnId": "turn", "itemId": "final", "delta": "continued"}})
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thr", "turn": map[string]any{"id": "turn", "status": "completed"}}})
			continue
		}
		var result any = map[string]any{}
		switch req.Method {
		case "account/read":
			result = map[string]any{"account": map[string]any{"type": "chatgpt", "planType": "plus"}, "requiresOpenaiAuth": true}
		case "thread/start":
			result = map[string]any{"thread": map[string]any{"id": "thr"}}
		case "turn/start":
			if mode == "early-error" {
				send(map[string]any{"id": req.ID, "error": map[string]any{"code": -32000, "message": "fixture failed before output"}})
				continue
			}
			result = map[string]any{"turn": map[string]any{"id": "turn", "status": "inProgress"}}
		}
		send(map[string]any{"id": req.ID, "result": result})
		if req.Method == "turn/start" {
			if mode == "tools" {
				send(map[string]any{"id": 99, "method": "item/tool/call", "params": map[string]any{"threadId": "thr", "turnId": "turn", "callId": "call-1", "tool": "lookup", "arguments": map[string]any{"q": "x"}}})
				continue
			}
			if mode == "zero" {
				send(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "thr", "tokenUsage": map[string]any{"last": map[string]any{"inputTokens": 0, "outputTokens": 0, "cachedInputTokens": 0}}}})
			}
			send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thr", "turnId": "turn", "itemId": "item", "delta": "hello"}})
			status := "completed"
			var failure any
			if mode == "late-error" {
				status = "failed"
				failure = map[string]any{"message": "fixture failed after output"}
			}
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thr", "turn": map[string]any{"id": "turn", "status": status, "error": failure}}})
		}
	}
	os.Exit(0)
}

func TestCodexHTTPProtocolsAndCommitBoundary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		api    APIProtocol
		stream bool
		mode   string
	}{
		{APIOpenAICompletions, false, "ok"}, {APIOpenAICompletions, true, "ok"},
		{APIOpenAICompletions, true, "early-error"}, {APIOpenAICompletions, true, "late-error"},
		{APIOpenAICompletions, false, "zero"}, {APIOpenAICompletions, true, "zero"},
	} {
		t.Run(fmt.Sprintf("%s/%t/%s", tc.api, tc.stream, tc.mode), func(t *testing.T) {
			cfg := &Config{Defaults: Defaults{MaxAttempts: 2, MaxBodyBytes: 1 << 20, BreakerFailures: 10, MaxStreamDuration: Duration(10 * time.Second)}, Upstreams: map[string]Upstream{"codex": {Kind: UpstreamCodex, Command: []string{exe, "-test.run=^TestCodexHTTPFixture$", "--", "--codex-http-fixture", tc.mode}, MaxConcurrency: 1, MaxSessions: 1}}, Models: map[string]Alias{"chat": {API: tc.api, Targets: []Target{{Upstream: "codex", Model: "observed-model", Weight: 1}}}}}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			router, err := NewRouter(cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer router.Close()
			server := NewServer(cfg, router, logger, nil)
			defer server.Close()
			path := "/v1/chat/completions"
			body := fmt.Sprintf(`{"model":"chat","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, tc.stream)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			got := rec.Body.String()
			if tc.mode == "early-error" {
				if rec.Code != http.StatusBadGateway || strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
					t.Fatalf("pre-output error committed: %d %s", rec.Code, got)
				}
				return
			}
			if rec.Code != http.StatusOK || !strings.Contains(got, "hello") {
				t.Fatalf("response: %d %s", rec.Code, got)
			}
			if tc.mode == "late-error" {
				if !strings.Contains(got, "upstream_error") || !strings.Contains(got, "[DONE]") {
					t.Fatalf("missing explicit stream failure: %s", got)
				}
				return
			}
			if tc.mode == "zero" {
				if !strings.Contains(got, `"usage"`) {
					t.Fatalf("required/reported usage lost: %s", got)
				}
			} else if strings.Contains(got, `"usage"`) {
				t.Fatalf("unknown usage invented: %s", got)
			}
			if tc.mode == "zero" {
				if !strings.Contains(logs.String(), "input_tokens=0") || strings.Contains(logs.String(), "usage=unreported") {
					t.Fatalf("reported zero accounting lost: %s", logs.String())
				}
			} else if !strings.Contains(logs.String(), "usage=unreported") {
				t.Fatalf("wire placeholders invented accounting: %s", logs.String())
			}
			if !strings.Contains(logs.String(), "cost_source=unreported") || strings.Contains(logs.String(), "cost_usd=") {
				t.Fatalf("subscription dollar cost invented: %s", logs.String())
			}
			if tc.stream && tc.api == APIOpenAICompletions && !strings.Contains(got, "[DONE]") {
				t.Fatalf("unterminated OpenAI stream: %s", got)
			}
		})
	}
}

func TestCodexHTTPDynamicToolsContinuation(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, api := range []APIProtocol{APIOpenAICompletions} {
		t.Run(string(api), func(t *testing.T) {
			cfg := &Config{Defaults: Defaults{MaxAttempts: 1, MaxBodyBytes: 1 << 20, BreakerFailures: 10, MaxStreamDuration: Duration(10 * time.Second)}, Upstreams: map[string]Upstream{"codex": {Kind: UpstreamCodex, Command: []string{exe, "-test.run=^TestCodexHTTPFixture$", "--", "--codex-http-fixture", "tools"}, MaxConcurrency: 1, MaxSessions: 1}}, Models: map[string]Alias{"chat": {API: api, Targets: []Target{{Upstream: "codex", Model: "observed-model", Weight: 1}}}}}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			router, err := NewRouter(cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer router.Close()
			server := NewServer(cfg, router, logger, nil)
			defer server.Close()
			path := "/v1/chat/completions"
			first := `{"model":"chat","stream":true,"messages":[{"role":"user","content":"find x"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`
			next := `{"model":"chat","messages":[{"role":"user","content":"find x"},{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"found"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`
			call := func(body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("X-Session-ID", "tools-session")
				rec := httptest.NewRecorder()
				server.Handler().ServeHTTP(rec, req)
				return rec
			}
			rec := call(first)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "lookup") || !strings.Contains(rec.Body.String(), "call-1") {
				t.Fatalf("tool call: %d %s", rec.Code, rec.Body.String())
			}
			rec = call(next)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "continued") {
				t.Fatalf("tool continuation: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCodexHTTPFailoverOnlyBeforeOutput(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"early-error", "late-error"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"backup\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
			}))
			defer backup.Close()
			cfg := &Config{Defaults: Defaults{MaxAttempts: 2, MaxBodyBytes: 1 << 20, BreakerFailures: 10, MaxStreamDuration: Duration(10 * time.Second), FirstByteTimeout: Duration(10 * time.Second), StreamIdleTimeout: Duration(10 * time.Second)}, Upstreams: map[string]Upstream{
				"codex":  {Kind: UpstreamCodex, Command: []string{exe, "-test.run=^TestCodexHTTPFixture$", "--", "--codex-http-fixture", mode}, MaxConcurrency: 1, MaxSessions: 1},
				"backup": {BaseURL: backup.URL, MaxConcurrency: 1},
			}, Models: map[string]Alias{"chat": {API: APIOpenAICompletions, Targets: []Target{{Upstream: "codex", Model: "observed-model", Weight: 1}, {Upstream: "backup", Model: "backup-model", Weight: 0}}}}}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			router, err := NewRouter(cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer router.Close()
			server := NewServer(cfg, router, logger, nil)
			defer server.Close()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if mode == "early-error" {
				if calls != 1 || !strings.Contains(rec.Body.String(), "backup") {
					t.Fatalf("expected precommit failover: calls=%d body=%s", calls, rec.Body.String())
				}
			} else if calls != 0 || !strings.Contains(rec.Body.String(), "upstream_error") {
				t.Fatalf("postcommit replay/error: calls=%d body=%s", calls, rec.Body.String())
			}
		})
	}
}

func TestCodexDiscoveryReportsAdapterCapabilities(t *testing.T) {
	cfg := &Config{Upstreams: map[string]Upstream{"codex": {Kind: UpstreamCodex, MaxConcurrency: 1}}, Models: map[string]Alias{"subscription": {API: APIOpenAICompletions, Targets: []Target{{Upstream: "codex", Model: "unlisted-model", Weight: 1}}}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router, err := NewRouter(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	server := NewServer(cfg, router, logger, nil)
	defer server.Close()
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/model_group/info", nil))
	var response struct {
		Data []struct {
			Info modelInfo `json:"model_info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(response.Data) != 1 {
		t.Fatalf("discovery: %d %s", rec.Code, rec.Body.String())
	}
	info := response.Data[0].Info
	if !info.SupportsFunctionCalling || info.SupportsVision {
		t.Fatalf("adapter capabilities: %+v", info)
	}
	if info.MaxInputTokens != 0 || info.MaxTokens != 0 || info.InputCostPerTok != 0 || info.OutputCostPerTk != 0 {
		t.Fatalf("invented metadata: %+v", info)
	}
}

func TestCodexHTTPRejectsExplicitOutputLimits(t *testing.T) {
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", field, stream), func(t *testing.T) {
				// An invalid executable makes any accidental backend launch visible.
				cfg := &Config{Defaults: Defaults{MaxAttempts: 1, MaxBodyBytes: 1 << 20, MaxStreamDuration: Duration(time.Second)}, Upstreams: map[string]Upstream{"codex": {Kind: UpstreamCodex, Command: []string{"/nonexistent/codex-must-not-start"}, MaxConcurrency: 1}}, Models: map[string]Alias{"chat": {API: APIOpenAICompletions, Targets: []Target{{Upstream: "codex", Model: "observed-model", Weight: 1}}}}}
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				router, err := NewRouter(cfg, logger)
				if err != nil {
					t.Fatal(err)
				}
				defer router.Close()
				server := NewServer(cfg, router, logger, nil)
				defer server.Close()
				body := fmt.Sprintf(`{"model":"chat","stream":%t,"%s":100,"messages":[{"role":"user","content":"hello"}]}`, stream, field)
				rec := httptest.NewRecorder()
				server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported_request") || !strings.Contains(rec.Body.String(), field) {
					t.Fatalf("explicit limit silently accepted or misclassified: %d %s", rec.Code, rec.Body.String())
				}
				if strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
					t.Fatal("unsupported request committed a stream")
				}
			})
		}
	}
}
