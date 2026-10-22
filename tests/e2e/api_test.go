package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// API assertions deliberately allow additive response/provider fields. Essential
// protocol fields and explicitly configured metadata remain mandatory.
func apiSubset(t *testing.T, got any, literal string) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(literal), &want); err != nil {
		t.Fatal(err)
	}
	var check func(any, any, string)
	check = func(g, w any, path string) {
		switch v := w.(type) {
		case map[string]any:
			m, ok := g.(map[string]any)
			if !ok {
				t.Errorf("%s: got %T, want object", path, g)
				return
			}
			for k, x := range v {
				check(m[k], x, path+"."+k)
			}
		case []any:
			a, ok := g.([]any)
			if !ok || len(a) != len(v) {
				t.Errorf("%s: got %#v, want array length %d", path, g, len(v))
				return
			}
			for i, x := range v {
				check(a[i], x, fmt.Sprintf("%s[%d]", path, i))
			}
		default:
			if !reflect.DeepEqual(g, w) {
				t.Errorf("%s: got %#v, want %#v", path, g, w)
			}
		}
	}
	check(got, want, "$")
}

func apiConfig(url string) string {
	return fmt.Sprintf(`apiKey: %s
defaults:
  rewriteModel: true
  maxBodyBytes: 1024
upstreams:
  fixture:
    baseUrl: %s/v1
    apiKey: provider-fixture-key
models:
  friendly/chat:
    api: openai-completions
    contextWindow: 12345
    maxOutputTokens: 321
    cost: {input: 2, output: 6, cacheRead: 1, cacheWrite: 3}
    targets: [{upstream: fixture, model: provider-model, weight: 1}]
  messages:
    api: anthropic-messages
    targets: [{upstream: fixture, model: provider-model, weight: 1}]
  responses:
    api: openai-responses
    targets: [{upstream: fixture, model: provider-model, weight: 1}]
`, gatewayKey, url)
}

func TestAPIH1ProtocolsPrefixesAndCredentials(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" {
			t.Errorf("provider method: %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer provider-fixture-key" {
			t.Errorf("provider authorization: %q", r.Header.Get("Authorization"))
		}
		for _, key := range []string{"X-Api-Key", "Api-Key"} {
			if r.Header.Get(key) != "" {
				t.Errorf("gateway credential leaked in %s", key)
			}
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var body map[string]any
		if err = json.Unmarshal(b, &body); err != nil {
			t.Error(err)
			return
		}
		apiSubset(t, body, `{"model":"provider-model","provider_extra":{"enabled":true}}`)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			io.WriteString(w, `{"id":"chat-1","object":"chat.completion","model":"provider-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"vendor_extra":{"trace":"fixture"}}`)
		case "/v1/messages":
			io.WriteString(w, `{"id":"msg-1","type":"message","role":"assistant","model":"provider-model","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1},"vendor_extra":{"trace":"fixture"}}`)
		case "/v1/responses":
			io.WriteString(w, `{"id":"resp-1","object":"response","model":"provider-model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"vendor_extra":{"trace":"fixture"}}`)
		default:
			t.Errorf("unexpected provider path %s", r.URL.Path)
			http.Error(w, "wrong path", 404)
		}
	}))
	defer provider.Close()
	router := startRouter(t, apiConfig(provider.URL))
	cases := []struct{ path, model, fields, expected string }{
		{"chat/completions", "friendly/chat", `"messages":[{"role":"user","content":"hi"}]`, `{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`},
		{"messages", "messages", `"max_tokens":10,"messages":[{"role":"user","content":"hi"}]`, `{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`},
		{"responses", "responses", `"input":"hi"`, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`},
	}
	for _, prefix := range []string{"/v1/", "/api/v1/"} {
		for _, tc := range cases {
			t.Run(prefix+tc.path, func(t *testing.T) {
				body := fmt.Sprintf(`{"model":%q,%s,"provider_extra":{"enabled":true}}`, tc.model, tc.fields)
				res := request(t, "POST", router.URL+prefix+tc.path, body, map[string]string{"Authorization": "Bearer " + gatewayKey, "X-Api-Key": "inbound-secret-one", "Api-Key": "inbound-secret-two"})
				if res.Status != 200 {
					t.Fatalf("status %d: %s", res.Status, res.Body)
				}
				got := decodeJSON(t, res.Body)
				apiSubset(t, got, tc.expected)
				apiSubset(t, got, fmt.Sprintf(`{"model":%q,"vendor_extra":{"trace":"fixture"}}`, tc.model))
				if res.Header.Get("X-Router-Upstream") != "fixture" || res.Header.Get("X-Router-Upstream-Model") != "provider-model" {
					t.Errorf("route headers: %v", res.Header)
				}
			})
		}
	}
	if calls.Load() != 6 {
		t.Errorf("dispatches=%d want 6", calls.Load())
	}
}

func TestAPIH1Discovery(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("discovery contacted provider")
		w.WriteHeader(500)
	}))
	defer provider.Close()
	router := startRouter(t, apiConfig(provider.URL))
	headers := map[string]string{"Authorization": "Bearer " + gatewayKey}
	for _, prefix := range []string{"/v1", "/api/v1"} {
		list := request(t, "GET", router.URL+prefix+"/models", "", headers)
		if list.Status != 200 {
			t.Fatalf("list status %d: %s", list.Status, list.Body)
		}
		got := decodeJSON(t, list.Body)
		if got["object"] != "list" {
			t.Errorf("list object: %v", got)
		}
		data, ok := got["data"].([]any)
		if !ok || len(data) != 3 {
			t.Fatalf("discovery aliases: %v", got)
		}
		found := false
		for _, entry := range data {
			if card, ok := entry.(map[string]any); ok && card["id"] == "friendly/chat" {
				found = true
				apiSubset(t, card, `{"object":"model","context_length":12345,"max_model_len":12345,"limits":{"max_input_tokens":12345,"max_output_tokens":321},"supported_endpoint_types":["openai"]}`)
			}
		}
		if !found {
			t.Error("slash alias missing")
		}
		detail := request(t, "GET", router.URL+prefix+"/models/friendly/chat", "", headers)
		if detail.Status != 200 {
			t.Fatalf("detail status %d: %s", detail.Status, detail.Body)
		}
		apiSubset(t, decodeJSON(t, detail.Body), `{"id":"friendly/chat","context_length":12345,"limits":{"max_output_tokens":321}}`)
	}
	for _, path := range []string{"/model_group/info", "/api/v1/model_group/info"} {
		res := request(t, "GET", router.URL+path, "", headers)
		if res.Status != 200 {
			t.Fatalf("group status %d: %s", res.Status, res.Body)
		}
		got := decodeJSON(t, res.Body)
		entries, ok := got["data"].([]any)
		if !ok {
			t.Fatalf("group data: %v", got)
		}
		found := false
		for _, raw := range entries {
			entry, ok := raw.(map[string]any)
			if ok && entry["model_name"] == "friendly/chat" {
				found = true
				apiSubset(t, entry, `{"model_info":{"max_input_tokens":12345,"max_tokens":321,"input_cost_per_token":0.000002,"output_cost_per_token":0.000006,"cache_read_input_token_cost":0.000001,"cache_creation_input_token_cost":0.000003,"providers":["fixture"]}}`)
			}
		}
		if !found {
			t.Error("group missing configured alias")
		}
	}
}

func TestAPIH2GuardsAndBodyBoundary(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"model":"provider-model","choices":[]}`)
	}))
	defer provider.Close()
	router := startRouter(t, apiConfig(provider.URL))
	valid := `{"model":"friendly/chat","messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name, method, path, body, auth string
		status                         int
		kind                           string
	}{
		{"missing credentials", "POST", "/v1/chat/completions", valid, "", 401, "unauthorized"},
		{"wrong credentials", "POST", "/api/v1/chat/completions", valid, "wrong", 401, "unauthorized"},
		{"malformed", "POST", "/v1/chat/completions", "{", gatewayKey, 400, "invalid_body"},
		{"oversize", "POST", "/v1/chat/completions", valid + strings.Repeat(" ", 1025-len(valid)), gatewayKey, 413, "body_too_large"},
		{"unknown model", "POST", "/v1/chat/completions", `{"model":"missing"}`, gatewayKey, 404, "unknown_model"},
		{"wrong wire", "POST", "/v1/messages", valid, gatewayKey, 400, "protocol_mismatch"},
		{"unsupported path", "POST", "/v1/embeddings", valid, gatewayKey, 404, "unsupported_endpoint"},
		{"wrong inference method", "GET", "/v1/chat/completions", "", gatewayKey, 405, "method_not_allowed"},
		{"wrong discovery method", "POST", "/api/v1/models", valid, gatewayKey, 405, "method_not_allowed"},
		{"unknown detail", "GET", "/v1/models/missing", "", gatewayKey, 404, "unknown_model"},
		{"discovery auth", "GET", "/api/v1/models", "", "", 401, "unauthorized"},
		{"group auth", "GET", "/api/v1/model_group/info", "", "", 401, "unauthorized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.auth != "" {
				headers["Authorization"] = "Bearer " + tc.auth
			}
			res := request(t, tc.method, router.URL+tc.path, tc.body, headers)
			if res.Status != tc.status {
				t.Fatalf("status %d want %d: %s", res.Status, tc.status, res.Body)
			}
			got := decodeJSON(t, res.Body)
			apiSubset(t, got, fmt.Sprintf(`{"error":{"type":%q,"code":%d}}`, tc.kind, tc.status))
			envelope, _ := got["error"].(map[string]any)
			message, _ := envelope["message"].(string)
			if strings.TrimSpace(message) == "" {
				t.Errorf("error message must be useful and nonempty: %s", res.Body)
			}
			if calls.Load() != 0 {
				t.Errorf("rejected request dispatched: %d", calls.Load())
			}
		})
	}
	for _, size := range []int{1023, 1024} {
		body := valid + strings.Repeat(" ", size-len(valid))
		res := request(t, "POST", router.URL+"/v1/chat/completions", body, map[string]string{"Authorization": "Bearer " + gatewayKey})
		if res.Status != 200 {
			t.Errorf("valid %d-byte request rejected: %d %s", size, res.Status, res.Body)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("boundary dispatches=%d want 2", calls.Load())
	}
}
