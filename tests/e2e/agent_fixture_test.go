package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAgentProtocolFixture is an external protocol peer, never an inference or
// tool implementation. Its files record only bytes crossing the child boundary.
func TestAgentProtocolFixture(t *testing.T) {
	if os.Getenv("ROUTER_E2E_HELPER") != "1" {
		return
	}
	kind, mode := "", "ok"
	for i, arg := range os.Args {
		if i+1 < len(os.Args) {
			switch arg {
			case "--agent-fixture":
				kind = os.Args[i+1]
			case "--agent-mode":
				mode = os.Args[i+1]
			}
		}
	}
	if kind == "" {
		return
	}
	dir := os.Getenv("AGENT_FIXTURE_DIR")
	capture, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("child-%d.jsonl", os.Getpid())), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	defer capture.Close()
	record := func(v any) { b, _ := json.Marshal(v); fmt.Fprintln(capture, string(b)); capture.Sync() }
	record(map[string]any{"argv": os.Args, "pid": os.Getpid()})
	send := func(v any) { b, _ := json.Marshal(v); fmt.Println(string(b)) }
	if kind == "claude" {
		send(map[string]any{"type": "system", "subtype": "init", "session_id": fmt.Sprintf("saved-%d", os.Getpid())})
	}
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), 8<<20)
	if kind == "claude" && mode == "oneshot" {
		send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "oneshot answer", "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}})
		os.Exit(0)
	}
	if mode == "blocked-input" {
		record(map[string]any{"blocked_input": true})
		for {
			time.Sleep(time.Hour)
		}
	}
	turn := 0
	for scan.Scan() {
		var req map[string]any
		if json.Unmarshal(scan.Bytes(), &req) != nil {
			os.Exit(3)
		}
		record(req)
		if kind == "claude" {
			turn++
			if mode == "hold" && strings.Contains(string(scan.Bytes()), "HOLD_ACTIVE") {
				for {
					if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			result := fmt.Sprintf("turn%d", turn)
			if mode == "tools" {
				if turn == 1 {
					result = `{"kind":"client_tool_request","id":"call-1","name":"lookup","arguments":{"q":"x"}}`
				} else {
					result = `{"kind":"final_response","text":"continued"}`
				}
			} else {
				send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": result}}})
			}
			send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": result, "total_cost_usd": 0.0123, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "cache_read_input_tokens": 3}})
			continue
		}
		id, hasID := req["id"]
		if !hasID {
			continue
		}
		method, _ := req["method"].(string)
		if method == "" { // The only server RPC is the pending dynamic tool or denied approval.
			if mode == "approval" || mode == "host-tool" {
				continue
			}
			send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thr", "turnId": "turn", "itemId": "final", "delta": "continued"}})
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thr", "turn": map[string]any{"id": "turn", "status": "completed"}}})
			continue
		}
		var result any = map[string]any{}
		switch method {
		case "account/read":
			result = map[string]any{"account": map[string]any{"type": "chatgpt", "planType": "plus"}, "requiresOpenaiAuth": true}
		case "model/list":
			result = map[string]any{"data": []any{map[string]any{"id": "fixture", "model": "fixture", "displayName": "Fixture", "isDefault": true}}, "nextCursor": nil}
		case "thread/start":
			result = map[string]any{"thread": map[string]any{"id": "thr"}}
		case "turn/start":
			turn++
			if mode == "early-error" {
				send(map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": "fixture failed before output"}})
				continue
			}
			result = map[string]any{"turn": map[string]any{"id": "turn", "status": "inProgress"}}
		}
		send(map[string]any{"id": id, "result": result})
		if method != "turn/start" {
			continue
		}
		if mode == "tools" {
			send(map[string]any{"id": 99, "method": "item/tool/call", "params": map[string]any{"threadId": "thr", "turnId": "turn", "callId": "call-1", "tool": "lookup", "arguments": map[string]any{"q": "x"}}})
			continue
		}
		if mode == "approval" || mode == "host-tool" {
			m := "item/commandExecution/requestApproval"
			if mode == "host-tool" {
				m = "item/fileChange/requestApproval"
			}
			send(map[string]any{"id": 99, "method": m, "params": map[string]any{"threadId": "thr", "turnId": "turn", "command": "touch MUST_NOT_EXECUTE"}})
			continue
		}
		send(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "thr", "tokenUsage": map[string]any{"last": map[string]any{"inputTokens": 13, "outputTokens": 2, "cachedInputTokens": 3}, "total": map[string]any{"inputTokens": 999, "outputTokens": 999}}}})
		send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thr", "turnId": "turn", "itemId": "text", "delta": "hello"}})
		status := "completed"
		var failure any
		if mode == "late-error" {
			status = "failed"
			failure = map[string]any{"message": "fixture failed after output"}
		}
		send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thr", "turn": map[string]any{"id": "turn", "status": status, "error": failure}}})
	}
	os.Exit(0)
}
