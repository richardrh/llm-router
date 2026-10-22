package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const gatewayKey = "gateway-e2e-key"

var repositoryRoot string
var suiteRouterBinary string

// The helper subprocesses speak provider protocols. They must not recursively
// build the router or initialize the suite before reading their stdin.
func TestMain(m *testing.M) {
	if os.Getenv("ROUTER_E2E_HELPER") == "1" {
		os.Exit(m.Run())
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	repositoryRoot = filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(repositoryRoot, "go.mod")); err != nil {
		fmt.Fprintln(os.Stderr, "cannot locate repository root:", err)
		os.Exit(1)
	}

	var buildDir string
	if binary := os.Getenv("ROUTER_E2E_BINARY"); binary != "" {
		if !filepath.IsAbs(binary) {
			fmt.Fprintln(os.Stderr, "ROUTER_E2E_BINARY must be an absolute path")
			os.Exit(1)
		}
		info, err := os.Stat(binary)
		if err != nil || info.IsDir() {
			fmt.Fprintln(os.Stderr, "invalid ROUTER_E2E_BINARY:", binary, err)
			os.Exit(1)
		}
		suiteRouterBinary = binary
	} else {
		buildDir, err = os.MkdirTemp("", "llm-router-e2e-build-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		suiteRouterBinary = filepath.Join(buildDir, "llm-router")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		cmd := exec.CommandContext(ctx, "go", "build", "-o", suiteRouterBinary, "./cmd/llm-router")
		cmd.Dir = repositoryRoot
		output, buildErr := cmd.CombinedOutput()
		cancel()
		if buildErr != nil {
			fmt.Fprintf(os.Stderr, "build router: %v\n%s", buildErr, output)
			_ = os.RemoveAll(buildDir)
			os.Exit(1)
		}
	}
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func routerBinary(t *testing.T) string {
	t.Helper()
	if suiteRouterBinary == "" {
		t.Fatal("router binary is not initialized")
	}
	return suiteRouterBinary
}

type routerOptions struct {
	Env  map[string]string
	Args []string
	Dir  string
}

type routerProcess struct {
	URL        string
	ConfigPath string
	Cmd        *exec.Cmd

	logs     *processLog
	done     chan struct{}
	waitErr  error // published by closing done
	stopOnce sync.Once
}

var boundAddress = regexp.MustCompile(`\baddr="?(127\.0\.0\.1:[0-9]+)`)

type processLog struct {
	mu    sync.Mutex
	data  []byte
	ready chan string
	found bool
}

func (l *processLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.data = append(l.data, p...)
	const maxLog = 256 << 10
	if len(l.data) > maxLog {
		copy(l.data, l.data[len(l.data)-maxLog:])
		l.data = l.data[:maxLog]
	}
	if !l.found && l.ready != nil {
		// An os/exec pipe read can end halfway through the port number.
		// Only complete log lines can announce a usable address.
		if end := bytes.LastIndexByte(l.data, '\n'); end >= 0 {
			if match := boundAddress.FindSubmatch(l.data[:end]); match != nil {
				l.found = true
				l.ready <- string(match[1])
			}
		}
	}
	return len(p), nil
}

func (l *processLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.ReplaceAll(string(l.data), gatewayKey, "[gateway-key]")
}

func isolatedEnv(t *testing.T, sandbox string, extra map[string]string) []string {
	t.Helper()
	values := map[string]string{
		"PATH":            os.Getenv("PATH"),
		"HOME":            filepath.Join(sandbox, "home"),
		"CODEX_HOME":      filepath.Join(sandbox, "codex"),
		"XDG_CONFIG_HOME": filepath.Join(sandbox, "config"),
		"XDG_CACHE_HOME":  filepath.Join(sandbox, "cache"),
		"TMPDIR":          filepath.Join(sandbox, "tmp"),
		"LANG":            "C.UTF-8",
		"TZ":              "UTC",
	}
	for _, key := range []string{"HOME", "CODEX_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "TMPDIR"} {
		if err := os.MkdirAll(values[key], 0700); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range extra {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func writeConfig(t *testing.T, yamlText string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router.yaml")
	if err := os.WriteFile(path, []byte(yamlText), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startRouter(t *testing.T, yamlText string, options ...routerOptions) *routerProcess {
	t.Helper()
	sandbox := t.TempDir()
	opt := routerOptions{Dir: sandbox, Env: map[string]string{}}
	for _, supplied := range options {
		if supplied.Dir != "" {
			opt.Dir = supplied.Dir
		}
		opt.Args = append(opt.Args, supplied.Args...)
		for key, value := range supplied.Env {
			opt.Env[key] = value
		}
	}
	path := writeConfig(t, yamlText)
	args := append([]string{"-config", path, "-listen", "127.0.0.1:0"}, opt.Args...)
	cmd := exec.Command(routerBinary(t), args...)
	cmd.Dir = opt.Dir
	cmd.Env = isolatedEnv(t, sandbox, opt.Env)
	// On Linux/macOS every descendant belongs to this test-owned process group.
	// Escalation during failed-test cleanup cannot kill unrelated user processes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logs := &processLog{ready: make(chan string, 1)}
	cmd.Stdout, cmd.Stderr = logs, logs
	p := &routerProcess{ConfigPath: path, Cmd: cmd, logs: logs, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start router: %v", err)
	}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.Stop(t, syscall.SIGTERM)
		// A clean parent exit is not permission to leave fixture descendants alive.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if t.Failed() {
			t.Logf("router diagnostics:\n%s", p.Logs())
		}
	})
	select {
	case addr := <-logs.ready:
		p.URL = "http://" + addr
	case <-p.done:
		t.Fatalf("router exited before listening: %v\n%s", p.waitErr, p.Logs())
	case <-time.After(15 * time.Second):
		t.Fatalf("router did not report a bound listener\n%s", p.Logs())
	}
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	eventually(t, "router health readiness", func() bool {
		response, err := client.Get(p.URL + "/healthz")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		var health struct {
			Status string `json:"status"`
		}
		return response.StatusCode == http.StatusOK && json.NewDecoder(response.Body).Decode(&health) == nil && health.Status == "ok"
	})
	return p
}

func (p *routerProcess) Logs() string { return p.logs.String() }

func (p *routerProcess) Stop(t *testing.T, signal os.Signal) {
	t.Helper()
	p.stopWithExit(t, signal, 0)
}

func (p *routerProcess) stopWithExit(t *testing.T, signal os.Signal, expectedExit int) {
	t.Helper()
	p.stopOnce.Do(func() {
		select {
		case <-p.done:
		default:
			if err := p.Cmd.Process.Signal(signal); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("signal router: %v", err)
			}
		}
		select {
		case <-p.done:
			if p.Cmd.ProcessState.ExitCode() != expectedExit {
				t.Errorf("router exit after %v: %v (want exit %d)\n%s", signal, p.waitErr, expectedExit, p.Logs())
			}
		case <-time.After(25 * time.Second):
			_ = syscall.Kill(-p.Cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
			}
			t.Errorf("router did not exit after %v\n%s", signal, p.Logs())
		}
	})
}

type commandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

func runRouter(t *testing.T, args []string, dir string, env map[string]string) commandResult {
	t.Helper()
	sandbox := t.TempDir()
	if dir == "" {
		dir = sandbox
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, routerBinary(t), args...)
	cmd.Dir = dir
	cmd.Env = isolatedEnv(t, sandbox, env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("router command exceeded deadline: %v\n%s", args, stderr.String())
	}
	result := commandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("execute router: %v", err)
		}
		result.ExitCode = exit.ExitCode()
	}
	return result
}

type httpResult struct {
	Status int
	Header http.Header
	Body   []byte
}

func request(t *testing.T, method, url, body string, headers map[string]string) httpResult {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return httpResult{Status: response.StatusCode, Header: response.Header.Clone(), Body: raw}
}

func decodeJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, body)
	}
	return value
}

func eventually(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("deadline waiting for %s", description)
		case <-tick.C:
		}
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("deadline waiting for %s", description)
	}
}
