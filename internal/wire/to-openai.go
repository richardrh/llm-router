package wire

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

func anthropicToOpenAIRequest(body []byte) ([]byte, error) {
	var in map[string]interface{}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	out := map[string]interface{}{}
	for _, k := range []string{"model", "max_tokens", "temperature", "top_p", "stream"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	if v, ok := in["stop_sequences"]; ok {
		out["stop"] = v
	}
	msgs := []interface{}{}
	if s, ok := in["system"]; ok {
		text := anthropicText(s)
		if text != "" {
			msgs = append(msgs, map[string]interface{}{"role": "system", "content": text})
		}
	}
	if arr, ok := in["messages"].([]interface{}); ok {
		for _, raw := range arr {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			content := m["content"]
			blocks, isBlocks := content.([]interface{})
			if !isBlocks {
				msgs = append(msgs, map[string]interface{}{"role": role, "content": content})
				continue
			}
			var text []string
			var calls []interface{}
			for _, braw := range blocks {
				b, ok := braw.(map[string]interface{})
				if !ok {
					continue
				}
				typ, _ := b["type"].(string)
				switch typ {
				case "text":
					if t, ok := b["text"].(string); ok {
						text = append(text, t)
					}
				case "tool_use":
					call := map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": b["name"], "arguments": jsonString(b["input"])}}
					if id, ok := b["id"]; ok {
						call["id"] = id
					}
					calls = append(calls, call)
				case "tool_result":
					id, _ := b["tool_use_id"].(string)
					msgs = append(msgs, map[string]interface{}{"role": "tool", "tool_call_id": id, "content": anthropicText(b["content"])})
				}
			}
			if len(text) > 0 || len(calls) > 0 {
				mm := map[string]interface{}{"role": role}
				if len(text) > 0 {
					mm["content"] = strings.Join(text, "")
				}
				if len(calls) > 0 {
					mm["tool_calls"] = calls
					if _, ok := mm["content"]; !ok {
						mm["content"] = nil
					}
				}
				msgs = append(msgs, mm)
			}
		}
	}
	out["messages"] = msgs
	if ts, ok := in["tools"].([]interface{}); ok {
		tools := []interface{}{}
		for _, x := range ts {
			t, ok := x.(map[string]interface{})
			if !ok {
				continue
			}
			f := map[string]interface{}{"name": t["name"], "description": t["description"], "parameters": t["input_schema"]}
			tools = append(tools, map[string]interface{}{"type": "function", "function": f})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if tc, ok := in["tool_choice"]; ok {
		if s, ok := tc.(string); ok {
			out["tool_choice"] = s
		} else if m, ok := tc.(map[string]interface{}); ok {
			typ, _ := m["type"].(string)
			switch typ {
			case "auto":
				out["tool_choice"] = "auto"
			case "any":
				out["tool_choice"] = "required"
			case "tool":
				out["tool_choice"] = map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": m["name"]}}
			}
		}
	}
	if p, ok := in["thinking"].(map[string]interface{}); ok {
		if typ, _ := p["type"].(string); typ == "disabled" {
			out["reasoning_effort"] = "none"
		}
	}
	if oc, ok := in["output_config"].(map[string]interface{}); ok {
		if e, ok := oc["effort"].(string); ok {
			switch e {
			case "low":
				// Phase 4a maps OpenAI minimal to Anthropic low.
				out["reasoning_effort"] = "minimal"
			case "medium", "high", "xhigh", "max":
				out["reasoning_effort"] = e
			}
		}
	}
	if d, ok := in["disable_parallel_tool_use"].(bool); ok {
		out["parallel_tool_calls"] = !d
	}
	if d, ok := in["tool_choice"].(map[string]interface{}); ok {
		if x, ok := d["disable_parallel_tool_use"].(bool); ok {
			out["parallel_tool_calls"] = !x
		}
	}
	return json.Marshal(out)
}

func anthropicText(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	var out []string
	if a, ok := v.([]interface{}); ok {
		for _, x := range a {
			if m, ok := x.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					out = append(out, t)
				} else if t, ok := m["content"].(string); ok {
					out = append(out, t)
				}
			}
		}
	}
	return strings.Join(out, "")
}
func jsonString(v interface{}) string {
	if v == nil {
		return "{}"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func openAIToAnthropicResponse(body []byte, alias string) ([]byte, error) {
	var in map[string]interface{}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	out := map[string]interface{}{"type": "message", "role": "assistant", "id": in["id"], "model": alias, "stop_sequence": nil}
	ch, _ := in["choices"].([]interface{})
	var c map[string]interface{}
	if len(ch) > 0 {
		c, _ = ch[0].(map[string]interface{})
	}
	msg, _ := c["message"].(map[string]interface{})
	blocks := []interface{}{}
	if r, ok := msg["reasoning_content"].(string); ok && r != "" {
		blocks = append(blocks, map[string]interface{}{"type": "thinking", "thinking": r})
	}
	if t, ok := msg["content"].(string); ok && t != "" {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": t})
	}
	if calls, ok := msg["tool_calls"].([]interface{}); ok {
		for _, x := range calls {
			q, _ := x.(map[string]interface{})
			f, _ := q["function"].(map[string]interface{})
			var input interface{}
			if a, ok := f["arguments"].(string); ok {
				if json.Unmarshal([]byte(a), &input) != nil {
					input = map[string]interface{}{}
				}
			}
			blocks = append(blocks, map[string]interface{}{"type": "tool_use", "id": q["id"], "name": f["name"], "input": input})
		}
	}
	out["content"] = blocks
	reason, _ := c["finish_reason"].(string)
	out["stop_reason"] = anthropicStop(reason)
	u := map[string]interface{}{}
	if x, ok := in["usage"].(map[string]interface{}); ok {
		u["input_tokens"] = num(x["prompt_tokens"])
		u["output_tokens"] = num(x["completion_tokens"])
		if d, ok := x["prompt_tokens_details"].(map[string]interface{}); ok {
			if v := num(d["cached_tokens"]); v > 0 {
				u["cache_read_input_tokens"] = v
			}
			if v := num(d["cache_creation_tokens"]); v > 0 {
				u["cache_creation_input_tokens"] = v
			}
		}
	} else {
		u["input_tokens"] = 0
		u["output_tokens"] = 0
	}
	out["usage"] = u
	return json.Marshal(out)
}
func num(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}
func anthropicStop(s string) string {
	switch s {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

type openAIToAnthropicStream struct {
	buf           []byte
	alias, id     string
	started       bool
	next          int
	active        int
	activeKind    string
	tools         map[int]int
	closed        bool
	input, output int
}

func newOpenAIToAnthropicStream(alias string) *openAIToAnthropicStream {
	return &openAIToAnthropicStream{alias: alias, tools: map[int]int{}, active: -1}
}
func (t *openAIToAnthropicStream) emit(name string, v interface{}) []byte {
	b, _ := json.Marshal(v)
	return []byte("event: " + name + "\ndata: " + string(b) + "\n\n")
}
func (t *openAIToAnthropicStream) start() []byte {
	t.started = true
	return t.emit("message_start", map[string]interface{}{"type": "message_start", "message": map[string]interface{}{"id": t.id, "type": "message", "role": "assistant", "model": t.alias, "content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]interface{}{"input_tokens": t.input, "output_tokens": 0}}})
}
func (t *openAIToAnthropicStream) closeActive() []byte {
	if t.active < 0 {
		return nil
	}
	b := t.emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": t.active})
	t.active = -1
	t.activeKind = ""
	return b
}
func (t *openAIToAnthropicStream) Write(p []byte) []byte {
	t.buf = append(t.buf, p...)
	var out []byte
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(t.buf[:i]))
		t.buf = t.buf[i+1:]
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if d == "[DONE]" {
			continue
		}
		var x map[string]interface{}
		if json.Unmarshal([]byte(d), &x) != nil {
			continue
		}
		if t.id == "" {
			t.id, _ = x["id"].(string)
		}
		if u, ok := x["usage"].(map[string]interface{}); ok {
			t.input = num(u["prompt_tokens"])
			t.output = num(u["completion_tokens"])
		}
		if !t.started {
			out = append(out, t.start()...)
		}
		out = append(out, t.chunk(x)...)
	}
	return out
}
func (t *openAIToAnthropicStream) chunk(x map[string]interface{}) []byte {
	var out []byte
	ch, _ := x["choices"].([]interface{})
	if len(ch) == 0 {
		return nil
	}
	c, _ := ch[0].(map[string]interface{})
	d, _ := c["delta"].(map[string]interface{})
	if u, ok := x["usage"].(map[string]interface{}); ok {
		t.input = num(u["prompt_tokens"])
		t.output = num(u["completion_tokens"])
	}
	if r, ok := d["reasoning_content"].(string); ok && r != "" {
		if t.activeKind != "thinking" {
			out = append(out, t.closeActive()...)
			t.active = t.next
			t.next++
			t.activeKind = "thinking"
			out = append(out, t.emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": t.active, "content_block": map[string]interface{}{"type": "thinking", "thinking": ""}})...)
		}
		out = append(out, t.emit("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": t.active, "delta": map[string]interface{}{"type": "thinking_delta", "thinking": r}})...)
	}
	if r, ok := d["content"].(string); ok && r != "" {
		if t.activeKind != "text" {
			out = append(out, t.closeActive()...)
			t.active = t.next
			t.next++
			t.activeKind = "text"
			out = append(out, t.emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": t.active, "content_block": map[string]interface{}{"type": "text", "text": ""}})...)
		}
		out = append(out, t.emit("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": t.active, "delta": map[string]interface{}{"type": "text_delta", "text": r}})...)
	}
	if calls, ok := d["tool_calls"].([]interface{}); ok {
		for _, z := range calls {
			q, _ := z.(map[string]interface{})
			oi := num(q["index"])
			bi, exists := t.tools[oi]
			if !exists {
				out = append(out, t.closeActive()...)
				bi = t.next
				t.next++
				t.tools[oi] = bi
				f, _ := q["function"].(map[string]interface{})
				out = append(out, t.emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": bi, "content_block": map[string]interface{}{"type": "tool_use", "id": q["id"], "name": f["name"], "input": map[string]interface{}{}}})...)
			}
			f, _ := q["function"].(map[string]interface{})
			if a, ok := f["arguments"].(string); ok && a != "" {
				out = append(out, t.emit("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": bi, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": a}})...)
			}
		}
	}
	if reason, ok := c["finish_reason"].(string); ok && reason != "" {
		out = append(out, t.closeActive()...)
		out = append(out, t.toolStops()...)
		out = append(out, t.emit("message_delta", map[string]interface{}{"type": "message_delta", "delta": map[string]interface{}{"stop_reason": anthropicStop(reason), "stop_sequence": nil}, "usage": map[string]interface{}{"output_tokens": t.output}})...)
		out = append(out, t.emit("message_stop", map[string]interface{}{"type": "message_stop"})...)
		t.closed = true
	}
	return out
}
func (t *openAIToAnthropicStream) toolStops() []byte {
	indices := make([]int, 0, len(t.tools))
	for _, index := range t.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	var out []byte
	for _, index := range indices {
		out = append(out, t.emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": index})...)
	}
	return out
}
func (t *openAIToAnthropicStream) Close() []byte {
	if len(t.buf) > 0 {
		b := append(append([]byte(nil), t.buf...), '\n')
		t.buf = nil
		return t.Write(b)
	}
	if t.closed {
		return nil
	}
	if !t.started {
		t.id = ""
		return t.start()
	}
	out := t.closeActive()
	out = append(out, t.toolStops()...)
	out = append(out, t.emit("message_delta", map[string]interface{}{"type": "message_delta", "delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]interface{}{"output_tokens": t.output}})...)
	out = append(out, t.emit("message_stop", map[string]interface{}{"type": "message_stop"})...)
	t.closed = true
	return out
}
