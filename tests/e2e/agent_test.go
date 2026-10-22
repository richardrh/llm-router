package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func agentConfig(t *testing.T, kind, mode, extra string) (string, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	command, _ := json.Marshal([]string{exe, "-test.run=^TestAgentProtocolFixture$", "--", "--agent-fixture", kind, "--agent-mode", mode})
	cli := ""
	if kind == "claude" {
		kind = "cli"
		toolMode := "claude"
		if mode == "tools" {
			toolMode = "client"
		}
		cli = "    mode: persistent\n    toolMode: " + toolMode + "\n"
	}
	return fmt.Sprintf("apiKey: %q\nstore:\n  path: %q\ndefaults:\n  maxAttempts: 1\n  firstByteTimeout: 5s\n  streamIdleTimeout: 5s\n  maxStreamDuration: 10s\nupstreams:\n  agent:\n    kind: %s\n%s    command: %s\n    maxConcurrency: 1\n    maxSessions: 2\n    timeout: 8s\n%smodels:\n  chat:\n    api: openai-completions\n    cost: {input: 100, output: 100, cacheRead: 100}\n    targets:\n      - {upstream: agent, model: fixture, weight: 1}\n", gatewayKey, filepath.Join(dir, "usage.db"), kind, cli, command, extra), dir
}
func agentStart(t *testing.T, kind, mode, extra string) (*routerProcess, string) {
	t.Helper()
	cfg, dir := agentConfig(t, kind, mode, extra)
	return startRouter(t, cfg, routerOptions{Env: map[string]string{"ROUTER_E2E_HELPER": "1", "AGENT_FIXTURE_DIR": dir}}), dir
}
func agentPost(t *testing.T, p *routerProcess, session, body string) httpResult {
	t.Helper()
	return request(t, "POST", p.URL+"/v1/chat/completions", body, map[string]string{"Authorization": "Bearer " + gatewayKey, "X-OMP-Session": session})
}
func agentMessages(text string) string {
	b, _ := json.Marshal(text)
	return `{"model":"chat","messages":[{"role":"user","content":` + string(b) + `}]}`
}
func agentCaptures(dir string) string {
	paths, _ := filepath.Glob(filepath.Join(dir, "child-*.jsonl"))
	var b strings.Builder
	for _, p := range paths {
		raw, _ := os.ReadFile(p)
		b.Write(raw)
	}
	return b.String()
}
func agentRequireOK(t *testing.T, r httpResult, text string) {
	t.Helper()
	if r.Status != 200 || !strings.Contains(string(r.Body), text) {
		t.Fatalf("response = %d %s; want success containing %q", r.Status, r.Body, text)
	}
}

func TestAgentClaudeConversationIdentityAndLatestTurn(t *testing.T) {
	p, dir := agentStart(t, "claude", "ok", "")
	agentRequireOK(t, agentPost(t, p, "alpha", `{"model":"chat","messages":[{"role":"system","content":"INITIAL_SYSTEM"},{"role":"user","content":"FIRST_USER"}]}`), "turn1")
	agentRequireOK(t, agentPost(t, p, "alpha", `{"model":"chat","messages":[{"role":"system","content":"INITIAL_SYSTEM"},{"role":"user","content":"FIRST_USER"},{"role":"assistant","content":"PRIOR_ASSISTANT"},{"role":"user","content":"LATEST_USER"}]}`), "turn2")
	agentRequireOK(t, agentPost(t, p, "beta", agentMessages("OTHER_IDENTITY")), "turn1")
	captured := agentCaptures(dir)
	var latest string
	for _, line := range strings.Split(captured, "\n") {
		if strings.Contains(line, "LATEST_USER") {
			latest = line
		}
	}
	if !strings.Contains(captured, "INITIAL_SYSTEM") || !strings.Contains(captured, "FIRST_USER") || latest == "" || strings.Contains(latest, "PRIOR_ASSISTANT") || strings.Contains(latest, "FIRST_USER") {
		t.Fatalf("initial/latest conversation contract: %s", captured)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "child-*.jsonl"))
	if len(paths) != 2 {
		t.Fatalf("explicit identities share child: %v", paths)
	}
}

const agentTools = `"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]`

func TestAgentClientToolRoundTrip(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			p, dir := agentStart(t, kind, "tools", "")
			first := `{"model":"chat","messages":[{"role":"user","content":"find x"}],` + agentTools + `}`
			r := agentPost(t, p, "tools", first)
			agentRequireOK(t, r, "call-1")
			if !strings.Contains(string(r.Body), "lookup") {
				t.Fatalf("missing tool name: %s", r.Body)
			}
			next := `{"model":"chat","messages":[{"role":"user","content":"find x"},{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"CLIENT_RESULT"}],` + agentTools + `}`
			agentRequireOK(t, agentPost(t, p, "tools", next), "continued")
			captured := agentCaptures(dir)
			if !strings.Contains(captured, "CLIENT_RESULT") {
				t.Fatalf("client result not delivered: %s", captured)
			}
			if kind == "codex" {
				if strings.Count(captured, `"method":"turn/start"`) != 1 || !strings.Contains(captured, `"contentItems"`) || !strings.Contains(captured, `"success":true`) {
					t.Fatalf("native same-turn continuation violated: %s", captured)
				}
				if !strings.Contains(captured, `"environments":[]`) {
					t.Fatalf("host environments not disabled: %s", captured)
				}
			}
		})
	}
}

func TestAgentCodexRejectsUnsupportedBeforeExecution(t *testing.T) {
	for _, field := range []string{
		`"temperature":0.2`, `"top_p":0.8`, `"max_tokens":12`, `"max_completion_tokens":12`,
		`"stop":["END"]`, `"seed":1`, `"frequency_penalty":0`, `"presence_penalty":0`,
		`"response_format":{"type":"json_object"}`, `"parallel_tool_calls":true`,
		`"logprobs":false`, `"top_logprobs":1`, `"n":1`, `"logit_bias":{}`,
		`"prediction":{"type":"content","content":"x"}`, `"modalities":["text"]`,
		`"audio":{"voice":"alloy","format":"wav"}`, `"service_tier":"auto"`,
		`"tool_choice":"required"`, `"unknown_control":true`,
	} {
		t.Run(field, func(t *testing.T) {
			p, dir := agentStart(t, "codex", "ok", "")
			r := agentPost(t, p, "unsupported", `{"model":"chat","messages":[{"role":"user","content":"hello"}],`+field+`}`)
			if r.Status != 400 || !strings.Contains(string(r.Body), "unsupported_request") {
				t.Fatalf("unsupported control = %d %s", r.Status, r.Body)
			}
			if strings.Contains(agentCaptures(dir), `"method":"turn/start"`) {
				t.Fatal("rejected control executed turn")
			}
		})
	}
}
func TestAgentCodexUnmatchedToolResult(t *testing.T) {
	p, dir := agentStart(t, "codex", "ok", "")
	r := agentPost(t, p, "unmatched", `{"model":"chat","messages":[{"role":"user","content":"hello"},{"role":"tool","tool_call_id":"missing","content":"must not execute"}]}`)
	if r.Status != 400 {
		t.Fatalf("unmatched tool result = %d %s", r.Status, r.Body)
	}
	if strings.Contains(agentCaptures(dir), `"method":"turn/start"`) {
		t.Fatal("unmatched result executed turn")
	}
}
func TestAgentCodexCommitBoundary(t *testing.T) {
	for _, mode := range []string{"early-error", "late-error"} {
		t.Run(mode, func(t *testing.T) {
			var backupCalls atomic.Int32
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backupCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"backup\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
			}))
			defer backup.Close()
			cfg, dir := agentConfig(t, "codex", mode, "")
			cfg = strings.Replace(cfg, "maxAttempts: 1", "maxAttempts: 2", 1)
			cfg = strings.Replace(cfg, "models:\n", fmt.Sprintf("  backup:\n    baseUrl: %s\nmodels:\n", backup.URL), 1)
			cfg += "      - {upstream: backup, model: backup, weight: 0}\n"
			p := startRouter(t, cfg, routerOptions{Env: map[string]string{"ROUTER_E2E_HELPER": "1", "AGENT_FIXTURE_DIR": dir}})
			r := agentPost(t, p, "commit", `{"model":"chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
			if r.Status != 200 {
				t.Fatalf("stream status = %d %s", r.Status, r.Body)
			}
			if mode == "early-error" {
				if backupCalls.Load() != 1 || !strings.Contains(string(r.Body), "backup") {
					t.Fatalf("no early failover: calls=%d %s", backupCalls.Load(), r.Body)
				}
			} else if backupCalls.Load() != 0 || !strings.Contains(string(r.Body), "hello") || !strings.Contains(string(r.Body), "upstream_error") || !strings.Contains(string(r.Body), "[DONE]") {
				t.Fatalf("late error replay/termination: calls=%d %s", backupCalls.Load(), r.Body)
			}
		})
	}
}
func TestAgentCodexDeniesHostApprovals(t *testing.T) {
	for _, mode := range []string{"approval", "host-tool"} {
		t.Run(mode, func(t *testing.T) {
			p, dir := agentStart(t, "codex", mode, "")
			r := agentPost(t, p, "denied", agentMessages("hello"))
			if r.Status < 400 {
				t.Fatalf("host action accepted: %d %s", r.Status, r.Body)
			}
			eventually(t, "approval receives JSON-RPC error", func() bool {
				c := agentCaptures(dir)
				return strings.Contains(c, `"error"`) && strings.Contains(c, `"id":99`)
			})
			if strings.Contains(agentCaptures(dir), `"approved"`) {
				t.Fatal("approval was granted")
			}
		})
	}
}
func TestAgentSessionRetirement(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			p, dir := agentStart(t, kind, "ok", "    sessionIdleTimeout: 100ms\n")
			r := agentPost(t, p, "retired", agentMessages("first"))
			if r.Status != 200 {
				t.Fatalf("first: %d %s", r.Status, r.Body)
			}
			// Wait beyond the idle lease before the next request triggers expiry.
			time.Sleep(300 * time.Millisecond)
			r = agentPost(t, p, "retired", `{"model":"chat","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"hello"},{"role":"user","content":"second"}]}`)
			if kind == "claude" {
				agentRequireOK(t, r, "turn1")
				if !strings.Contains(agentCaptures(dir), "--resume") || !strings.Contains(agentCaptures(dir), "saved-") {
					t.Fatalf("saved session not resumed: %s", agentCaptures(dir))
				}
			} else {
				if r.Status < 400 {
					t.Fatalf("lost established Codex session silently restarted: %d %s", r.Status, r.Body)
				}
				if strings.Count(agentCaptures(dir), `"method":"thread/start"`) != 1 {
					t.Fatal("lost thread was replayed")
				}
			}
		})
	}
}
func TestAgentUsageReportedAndSubscription(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			p, _ := agentStart(t, kind, "ok", "")
			r := agentPost(t, p, "usage", agentMessages("hello"))
			if r.Status != 200 {
				t.Fatalf("inference: %d %s", r.Status, r.Body)
			}
			var row map[string]any
			eventually(t, "agent usage row", func() bool {
				r := request(t, "GET", p.URL+"/usage?limit=10", "", map[string]string{"Authorization": "Bearer " + gatewayKey})
				var data struct {
					Requests []map[string]any `json:"requests"`
				}
				if json.Unmarshal(r.Body, &data) != nil || len(data.Requests) != 1 {
					return false
				}
				row = data.Requests[0]
				return true
			})
			if row["input_tokens"] != float64(10) || row["output_tokens"] != float64(2) || row["cache_read_tokens"] != float64(3) {
				t.Fatalf("wrong measured last-turn tokens: %#v", row)
			}
			if kind == "claude" {
				if row["cost_source"] != "reported" || row["cost_usd"] != 0.0123 {
					t.Fatalf("reported CLI dollars lost: %#v", row)
				}
			} else if row["cost_source"] != "unreported" || row["cost_usd"] != float64(0) {
				t.Fatalf("subscription dollars invented from alias rates: %#v", row)
			}
		})
	}
}

func TestAgentQueuedCancellationAndActiveCapacity(t *testing.T) {
	cfg, dir := agentConfig(t, "claude", "hold", "")
	cfg = strings.Replace(cfg, "maxConcurrency: 1", "maxConcurrency: 2", 1)
	p := startRouter(t, cfg, routerOptions{Env: map[string]string{"ROUTER_E2E_HELPER": "1", "AGENT_FIXTURE_DIR": dir}})
	activeDone := make(chan httpResult, 1)
	go func() { activeDone <- agentPost(t, p, "active", agentMessages("HOLD_ACTIVE")) }()
	eventually(t, "active child received turn", func() bool { return strings.Contains(agentCaptures(dir), "HOLD_ACTIVE") })
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "POST", p.URL+"/v1/chat/completions", strings.NewReader(agentMessages("QUEUED_CANCELLED")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayKey)
	req.Header.Set("X-OMP-Session", "active")
	req.Header.Set("Content-Type", "application/json")
	queuedDone := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if res != nil {
			res.Body.Close()
		}
		queuedDone <- err
	}()
	select {
	case err := <-queuedDone:
		t.Fatalf("queued turn completed while active was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-queuedDone:
		if err == nil {
			t.Fatal("cancelled queue returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued cancellation blocked")
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-activeDone:
		agentRequireOK(t, r, "turn1")
	case <-time.After(5 * time.Second):
		t.Fatal("queued cancellation killed or blocked active turn")
	}
	if strings.Contains(agentCaptures(dir), "QUEUED_CANCELLED") {
		t.Fatal("cancelled queued request reached child")
	}
	agentRequireOK(t, agentPost(t, p, "reusable", agentMessages("capacity reused")), "turn1")
}

func TestAgentActiveCancellationAndShutdown(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		t.Run(signal.String(), func(t *testing.T) {
			p, dir := agentStart(t, "claude", "hold", "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "POST", p.URL+"/v1/chat/completions", strings.NewReader(agentMessages("HOLD_ACTIVE")))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+gatewayKey)
			req.Header.Set("X-OMP-Session", "cancelled")
			req.Header.Set("Content-Type", "application/json")
			done := make(chan struct{})
			go func() {
				defer close(done)
				res, _ := http.DefaultClient.Do(req)
				if res != nil {
					io.Copy(io.Discard, res.Body)
					res.Body.Close()
				}
			}()
			eventually(t, "active child input", func() bool { return strings.Contains(agentCaptures(dir), "HOLD_ACTIVE") })
			var cancelledPID int
			for _, line := range strings.Split(agentCaptures(dir), "\n") {
				var v struct {
					PID int `json:"pid"`
				}
				if json.Unmarshal([]byte(line), &v) == nil && v.PID > 0 {
					cancelledPID = v.PID
				}
			}
			if cancelledPID == 0 {
				t.Fatal("active fixture PID missing")
			}
			cancel()
			awaitSignal(t, done, "active HTTP cancellation")
			eventually(t, "active cancellation retires child", func() bool { return syscall.Kill(cancelledPID, 0) != nil })
			agentRequireOK(t, agentPost(t, p, "fresh", agentMessages("capacity after cancel")), "turn1")
			// Start active work again: shutdown must terminate this owned child too.
			req2, err := http.NewRequest("POST", p.URL+"/v1/chat/completions", strings.NewReader(agentMessages("HOLD_ACTIVE")))
			if err != nil {
				t.Fatal(err)
			}
			req2.Header = req.Header.Clone()
			req2.Header.Set("X-OMP-Session", "shutdown")
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				res, _ := http.DefaultClient.Do(req2)
				if res != nil {
					res.Body.Close()
				}
			}()
			eventually(t, "shutdown child active", func() bool { return strings.Count(agentCaptures(dir), "HOLD_ACTIVE") >= 2 })
			var pids []int
			for _, line := range strings.Split(agentCaptures(dir), "\n") {
				var v struct {
					PID int `json:"pid"`
				}
				if json.Unmarshal([]byte(line), &v) == nil && v.PID > 0 {
					pids = append(pids, v.PID)
				}
			}
			p.Stop(t, signal)
			awaitSignal(t, stopped, "shutdown closes active transport")
			eventually(t, "owned agent children reaped", func() bool {
				for _, pid := range pids {
					if syscall.Kill(pid, 0) == nil {
						return false
					}
				}
				return true
			})
		})
	}
}

func TestAgentIncompatibleStreamDoesNotSpendAttempt(t *testing.T) {
	var calls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"compatible backup\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
	}))
	defer backup.Close()
	cfg, dir := agentConfig(t, "claude", "tools", "")
	cfg = strings.Replace(cfg, "models:\n", fmt.Sprintf("  backup:\n    baseUrl: %s\nmodels:\n", backup.URL), 1)
	cfg += "      - {upstream: backup, model: backup, weight: 0}\n"
	p := startRouter(t, cfg, routerOptions{Env: map[string]string{"ROUTER_E2E_HELPER": "1", "AGENT_FIXTURE_DIR": dir}})
	r := agentPost(t, p, "compatible", `{"model":"chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	agentRequireOK(t, r, "compatible backup")
	if calls.Load() != 1 || strings.Contains(agentCaptures(dir), `"type":"user"`) {
		t.Fatalf("incompatible candidate dispatched or consumed sole attempt: calls=%d capture=%s", calls.Load(), agentCaptures(dir))
	}
}

func TestAgentOmittedCLIModeExecutesOneShot(t *testing.T) {
	cfg, dir := agentConfig(t, "claude", "oneshot", "")
	cfg = strings.Replace(cfg, "    mode: persistent\n    toolMode: claude\n", "", 1)
	cfg = strings.Replace(cfg, `"--agent-mode","oneshot"]`, `"--agent-mode","oneshot","-p","{prompt}"]`, 1)
	p := startRouter(t, cfg, routerOptions{Env: map[string]string{"ROUTER_E2E_HELPER": "1", "AGENT_FIXTURE_DIR": dir}})
	agentRequireOK(t, agentPost(t, p, "oneshot", agentMessages("ONE_SHOT_PROMPT")), "oneshot answer")
	if !strings.Contains(agentCaptures(dir), "ONE_SHOT_PROMPT") {
		t.Fatalf("one-shot prompt missing from argv: %s", agentCaptures(dir))
	}
}

func TestAgentBlockedChildInputCancellation(t *testing.T) {
	p, dir := agentStart(t, "claude", "blocked-input", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", p.URL+"/v1/chat/completions", strings.NewReader(agentMessages(strings.Repeat("BLOCKED_INPUT_", 100000))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OMP-Session", "blocked")
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, _ := http.DefaultClient.Do(req)
		if res != nil {
			res.Body.Close()
		}
	}()
	eventually(t, "child intentionally stops reading pipe", func() bool { return strings.Contains(agentCaptures(dir), `"blocked_input":true`) })
	cancel()
	awaitSignal(t, done, "blocked child-input cancellation")
	var pid int
	for _, line := range strings.Split(agentCaptures(dir), "\n") {
		var v struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal([]byte(line), &v) == nil && v.PID > 0 {
			pid = v.PID
		}
	}
	if pid == 0 {
		t.Fatal("fixture PID missing")
	}
	eventually(t, "cancelled blocked writer child terminated", func() bool { return syscall.Kill(pid, 0) != nil })
}
