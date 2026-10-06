package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIToAnthropicRequest(t *testing.T) {
	tests := []struct {
		name  string
		in    map[string]interface{}
		check func(t *testing.T, m map[string]interface{})
	}{
		{"hoist and merge", map[string]interface{}{"model": "m", "messages": []interface{}{map[string]interface{}{"role": "system", "content": "one"}, map[string]interface{}{"role": "system", "content": "two"}, map[string]interface{}{"role": "user", "content": "hi"}, map[string]interface{}{"role": "tool", "tool_call_id": "x", "content": "ok"}}, "parallel_tool_calls": false}, func(t *testing.T, m map[string]interface{}) {
			if len(m["system"].([]interface{})) != 2 {
				t.Fatal(m)
			}
			if m["max_tokens"].(float64) != 4096 {
				t.Fatal(m)
			}
			if m["tool_choice"].(map[string]interface{})["disable_parallel_tool_use"] != true {
				t.Fatal(m)
			}
			if len(m["messages"].([]interface{})) != 1 {
				t.Fatal(m)
			}
		}},
		{"tools", map[string]interface{}{"messages": []interface{}{map[string]interface{}{"role": "user", "content": "x"}}, "tools": []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "f", "parameters": map[string]interface{}{"type": "object"}}}}, "tool_choice": "auto"}, func(t *testing.T, m map[string]interface{}) {
			if len(m["tools"].([]interface{})) != 1 {
				t.Fatal(m)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, e := json.Marshal(tt.in)
			if e != nil {
				t.Fatal(e)
			}
			out, e := openAIToAnthropicRequest(b)
			if e != nil {
				t.Fatal(e)
			}
			var m map[string]interface{}
			if json.Unmarshal(out, &m) != nil {
				t.Fatal(string(out))
			}
			tt.check(t, m)
		})
	}
}

func TestAnthropicStreamFragments(t *testing.T) {
	src := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","usage":{"input_tokens":2}}}` + "\n\n",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call","name":"fn","input":{}}}` + "\n\n",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\\\"a\\\":"}}` + "\n\n",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1}"}}` + "\n\n",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}` + "\n\n",
	}, "")
	whole := newAnthropicToOpenAIStream("alias")
	got := append(whole.Write([]byte(src)), whole.Close()...)
	frag := newAnthropicToOpenAIStream("alias")
	frag.created = whole.created
	var gotFrag []byte
	for i := range src {
		gotFrag = append(gotFrag, frag.Write([]byte{src[i]})...)
	}
	gotFrag = append(gotFrag, frag.Close()...)
	if !bytes.Contains(got, []byte(`"id":"call"`)) || bytes.Count(got, []byte(`"id":"call"`)) != 1 {
		t.Fatal("tool id should occur once")
	}
	if !bytes.Contains(got, []byte(`"name":"fn"`)) || bytes.Count(got, []byte(`"name":"fn"`)) != 1 {
		t.Fatal("tool name should occur once")
	}
	// Fragmentation must not change the output: the state machine may not depend
	// on where the read boundaries happen to fall.
	if !bytes.Equal(got, gotFrag) {
		t.Fatalf("fragment output differs:\nwhole: %s\nfrag:  %s", got, gotFrag)
	}
}

func TestAnthropicToOpenAIResponse(t *testing.T) {
	b := []byte(`{"id":"m","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"x","name":"f","input":{"a":1}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":3}}`)
	out, e := anthropicToOpenAIResponse(b, "alias")
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(out, []byte(`"finish_reason":"tool_calls"`)) {
		t.Fatal(string(out))
	}
	if !bytes.Contains(out, []byte(`"model":"alias"`)) {
		t.Fatal(string(out))
	}
}
