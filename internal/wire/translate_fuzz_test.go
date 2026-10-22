package wire_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"llm-router/internal/wire"
)

func FuzzWireSupportedMessages(f *testing.F) {
	for _, raw := range []string{"{", "[]", "null", `{"messages":42}`, `{"messages":[{"role":"user","content":{}}]}`} {
		f.Add(uint8(0), raw, []byte{1, 7})
	}
	f.Add(uint8(1), "hello é", []byte{1, 2, 255})
	f.Add(uint8(2), "tool result", []byte{7, 3})
	f.Fuzz(func(t *testing.T, mode uint8, text string, plan []byte) {
		if len(text) > 4096 || len(plan) > 1024 {
			return
		}
		a, b := wire.APIOpenAICompletions, wire.APIAnthropicMessages
		raw := []byte(text)
		for _, p := range []wire.APIProtocol{a, b} {
			got, err := wire.TranslateRequest(p, p, raw)
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("identity request changed bytes")
			}
			got, err = wire.TranslateResponse(p, p, raw, "alias")
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("identity response changed bytes")
			}
			if wire.NewStreamTranslator(p, p, "alias") != nil {
				t.Fatal("identity stream must be transparent")
			}
		}
		if mode%3 == 0 {
			for _, pair := range [][2]wire.APIProtocol{{a, b}, {b, a}} {
				if out, err := wire.TranslateRequest(pair[0], pair[1], raw); err == nil && !json.Valid(out) {
					t.Fatal("accepted request produced invalid JSON")
				}
				if out, err := wire.TranslateResponse(pair[0], pair[1], raw, "alias"); err == nil && !json.Valid(out) {
					t.Fatal("accepted response produced invalid JSON")
				}
			}
			return
		}
		// The structured domain contains valid UTF-8; raw malformed bytes remain above.
		text = "message: " + strings.ToValidUTF8(text, "�")
		if !utf8.ValidString(text) {
			t.Fatal("invalid generated text")
		}
		quoted, _ := json.Marshal(text)
		args, _ := json.Marshal(map[string]string{"q": text})
		argString, _ := json.Marshal(string(args))
		openRequest := []byte(`{"model":"fixture","messages":[{"role":"user","content":` + string(quoted) + `},{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":` + string(argString) + `}}]},{"role":"tool","tool_call_id":"call_1","content":"result"}]}`)
		anthropicRequest := []byte(`{"model":"fixture","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":` + string(quoted) + `}]},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":` + string(args) + `}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"result"}]}]}`)
		for _, fixture := range []struct {
			client, upstream wire.APIProtocol
			body             []byte
		}{{a, b, openRequest}, {b, a, anthropicRequest}} {
			out, err := wire.TranslateRequest(fixture.client, fixture.upstream, fixture.body)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Messages []struct {
					Role       string
					Content    json.RawMessage
					ToolCallID string `json:"tool_call_id"`
					ToolCalls  []struct {
						ID       string
						Function struct {
							Name      string
							Arguments string
						}
					} `json:"tool_calls"`
				}
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Messages) != 3 || got.Messages[0].Role != "user" || got.Messages[1].Role != "assistant" {
				t.Fatalf("message identity/order: %s", out)
			}
			if fixture.upstream == a {
				var content string
				if err := json.Unmarshal(got.Messages[0].Content, &content); err != nil || content != text {
					t.Fatalf("text lost: %s", out)
				}
				calls := got.Messages[1].ToolCalls
				if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Name != "lookup" || got.Messages[2].ToolCallID != "call_1" {
					t.Fatalf("tool identity lost: %s", out)
				}
				var argument map[string]string
				if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &argument); err != nil || argument["q"] != text {
					t.Fatalf("tool argument lost: %s", out)
				}
			} else {
				var first []struct{ Type, Text string }
				if err := json.Unmarshal(got.Messages[0].Content, &first); err != nil || len(first) != 1 || first[0].Text != text {
					t.Fatalf("text lost: %s", out)
				}
				var blocks []struct {
					Type, ID, Name, Text string
					Input                map[string]string
				}
				if err := json.Unmarshal(got.Messages[1].Content, &blocks); err != nil {
					t.Fatal(err)
				}
				toolCount := 0
				for _, block := range blocks {
					switch block.Type {
					case "tool_use":
						toolCount++
						if block.ID != "call_1" || block.Name != "lookup" || block.Input["q"] != text {
							t.Fatalf("tool identity/arguments lost: %s", out)
						}
					case "text":
						// Empty assistant content can be omitted or retained as
						// an empty text block; neither changes tool semantics.
						if block.Text != "" {
							t.Fatalf("translation invented assistant text: %s", out)
						}
					default:
						t.Fatalf("translation invented assistant content: %s", out)
					}
				}
				if toolCount != 1 {
					t.Fatalf("tool count = %d, want one: %s", toolCount, out)
				}
				var results []struct {
					ToolUseID string `json:"tool_use_id"`
					Content   string
				}
				if err := json.Unmarshal(got.Messages[2].Content, &results); err != nil || len(results) != 1 || results[0].ToolUseID != "call_1" || results[0].Content != "result" {
					t.Fatalf("tool result lost: %s", out)
				}
			}
		}
		response := []byte(`{"id":"fixture","content":[{"type":"text","text":` + string(quoted) + `},{"type":"tool_use","id":"call_1","name":"lookup","input":` + string(args) + `}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":3}}`)
		out, err := wire.TranslateResponse(a, b, response, "alias")
		if err != nil {
			t.Fatal(err)
		}
		var reply struct {
			Model   string
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content   string
					ToolCalls []struct {
						ID       string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				}
			}
		}
		if err := json.Unmarshal(out, &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Model != "alias" || len(reply.Choices) != 1 || reply.Choices[0].FinishReason != "tool_calls" || reply.Choices[0].Message.Content != text {
			t.Fatalf("response semantics: %s", out)
		}
		calls := reply.Choices[0].Message.ToolCalls
		if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Name != "lookup" {
			t.Fatalf("response tool identity: %s", out)
		}
		var decoded map[string]string
		if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &decoded); err != nil || decoded["q"] != text {
			t.Fatalf("response arguments: %s", out)
		}
		stream := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":" + string(quoted) + "}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
		toolFragment, _ := json.Marshal(string(args))
		stream += "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"lookup\",\"input\":{}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":" + string(toolFragment) + "}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
		translate := func(fragment bool) []any {
			translator := wire.NewStreamTranslator(a, b, "alias")
			var output []byte
			for offset, step := 0, 0; offset < len(stream); step++ {
				n := len(stream) - offset
				if fragment && len(plan) > 0 {
					n = 1 + int(plan[step%len(plan)])
				}
				if n > len(stream)-offset {
					n = len(stream) - offset
				}
				output = append(output, translator.Write([]byte(stream[offset:offset+n]))...)
				offset += n
			}
			output = append(output, translator.Close()...)
			var events []any
			var combined strings.Builder
			finished := false
			var argumentFragments strings.Builder
			toolIDs, toolNames := 0, 0
			for _, line := range strings.Split(string(output), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				data := strings.TrimPrefix(line, "data: ")
				if data == "[DONE]" {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					t.Fatalf("invalid translated event: %s", line)
				}
				delete(event, "created")
				delete(event, "id")
				events = append(events, event)
				var chunk struct {
					Choices []struct {
						Delta        struct{ Content string }
						FinishReason *string `json:"finish_reason"`
					}
				}
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					t.Fatal(err)
				}
				for _, choice := range chunk.Choices {
					combined.WriteString(choice.Delta.Content)
					if choice.FinishReason != nil && *choice.FinishReason == "tool_calls" {
						finished = true
					}
				}
				var toolChunk struct {
					Choices []struct {
						Delta struct {
							ToolCalls []struct {
								ID       string
								Function struct{ Name, Arguments string }
							} `json:"tool_calls"`
						}
					}
				}
				if err := json.Unmarshal([]byte(data), &toolChunk); err != nil {
					t.Fatal(err)
				}
				for _, choice := range toolChunk.Choices {
					for _, call := range choice.Delta.ToolCalls {
						if call.ID != "" {
							if call.ID != "call_1" {
								t.Fatal("stream tool ID lost")
							}
							toolIDs++
						}
						if call.Function.Name != "" {
							if call.Function.Name != "lookup" {
								t.Fatal("stream tool name lost")
							}
							toolNames++
						}
						argumentFragments.WriteString(call.Function.Arguments)
					}
				}
			}
			var streamArguments map[string]string
			if err := json.Unmarshal([]byte(argumentFragments.String()), &streamArguments); err != nil || streamArguments["q"] != text || toolIDs != 1 || toolNames != 1 {
				t.Fatalf("stream tool semantics: %q, ids=%d names=%d", argumentFragments.String(), toolIDs, toolNames)
			}
			if combined.String() != text || !finished {
				t.Fatalf("stream text/terminal = %q/%v, want %q/true", combined.String(), finished, text)
			}
			return events
		}
		if whole, fragmented := translate(false), translate(true); !reflect.DeepEqual(whole, fragmented) {
			t.Fatal("stream partitions changed event semantics")
		}
		reverseStream := "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":" + string(quoted) + "},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
		reverse := func(fragment bool) []any {
			translator := wire.NewStreamTranslator(b, a, "alias")
			var output []byte
			for offset, step := 0, 0; offset < len(reverseStream); step++ {
				n := len(reverseStream) - offset
				if fragment && len(plan) > 0 {
					n = 1 + int(plan[step%len(plan)])
				}
				if n > len(reverseStream)-offset {
					n = len(reverseStream) - offset
				}
				output = append(output, translator.Write([]byte(reverseStream[offset:offset+n]))...)
				offset += n
			}
			output = append(output, translator.Close()...)
			var events []any
			var combined strings.Builder
			terminal := false
			for _, line := range strings.Split(string(output), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				data := strings.TrimPrefix(line, "data: ")
				var event map[string]any
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					t.Fatal(err)
				}
				// Only generated message IDs are nondeterministic.
				if message, ok := event["message"].(map[string]any); ok {
					delete(message, "id")
				}
				events = append(events, event)
				var decoded struct {
					Type  string
					Delta struct {
						Text       string
						StopReason string `json:"stop_reason"`
					}
				}
				if err := json.Unmarshal([]byte(data), &decoded); err != nil {
					t.Fatal(err)
				}
				combined.WriteString(decoded.Delta.Text)
				if decoded.Type == "message_delta" && decoded.Delta.StopReason == "end_turn" {
					terminal = true
				}
			}
			if combined.String() != text || !terminal {
				t.Fatalf("reverse stream text/finish = %q/%v, want %q/true", combined.String(), terminal, text)
			}
			return events
		}
		if whole, fragmented := reverse(false), reverse(true); !reflect.DeepEqual(whole, fragmented) {
			t.Fatal("reverse stream partitions changed semantics")
		}
		reverseBody := []byte(`{"id":"fixture","choices":[{"message":{"role":"assistant","content":` + string(quoted) + `,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":` + string(argString) + `}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
		reverseOut, err := wire.TranslateResponse(b, a, reverseBody, "alias")
		if err != nil {
			t.Fatal(err)
		}
		var reverseReply struct {
			Model      string
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type, Text, ID, Name string
				Input                map[string]string
			}
		}
		if err := json.Unmarshal(reverseOut, &reverseReply); err != nil {
			t.Fatal(err)
		}
		expectedBlocks := 1
		if text != "" {
			expectedBlocks = 2
		}
		if reverseReply.Model != "alias" || reverseReply.StopReason != "tool_use" || len(reverseReply.Content) != expectedBlocks {
			t.Fatalf("reverse response semantics: %s", reverseOut)
		}
		if text != "" && (reverseReply.Content[0].Type != "text" || reverseReply.Content[0].Text != text) {
			t.Fatalf("reverse response text: %s", reverseOut)
		}
		tool := reverseReply.Content[len(reverseReply.Content)-1]
		if tool.Type != "tool_use" || tool.ID != "call_1" || tool.Name != "lookup" || tool.Input["q"] != text {
			t.Fatalf("reverse response tool: %s", reverseOut)
		}
	})
}
