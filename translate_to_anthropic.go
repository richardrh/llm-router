package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var anthropicSafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func openAIToAnthropicRequest(body []byte) ([]byte, error) {
	var in map[string]interface{}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	out := map[string]interface{}{}
	for _, k := range []string{"model", "temperature", "top_p", "stream"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	if v, ok := in["max_tokens"]; ok {
		out["max_tokens"] = v
	} else {
		out["max_tokens"] = 4096
	}
	if v, ok := in["stop"]; ok {
		if s, ok := v.(string); ok {
			out["stop_sequences"] = []interface{}{s}
		} else {
			out["stop_sequences"] = v
		}
	}
	if r, ok := in["reasoning_effort"].(string); ok && r != "none" {
		if r == "minimal" {
			r = "low"
		}
		out["output_config"] = map[string]interface{}{"effort": r}
	}
	// response_format and provider-specific controls are intentionally dropped:
	// they are not valid on Anthropic's Messages wire.

	msgs, _ := in["messages"].([]interface{})
	var system []interface{}
	pos := 0
	for pos < len(msgs) {
		m, ok := msgs[pos].(map[string]interface{})
		if !ok || m["role"] != "system" {
			break
		}
		blocks := contentBlocks(m["content"])
		if len(blocks) == 0 {
			blocks = []interface{}{map[string]interface{}{"type": "text", "text": ""}}
		}
		system = append(system, blocks...)
		pos++
	}
	if len(system) > 0 {
		out["system"] = system
	}
	var result []interface{}
	for pos < len(msgs) {
		m, ok := msgs[pos].(map[string]interface{})
		if !ok {
			pos++
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" {
			result = append(result, map[string]interface{}{"role": "user", "content": []interface{}{map[string]interface{}{"type": "text", "text": "Operator note (not from the user): " + fmt.Sprint(m["content"])}}})
			pos++
			continue
		}
		if role == "assistant" {
			var all []interface{}
			for pos < len(msgs) {
				x, _ := msgs[pos].(map[string]interface{})
				if x["role"] != "assistant" {
					break
				}
				all = append(all, assistantBlocks(x)...)
				pos++
			}
			result = append(result, map[string]interface{}{"role": "assistant", "content": all})
			continue
		}
		// user/tool/function messages are merged into a single user turn.
		var normal, tools []interface{}
		for pos < len(msgs) {
			x, _ := msgs[pos].(map[string]interface{})
			r, _ := x["role"].(string)
			if r != "user" && r != "tool" && r != "function" {
				break
			}
			if r == "tool" || r == "function" {
				id := fmt.Sprint(x["tool_call_id"])
				if id == "<nil>" {
					id = fmt.Sprint(x["id"])
				}
				tools = append(tools, map[string]interface{}{"type": "tool_result", "tool_use_id": sanitize(id), "content": contentValue(x["content"])})
			} else {
				if x["content"] == nil {
					pos++
					continue
				}
				normal = append(normal, contentBlocks(x["content"])...)
			}
			pos++
		}
		// later system notes become user text with explicit prefix.
		for pos < len(msgs) {
			break
		}
		result = append(result, map[string]interface{}{"role": "user", "content": append(tools, normal...)})
	}
	if len(result) == 0 || result[0].(map[string]interface{})["role"] != "user" {
		result = append([]interface{}{map[string]interface{}{"role": "user", "content": []interface{}{}}}, result...)
	}
	out["messages"] = result
	if tc, exists := in["tool_choice"]; exists {
		if s, ok := tc.(string); ok {
			if s == "none" {
				delete(out, "tools")
			} else {
				out["tool_choice"] = map[string]interface{}{"type": map[string]string{"auto": "auto", "required": "any"}[s]}
			}
		} else if mm, ok := tc.(map[string]interface{}); ok {
			if f, ok := mm["function"].(map[string]interface{}); ok {
				out["tool_choice"] = map[string]interface{}{"type": "tool", "name": sanitize(fmt.Sprint(f["name"]))}
			}
		}
	}
	if _, ok := out["tool_choice"]; !ok {
		if v, ok := in["parallel_tool_calls"].(bool); ok {
			out["tool_choice"] = map[string]interface{}{"disable_parallel_tool_use": !v}
		}
	}
	if tv, ok := in["tools"].([]interface{}); ok {
		ts := []interface{}{}
		for _, z := range tv {
			f, _ := z.(map[string]interface{})["function"].(map[string]interface{})
			if f == nil {
				continue
			}
			ts = append(ts, map[string]interface{}{"name": sanitize(fmt.Sprint(f["name"])), "description": f["description"], "input_schema": f["parameters"]})
		}
		if len(ts) > 0 && in["tool_choice"] != "none" {
			out["tools"] = ts
		}
	}
	return json.Marshal(out)
}
func sanitize(s string) string {
	s = anthropicSafe.ReplaceAllString(s, "_")
	if s == "" {
		s = "tool"
	}
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}
func contentValue(v interface{}) interface{} {
	if s, ok := v.(string); ok {
		return s
	}
	return v
}
func contentBlocks(v interface{}) []interface{} {
	var out []interface{}
	if s, ok := v.(string); ok {
		return []interface{}{map[string]interface{}{"type": "text", "text": s}}
	}
	a, _ := v.([]interface{})
	for _, p := range a {
		m, _ := p.(map[string]interface{})
		typ, _ := m["type"].(string)
		if typ == "text" {
			out = append(out, map[string]interface{}{"type": "text", "text": m["text"]})
		} else if typ == "image_url" {
			im, _ := m["image_url"].(map[string]interface{})
			u, _ := im["url"].(string)
			src := map[string]interface{}{}
			if strings.HasPrefix(u, "data:") {
				q := strings.SplitN(u, ",", 2)
				h := strings.TrimPrefix(q[0], "data:")
				parts := strings.SplitN(h, ";", 2)
				mt := parts[0]
				src = map[string]interface{}{"type": "base64", "media_type": mt, "data": ""}
				if len(q) > 1 {
					src["data"] = q[1]
				}
			} else {
				src = map[string]interface{}{"type": "url", "url": u}
			}
			out = append(out, map[string]interface{}{"type": "image", "source": src})
		}
	}
	return out
}
func assistantBlocks(m map[string]interface{}) []interface{} {
	var out []interface{}
	out = append(out, contentBlocks(m["content"])...)
	seen := map[string]bool{}
	if a, ok := m["tool_calls"].([]interface{}); ok {
		for _, z := range a {
			c := z.(map[string]interface{})
			f := c["function"].(map[string]interface{})
			id := sanitize(fmt.Sprint(c["id"]))
			if seen[id] {
				continue
			}
			seen[id] = true
			var input interface{}
			if json.Unmarshal([]byte(fmt.Sprint(f["arguments"])), &input) != nil {
				input = map[string]interface{}{}
			}
			out = append(out, map[string]interface{}{"type": "tool_use", "id": id, "name": sanitize(fmt.Sprint(f["name"])), "input": input})
		}
	}
	return out
}

func anthropicToOpenAIResponse(body []byte, alias string) ([]byte, error) {
	var in map[string]interface{}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	msg := map[string]interface{}{"role": "assistant", "content": ""}
	var texts []string
	var tools []interface{}
	var reason string
	if a, ok := in["content"].([]interface{}); ok {
		for _, z := range a {
			m, _ := z.(map[string]interface{})
			switch m["type"] {
			case "text":
				texts = append(texts, fmt.Sprint(m["text"]))
			case "thinking":
				reason = fmt.Sprint(m["thinking"])
			case "tool_use":
				b, _ := json.Marshal(m["input"])
				tools = append(tools, map[string]interface{}{"id": m["id"], "type": "function", "function": map[string]interface{}{"name": m["name"], "arguments": string(b)}})
			}
		}
	}
	if len(texts) > 0 {
		msg["content"] = strings.Join(texts, "")
	}
	if len(tools) > 0 {
		msg["tool_calls"] = tools
	}
	if reason != "" {
		msg["reasoning_content"] = reason
	}
	sr := fmt.Sprint(in["stop_reason"])
	fr := map[string]string{"end_turn": "stop", "stop_sequence": "stop", "pause_turn": "stop", "max_tokens": "length", "tool_use": "tool_calls", "refusal": "content_filter"}[sr]
	u, _ := in["usage"].(map[string]interface{})
	prompt := numAnthropic(u["input_tokens"]) + numAnthropic(u["cache_read_input_tokens"]) + numAnthropic(u["cache_creation_input_tokens"])
	comp := numAnthropic(u["output_tokens"])
	usage := map[string]interface{}{"prompt_tokens": prompt, "completion_tokens": comp, "total_tokens": prompt + comp}
	details := map[string]interface{}{}
	if u["cache_read_input_tokens"] != nil {
		details["cached_tokens"] = numAnthropic(u["cache_read_input_tokens"])
	}
	if u["cache_creation_input_tokens"] != nil {
		details["cache_creation_tokens"] = numAnthropic(u["cache_creation_input_tokens"])
	}
	if len(details) > 0 {
		usage["prompt_tokens_details"] = details
	}
	out := map[string]interface{}{"id": in["id"], "object": "chat.completion", "created": time.Now().Unix(), "model": alias, "choices": []interface{}{map[string]interface{}{"index": 0, "message": msg, "finish_reason": fr}}, "usage": usage}
	return json.Marshal(out)
}
func numAnthropic(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

type anthropicToOpenAIStream struct {
	alias       string
	buf         []byte
	id          string
	created     int64
	model       string
	toolIndex   int
	activeTools map[int]bool
	done        bool
	inputUsage  map[string]interface{}
}

func streamUsage(v interface{}) interface{} {
	u, _ := v.(map[string]interface{})
	p := numAnthropic(u["input_tokens"]) + numAnthropic(u["cache_read_input_tokens"]) + numAnthropic(u["cache_creation_input_tokens"])
	c := numAnthropic(u["output_tokens"])
	out := map[string]interface{}{"prompt_tokens": p, "completion_tokens": c, "total_tokens": p + c}
	if n := numAnthropic(u["cache_read_input_tokens"]); n > 0 {
		out["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": n}
	}
	return out
}

func newAnthropicToOpenAIStream(alias string) *anthropicToOpenAIStream {
	return &anthropicToOpenAIStream{alias: alias, created: time.Now().Unix(), activeTools: map[int]bool{}}
}
func (t *anthropicToOpenAIStream) chunk(delta map[string]interface{}, finish interface{}, usage interface{}) []byte {
	c := map[string]interface{}{"object": "chat.completion.chunk", "id": t.id, "created": t.created, "model": t.alias, "choices": []interface{}{map[string]interface{}{"index": 0, "delta": delta, "finish_reason": finish}}}
	if usage != nil {
		c["usage"] = usage
	}
	b, _ := json.Marshal(c)
	return []byte("data: " + string(b) + "\n\n")
}
func (t *anthropicToOpenAIStream) event(data []byte) []byte {
	var e map[string]interface{}
	if json.Unmarshal(data, &e) != nil {
		return nil
	}
	typ, _ := e["type"].(string)
	switch typ {
	case "message_start":
		m, _ := e["message"].(map[string]interface{})
		if t.id == "<nil>" {
			t.id = "chatcmpl-" + fmt.Sprint(t.created)
		}
		t.inputUsage, _ = m["usage"].(map[string]interface{})
		return t.chunk(map[string]interface{}{"role": "assistant"}, nil, nil)
	case "content_block_start":
		b, _ := e["content_block"].(map[string]interface{})
		if b["type"] == "tool_use" {
			i := t.toolIndex
			t.toolIndex++
			return t.chunk(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"index": i, "id": b["id"], "type": "function", "function": map[string]interface{}{"name": b["name"], "arguments": ""}}}}, nil, nil)
		}
	case "content_block_delta":
		d, _ := e["delta"].(map[string]interface{})
		switch d["type"] {
		case "text_delta":
			return t.chunk(map[string]interface{}{"content": d["text"]}, nil, nil)
		case "thinking_delta":
			return t.chunk(map[string]interface{}{"reasoning_content": d["thinking"]}, nil, nil)
		case "input_json_delta":
			i, _ := e["index"].(float64)
			return t.chunk(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"index": int(i), "function": map[string]interface{}{"arguments": d["partial_json"]}}}}, nil, nil)
		}
	case "message_delta":
		d, _ := e["delta"].(map[string]interface{})
		raw, _ := e["usage"].(map[string]interface{})
		if raw == nil {
			raw = t.inputUsage
		}
		if t.inputUsage != nil && raw != nil {
			merged := map[string]interface{}{}
			for k, v := range t.inputUsage {
				merged[k] = v
			}
			for k, v := range raw {
				merged[k] = v
			}
			raw = merged
		}
		u := streamUsage(raw)
		fr := map[string]string{"end_turn": "stop", "stop_sequence": "stop", "pause_turn": "stop", "max_tokens": "length", "tool_use": "tool_calls", "refusal": "content_filter"}[fmt.Sprint(d["stop_reason"])]
		return t.chunk(map[string]interface{}{}, fr, u)
	case "error":
		return t.chunk(map[string]interface{}{"content": fmt.Sprint(e["error"])}, "error", nil)
	}
	return nil
}
func (t *anthropicToOpenAIStream) Write(p []byte) []byte {
	if t.done {
		return nil
	}
	t.buf = append(t.buf, p...)
	var out []byte
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(t.buf[:i]))
		t.buf = t.buf[i+1:]
		if strings.HasPrefix(line, "data:") {
			out = append(out, t.event([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))...)
		}
	}
	return out
}
func (t *anthropicToOpenAIStream) Close() []byte {
	if t.done {
		return nil
	}
	t.done = true
	return []byte("data: [DONE]\n\n")
}
