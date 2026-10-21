package inference

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-router/internal/config"
	"llm-router/internal/usage"
)

func TestHTTPRunnerFragmentedStreams(t *testing.T) {
	answer := strings.Repeat("x", 70000)
	for _, protocol := range []config.APIProtocol{config.APIOpenAICompletions, config.APIAnthropicMessages} {
		t.Run(string(protocol), func(t *testing.T) {
			handlerFinished := false
			run := HTTPRunner(func(w http.ResponseWriter, r *http.Request, p config.APIProtocol) {
				defer func() { handlerFinished = true }()
				w.Header().Set("X-Router-Upstream", "provider")
				w.Header().Set("X-Router-Upstream-Model", "native-model")
				var event any
				if p == config.APIOpenAICompletions {
					event = map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": answer}}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "prompt_tokens_details": map[string]int{"cached_tokens": 3}}}
				} else {
					_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":3}}}\n\n")
					event = map[string]any{"type": "content_block_delta", "delta": map[string]string{"type": "text_delta", "text": answer}, "usage": map[string]int{"output_tokens": 2}}
				}
				encoded, _ := json.Marshal(event)
				wire := "data: " + string(encoded) + "\r\n\r\ndata: [DONE]\n\n"
				for len(wire) > 0 {
					n := min(len(wire), 137)
					if _, err := io.WriteString(w, wire[:n]); err != nil {
						t.Error(err)
						return
					}
					wire = wire[n:]
					w.(http.Flusher).Flush()
				}
			})
			var streamed strings.Builder
			var route Route
			result, err := run(context.Background(), Request{Model: "alias", Protocol: protocol, Stream: true}, func(event Event) error {
				switch event.Kind {
				case RouteSelected:
					route = event.Route
				case TextDelta:
					if handlerFinished {
						t.Error("text was buffered until completion")
					}
					streamed.WriteString(event.Text)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if streamed.String() != answer || result.Text != answer {
				t.Fatal("fragmented text was truncated or duplicated")
			}
			if route != (Route{Upstream: "provider", Model: "native-model"}) || result.Route != route {
				t.Fatalf("route=%+v", route)
			}
			if !result.HasUsage || result.Usage != (usage.Usage{InputTokens: 7, OutputTokens: 2, CacheReadTokens: 3}) {
				t.Fatalf("normalized usage=%+v reported=%v", result.Usage, result.HasUsage)
			}
		})
	}
}

func TestHTTPRunnerAssemblesToolArguments(t *testing.T) {
	for _, tc := range []struct {
		protocol config.APIProtocol
		wire     string
	}{
		{config.APIOpenAICompletions, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"alpha\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"},
		{config.APIAnthropicMessages, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call1\",\"name\":\"lookup\",\"input\":{}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"alpha\\\"}\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			run := HTTPRunner(func(w http.ResponseWriter, _ *http.Request, _ config.APIProtocol) {
				for i := range len(tc.wire) {
					if _, err := io.WriteString(w, tc.wire[i:i+1]); err != nil {
						t.Error(err)
						return
					}
				}
			})
			var calls []ToolCall
			result, err := run(context.Background(), Request{Protocol: tc.protocol, Stream: true}, func(event Event) error {
				if event.Kind == ToolRequested {
					calls = append(calls, event.ToolCall)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || len(result.ToolCalls) != 1 || calls[0].ID != "call1" || calls[0].Name != "lookup" || string(calls[0].Arguments) != `{"q":"alpha"}` {
				t.Fatalf("assembled calls=%+v", calls)
			}
			if result.HasUsage {
				t.Fatal("tool request invented token usage")
			}
		})
	}
}

func TestHTTPRunnerPreservesUnknownVersusZeroUsage(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		reported   bool
	}{
		{"unknown", `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`, false},
		{"zero", `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := HTTPRunner(func(w http.ResponseWriter, _ *http.Request, _ config.APIProtocol) { _, _ = io.WriteString(w, tc.body) })
			result, err := run(context.Background(), Request{Protocol: config.APIOpenAICompletions}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != "done" || result.HasUsage != tc.reported || result.Usage != (usage.Usage{}) {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestHTTPRunnerRejectsIncompleteStreamsAndProviderErrors(t *testing.T) {
	run := HTTPRunner(func(w http.ResponseWriter, _ *http.Request, _ config.APIProtocol) {
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	})
	result, err := run(context.Background(), Request{Protocol: config.APIOpenAICompletions, Stream: true}, nil)
	if !errors.Is(err, ErrIncompleteResponse) || result.Text != "partial" {
		t.Fatalf("incomplete result=%+v error=%v", result, err)
	}
	run = HTTPRunner(func(w http.ResponseWriter, _ *http.Request, _ config.APIProtocol) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exhausted"}}`)
	})
	_, err = run(context.Background(), Request{Protocol: config.APIOpenAICompletions, Stream: true}, nil)
	var provider *ProviderError
	if !errors.As(err, &provider) || provider.Status != 429 || provider.Message != "quota exhausted" {
		t.Fatalf("provider error=%v", err)
	}
}

func TestHTTPRunnerCancellationAndResponseBound(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		name := "callback"
		if oversize {
			name = "response limit"
		}
		t.Run(name, func(t *testing.T) {
			callbackErr := errors.New("observer closed")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			run := HTTPRunner(func(w http.ResponseWriter, r *http.Request, _ config.APIProtocol) {
				payload := "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"
				if oversize {
					payload = strings.Repeat("x", MaxResponseBytes+1)
				}
				if _, err := io.WriteString(w, payload); err == nil {
					t.Error("aborted output was accepted")
				}
				<-r.Context().Done()
			})
			_, err := run(ctx, Request{Protocol: config.APIOpenAICompletions, Stream: true}, func(event Event) error {
				if event.Kind == TextDelta {
					return callbackErr
				}
				return nil
			})
			want := callbackErr
			if oversize {
				want = ErrResponseTooLarge
			}
			if !errors.Is(err, want) || ctx.Err() != nil {
				t.Fatalf("error=%v outer context=%v", err, ctx.Err())
			}
		})
	}
}

func TestHTTPRunnerKeepsCodexChatRequestsUncapped(t *testing.T) {
	run := HTTPRunner(func(w http.ResponseWriter, r *http.Request, _ config.APIProtocol) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["max_tokens"]; exists {
			http.Error(w, "Codex does not support token caps", 400)
			return
		}
		if r.Header.Get("X-OMP-Session") != "conversation" {
			http.Error(w, "missing conversation identity", 400)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"accepted"},"finish_reason":"stop"}]}`)
	})
	result, err := run(context.Background(), Request{Protocol: config.APIOpenAICompletions, ConversationID: "conversation", MaxOutputTokens: 128000, Messages: []Message{{Role: "user", Content: "hello"}}}, nil)
	if err != nil || result.Text != "accepted" {
		t.Fatalf("strict chat request: %+v %v", result, err)
	}
}
