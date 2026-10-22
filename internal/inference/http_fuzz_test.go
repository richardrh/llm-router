package inference_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"llm-router/internal/config"
	"llm-router/internal/inference"
	"llm-router/internal/usage"
)

func FuzzSSEFraming(f *testing.F) {
	for i := uint8(0); i < 8; i++ {
		f.Add(i, []byte{1, 2, 7, 255})
	}
	f.Fuzz(func(t *testing.T, selector uint8, plan []byte) {
		if len(plan) > 1024 {
			return
		}
		protocol := config.APIOpenAICompletions
		text := "héllo"
		stream := ": heartbeat\r\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"héllo\"}}]}\r\n\r\n"
		terminal := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":6}}}\n\ndata: [DONE]\n\n"
		expectedUsage := usage.Usage{InputTokens: 14, OutputTokens: 7, CacheReadTokens: 6}
		kind := selector % 8
		if kind == 1 || kind == 3 || kind == 5 {
			protocol = config.APIAnthropicMessages
			stream = ": heartbeat\r\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"héllo\"}}\r\n\r\n"
			terminal = "data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":14,\"output_tokens\":7,\"cache_read_input_tokens\":6}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
		}
		incomplete := kind == 2 || kind == 3
		tools := kind == 4 || kind == 5
		if tools {
			if protocol == config.APIOpenAICompletions {
				stream += "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\"}\"}}]}}]}\n\n"
			} else {
				stream += "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"lookup\",\"input\":{}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"x\\\"}\"}}\n\n"
			}
		}
		if !incomplete {
			stream += terminal
		}
		if kind == 6 {
			stream = `{"error":{"message":"fixture unavailable"}}`
			text = ""
		}
		if kind == 7 {
			stream = "data: {broken}\n\n"
			text = ""
		}
		runner := inference.HTTPRunner(func(w http.ResponseWriter, r *http.Request, p config.APIProtocol) {
			if p != protocol {
				t.Fatal("runner changed protocol")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if kind == 6 {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			for offset, step := 0, 0; offset < len(stream); step++ {
				size := len(stream) - offset
				if len(plan) > 0 {
					size = 1 + int(plan[step%len(plan)])
				}
				if size > len(stream)-offset {
					size = len(stream) - offset
				}
				if _, err := w.Write([]byte(stream[offset : offset+size])); err != nil {
					return
				}
				offset += size
			}
		})
		var eventsText strings.Builder
		toolEvents, usageEvents := 0, 0
		result, err := runner(context.Background(), inference.Request{Model: "fixture", Protocol: protocol, Stream: true}, func(e inference.Event) error {
			switch e.Kind {
			case inference.TextDelta:
				eventsText.WriteString(e.Text)
			case inference.ToolRequested:
				toolEvents++
			case inference.UsageReported:
				usageEvents++
			}
			return nil
		})
		if result.Text != text || eventsText.String() != text {
			t.Fatalf("text result/events = %q/%q; want %q", result.Text, eventsText.String(), text)
		}
		if incomplete {
			if !errors.Is(err, inference.ErrIncompleteResponse) {
				t.Fatalf("prefix error = %v", err)
			}
			return
		}
		if kind == 6 {
			var provider *inference.ProviderError
			if !errors.As(err, &provider) || provider.Status != 503 || provider.Message != "fixture unavailable" {
				t.Fatalf("provider error = %v", err)
			}
			return
		}
		if kind == 7 {
			if err == nil {
				t.Fatal("malformed event succeeded")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if !result.HasUsage || result.Usage != expectedUsage || usageEvents != 1 {
			t.Fatalf("usage = %+v/%v, events %d", result.Usage, result.HasUsage, usageEvents)
		}
		if tools {
			if len(result.ToolCalls) != 1 || toolEvents != 1 {
				t.Fatalf("tools = %+v, events %d", result.ToolCalls, toolEvents)
			}
			call := result.ToolCalls[0]
			if call.ID != "call_1" || call.Name != "lookup" || string(call.Arguments) != `{"q":"x"}` {
				t.Fatalf("tool = %+v", call)
			}
		} else if len(result.ToolCalls) != 0 || toolEvents != 0 {
			t.Fatal("unexpected tools")
		}
	})
}
