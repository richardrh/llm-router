package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const translationOpenAIConversation = `{"model":"translated","max_tokens":50,"stop":["END"],"messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"Find alpha"},{"role":"assistant","content":"Checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"alpha\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"found"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Find a term","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}} ]}`
const translationAnthropicConversation = `{"model":"translated","max_tokens":50,"stop_sequences":["END"],"system":"Be concise","messages":[{"role":"user","content":"Find alpha"},{"role":"assistant","content":[{"type":"text","text":"Checking"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"alpha"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"found"}]}],"tools":[{"name":"lookup","description":"Find a term","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`
const translationAnthropicReply = `{"id":"msg_fixture","type":"message","role":"assistant","model":"native-model","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"call_2","name":"lookup","input":{"q":"beta"}}],"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":7}}`
const translationOpenAIReply = `{"id":"chat_fixture","object":"chat.completion","model":"native-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"beta\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

func translationConfig(url string, reverse, shaping bool) string {
	client, target := "openai-completions", "anthropic-messages"
	if reverse {
		client, target = target, client
	}
	upstreamShape, targetShape := "", ""
	if shaping {
		// stop/stop_sequences only exists in the destination vocabulary after mapping.
		drop := "stop_sequences"
		if reverse {
			drop = "stop"
		}
		upstreamShape = fmt.Sprintf("    bodyDrop: [%s]\n    bodyPatch: {max_tokens: 100, metadata: {user_id: upstream, upstream_only: true}, remove_at_target: true}\n", drop)
		targetShape = "        bodyDrop: [remove_at_target]\n        bodyPatch: {max_tokens: 200, metadata: {user_id: target}}\n"
	}
	return fmt.Sprintf(`apiKey: %s
upstreams:
  fixture:
    baseUrl: %s/v1
%smodels:
  translated:
    api: %s
    targets:
      - upstream: fixture
        model: native-model
        weight: 1
        api: %s
%s`, gatewayKey, url, upstreamShape, client, target, targetShape)
}

func translationCheckRequest(t *testing.T, r *http.Request, reverse, shaping, stream bool) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		return
	}
	var got map[string]any
	if err = json.Unmarshal(body, &got); err != nil {
		t.Error(err)
		return
	}
	if reverse {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("destination path %s", r.URL.Path)
		}
		apiSubset(t, got, `{"model":"native-model","messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"Find alpha"},{"role":"assistant","content":"Checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"alpha\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"found"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Find a term","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`)
	} else {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("destination path %s", r.URL.Path)
		}
		apiSubset(t, got, `{"model":"native-model","system":[{"type":"text","text":"Be concise"}],"messages":[{"role":"user","content":[{"type":"text","text":"Find alpha"}]},{"role":"assistant","content":[{"type":"text","text":"Checking"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"alpha"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"found"}]}],"tools":[{"name":"lookup","description":"Find a term","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)
	}
	if stream {
		apiSubset(t, got, `{"stream":true}`)
	}
	if shaping {
		apiSubset(t, got, `{"max_tokens":200,"metadata":{"user_id":"target"}}`)
		metadata, _ := got["metadata"].(map[string]any)
		if len(metadata) != 1 {
			t.Errorf("patch must replace metadata shallowly: %v", metadata)
		}
		for _, key := range []string{"stop", "stop_sequences", "remove_at_target"} {
			if _, ok := got[key]; ok {
				t.Errorf("post-translation drop failed for %s: %v", key, got)
			}
		}
	} else {
		apiSubset(t, got, `{"max_tokens":50}`)
	}
}

func TestTranslationH5NonstreamAndShaping(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, shaping := range []bool{false, true} {
			t.Run(fmt.Sprintf("reverse=%t/shaping=%t", reverse, shaping), func(t *testing.T) {
				seen := make(chan struct{}, 1)
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					translationCheckRequest(t, r, reverse, shaping, false)
					seen <- struct{}{}
					w.Header().Set("Content-Type", "application/json")
					if reverse {
						io.WriteString(w, translationOpenAIReply)
					} else {
						io.WriteString(w, translationAnthropicReply)
					}
				}))
				defer provider.Close()
				router := startRouter(t, translationConfig(provider.URL, reverse, shaping))
				path, body := "/v1/chat/completions", translationOpenAIConversation
				if reverse {
					path, body = "/v1/messages", translationAnthropicConversation
				}
				res := request(t, "POST", router.URL+path, body, map[string]string{"Authorization": "Bearer " + gatewayKey})
				if res.Status != 200 {
					t.Fatalf("translation status %d: %s", res.Status, res.Body)
				}
				awaitSignal(t, seen, "translated upstream request")
				got := decodeJSON(t, res.Body)
				if reverse {
					apiSubset(t, got, `{"model":"translated","role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"call_2","name":"lookup","input":{"q":"beta"}}],"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":7}}`)
				} else {
					apiSubset(t, got, `{"model":"translated","choices":[{"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"beta\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
				}
			})
		}
	}
}

const translationAnthropicFirst = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream\",\"model\":\"native-model\",\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"
const translationAnthropicRest = `event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_2","name":"lookup","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"beta\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`
const translationOpenAIFirst = `data: {"id":"chat_stream","model":"native-model","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}],"usage":{"prompt_tokens":11,"completion_tokens":0}}

`
const translationOpenAIRest = `data: {"id":"chat_stream","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}

data: {"id":"chat_stream","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"beta\"}"}}]},"finish_reason":null}]}

data: {"id":"chat_stream","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}

data: [DONE]

`

func TestTranslationH5IncrementalStreams(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			gate := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(gate) }) }
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				translationCheckRequest(t, r, reverse, true, true)
				w.Header().Set("Content-Type", "text/event-stream")
				first, rest := translationAnthropicFirst, translationAnthropicRest
				if reverse {
					first, rest = translationOpenAIFirst, translationOpenAIRest
				}
				io.WriteString(w, first)
				w.(http.Flusher).Flush()
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
				io.WriteString(w, rest)
				w.(http.Flusher).Flush()
			}))
			defer func() { release(); provider.Close() }()
			router := startRouter(t, translationConfig(provider.URL, reverse, true))
			path, body := "/v1/chat/completions", translationOpenAIConversation
			if reverse {
				path, body = "/v1/messages", translationAnthropicConversation
			}
			body = strings.TrimSuffix(body, "}") + `,"stream":true}`
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "POST", router.URL+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+gatewayKey)
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				b, _ := io.ReadAll(res.Body)
				t.Fatalf("stream status %d: %s", res.StatusCode, b)
			}
			scanner := bufio.NewScanner(res.Body)
			scanner.Buffer(make([]byte, 4096), 1024*1024)
			text, args, toolID, toolName, finish := "", "", "", "", ""
			input, output := float64(-1), float64(-1)
			terminal := false
			consume := func(line string) {
				if !strings.HasPrefix(line, "data:") {
					return
				}
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "[DONE]" {
					terminal = true
					return
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					t.Errorf("invalid translated SSE JSON: %s", data)
					return
				}
				if reverse {
					switch event["type"] {
					case "message_start":
						m, _ := event["message"].(map[string]any)
						apiSubset(t, m, `{"model":"translated","role":"assistant"}`)
						u, _ := m["usage"].(map[string]any)
						input, _ = u["input_tokens"].(float64)
					case "content_block_start":
						b, _ := event["content_block"].(map[string]any)
						if b["type"] == "tool_use" {
							toolID, _ = b["id"].(string)
							toolName, _ = b["name"].(string)
						}
					case "content_block_delta":
						d, _ := event["delta"].(map[string]any)
						if s, ok := d["text"].(string); ok {
							text += s
						}
						if s, ok := d["partial_json"].(string); ok {
							args += s
						}
					case "message_delta":
						d, _ := event["delta"].(map[string]any)
						finish, _ = d["stop_reason"].(string)
						u, _ := event["usage"].(map[string]any)
						output, _ = u["output_tokens"].(float64)
					case "message_stop":
						terminal = true
					}
				} else {
					apiSubset(t, event, `{"model":"translated","object":"chat.completion.chunk"}`)
					if u, ok := event["usage"].(map[string]any); ok {
						input, _ = u["prompt_tokens"].(float64)
						output, _ = u["completion_tokens"].(float64)
					}
					choices, _ := event["choices"].([]any)
					for _, raw := range choices {
						c, _ := raw.(map[string]any)
						if s, ok := c["finish_reason"].(string); ok {
							finish = s
						}
						d, _ := c["delta"].(map[string]any)
						if s, ok := d["content"].(string); ok {
							text += s
						}
						calls, _ := d["tool_calls"].([]any)
						for _, rawCall := range calls {
							call, _ := rawCall.(map[string]any)
							if s, ok := call["id"].(string); ok {
								toolID = s
							}
							f, _ := call["function"].(map[string]any)
							if s, ok := f["name"].(string); ok {
								toolName = s
							}
							if s, ok := f["arguments"].(string); ok {
								args += s
							}
						}
					}
				}
			}
			// The provider cannot send tool/finish events until text crosses real TCP.
			for text == "" && scanner.Scan() {
				consume(scanner.Text())
			}
			if text != "hello" {
				t.Fatalf("first translated text not incremental: %q (%v)", text, scanner.Err())
			}
			if terminal {
				t.Error("stream completed before provider release")
			}
			release()
			for scanner.Scan() {
				consume(scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			expectedFinish := "tool_calls"
			if reverse {
				expectedFinish = "tool_use"
			}
			if text != "hello" || toolID != "call_2" || toolName != "lookup" || finish != expectedFinish || input != 11 || output != 7 || !terminal {
				t.Errorf("translated stream text=%q tool=%q/%q finish=%q usage=%v/%v terminal=%t", text, toolID, toolName, finish, input, output, terminal)
			}
			apiSubset(t, decodeJSON(t, []byte(args)), `{"q":"beta"}`)
		})
	}
}
