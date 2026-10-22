package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// The subprocess speaks the version-specific app-server JSONL protocol, not HTTP.
func TestCodexProtocolHelper(t *testing.T) {
	if os.Getenv("ROUTER_CODEX_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	send := func(v any) { b, _ := json.Marshal(v); fmt.Println(string(b)) }
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
			Result json.RawMessage
		}
		_ = json.Unmarshal(scanner.Bytes(), &req)
		result := any(map[string]any{})
		switch req.Method {
		case "initialize":
		case "initialized":
			continue
		case "account/read":
			result = map[string]any{"account": map[string]any{"type": "chatgpt", "email": nil, "planType": "plus"}, "requiresOpenaiAuth": true}
		case "model/list":
			result = map[string]any{"data": []any{map[string]any{"id": "model-id", "model": "model-id", "displayName": "Observed Model", "isDefault": true, "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high", "description": "High"}}, "inputModalities": []string{"text"}}}, "nextCursor": nil}
		case "thread/start":
			var p map[string]any
			_ = json.Unmarshal(req.Params, &p)
			env, ok := p["environments"].([]any)
			if !ok || len(env) != 0 || p["sandbox"] != "read-only" || p["approvalPolicy"] != "on-request" {
				send(map[string]any{"id": req.ID, "error": map[string]any{"code": -1, "message": "unsafe thread"}})
				continue
			}
			result = map[string]any{"thread": map[string]any{"id": "thread-test"}}
		case "turn/start":
			send(map[string]any{"id": req.ID, "result": map[string]any{"turn": map[string]any{"id": "turn-test", "status": "inProgress"}}})
			send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread-test", "turnId": "turn-test", "itemId": "message-test", "delta": "Checking"}})
			send(map[string]any{"id": 999, "method": "item/tool/call", "params": map[string]any{"threadId": "thread-test", "turnId": "turn-test", "callId": "call-test", "tool": "lookup", "arguments": map[string]any{"key": "value"}}})
			continue
		case "":
			if string(req.ID) != "999" {
				os.Exit(3)
			}
			var r struct {
				ContentItems []struct{ Type, Text string }
				Success      bool
			}
			_ = json.Unmarshal(req.Result, &r)
			if !r.Success || len(r.ContentItems) != 1 || r.ContentItems[0].Type != "inputText" || r.ContentItems[0].Text != "found" {
				os.Exit(4)
			}
			send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"delta": "Done"}})
			send(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"tokenUsage": map[string]any{"last": map[string]any{"inputTokens": 12, "cachedInputTokens": 2, "outputTokens": 3}}}})
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-test", "turn": map[string]any{"id": "turn-test", "status": "completed", "error": nil}}})
			continue
		default:
			send(map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": "unsupported"}})
			continue
		}
		send(map[string]any{"id": req.ID, "result": result})
	}
	os.Exit(0)
}
func helperManager(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("ROUTER_CODEX_HELPER", "1")
	m := New([]string{os.Args[0], "-test.run=^TestCodexProtocolHelper$", "--"}, "", 2, time.Minute)
	t.Cleanup(m.Close)
	return m
}
func TestSubscriptionDynamicToolContinuation(t *testing.T) {
	m := helperManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fields := map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":"Find it"}]`), "tools": json.RawMessage(`[{"type":"function","function":{"name":"lookup","description":"Lookup","parameters":{"type":"object"}}}]`)}
	var text string
	r, e := m.Run(ctx, "conversation", "model-id", fields, func(delta string) error { text += delta; return nil })
	if e != nil {
		t.Fatal(e)
	}
	if text != "Checking" || len(r.ToolCalls) != 1 || r.ToolCalls[0].ID != "call-test" || string(r.ToolCalls[0].Arguments) != `{"key":"value"}` {
		t.Fatalf("unexpected result: %+v text=%q", r, text)
	}
	fields["messages"] = json.RawMessage(`[{"role":"user","content":"Find it"},{"role":"assistant","content":"Checking","tool_calls":[{"id":"call-test"}]},{"role":"tool","tool_call_id":"call-test","content":"found"}]`)
	r, e = m.Run(ctx, "conversation", "model-id", fields, nil)
	if e != nil {
		t.Fatal(e)
	}
	if r.Text != "Done" || !r.HasUsage || r.Usage.InputTokens != 10 || r.Usage.CacheReadTokens != 2 || r.Usage.OutputTokens != 3 {
		t.Fatalf("unexpected continuation %+v", r)
	}
}
func TestManagedAccountAndObservedModels(t *testing.T) {
	m := helperManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := m.Refresh(ctx)
	a := m.Snapshot()
	if e != nil || !a.SignedIn || a.Type != "chatgpt" || a.Plan != "plus" {
		t.Fatalf("account %+v %v", a, e)
	}
	models, e := m.Models(ctx)
	if e != nil || len(models) != 1 || models[0].ID != "model-id" || !models[0].Default || len(models[0].ReasoningEfforts) != 1 {
		t.Fatalf("models %+v %v", models, e)
	}
	a = m.Snapshot()
	a.Models[0].ID = "mutated"
	a.Models[0].ReasoningEfforts[0] = "mutated"
	cached := m.Snapshot()
	if cached.Models[0].ID != "model-id" || cached.Models[0].ReasoningEfforts[0] == "mutated" {
		t.Fatal("snapshot aliases model catalog")
	}
}
func TestUnmatchedToolResultFails(t *testing.T) {
	m := helperManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, e := m.Run(ctx, "bad", "model-id", map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"tool","tool_call_id":"missing","content":"no"}]`)}, nil)
	if e == nil {
		t.Fatal("unmatched tool result accepted")
	}
}
