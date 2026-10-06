package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestAnthropicToOpenAIRequest(t *testing.T) {
	body := []byte(`{"model":"m","system":[{"type":"text","text":"Be concise"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}],"tools":[{"name":"lookup","description":"find","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"},"stop_sequences":["END"]}`)
	got, err := anthropicToOpenAIRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]interface{}
	if err = json.Unmarshal(got, &v); err != nil {
		t.Fatal(err)
	}
	msgs := v["messages"].([]interface{})
	if msgs[0].(map[string]interface{})["role"] != "system" {
		t.Fatal("system not leading")
	}
	if msgs[1].(map[string]interface{})["role"] != "tool" {
		t.Fatal("tool result role")
	}
	if msgs[1].(map[string]interface{})["tool_call_id"] != "call_1" {
		t.Fatal("wrong tool id")
	}
	if v["tool_choice"] != "required" {
		t.Fatal("tool choice")
	}
}

func TestOpenAIToAnthropicStreamFragments(t *testing.T) {
	parts := []string{`data: {"id":"x","choices":[{"delta":{"role":"assistant","content":"hel"},"index":0,"finish_reason":null}]}`, "\n\n", `data: {"id":"x","choices":[{"delta":{"content":"lo"},"index":0,"finish_reason":null}]}\n\n`, `data: {"id":"x","choices":[{"delta":{},"index":0,"finish_reason":"stop"}]}\n\n`}
	a := newOpenAIToAnthropicStream("alias")
	var one []byte
	for _, p := range parts {
		one = append(one, a.Write([]byte(p))...)
	}
	one = append(one, a.Close()...)
	b := newOpenAIToAnthropicStream("alias")
	var many []byte
	whole := bytes.Join([][]byte{[]byte(parts[0]), []byte(parts[1]), []byte(parts[2]), []byte(parts[3])}, nil)
	for _, x := range whole {
		many = append(many, b.Write([]byte{x})...)
	}
	many = append(many, b.Close()...)
	if !bytes.Equal(one, many) {
		t.Fatal("fragment output differs")
	}
}
