package router

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFakeCLI writes an executable that plays Claude Code's stream-json
// contract, and dumps the prompt it was handed to capturePath so a test can
// prove the conversation reached it.
func writeFakeCLI(t *testing.T, capturePath string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-claude")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("printf '%s' \"$2\" > " + capturePath + "\n")
	for _, line := range []string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":" world"}}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Hello world","total_cost_usd":0.0123,"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":900}}`,
	} {
		b.WriteString("printf '%s\\n' '" + line + "'\n")
	}
	if err := os.WriteFile(script, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}
	return script
}

func writePersistentFakeCLI(t *testing.T, capturePath string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-claude-session")
	content := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"fake-session\"}'\nn=0\nwhile IFS= read -r line; do\n" +
		"  printf '%s\\n' \"$line\" >> " + capturePath + "\n" +
		"  n=$((n + 1))\n" +
		"  printf '{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"turn%s\"}}}\\n' \"$n\"\n" +
		"  printf '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"turn%s\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}\\n' \"$n\"\n" +
		"done\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write persistent fake cli: %v", err)
	}
	return script
}

func writeClientToolFakeCLI(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-claude-client-tools")
	content := "#!/bin/sh\nn=0\nwhile IFS= read -r line; do\n" +
		"  n=$((n + 1))\n" +
		"  if [ \"$n\" -eq 1 ]; then\n" +
		"    printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"{\\\"kind\\\":\\\"client_tool_request\\\",\\\"id\\\":\\\"call-1\\\",\\\"name\\\":\\\"lookup\\\",\\\"arguments\\\":{\\\"q\\\":\\\"x\\\"}}\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}'\n" +
		"  else\n" +
		"    printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"{\\\"kind\\\":\\\"final_response\\\",\\\"text\\\":\\\"done\\\"}\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}'\n" +
		"  fi\n" +
		"done\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write client tool fake cli: %v", err)
	}
	return script
}

// cliTestServer builds a server whose only alias is served by a cli upstream
// running the given script.
func cliTestServer(t *testing.T, api APIProtocol, script string) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Defaults: Defaults{
			MaxAttempts:       1,
			BreakerFailures:   100,
			MaxBodyBytes:      1 << 20,
			FirstByteTimeout:  Duration(20 * time.Second),
			StreamIdleTimeout: Duration(20 * time.Second),
			MaxStreamDuration: Duration(30 * time.Second),
			RewriteModel:      true,
			CacheAffinityTTL:  Duration(time.Minute),
		},
		Upstreams: map[string]Upstream{
			"cli": {
				Kind: UpstreamCLI,
				// $1 is -p, $2 is the prompt, so the fake can capture it.
				Command:        []string{script, "-p", "{prompt}", "--output-format", "stream-json"},
				MaxConcurrency: 4,
				Timeout:        Duration(20 * time.Second),
			},
		},
		Models: map[string]Alias{
			"test-alias": {
				API:             api,
				ContextWindow:   200000,
				MaxOutputTokens: 8000,
				Cost:            Cost{Input: 1, Output: 5},
				Targets:         []Target{{Upstream: "cli", Model: "cli-model", Weight: 1}},
			},
		},
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	r, err := NewRouter(cfg, log)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return NewServer(cfg, r, log, nil), &logs
}

func persistentCLITestServer(t *testing.T, script string) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Defaults: Defaults{
			MaxAttempts:       1,
			BreakerFailures:   100,
			MaxBodyBytes:      1 << 20,
			FirstByteTimeout:  Duration(20 * time.Second),
			StreamIdleTimeout: Duration(20 * time.Second),
			MaxStreamDuration: Duration(30 * time.Second),
			RewriteModel:      true,
			CacheAffinityTTL:  Duration(time.Minute),
		},
		Upstreams: map[string]Upstream{
			"cli": {
				Kind:           UpstreamCLI,
				Mode:           CLIPersistent,
				ToolMode:       CLIToolsClient,
				Command:        []string{script, "--model", "{model}", "-p", "--input-format", "stream-json", "--output-format", "stream-json"},
				MaxConcurrency: 4,
				MaxSessions:    2,
				Timeout:        Duration(20 * time.Second),
			},
		},
		Models: map[string]Alias{
			"test-alias": {
				API:             APIOpenAICompletions,
				ContextWindow:   200000,
				MaxOutputTokens: 8000,
				Cost:            Cost{Input: 1, Output: 5},
				Targets:         []Target{{Upstream: "cli", Model: "cli-model", Weight: 1}},
			},
		},
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	r, err := NewRouter(cfg, log)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(r.Close)
	return NewServer(cfg, r, log, nil), &logs
}

func cliPost(t *testing.T, srv *Server, path string, body map[string]any) (*http.Response, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	res := rec.Result()
	out, _ := io.ReadAll(res.Body)
	return res, string(out)
}

func cliPostWithHeaders(t *testing.T, srv *Server, path string, body map[string]any, headers map[string]string) (*http.Response, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	res := rec.Result()
	out, _ := io.ReadAll(res.Body)
	return res, string(out)
}

func TestPersistentCLIUpstreamKeepsAgentSession(t *testing.T) {
	inputFile := filepath.Join(t.TempDir(), "inputs.ndjson")
	script := writePersistentFakeCLI(t, inputFile)
	srv, _ := persistentCLITestServer(t, script)
	headers := map[string]string{"X-OMP-Session": "session-1"}

	res, body := cliPostWithHeaders(t, srv, "/api/v1/chat/completions", map[string]any{
		"model":    "test-alias",
		"messages": []any{map[string]any{"role": "user", "content": "first"}},
	}, headers)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"content":"turn1"`) {
		t.Fatalf("first response = %d %s", res.StatusCode, body)
	}

	res, body = cliPostWithHeaders(t, srv, "/api/v1/chat/completions", map[string]any{
		"model": "test-alias",
		"messages": []any{
			map[string]any{"role": "user", "content": "first"},
			map[string]any{"role": "assistant", "content": "turn1"},
			map[string]any{"role": "user", "content": "second"},
		},
	}, headers)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"content":"turn2"`) {
		t.Fatalf("second response = %d %s", res.StatusCode, body)
	}

	lines, err := os.ReadFile(inputFile)
	if err != nil {
		t.Fatalf("read captured inputs: %v", err)
	}
	captured := strings.Split(strings.TrimSpace(string(lines)), "\n")
	if len(captured) != 2 {
		t.Fatalf("captured %d messages, want 2: %q", len(captured), captured)
	}
	if !strings.Contains(captured[0], "first") || !strings.Contains(captured[1], "second") {
		t.Fatalf("captured inputs = %q, want first then second", captured)
	}
	if strings.Contains(captured[1], "turn1") {
		t.Fatalf("second input replayed the prior assistant response: %q", captured[1])
	}
}

func TestPersistentCLIUpstreamRoundTripsHarnessTool(t *testing.T) {
	script := writeClientToolFakeCLI(t)
	srv, _ := persistentCLITestServer(t, script)
	headers := map[string]string{"X-OMP-Session": "tool-session"}
	tools := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "lookup",
			"description": "Look something up",
			"parameters":  map[string]any{"type": "object"},
		},
	}}

	res, body := cliPostWithHeaders(t, srv, "/api/v1/chat/completions", map[string]any{
		"model":    "test-alias",
		"tools":    tools,
		"messages": []any{map[string]any{"role": "user", "content": "find x"}},
	}, headers)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"finish_reason":"tool_calls"`) || !strings.Contains(body, `"name":"lookup"`) {
		t.Fatalf("tool request response = %d %s", res.StatusCode, body)
	}

	res, body = cliPostWithHeaders(t, srv, "/api/v1/chat/completions", map[string]any{
		"model": "test-alias",
		"messages": []any{
			map[string]any{"role": "user", "content": "find x"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"x"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "lookup result"},
		},
	}, headers)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"content":"done"`) {
		t.Fatalf("tool result response = %d %s", res.StatusCode, body)
	}
}

// TestCLIUpstreamServesCompletion: a cli upstream is reached by running a
// command, and its text is wrapped in whichever wire the client speaks.
func TestCLIUpstreamServesCompletion(t *testing.T) {
	promptFile := filepath.Join(t.TempDir(), "prompt.txt")
	script := writeFakeCLI(t, promptFile)
	srv, logs := cliTestServer(t, APIOpenAICompletions, script)

	res, body := cliPost(t, srv, "/api/v1/chat/completions", map[string]any{
		"model":    "test-alias",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", res.StatusCode, body)
	}

	// The conversation must have reached the command.
	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("the command was not run: %v", err)
	}
	if !strings.Contains(string(prompt), "hi") {
		t.Errorf("prompt = %q, want it to carry the user's message", prompt)
	}

	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v; body: %s", err, body)
	}
	if out.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", out.Object)
	}
	if out.Model != "test-alias" {
		t.Errorf("model = %q, want the alias", out.Model)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hello world" {
		t.Errorf("content = %+v, want %q", out.Choices, "Hello world")
	}

	if got := res.Header.Get("X-Router-Upstream"); got != "cli" {
		t.Errorf("X-Router-Upstream = %q, want cli", got)
	}

	// The command reports what it spent, and that beats the router's estimate.
	line := logs.String()
	if !strings.Contains(line, "cost_source=reported") {
		t.Errorf("cost was not taken from the command:\n%s", line)
	}
	if !strings.Contains(line, "cost_usd=0.0123") {
		t.Errorf("reported cost missing from the log:\n%s", line)
	}
	if !strings.Contains(line, "cache_read_tokens=900") {
		t.Errorf("usage from the result line missing:\n%s", line)
	}
}

// TestCLIUpstreamStreams: the deltas must reach the client as they arrive, in
// the client's wire.
func TestCLIUpstreamStreams(t *testing.T) {
	promptFile := filepath.Join(t.TempDir(), "prompt.txt")
	script := writeFakeCLI(t, promptFile)
	srv, _ := cliTestServer(t, APIOpenAICompletions, script)

	res, body := cliPost(t, srv, "/api/v1/chat/completions", map[string]any{
		"model":    "test-alias",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want an event stream", ct)
	}
	for _, want := range []string{`"content":"Hello"`, `"content":" world"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream is missing %s:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Errorf("stream is not OpenAI-shaped:\n%s", body)
	}
}

// TestCLIUpstreamServesTheAnthropicWire: the same command backs an
// Anthropic-wire alias, so an Anthropic client can be served by a subscription.
func TestCLIUpstreamServesTheAnthropicWire(t *testing.T) {
	promptFile := filepath.Join(t.TempDir(), "prompt.txt")
	script := writeFakeCLI(t, promptFile)
	srv, _ := cliTestServer(t, APIAnthropicMessages, script)

	res, body := cliPost(t, srv, "/v1/messages", map[string]any{
		"model":      "test-alias",
		"max_tokens": 128,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", res.StatusCode, body)
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
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v; body: %s", err, body)
	}
	if out.Type != "message" || out.Role != "assistant" {
		t.Errorf("envelope = %+v, want an assistant message", out)
	}
	if out.Model != "test-alias" {
		t.Errorf("model = %q, want the alias", out.Model)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "Hello world" {
		t.Errorf("content = %+v, want %q", out.Content, "Hello world")
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", out.StopReason)
	}
}

// TestCLIUpstreamFailureIsRetryable: a command that fails produces an error
// before anything is written, so the router can still fail over.
func TestCLIUpstreamFailureIsRetryable(t *testing.T) {
	script := filepath.Join(t.TempDir(), "failing")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'not logged in' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	srv, logs := cliTestServer(t, APIOpenAICompletions, script)

	res, body := cliPost(t, srv, "/api/v1/chat/completions", map[string]any{
		"model":    "test-alias",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if res.StatusCode < 500 {
		t.Errorf("status = %d, want a server error; body: %s", res.StatusCode, body)
	}
	// The command's stderr is the only clue an operator gets, so it must survive.
	if !strings.Contains(logs.String(), "not logged in") {
		t.Errorf("the command's stderr was not surfaced:\n%s", logs)
	}
}

// TestCLIConfigRejectsAnAPIKey: a cli upstream owns its credential, so handing
// it one is a contradiction rather than a convenience.
func TestCLIConfigRejectsAnAPIKey(t *testing.T) {
	_, err := loadYAML(t, `upstreams:
  c:
    kind: cli
    command: [claude, -p, "{prompt}"]
    apiKey: sk-should-not-be-here
models:
  alias-one:
    api: openai-completions
    targets:
      - {upstream: c, model: m, weight: 1}
`)
	if err == nil {
		t.Fatal("a cli upstream with an apiKey was accepted")
	}
	if !strings.Contains(err.Error(), "manages its own credentials") {
		t.Errorf("error does not explain why: %v", err)
	}
}

// TestCLIConfigRequiresOnePromptPlaceholder: the substitution has to be
// unambiguous, or the prompt lands in the wrong argument.
func TestCLIConfigRequiresOnePromptPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		ok      bool
	}{
		{"one placeholder", `[claude, -p, "{prompt}"]`, true},
		{"no placeholder", `[claude, -p]`, false},
		{"two placeholders", `[claude, -p, "{prompt}", "{prompt}"]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, `upstreams:
  c:
    kind: cli
    command: `+tc.command+`
models:
  alias-one:
    api: openai-completions
    targets:
      - {upstream: c, model: m, weight: 1}
`)
			if tc.ok && err != nil {
				t.Fatalf("a valid cli command was rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("an invalid cli command was accepted")
			}
		})
	}
}

func TestPersistentCLIConfigUsesModelPlaceholder(t *testing.T) {
	_, err := loadYAML(t, `upstreams:
  c:
    kind: cli
    mode: persistent
    command: [claude, -p, --input-format, stream-json, --model, "{model}"]
models:
  alias-one:
    api: openai-completions
    targets:
      - {upstream: c, model: claude-sonnet, weight: 1}
`)
	if err != nil {
		t.Fatalf("valid persistent CLI config rejected: %v", err)
	}

	_, err = loadYAML(t, `upstreams:
  c:
    kind: cli
    mode: persistent
    command: [claude, -p, "{prompt}"]
models:
  alias-one:
    api: openai-completions
    targets:
      - {upstream: c, model: claude-sonnet, weight: 1}
`)
	if err == nil || !strings.Contains(err.Error(), "must not contain") {
		t.Fatalf("persistent prompt placeholder error = %v", err)
	}
}
