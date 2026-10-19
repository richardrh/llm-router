package router

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistentCLIHeaderlessContinuation(t *testing.T) {
	for _, preamble := range []bool{false, true} {
		name := "user-only"
		if preamble {
			name = "system-preamble"
		}
		t.Run(name, func(t *testing.T) {
			script := writePersistentFakeCLI(t, filepath.Join(t.TempDir(), "inputs"))
			srv, _ := persistentCLITestServer(t, script, func(c *Config) {
				up := c.Upstreams["cli"]
				up.MaxSessions = 1
				c.Upstreams["cli"] = up
			})
			messages := []any{}
			if preamble {
				messages = append(messages, map[string]any{"role": "system", "content": "shared instructions"}, map[string]any{"role": "system", "content": "more shared instructions"})
			}
			messages = append(messages, map[string]any{"role": "user", "content": "first"})
			res, body := cliPost(t, srv, "/api/v1/chat/completions", map[string]any{"model": "test-alias", "messages": messages})
			if res.StatusCode != http.StatusOK || !strings.Contains(body, `"content":"turn1"`) {
				t.Fatalf("first turn: %d %s", res.StatusCode, body)
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": "turn1"}, map[string]any{"role": "user", "content": "second"})
			res, body = cliPost(t, srv, "/api/v1/chat/completions", map[string]any{"model": "test-alias", "messages": messages})
			if res.StatusCode != http.StatusOK || !strings.Contains(body, `"content":"turn2"`) {
				t.Fatalf("continuation: %d %s", res.StatusCode, body)
			}
			// Sharing a system preamble must not merge different conversations.
			other := append([]any(nil), messages[:len(messages)-3]...)
			other = append(other, map[string]any{"role": "user", "content": "a different task"})
			res, body = cliPost(t, srv, "/api/v1/chat/completions", map[string]any{"model": "test-alias", "messages": other})
			if res.StatusCode == http.StatusOK {
				t.Fatalf("different conversation reused occupied session: %s", body)
			}
		})
	}
}

func TestPersistentCLIHeaderlessStickySelection(t *testing.T) {
	script := writePersistentFakeCLI(t, filepath.Join(t.TempDir(), "inputs"))
	srv, _ := persistentCLITestServer(t, script, func(c *Config) {
		c.Defaults.Sticky = true
		c.Defaults.StickyByPrefix = false
		c.Upstreams["backup"] = c.Upstreams["cli"]
		alias := c.Models["test-alias"]
		alias.Targets = []Target{{Upstream: "cli", Model: "cli-model", Weight: 1}, {Upstream: "backup", Model: "cli-model", Weight: 0}}
		c.Models["test-alias"] = alias
	})
	messages := []any{map[string]any{"role": "user", "content": "first"}}
	res, body := cliPost(t, srv, "/api/v1/chat/completions", map[string]any{"model": "test-alias", "messages": messages})
	if res.StatusCode != http.StatusOK || res.Header.Get("X-Router-Upstream") != "cli" {
		t.Fatalf("initial target: %d %s", res.StatusCode, body)
	}
	// Changing weights must not dislodge a healthy conversation pin.
	alias := srv.cfg.Models["test-alias"]
	alias.Targets[0].Weight, alias.Targets[1].Weight = 0, 1
	srv.cfg.Models["test-alias"] = alias
	messages = append(messages, map[string]any{"role": "assistant", "content": "turn1"}, map[string]any{"role": "user", "content": "second"})
	res, body = cliPost(t, srv, "/api/v1/chat/completions", map[string]any{"model": "test-alias", "messages": messages})
	if res.StatusCode != http.StatusOK || res.Header.Get("X-Router-Upstream") != "cli" || !strings.Contains(body, `"content":"turn2"`) {
		t.Fatalf("conversation lost its pin: %d %s", res.StatusCode, body)
	}
}

func TestStreamingIgnoresIncompatibleCLIBackup(t *testing.T) {
	primary := newFakeUpstream(t, "primary")
	primary.stream.Store(true)
	srv := newTestServer(t, map[string]*fakeUpstream{"primary": primary}, map[string]int{"primary": 1}, func(c *Config, a *Alias) {
		c.Upstreams["backup"] = Upstream{Kind: UpstreamCLI, Mode: CLIPersistent, ToolMode: CLIToolsClient, Command: []string{"/does-not-exist"}, MaxSessions: 1, MaxConcurrency: 1}
		a.Targets = append(a.Targets, Target{Upstream: "backup", Model: "cli-model", Weight: 0})
	})
	t.Cleanup(srv.router.Close)
	res, body := cliPost(t, srv, "/api/v1/chat/completions", map[string]any{"model": "test-alias", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "data: [DONE]") || primary.hits() != 1 {
		t.Fatalf("HTTP primary stream: %d %s", res.StatusCode, body)
	}
}

func TestStreamingSkipsIncompatibleCLIPrimary(t *testing.T) {
	fallback := newFakeUpstream(t, "fallback")
	fallback.stream.Store(true)
	srv := newTestServer(t, map[string]*fakeUpstream{"fallback": fallback}, map[string]int{"fallback": 0}, func(c *Config, a *Alias) {
		c.Defaults.MaxAttempts = 1
		c.Upstreams["cli"] = Upstream{Kind: UpstreamCLI, Mode: CLIPersistent, ToolMode: CLIToolsClient, Command: []string{"/does-not-exist"}, MaxSessions: 1, MaxConcurrency: 1}
		a.Targets = append(a.Targets, Target{Upstream: "cli", Model: "cli-model", Weight: 1})
	})
	t.Cleanup(srv.router.Close)
	res, body := cliPost(t, srv, "/api/v1/chat/completions", map[string]any{"model": "test-alias", "stream": true})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("compatible fallback stream: %d %s", res.StatusCode, body)
	}
}

func TestPersistentCLIRejectsUnsupportedRequestWithoutStartingProcess(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"client-tool-stream", map[string]any{"model": "test-alias", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, "client_tools_stream_unsupported"},
		{"missing-session", map[string]any{"model": "test-alias", "messages": []any{map[string]any{"role": "system", "content": "instructions only"}}}, "session_required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := persistentCLITestServer(t, "/does-not-exist")
			res, body := cliPost(t, srv, "/api/v1/chat/completions", tc.body)
			if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"type":"`+tc.code+`"`) {
				t.Fatalf("rejection: %d %s", res.StatusCode, body)
			}
		})
	}
}
