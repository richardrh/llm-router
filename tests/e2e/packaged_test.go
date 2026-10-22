//go:build packaged

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestPackagedRuntime exercises P1 through the shipped image, not a host binary.
// Host networking deliberately confines both provider and router to Linux loopback.
func TestPackagedRuntime(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatal("packaged smoke requires Linux host networking")
	}
	engine := os.Getenv("ROUTER_CONTAINER_RUNTIME")
	if engine == "" {
		engine = "docker"
	}
	if _, err := exec.LookPath(engine); err != nil {
		t.Fatalf("container runtime prerequisite: %v", err)
	}
	image := os.Getenv("ROUTER_CONTAINER_IMAGE")
	if image == "" {
		image = "llm-router:ci"
	}
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, engine, args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", engine, args, err, out)
		}
		return out
	}
	must("info")
	user := must("image", "inspect", "--format", "{{.Config.User}}", image)
	uid := strings.Split(user, ":")[0]
	if uid == "" || uid == "0" || uid == "root" {
		t.Fatalf("image must run nonroot, Config.User=%q", user)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer packaged-provider-key" {
			t.Errorf("unexpected provider request: %s %s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "fixture-model" {
			t.Errorf("provider model=%q decode=%v", body.Model, err)
			http.Error(w, "bad model", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"packaged-reply","object":"chat.completion","model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"packaged hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
	}))
	defer provider.Close()
	dir := t.TempDir()
	// The image's unprivileged UID must traverse the bind source and create WAL files.
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "store")
	if err := os.Mkdir(store, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store, 0777); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "router.yaml")
	yaml := fmt.Sprintf(`listen: 127.0.0.1:0
apiKey: gateway-e2e-key
store:
  path: /data/usage.db
upstreams:
  fixture:
    baseUrl: %s
    apiKey: packaged-provider-key
models:
  packaged:
    api: openai-completions
    targets:
      - upstream: fixture
        model: fixture-model
        weight: 1
`, provider.URL)
	if err := os.WriteFile(config, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("router-packaged-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		logs, _ := run("logs", name)
		if t.Failed() {
			t.Logf("packaged router logs:\n%s", logs)
		}
		out, err := run("rm", "--force", name)
		if err != nil {
			t.Errorf("remove test container: %v: %s", err, out)
		}
	})
	must("run", "--detach", "--name", name, "--network", "host", "--volume", config+":/etc/llm-router/router.yaml:ro,Z", "--volume", store+":/data:Z", image, "-config", "/etc/llm-router/router.yaml")
	addrPattern := regexp.MustCompile(`addr[=:"]+\s*"?(127\.0\.0\.1:[0-9]+)`)
	ready := func() string {
		t.Helper()
		var url string
		eventually(t, "packaged listener readiness", func() bool {
			logs, err := run("logs", name)
			if err != nil {
				return false
			}
			match := addrPattern.FindStringSubmatch(logs)
			if len(match) != 2 {
				return false
			}
			url = "http://" + match[1]
			client := &http.Client{Timeout: time.Second}
			resp, err := client.Get(url + "/healthz")
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		})
		return url
	}
	url := ready()
	result := request(t, http.MethodPost, url+"/v1/chat/completions", `{"model":"packaged","messages":[{"role":"user","content":"hello"}]}`, map[string]string{"Authorization": "Bearer " + gatewayKey})
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(result.Body, &completion); err != nil {
		t.Fatalf("decode completion: %v: %s", err, result.Body)
	}
	if result.Status != http.StatusOK || len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "packaged hello" {
		t.Fatalf("packaged completion: status=%d body=%s", result.Status, result.Body)
	}
	stop := func() {
		t.Helper()
		must("stop", "--time", "15", name)
		if state := must("inspect", "--format", "{{.State.ExitCode}} {{.State.OOMKilled}}", name); state != "0 false" {
			t.Fatalf("container shutdown state=%q", state)
		}
	}
	stop()
	if info, err := os.Stat(filepath.Join(store, "usage.db")); err != nil || info.Size() == 0 {
		t.Fatalf("mounted SQLite persistence: info=%v err=%v", info, err)
	}
	must("start", name)
	// Restart uses a new ephemeral port; ignore the previous startup line.
	// Docker logs contain both starts, so take the final advertised listener.
	var restartedURL string
	eventually(t, "restarted packaged listener", func() bool {
		logs, err := run("logs", name)
		if err != nil {
			return false
		}
		matches := addrPattern.FindAllStringSubmatch(logs, -1)
		if len(matches) < 2 {
			return false
		}
		restartedURL = "http://" + matches[len(matches)-1][1]
		client := &http.Client{Timeout: time.Second}
		resp, err := client.Get(restartedURL + "/healthz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	report := request(t, http.MethodGet, restartedURL+"/usage", "", map[string]string{"Authorization": "Bearer " + gatewayKey})
	var usage struct {
		Totals struct {
			Requests int `json:"requests"`
			Input    int `json:"input_tokens"`
			Output   int `json:"output_tokens"`
		} `json:"totals"`
		Requests []struct {
			Alias string `json:"alias"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(report.Body, &usage); err != nil {
		t.Fatalf("decode usage: %v: %s", err, report.Body)
	}
	if report.Status != http.StatusOK || usage.Totals.Requests != 1 || usage.Totals.Input != 11 || usage.Totals.Output != 7 || len(usage.Requests) != 1 || usage.Requests[0].Alias != "packaged" {
		t.Fatalf("persisted packaged report: status=%d body=%s", report.Status, report.Body)
	}
	stop()
}
