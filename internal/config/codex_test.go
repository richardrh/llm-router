package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, extra, api string
		invalid          bool
	}{
		{name: "defaults", api: "openai-completions"},
		{name: "anthropic", api: "anthropic-messages", invalid: true},
		{name: "responses", api: "openai-responses", invalid: true},
		{name: "key", extra: "    apiKey: secret\n", invalid: true},
		{name: "auth", extra: "    authStyle: bearer\n", invalid: true},
		{name: "url", extra: "    baseUrl: https://example.com\n", invalid: true},
		{name: "headers", extra: "    headers: {X-Test: value}\n", invalid: true},
		{name: "patch", extra: "    bodyPatch: {temperature: 1}\n", invalid: true},
		{name: "oneshot", extra: "    mode: one-shot\n", invalid: true},
		{name: "owned-tools", extra: "    toolMode: claude\n", invalid: true},
		{name: "command", extra: "    command: [/custom/codex, app-server, --listen, 'stdio://']\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := tc.api
			if api == "" {
				api = "openai-completions"
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			text := "upstreams:\n  subscription:\n    kind: codex\n" + tc.extra + "models:\n  chat:\n    api: " + api + "\n    targets:\n      - upstream: subscription\n        model: observed-model\n        weight: 1\n"
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if tc.invalid {
				if err == nil {
					t.Fatal("expected rejected configuration")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			u := cfg.Upstreams["subscription"]
			if u.Mode != CLIPersistent || u.ToolMode != CLIToolsClient || u.AuthStyle != "" {
				t.Fatalf("unsafe defaults: %+v", u)
			}
			want := "codex app-server --listen stdio://"
			if tc.name == "command" {
				want = "/custom/codex app-server --listen stdio://"
			}
			if strings.Join(u.Command, " ") != want {
				t.Fatalf("command = %q", u.Command)
			}
		})
	}
}
