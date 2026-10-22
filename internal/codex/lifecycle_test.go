package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexLifecycleHelper(t *testing.T) {
	if os.Getenv("ROUTER_CODEX_LIFECYCLE_HELPER") != "1" {
		return
	}
	send := func(v any) { b, _ := json.Marshal(v); fmt.Println(string(b)) }
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		_ = json.Unmarshal(scanner.Bytes(), &req)
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
		case "initialized":
			continue
		case "account/read":
			var account any = map[string]any{"type": "chatgpt", "email": nil, "planType": "plus"}
			if marker := os.Getenv("ROUTER_CODEX_AUTH_MARKER"); marker != "" {
				if _, e := os.Stat(marker); e != nil {
					account = nil
				}
			}
			result = map[string]any{"account": account, "requiresOpenaiAuth": true}
		case "model/list":
			result = map[string]any{"data": []any{}, "nextCursor": nil}
		case "account/login/start":
			if marker := os.Getenv("ROUTER_CODEX_AUTH_MARKER"); marker != "" {
				_ = os.WriteFile(marker, []byte("signed in"), 0600)
			}
			success := os.Getenv("ROUTER_CODEX_LOGIN_FAILURE") == ""
			var loginError any = nil
			if !success {
				loginError = "Browser flow rejected"
			}
			// Completion before the response deterministically exercises the pending-state race.
			send(map[string]any{"method": "account/login/completed", "params": map[string]any{"loginId": "immediate-login", "success": success, "error": loginError}})
			result = map[string]any{"type": "chatgpt", "loginId": "immediate-login", "authUrl": "https://auth.openai.com/oauth/authorize?state=fake"}
		case "account/login/cancel":
			result = map[string]any{"status": "canceled"}
		case "thread/start":
			var p map[string]any
			_ = json.Unmarshal(req.Params, &p)
			if expected := os.Getenv("ROUTER_CODEX_EXPECT_INSTRUCTIONS"); expected != "" && p["developerInstructions"] != expected {
				send(map[string]any{"id": req.ID, "error": map[string]any{"code": -1, "message": "developer instructions were not preserved"}})
				continue
			}
			result = map[string]any{"thread": map[string]any{"id": "thread-lifecycle"}}
			if os.Getenv("ROUTER_CODEX_STALL_WRITE") == "1" {
				send(map[string]any{"id": req.ID, "result": result})
				time.Sleep(time.Hour)
				os.Exit(0)
			}
		case "turn/start":
			var p map[string]any
			_ = json.Unmarshal(req.Params, &p)
			if expected := os.Getenv("ROUTER_CODEX_EXPECT_EFFORT"); expected != "" && p["effort"] != expected {
				send(map[string]any{"id": req.ID, "error": map[string]any{"code": -1, "message": "effort missing"}})
				continue
			}
			send(map[string]any{"id": req.ID, "result": map[string]any{"turn": map[string]any{"id": "turn-lifecycle", "status": "inProgress"}}})
			send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"delta": "started"}})
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"id": "turn-lifecycle", "status": "completed", "error": nil}}})
			continue
		default:
			send(map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": "unsupported"}})
			continue
		}
		send(map[string]any{"id": req.ID, "result": result})
	}
	os.Exit(0)
}
func lifecycleManager(t *testing.T, max int) *Manager {
	t.Helper()
	t.Setenv("ROUTER_CODEX_LIFECYCLE_HELPER", "1")
	m := New([]string{os.Args[0], "-test.run=^TestCodexLifecycleHelper$", "--"}, "", max, 0)
	t.Cleanup(m.Close)
	return m
}
func lifecycleFields() map[string]json.RawMessage {
	return map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":"hello"}]`)}
}
func TestQueuedCancellationDoesNotRetireActiveConversation(t *testing.T) {
	m := lifecycleManager(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := m.Run(ctx, "one", "model", lifecycleFields(), func(string) error { close(started); <-release; return nil })
		done <- e
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	queued, cancelQueued := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancelQueued()
	_, e := m.Run(queued, "one", "model", lifecycleFields(), nil)
	if e != context.DeadlineExceeded {
		t.Fatalf("queued cancellation: %v", e)
	}
	_, e = m.Run(ctx, "two", "model", lifecycleFields(), nil)
	if e == nil || !strings.Contains(e.Error(), "capacity") {
		t.Fatalf("active session evicted: %v", e)
	}
	close(release)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	live, limit := m.Counts()
	if live != 1 || limit != 1 {
		t.Fatalf("counts %d/%d", live, limit)
	}
	next := map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":"hello"},{"role":"assistant","content":"started"},{"role":"user","content":"again"}]`)}
	if _, e = m.Run(ctx, "one", "model", next, nil); e != nil {
		t.Fatalf("queued cancellation poisoned session: %v", e)
	}
}
func TestSignedOutSessionKeyRecoversAfterExternalLogin(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "authenticated")
	t.Setenv("ROUTER_CODEX_AUTH_MARKER", marker)
	m := lifecycleManager(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, e := m.Run(ctx, "retry-key", "model", lifecycleFields(), nil); e == nil || !strings.Contains(e.Error(), "login") {
		t.Fatalf("expected subscription error: %v", e)
	}
	if _, e := m.Login(ctx); e != nil {
		t.Fatal(e)
	}
	a := m.Snapshot()
	if !a.SignedIn || a.Login != nil {
		t.Fatalf("immediate completion restored pending state: %+v", a)
	}
	if _, e := m.Run(ctx, "retry-key", "model", lifecycleFields(), nil); e != nil {
		t.Fatalf("signed-out attempt poisoned key: %v", e)
	}
}
func TestLoginCompletionErrorSurvivesAdminRestart(t *testing.T) {
	t.Setenv("ROUTER_CODEX_LOGIN_FAILURE", "1")
	m := lifecycleManager(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, e := m.Login(ctx); e != nil {
		t.Fatal(e)
	}
	for i := range 2 {
		e := m.Refresh(ctx)
		a := m.Snapshot()
		if e != nil || a.Login != nil || a.Error != "Browser flow rejected" {
			t.Fatalf("refresh %d lost failure: %+v %v", i, a, e)
		}
	}
	if e := m.CancelLogin(ctx, "immediate-login"); e != nil {
		t.Fatal(e)
	}
	a := m.Snapshot()
	if a.Error != "" {
		t.Fatalf("cancel retained failure: %+v", a)
	}
}
func TestDeveloperInstructionsAndReasoningEffortUseNativeFields(t *testing.T) {
	t.Setenv("ROUTER_CODEX_EXPECT_INSTRUCTIONS", "System instructions\nDeveloper instructions\n")
	t.Setenv("ROUTER_CODEX_EXPECT_EFFORT", "high")
	m := lifecycleManager(t, 1)
	fields := map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"system","content":"System instructions"},{"role":"developer","content":"Developer instructions"},{"role":"user","content":"hello"}]`), "reasoning_effort": json.RawMessage(`"high"`)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, e := m.Run(ctx, "instructions", "model", fields, nil); e != nil {
		t.Fatal(e)
	}
}
func TestUnsupportedExplicitControlsFailBeforeStartingProcess(t *testing.T) {
	m := New([]string{"must-not-run"}, "", 1, 0)
	defer m.Close()
	for _, choice := range []string{`"none"`, `"required"`, `{"type":"function","function":{"name":"lookup"}}`} {
		fields := lifecycleFields()
		fields["tool_choice"] = json.RawMessage(choice)
		_, e := m.Run(context.Background(), "controls", "model", fields, nil)
		if e == nil || !strings.Contains(e.Error(), "tool_choice") {
			t.Fatalf("tool_choice accepted: %s %v", choice, e)
		}
	}
}
func TestCancellationInterruptsBlockedProtocolWrite(t *testing.T) {
	t.Setenv("ROUTER_CODEX_STALL_WRITE", "1")
	m := lifecycleManager(t, 1)
	text, _ := json.Marshal([]map[string]any{{"role": "user", "content": strings.Repeat("x", 4<<20)}})
	fields := map[string]json.RawMessage{"messages": text}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, e := m.Run(ctx, "blocked-write", "model", fields, nil); finished <- e }()
	select {
	case e := <-finished:
		if e == nil {
			t.Fatal("blocked write ignored cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("context cancellation did not interrupt pipe write")
	}
	if live, _ := m.Counts(); live != 0 {
		t.Fatalf("canceled process retained: %d", live)
	}
}

func TestExplicitRefreshObservesExternalLogin(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "external-auth")
	t.Setenv("ROUTER_CODEX_AUTH_MARKER", marker)
	m := lifecycleManager(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot()
	if before.CheckedAt.IsZero() || before.SignedIn {
		t.Fatalf("expected checked signed-out state: %+v", before)
	}
	if err := os.WriteFile(marker, []byte("external login"), 0600); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().SignedIn {
		t.Fatal("read unexpectedly rechecked external credentials")
	}
	if err := m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	after := m.Snapshot()
	if !after.SignedIn || !after.CheckedAt.After(before.CheckedAt) {
		t.Fatalf("external login not refreshed: %+v", after)
	}
}
