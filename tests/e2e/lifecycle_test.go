package e2e

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

func lifecycleField(t *testing.T, body, name string) string {
	t.Helper()
	match := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `"[^>]*value="([^"]*)"`).FindStringSubmatch(body)
	if len(match) != 2 || match[1] == "" {
		t.Fatalf("missing form field %s", name)
	}
	return html.UnescapeString(match[1])
}
func lifecycleClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}
func lifecycleRequest(t *testing.T, client *http.Client, method, endpoint string, form url.Values) (int, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		origin, err := url.Parse(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", origin.Scheme+"://"+origin.Host)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}
func lifecycleLogin(t *testing.T, p *routerProcess) *http.Client {
	t.Helper()
	client := lifecycleClient(t)
	status, body := lifecycleRequest(t, client, "GET", p.URL+"/ui/login", nil)
	if status != 200 {
		t.Fatalf("login form: %d %s", status, body)
	}
	status, body = lifecycleRequest(t, client, "POST", p.URL+"/ui/login", url.Values{"csrf": {lifecycleField(t, body, "csrf")}, "key": {gatewayKey}})
	if status != http.StatusSeeOther {
		t.Fatalf("login: %d %s", status, body)
	}
	status, body = lifecycleRequest(t, client, "GET", p.URL+"/ui/api/status", nil)
	if status != 200 {
		t.Fatalf("authenticated status: %d %s", status, body)
	}
	return client
}

// R1: both operator signals cancel owned console inference and invalidate sessions.
// The fixture remains blocked until router cancellation; no timer releases it.
func TestLifecycleSignalsCancelConsoleAndInvalidateLogin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal os.Signal
	}{{"SIGTERM", syscall.SIGTERM}, {"SIGINT", os.Interrupt}} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			canceled := make(chan struct{})
			release := make(chan struct{})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"held\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"started\"}}]}\n\n")
				w.(http.Flusher).Flush()
				close(entered)
				select {
				case <-r.Context().Done():
					close(canceled)
				case <-release:
				}
			}))
			// In a failing test release the fixture before closing its server.
			defer provider.Close()
			defer close(release)
			config := accountingConfig(provider.URL, filepath.Join(t.TempDir(), "usage.db"), 0)
			p := startRouter(t, config)
			client := lifecycleLogin(t, p)
			status, page := lifecycleRequest(t, client, "GET", p.URL+"/ui/playground", nil)
			if status != 200 {
				t.Fatalf("playground: %d %s", status, page)
			}
			oldURL, err := url.Parse(p.URL)
			if err != nil {
				t.Fatal(err)
			}
			oldURL.Path = "/ui/"
			cookies := client.Jar.Cookies(oldURL)
			if len(cookies) == 0 {
				t.Fatal("login did not set real session cookie")
			}
			status, page = lifecycleRequest(t, client, "POST", p.URL+"/ui/playground/send", url.Values{
				"csrf":            {lifecycleField(t, page, "csrf")},
				"conversation_id": {lifecycleField(t, page, "conversation_id")},
				"alias":           {"measured"}, "prompt": {"hold this request until shutdown"},
			})
			if status != 200 {
				t.Fatalf("send: %d %s", status, page)
			}
			awaitSignal(t, entered, "console inference reached provider")
			p.Stop(t, tc.signal)
			awaitSignal(t, canceled, "shutdown canceled provider HTTP context")
			restarted := startRouter(t, config)
			// Explicitly replay the authentic old cookie even if the restart chose a new port.
			req, err := http.NewRequest("GET", restarted.URL+"/ui/api/status", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, cookie := range cookies {
				req.AddCookie(cookie)
			}
			bare := lifecycleClient(t)
			response, err := bare.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/login" {
				t.Fatalf("old session survived restart: status=%d location=%q", response.StatusCode, response.Header.Get("Location"))
			}
			// A new login must still work; rejection cannot just mean a broken console.
			lifecycleLogin(t, restarted)
		})
	}
}

func TestLifecycleSIGINTFlushesCompletedHTTPAccounting(t *testing.T) {
	provider := accountingProvider(t)
	config := accountingConfig(provider.URL, filepath.Join(t.TempDir(), "usage.db"), 0)
	p := startRouter(t, config)
	accountingInfer(t, p, "measured")
	p.Stop(t, os.Interrupt)
	restarted := startRouter(t, config)
	report := accountingGet(t, restarted, "")
	if report.Totals.Requests != 1 || len(report.Requests) != 1 || report.Dropped != 0 {
		t.Fatalf("SIGINT lost accepted completed row: %+v", report)
	}
	accountingCost(t, report.Totals.Cost, .000496)
}

// A direct HTTP stream can outlive the graceful shutdown deadline. Closing its
// socket must cancel and join the handler before the usage store is flushed.
func TestLifecycleHTTPDrainTimeoutPersistsFinalUsage(t *testing.T) {
	const event = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\n"
	canceled := make(chan struct{})
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, event)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer provider.Close()
	defer close(release)
	config := "defaults: {maxStreamDuration: 1m, streamIdleTimeout: 1m}\n" +
		accountingConfig(provider.URL, filepath.Join(t.TempDir(), "usage.db"), 0)
	p := startRouter(t, config)
	req, err := http.NewRequest(http.MethodPost, p.URL+"/v1/chat/completions", strings.NewReader(`{"model":"measured","stream":true,"messages":[{"role":"user","content":"hold past shutdown deadline"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 40 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, len(event))
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(first) != event {
		t.Fatalf("stream was not committed: status=%d body=%q", response.StatusCode, first)
	}
	// A graceful-drain deadline remains a reported error, not a false success.
	p.stopWithExit(t, syscall.SIGTERM, 1)
	awaitSignal(t, canceled, "drain timeout canceled the provider")
	response.Body.Close()
	restarted := startRouter(t, config)
	report := accountingGet(t, restarted, "")
	if report.Totals.Requests != 1 || len(report.Requests) != 1 || report.Dropped != 0 {
		t.Fatalf("shutdown lost the canceled handler's final accounting: %+v", report)
	}
	row := report.Requests[0]
	accountingNumber(t, "final uncached input", row.Input, 20)
	accountingNumber(t, "final output", row.Output, 20)
	accountingNumber(t, "final cache read", row.Read, 80)
	accountingCost(t, row.Cost, .000496)
}
