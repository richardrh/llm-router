//go:build browser

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserJourneys(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Fatal("browser lane requires Node/npm: ", err)
	}
	if _, err := os.Stat(filepath.Join(repositoryRoot, "tests/browser/node_modules/@playwright/test/package.json")); err != nil {
		t.Fatal("run npm ci in tests/browser and npm exec playwright install chromium: ", err)
	}
	fixture := browserNewFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command, _ := json.Marshal([]string{executable, "-test.run=^TestBrowserProtocolFixture$", "--", "codex", fixture.server.URL, fixture.token})
	cliCommand, _ := json.Marshal([]string{executable, "-test.run=^TestBrowserProtocolFixture$", "--", "claude", fixture.server.URL, fixture.token})
	config := func(secure bool) string {
		return fmt.Sprintf(`apiKey: %s
ui: {enabled: true, secureCookies: %t}
store: {path: %q, maxRows: 0}
defaults: {maxAttempts: 1, streamIdleTimeout: 30s, maxStreamDuration: 2m}
upstreams:
  fixture: {baseUrl: %q, apiKey: browser-provider-key, maxConcurrency: 8}
  codex: {kind: codex, command: %s, maxConcurrency: 4}
  cli: {kind: cli, mode: persistent, toolMode: client, command: %s, maxConcurrency: 4}
models:
  chat: {api: openai-completions, targets: [{upstream: fixture, model: chat, weight: 1}]}
  messages: {api: anthropic-messages, targets: [{upstream: fixture, model: messages, weight: 1}]}
  responses: {api: openai-responses, targets: [{upstream: fixture, model: responses, weight: 1}]}
  agent: {api: openai-completions, targets: [{upstream: cli, model: agent, weight: 1}]}
`, gatewayKey, secure, filepath.Join(t.TempDir(), "usage.db"), fixture.server.URL, command, cliCommand)
	}
	plain := startRouter(t, config(false), routerOptions{Env: map[string]string{"ROUTER_E2E_HELPER": "1"}})
	secure := startRouter(t, config(true), routerOptions{Env: map[string]string{"ROUTER_E2E_HELPER": "1"}})
	target, _ := url.Parse(secure.URL)
	proxy := httputil.NewSingleHostReverseProxy(target) // Director preserves the browser's Host; no forwarded TLS trust.
	tls := httptest.NewTLSServer(proxy)
	defer tls.Close()
	attacker := strings.Replace(fixture.server.URL, "127.0.0.1", "localhost", 1)
	network := browserNetworkProxy(t, plain.URL, tls.URL, fixture.server.URL, attacker)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "exec", "--no", "--", "playwright", "test")
	cmd.Dir = filepath.Join(repositoryRoot, "tests/browser")
	cmd.Env = append(os.Environ(), "BROWSER_BASE_URL="+plain.URL, "BROWSER_HTTPS_URL="+tls.URL, "BROWSER_FIXTURE_URL="+fixture.server.URL, "BROWSER_FIXTURE_TOKEN="+fixture.token, "BROWSER_GATEWAY_KEY="+gatewayKey, "BROWSER_NETWORK_PROXY="+network.URL, "BROWSER_ATTACKER_URL="+attacker)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser journeys: %v\n%s\nHTTP router:\n%s\nHTTPS router:\n%s", err, output, plain.Logs(), secure.Logs())
	}
	t.Log(string(output))
}
