package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// configFixture preserves manually authored compatibility YAML; substitutions
// are restricted to test resources, never schema/default values.
func configFixture(t *testing.T, name, url, command, store string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot, "tests/e2e/testdata/config", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer("{{URL}}", url, "{{COMMAND}}", filepath.ToSlash(command), "{{STORE}}", filepath.ToSlash(store)).Replace(string(data))
}

func configCheck(t *testing.T, text string, env map[string]string) commandResult {
	t.Helper()
	return runRouter(t, []string{"-config", writeConfig(t, text), "-check"}, t.TempDir(), env)
}

func configOutput(r commandResult) string { return r.Stdout + "\n" + r.Stderr }

// C1: validate independently authored compatibility inputs, not serialized structs.
func TestConfigCompatibilityFixturesCheck(t *testing.T) {
	for _, name := range []string{"http", "anthropic", "translation", "responses", "one-shot", "persistent", "codex", "ui", "store"} {
		t.Run(name, func(t *testing.T) {
			text := configFixture(t, name, "http://127.0.0.1:1", filepath.Join(t.TempDir(), "must-not-run"), filepath.Join(t.TempDir(), "usage.db"))
			r := configCheck(t, text, nil)
			if r.ExitCode != 0 {
				t.Fatalf("compatible config rejected: %s", configOutput(r))
			}
			if !strings.Contains(configOutput(r), "private-model") || !strings.Contains(configOutput(r), "fixture") {
				t.Fatalf("check omitted route identity: %s", configOutput(r))
			}
		})
	}
}

// C1/C5: omitted kind/model/protocol and configured discovery limits must work
// through the executable, including real Anthropic auth and translation.
func TestConfigCompatibilityFixturesServe(t *testing.T) {
	for _, name := range []string{"http", "anthropic", "translation", "responses", "ui", "store"} {
		t.Run(name, func(t *testing.T) {
			type observed struct{ path, auth, key, version, model string }
			seen := make(chan observed, 1)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				model, _ := body["model"].(string)
				seen <- observed{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), model}
				w.Header().Set("Content-Type", "application/json")
				switch name {
				case "anthropic", "translation":
					io.WriteString(w, `{"id":"msg_config","type":"message","role":"assistant","model":"private-native-model","content":[{"type":"text","text":"config reply"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`)
				case "responses":
					io.WriteString(w, `{"id":"resp_config","object":"response","status":"completed","model":"private-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"config reply"}]}]}`)
				default:
					io.WriteString(w, `{"id":"chat_config","object":"chat.completion","model":"private-model","choices":[{"index":0,"message":{"role":"assistant","content":"config reply"},"finish_reason":"stop"}]}`)
				}
			}))
			defer provider.Close()
			p := startRouter(t, configFixture(t, name, provider.URL, "", filepath.Join(t.TempDir(), "usage.db")), routerOptions{Env: map[string]string{"CONFIG_TEST_KEY": "config-env-secret"}})
			path, body := "/v1/chat/completions", `{"model":"private-model","messages":[{"role":"user","content":"hello"}]}`
			if name == "anthropic" {
				path = "/v1/messages"
				body = `{"model":"private-model","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`
			}
			if name == "responses" {
				path = "/v1/responses"
				body = `{"model":"private-model","input":"hello"}`
			}
			res := request(t, "POST", p.URL+path, body, map[string]string{"Authorization": "Bearer " + gatewayKey})
			if res.Status != 200 || !strings.Contains(string(res.Body), "config reply") {
				t.Fatalf("fixture did not serve: %d %s", res.Status, res.Body)
			}
			var got observed
			select {
			case got = <-seen:
			default:
				t.Fatal("provider never received request")
			}
			wantPath, wantModel := path, "private-model"
			if name == "translation" {
				wantPath, wantModel = "/v1/messages", "private-native-model"
			}
			if got.path != wantPath || got.model != wantModel {
				t.Errorf("dispatch = %+v, want path %s model %s", got, wantPath, wantModel)
			}
			if name == "anthropic" || name == "translation" {
				if got.key != "config-literal-secret" || got.version != "2023-06-01" {
					t.Errorf("Anthropic auth/version = %+v", got)
				}
			}
			if name == "http" {
				if got.auth != "Bearer config-literal-secret" {
					t.Errorf("HTTP auth = %q", got.auth)
				}
				models := request(t, "GET", p.URL+"/v1/models", "", map[string]string{"Authorization": "Bearer " + gatewayKey})
				var list struct {
					Data []struct {
						ID      string `json:"id"`
						Context int    `json:"context_length"`
						Limits  struct {
							Output int `json:"max_output_tokens"`
						} `json:"limits"`
					} `json:"data"`
				}
				if err := json.Unmarshal(models.Body, &list); err != nil {
					t.Fatal(err)
				}
				if models.Status != 200 || len(list.Data) != 1 || list.Data[0].ID != "private-model" || list.Data[0].Context != 123456 || list.Data[0].Limits.Output != 4321 {
					t.Fatalf("configured discovery lost: %s", models.Body)
				}
			}
		})
	}
}

// C2/C5: diagnostics identify the erroneous setting, without prose snapshots.
func TestConfigInvalidSettings(t *testing.T) {
	base := configFixture(t, "http", "http://127.0.0.1:1", "", "")
	for _, tc := range []struct{ name, old, replacement, diagnostic string }{
		{"unknown root", "listen:", "unknownSetting: true\nlisten:", "unknownSetting"},
		{"unknown upstream", "    apiKey: config-literal-secret", "    apiKey: config-literal-secret\n    typoSetting: true", "typoSetting"},
		{"unknown alias", "    contextWindow:", "    typoAlias: 1\n    contextWindow:", "typoAlias"},
		{"unknown target", "weight: 1", "weight: 1, typoTarget: true", "typoTarget"},
		{"duration", "upstreams:", "defaults: {firstByteTimeout: tomorrow}\nupstreams:", "duration"},
		{"reference", "upstream: fixture", "upstream: missing-upstream", "missing-upstream"},
		{"duplicate", "targets: [{upstream: fixture, weight: 1}]", "targets: [{upstream: fixture, weight: 1}, {upstream: fixture, weight: 1}]", "duplicate"},
		{"negative weight", "weight: 1", "weight: -1", "weight"},
		{"all zero", "weight: 1", "weight: 0", "weight"},
		{"negative attempts", "upstreams:", "defaults: {maxAttempts: -1}\nupstreams:", "maxAttempts"},
		{"upstream model patch", "    apiKey: config-literal-secret", "    bodyPatch: {model: forbidden}\n    apiKey: config-literal-secret", "model"},
		{"target model patch", "weight: 1", "weight: 1, bodyPatch: {model: forbidden}", "model"},
		{"credentials", "    apiKey: config-literal-secret", "    apiKey: config-literal-secret\n    apiKeyEnv: CONFIG_TEST_KEY", "apiKey"},
		{"UI key", "apiKey: gateway-e2e-key", "apiKey: '  '\nui: {enabled: true}", "apiKey"},
		{"negative queue", "upstreams:", "store: {queueSize: -1}\nupstreams:", "queueSize"},
		{"negative retention", "upstreams:", "store: {maxRows: -1}\nupstreams:", "maxRows"},
		{"forbidden adaptive", "upstream: fixture, weight: 1", "upstream: fixture, model: claude-haiku-4-5, weight: 1, bodyPatch: {thinking: {type: adaptive}}", "thinking"},
		{"forbidden effort", "upstream: fixture, weight: 1", "upstream: fixture, model: claude-haiku-4-5, weight: 1, bodyPatch: {output_config: {effort: high}}", "effort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(base, tc.old) {
				t.Fatalf("invalid test mutation %q", tc.old)
			}
			r := configCheck(t, strings.Replace(base, tc.old, tc.replacement, 1), nil)
			if r.ExitCode == 0 {
				t.Fatalf("invalid %s accepted: %s", tc.name, configOutput(r))
			}
			if !strings.Contains(strings.ToLower(configOutput(r)), strings.ToLower(tc.diagnostic)) {
				t.Fatalf("diagnostic omits %s: %s", tc.diagnostic, configOutput(r))
			}
		})
	}
}

func TestConfigInvalidAgentSettings(t *testing.T) {
	for _, tc := range []struct{ name, fixture, old, replacement, diagnostic string }{
		{"missing prompt", "one-shot", "'{prompt}'", "'hello'", "prompt"},
		{"duplicate prompt", "one-shot", "'{prompt}'", "'{prompt}{prompt}'", "prompt"},
		{"persistent prompt", "persistent", "'{model}'", "'{prompt}'", "prompt"},
		{"CLI literal key", "one-shot", "    kind: cli", "    kind: cli\n    apiKey: forbidden", "credentials"},
		{"CLI env key", "persistent", "    kind: cli", "    kind: cli\n    apiKeyEnv: CONFIG_TEST_KEY", "credentials"},
		{"Codex key", "codex", "    kind: codex", "    kind: codex\n    apiKey: forbidden", "credentials"},
		{"Codex headers", "codex", "    kind: codex", "    kind: codex\n    headers: {x-private: forbidden}", "headers"},
		{"Codex mode", "codex", "    kind: codex", "    kind: codex\n    mode: one-shot", "persistent"},
		{"Codex tools", "codex", "    kind: codex", "    kind: codex\n    toolMode: claude", "toolMode"},
		{"Codex placeholder", "codex", "{{COMMAND}}", "{model}", "placeholder"},
		{"Codex timeout", "codex", "timeout: 30s", "timeout: -1s", "timeouts"},
		{"Codex protocol", "codex", "api: openai-completions", "api: anthropic-messages", "openai-completions"},
		{"Codex target patch", "codex", "weight: 1", "weight: 1, bodyPatch: {temperature: 0}", "bodyPatch"},
		{"Codex upstream patch", "codex", "    kind: codex", "    kind: codex\n    bodyPatch: {temperature: 0}", "bodyPatch"},
		{"CLI base URL", "one-shot", "    kind: cli", "    kind: cli\n    baseUrl: http://127.0.0.1:1", "baseUrl"},
		{"CLI missing command", "one-shot", "    command: [\"{{COMMAND}}\", '{prompt}']", "    command: []", "command"},
		{"CLI bad mode", "one-shot", "    kind: cli", "    kind: cli\n    mode: invalid-mode", "mode"},
		{"Codex base URL", "codex", "    kind: codex", "    kind: codex\n    baseUrl: http://127.0.0.1:1", "baseUrl"},
		{"Codex env key", "codex", "    kind: codex", "    kind: codex\n    apiKeyEnv: CONFIG_TEST_KEY", "apiKeyEnv"},
		{"Codex auth style", "codex", "    kind: codex", "    kind: codex\n    authStyle: bearer", "authStyle"},
		{"Codex empty executable", "codex", "{{COMMAND}}", " ", "executable"},
		{"Codex prompt placeholder", "codex", "{{COMMAND}}", "{prompt}", "placeholder"},
		{"Codex concurrency", "codex", "maxConcurrency: 2", "maxConcurrency: -1", "concurrency"},
		{"Codex sessions", "codex", "maxSessions: 2", "maxSessions: -1", "sessions"},
		{"Codex upstream drop", "codex", "    kind: codex", "    kind: codex\n    bodyDrop: [temperature]", "bodyDrop"},
		{"Codex target drop", "codex", "weight: 1", "weight: 1, bodyDrop: [temperature]", "bodyDrop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Preserve the command token for the dedicated invalid placeholder case.
			text := configFixture(t, tc.fixture, "", "{{COMMAND}}", "")
			text = strings.Replace(text, tc.old, tc.replacement, 1)
			text = strings.ReplaceAll(text, "{{COMMAND}}", filepath.ToSlash(filepath.Join(t.TempDir(), "never-run")))
			r := configCheck(t, text, nil)
			if r.ExitCode == 0 || !strings.Contains(strings.ToLower(configOutput(r)), strings.ToLower(tc.diagnostic)) {
				t.Fatalf("invalid agent setting not diagnosed (%s): exit=%d %s", tc.diagnostic, r.ExitCode, configOutput(r))
			}
		})
	}
}

// C3: a listener deliberately remains occupied and an executable marker is
// available, so validation cannot accidentally start the runtime unnoticed.
func TestConfigCheckHasNoRuntimeSideEffects(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer provider.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dir := t.TempDir()
	marker, command, db := filepath.Join(dir, "command-ran"), filepath.Join(dir, "agent"), filepath.Join(dir, "new-parent", "usage.db")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n: > '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf(`listen: %s
apiKey: gateway-check-secret
store: {path: '%s'}
upstreams:
  http:
    baseUrl: %s/v1
    apiKey: provider-check-secret
  env:
    baseUrl: %s/v1
    apiKeyEnv: CONFIG_CHECK_ABSENT_KEY
  agent:
    kind: cli
    command: ['%s', '{prompt}']
  codex:
    kind: codex
    command: ['%s']
models:
  checked-route:
    api: openai-completions
    targets: [{upstream: http, weight: 1}, {upstream: env, weight: 0}, {upstream: agent, weight: 0}, {upstream: codex, weight: 0}]
`, ln.Addr(), filepath.ToSlash(db), provider.URL, provider.URL, filepath.ToSlash(command), filepath.ToSlash(command))
	r := configCheck(t, text, nil)
	if r.ExitCode != 0 {
		t.Fatalf("check started runtime or rejected absent environment: %s", configOutput(r))
	}
	out := configOutput(r)
	for _, token := range []string{"CONFIG_CHECK_ABSENT_KEY", "UNSET", "checked-route"} {
		if !strings.Contains(out, token) {
			t.Errorf("check omitted %s: %s", token, out)
		}
	}
	for _, secret := range []string{"gateway-check-secret", "provider-check-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("check leaked secret %q", secret)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("check contacted provider %d times", calls.Load())
	}
	for _, path := range []string{marker, filepath.Dir(db)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("check created runtime resource %s: %v", path, err)
		}
	}
}

func TestConfigCheckShippedWithoutCredentials(t *testing.T) {
	r := runRouter(t, []string{"-config", filepath.Join(repositoryRoot, "router.yaml"), "-check"}, t.TempDir(), nil)
	if r.ExitCode != 0 {
		t.Fatalf("shipped config must validate without live credentials: %s", configOutput(r))
	}
}

// C4: exercise flag/cwd/error behavior without reserving and releasing ports.
func TestConfigExecutableFlagsAndFailures(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			r := runRouter(t, []string{flag}, t.TempDir(), nil)
			if r.ExitCode != 0 {
				t.Fatalf("help exit %d: %s", r.ExitCode, configOutput(r))
			}
		})
	}
	t.Run("missing default", func(t *testing.T) {
		r := runRouter(t, nil, t.TempDir(), nil)
		if r.ExitCode == 0 || !strings.Contains(configOutput(r), "router.yaml") {
			t.Fatalf("missing config: %d %s", r.ExitCode, configOutput(r))
		}
	})
	t.Run("missing explicit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.yaml")
		r := runRouter(t, []string{"-config", path}, t.TempDir(), nil)
		if r.ExitCode == 0 || !strings.Contains(configOutput(r), "absent.yaml") {
			t.Fatalf("missing config: %d %s", r.ExitCode, configOutput(r))
		}
	})
	t.Run("default lookup", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "router.yaml"), []byte(configFixture(t, "http", "http://127.0.0.1:1", "", "")), 0600); err != nil {
			t.Fatal(err)
		}
		r := runRouter(t, []string{"-check"}, dir, nil)
		if r.ExitCode != 0 {
			t.Fatalf("default lookup failed: %s", configOutput(r))
		}
	})
	t.Run("listen override", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		text := strings.Replace(configFixture(t, "http", "http://127.0.0.1:1", "", ""), "listen: 127.0.0.1:0", "listen: "+ln.Addr().String(), 1)
		p := startRouter(t, text, routerOptions{Args: []string{"-listen", "127.0.0.1:0"}})
		if got := request(t, "GET", p.URL+"/healthz", "", nil); got.Status != 200 {
			t.Fatalf("override listener unhealthy: %d", got.Status)
		}
	})
	t.Run("occupied listener", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		text := strings.Replace(configFixture(t, "http", "http://127.0.0.1:1", "", ""), "listen: 127.0.0.1:0", "listen: "+ln.Addr().String(), 1)
		r := runRouter(t, []string{"-config", writeConfig(t, text)}, t.TempDir(), nil)
		if r.ExitCode == 0 || !strings.Contains(strings.ToLower(configOutput(r)), "listen") {
			t.Fatalf("occupied listener not diagnosed: %d %s", r.ExitCode, configOutput(r))
		}
	})
	t.Run("unusable store", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "file")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		text := configFixture(t, "store", "http://127.0.0.1:1", "", filepath.Join(blocker, "usage.db"))
		r := runRouter(t, []string{"-config", writeConfig(t, text), "-listen", "127.0.0.1:0"}, t.TempDir(), nil)
		if r.ExitCode == 0 || !strings.Contains(configOutput(r), "file") {
			t.Fatalf("unusable store not diagnosed: %d %s", r.ExitCode, configOutput(r))
		}
	})
}

// C5: a stale capability table must never prevent new/private models or
// configured metadata from being used. Only guaranteed-invalid controls fail.
func TestConfigMetadataDriftAccepted(t *testing.T) {
	text := configFixture(t, "http", "http://127.0.0.1:1", "", "")
	text = strings.Replace(text, "upstream: fixture, weight: 1", "upstream: fixture, model: claude-opus-5-5, weight: 1", 1)
	r := configCheck(t, text, nil)
	if r.ExitCode != 0 || !strings.Contains(configOutput(r), "contextWindow") || !strings.Contains(configOutput(r), "maxOutputTokens") {
		t.Fatalf("metadata drift must warn, not fail: %d %s", r.ExitCode, configOutput(r))
	}
}

func TestConfigOpenMapsNestedShapingAndExplicitFalse(t *testing.T) {
	seen := make(chan map[string]any, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("x-custom-schema-free") != "custom-value" {
			t.Errorf("open headers map lost: %v", r.Header)
		}
		seen <- body
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"shape\",\"object\":\"chat.completion.chunk\",\"model\":\"native-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	text := fmt.Sprintf(`apiKey: gateway-e2e-key
defaults:
  rewriteModel: false
  injectStreamUsage: false
  cacheAffinity: false
  sticky: true
store: {path: '', maxRows: 0}
upstreams:
  fixture:
    baseUrl: %s/v1
    headers: {x-custom-schema-free: custom-value}
    bodyPatch: {vendor_option: {source: upstream, stale: true}, upstream_only: kept}
models:
  private-model:
    api: openai-completions
    sticky: false
    targets:
      - upstream: fixture
        model: native-model
        weight: 1
        bodyDrop: [remove_me]
        bodyPatch: {vendor_option: {source: target, enabled: false}}
`, provider.URL)
	p := startRouter(t, text)
	res := request(t, "POST", p.URL+"/v1/chat/completions", `{"model":"private-model","stream":true,"remove_me":"gone","messages":[{"role":"user","content":"hello"}]}`, map[string]string{"Authorization": "Bearer " + gatewayKey})
	if res.Status != 200 || !strings.Contains(string(res.Body), `"model":"native-model"`) || strings.Contains(string(res.Body), `"model":"private-model"`) {
		t.Fatalf("rewriteModel false lost: %d %s", res.Status, res.Body)
	}
	var got map[string]any
	select {
	case got = <-seen:
	default:
		t.Fatal("provider never received shaped request")
	}
	if _, ok := got["stream_options"]; ok {
		t.Errorf("injectStreamUsage false ignored: %#v", got)
	}
	if _, ok := got["remove_me"]; ok {
		t.Errorf("target drop ignored: %#v", got)
	}
	if got["upstream_only"] != "kept" || got["model"] != "native-model" {
		t.Errorf("shaping lost upstream/model fields: %#v", got)
	}
	nested, ok := got["vendor_option"].(map[string]any)
	if !ok || nested["source"] != "target" || nested["enabled"] != false || len(nested) != 2 {
		t.Errorf("nested target patch must replace upstream value, preserving false: %#v", got)
	}
}

func TestConfigEnvironmentSecretNotPrinted(t *testing.T) {
	text := configFixture(t, "responses", "http://127.0.0.1:1", "", "")
	r := configCheck(t, text, map[string]string{"CONFIG_TEST_KEY": "environment-secret-not-for-logs"})
	if r.ExitCode != 0 {
		t.Fatalf("environment-key check failed: %s", configOutput(r))
	}
	if strings.Contains(configOutput(r), "environment-secret-not-for-logs") {
		t.Fatalf("check leaked environment credential: %s", configOutput(r))
	}
	if !strings.Contains(configOutput(r), "CONFIG_TEST_KEY") || strings.Contains(configOutput(r), "UNSET") {
		t.Fatalf("set environment key not reported correctly: %s", configOutput(r))
	}
}

func TestConfigDriftDiscoveryRetainsOperatorValues(t *testing.T) {
	text := configFixture(t, "http", "http://127.0.0.1:1", "", "")
	text = strings.Replace(text, "upstream: fixture, weight: 1", "upstream: fixture, model: claude-opus-5-5, weight: 1", 1)
	p := startRouter(t, text)
	res := request(t, "GET", p.URL+"/v1/models", "", map[string]string{"Authorization": "Bearer " + gatewayKey})
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			Context int    `json:"context_length"`
			Limits  struct {
				Output int `json:"max_output_tokens"`
			} `json:"limits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || len(out.Data) != 1 || out.Data[0].ID != "private-model" || out.Data[0].Context != 123456 || out.Data[0].Limits.Output != 4321 {
		t.Fatalf("table drift replaced configured discovery values: %d %s", res.Status, res.Body)
	}
}

func TestConfigUnknownModelPermitsProviderSpecificControls(t *testing.T) {
	text := configFixture(t, "http", "http://127.0.0.1:1", "", "")
	text = strings.Replace(text, "upstream: fixture, weight: 1", "upstream: fixture, model: private-next-generation-model, weight: 1, bodyPatch: {thinking: {type: adaptive}, output_config: {effort: max}, arbitrary_provider_key: {nested: [one, two], enabled: false}}", 1)
	r := configCheck(t, text, nil)
	if r.ExitCode != 0 {
		t.Fatalf("unknown model or open bodyPatch rejected: %s", configOutput(r))
	}
}

// An explicit zero remains the historical default sentinel. Verify useful
// failover rather than freezing the implementation's numeric default budget.
func TestConfigZeroAttemptsKeepsUsableDefault(t *testing.T) {
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"primary unavailable","type":"upstream_error"}}`)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"fallback","object":"chat.completion","model":"private-model","choices":[{"index":0,"message":{"role":"assistant","content":"backup reply"},"finish_reason":"stop"}]}`)
	}))
	defer backup.Close()
	text := fmt.Sprintf(`apiKey: gateway-e2e-key
defaults: {maxAttempts: 0}
upstreams:
  primary: {baseUrl: '%s/v1'}
  backup: {baseUrl: '%s/v1'}
models:
  private-model:
    api: openai-completions
    targets: [{upstream: primary, weight: 1}, {upstream: backup, weight: 0}]
`, primary.URL, backup.URL)
	p := startRouter(t, text)
	res := request(t, "POST", p.URL+"/v1/chat/completions", `{"model":"private-model","messages":[{"role":"user","content":"hello"}]}`, map[string]string{"Authorization": "Bearer " + gatewayKey})
	if res.Status != 200 || !strings.Contains(string(res.Body), "backup reply") {
		t.Fatalf("explicit-zero default did not permit failover: %d %s", res.Status, res.Body)
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 1 {
		t.Errorf("failover calls primary=%d backup=%d, want one each", primaryCalls.Load(), backupCalls.Load())
	}
}
