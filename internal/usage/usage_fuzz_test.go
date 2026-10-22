package usage_test

import (
	"strings"
	"testing"

	"llm-router/internal/usage"
)

func FuzzUsageChunking(f *testing.F) {
	for i := uint8(0); i < 8; i++ {
		f.Add(i, []byte{1, 2, 7, 255})
	}
	f.Fuzz(func(t *testing.T, selector uint8, plan []byte) {
		if len(plan) > 1024 {
			return
		}
		openai := `{"usage":{"prompt_tokens":20,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":6}}}`
		anthropic := `{"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}`
		payload, expected, present := openai, (usage.Usage{InputTokens: 14, OutputTokens: 7, CacheReadTokens: 6}), true
		stream := ""
		switch selector % 8 {
		case 1:
			payload = anthropic
			expected = usage.Usage{InputTokens: 11, OutputTokens: 1, CacheReadTokens: 5, CacheWriteTokens: 3}
		case 2:
			payload = `{"usage":{"input_tokens":0,"output_tokens":0}}`
			expected = usage.Usage{}
		case 3:
			payload = `{"choices":[]}`
			expected = usage.Usage{}
			present = false
		case 4:
			stream = "data: " + anthropic + "\n\ndata: {\"usage\":{\"output_tokens\":4}}\n\ndata: {\"usage\":{\"output_tokens\":9}}\n\ndata: {\"choices\":[]}\n\n"
			expected = usage.Usage{InputTokens: 11, OutputTokens: 9, CacheReadTokens: 5, CacheWriteTokens: 3}
		case 5, 6, 7:
			// Include exact below/at/above boundary cases rather than trimming them away.
			n := usage.MaxSSELine + int(selector%8) - 6
			stream = strings.Repeat("x", n) + "\ndata: " + openai + "\n\n"
		}
		if stream == "" {
			got, found := usage.UsageFromBody([]byte(payload))
			if got != expected || found != present {
				t.Fatalf("body usage = %+v,%v; want %+v,%v", got, found, expected, present)
			}
			stream = ": comment\r\ndata: " + payload + "\r\n\ndata: [DONE]\n\n"
		}
		var observer usage.Observer
		for offset, step := 0, 0; offset < len(stream); step++ {
			size := len(stream) - offset
			if len(plan) > 0 {
				size = 1 + int(plan[step%len(plan)])
			}
			if size > len(stream)-offset {
				size = len(stream) - offset
			}
			observer.Write([]byte(stream[offset : offset+size]))
			offset += size
			if observer.BufferLen() > usage.MaxSSELine {
				t.Fatalf("buffer exceeded line bound: %d", observer.BufferLen())
			}
		}
		got, found := observer.Result()
		if got != expected || found != present {
			t.Fatalf("stream usage = %+v,%v; want %+v,%v", got, found, expected, present)
		}
	})
}
