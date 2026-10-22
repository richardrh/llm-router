//go:build browser

package e2e

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real forward proxy denies all destinations except the two owned console
// listeners, including HTTPS CONNECT. No browser request interception is used.
func browserNetworkProxy(t *testing.T, origins ...string) *httptest.Server {
	allowed := map[string]bool{}
	for _, origin := range origins {
		u, err := url.Parse(origin)
		if err != nil {
			t.Fatal(err)
		}
		allowed[u.Host] = true
	}
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Host
		if r.Method == http.MethodConnect {
			host = r.Host
		}
		if !allowed[host] {
			http.Error(w, "browser network allowlist denied", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodConnect {
			upstream, err := net.DialTimeout("tcp", host, 3*time.Second)
			if err != nil {
				http.Error(w, "connect failed", 502)
				return
			}
			downstream, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				upstream.Close()
				return
			}
			fmt.Fprint(buffer, "HTTP/1.1 200 Connection Established\r\n\r\n")
			buffer.Flush()
			go func() { defer upstream.Close(); defer downstream.Close(); io.Copy(upstream, buffer) }()
			go func() { defer upstream.Close(); defer downstream.Close(); io.Copy(downstream, upstream) }()
			return
		}
		outgoing := r.Clone(r.Context())
		outgoing.RequestURI = ""
		response, err := transport.RoundTrip(outgoing)
		if err != nil {
			http.Error(w, "forward failed", 502)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		buffer := make([]byte, 4096)
		for {
			n, err := response.Body.Read(buffer)
			if n > 0 {
				w.Write(buffer[:n])
				w.(http.Flusher).Flush()
			}
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

type browserObservation struct {
	ID        int            `json:"id"`
	Body      map[string]any `json:"body"`
	Cancelled bool           `json:"cancelled"`
	Released  bool           `json:"released"`
}
type browserGate struct {
	observation browserObservation
	release     chan struct{}
	fail        bool
}
type browserFixture struct {
	mu           sync.Mutex
	server       *httptest.Server
	token        string
	gates        []*browserGate
	methods      []string
	signedIn     bool
	notification int
}

func browserNewFixture(t *testing.T) *browserFixture {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	f := &browserFixture{token: hex.EncodeToString(b), methods: []string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, g := range f.gates {
			if !g.observation.Released {
				close(g.release)
				g.observation.Released = true
			}
		}
	})
	return f
}
func (f *browserFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/attacker" {
		target, err := url.Parse(r.URL.Query().Get("target"))
		if err != nil || target.Scheme != "http" || target.Hostname() != "127.0.0.1" {
			http.Error(w, "invalid test target", 400)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><title>Attacker fixture</title><form method="post" action="%s/ui/logout"><input name="csrf" value="forged"><button type="submit">Attempt cross-origin logout</button></form>`, target.String())
		return
	}
	if strings.HasPrefix(r.URL.Path, "/control/") {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "fixture auth", 401)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/control/state":
			rows := make([]browserObservation, len(f.gates))
			for i, g := range f.gates {
				rows[i] = g.observation
			}
			json.NewEncoder(w).Encode(map[string]any{"requests": rows, "methods": f.methods, "signedIn": f.signedIn, "notification": f.notification})
		case "/control/release", "/control/fail":
			var p struct{ ID int }
			if json.NewDecoder(r.Body).Decode(&p) != nil || p.ID < 0 || p.ID >= len(f.gates) {
				http.Error(w, "invalid gate", 400)
				return
			}
			g := f.gates[p.ID]
			if !g.observation.Released {
				g.fail = r.URL.Path == "/control/fail"
				g.observation.Released = true
				close(g.release)
			}
		case "/control/rpc":
			var p struct{ Method string }
			json.NewDecoder(r.Body).Decode(&p)
			f.methods = append(f.methods, p.Method)
			if p.Method == "account/logout" {
				f.signedIn = false
			}
			json.NewEncoder(w).Encode(map[string]any{"signedIn": f.signedIn, "notification": f.notification})
		case "/control/notify":
			f.signedIn = true
			f.notification++
		default:
			http.NotFound(w, r)
		}
		return
	}
	if r.Method != "POST" || (r.URL.Path != "/chat/completions" && r.URL.Path != "/messages" && r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/messages") {
		http.Error(w, "unexpected provider request", 400)
		return
	}
	if r.Header.Get("Authorization") != "Bearer browser-provider-key" {
		http.Error(w, "provider credential mismatch", 401)
		return
	}
	var body map[string]any
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body) != nil {
		http.Error(w, "invalid provider JSON", 400)
		return
	}
	f.mu.Lock()
	g := &browserGate{observation: browserObservation{ID: len(f.gates), Body: body}, release: make(chan struct{})}
	f.gates = append(f.gates, g)
	f.mu.Unlock()
	if body["stream"] != true {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"browser","object":"chat.completion","model":"chat","choices":[{"index":0,"message":{"role":"assistant","content":"usage answer"},"finish_reason":"stop"}]}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	text := fmt.Sprintf("partial-%d <script>window.browserExecuted=true</script>", g.observation.ID)
	anthropic := strings.HasSuffix(r.URL.Path, "/messages")
	event := func(name string, v any) {
		data, _ := json.Marshal(v)
		if name != "" {
			fmt.Fprintf(w, "event: %s\n", name)
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}
	if anthropic {
		event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "browser", "type": "message", "role": "assistant", "model": "messages", "content": []any{}, "usage": map[string]int{"input_tokens": 3, "output_tokens": 0}}})
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	} else {
		event("", map[string]any{"id": "browser", "object": "chat.completion.chunk", "model": "chat", "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant", "content": text}}}})
	}
	select {
	case <-r.Context().Done():
		f.mu.Lock()
		g.observation.Cancelled = true
		f.mu.Unlock()
		return
	case <-g.release:
	case <-time.After(60 * time.Second):
		return
	}
	if g.fail {
		return
	}
	if anthropic {
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 5}})
		event("message_stop", map[string]string{"type": "message_stop"})
	} else {
		event("", map[string]any{"id": "browser", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
		io.WriteString(w, "data: [DONE]\n\n")
	}
}

// This process replaces only external executables. It never touches router state.
func TestBrowserProtocolFixture(t *testing.T) {
	if os.Getenv("ROUTER_E2E_HELPER") != "1" {
		return
	}
	pos := -1
	for i, v := range os.Args {
		if v == "--" {
			pos = i
			break
		}
	}
	if pos < 0 || len(os.Args) < pos+4 {
		os.Exit(2)
	}
	mode, base, token := os.Args[pos+1], os.Args[pos+2], os.Args[pos+3]
	client := &http.Client{Timeout: 3 * time.Second}
	state := func(method string) map[string]any {
		req, _ := http.NewRequest("POST", base+"/control/rpc", strings.NewReader(fmt.Sprintf(`{"Method":%q}`, method)))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			os.Exit(3)
		}
		defer resp.Body.Close()
		var v map[string]any
		json.NewDecoder(resp.Body).Decode(&v)
		return v
	}
	send := func(v any) { data, _ := json.Marshal(v); fmt.Println(string(data)) }
	if mode == "claude" {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var incoming struct {
				Type    string
				Message struct {
					Role    string
					Content string
				}
			}
			if json.Unmarshal(scanner.Bytes(), &incoming) != nil || incoming.Type != "user" || incoming.Message.Role != "user" || incoming.Message.Content == "" {
				os.Exit(5)
			}
			state("cli/run")
			envelope := `{"kind":"client_tool_request","id":"browser-tool","name":"never_execute_browser_tool","arguments":{"command":"touch forbidden"}}`
			send(map[string]any{"type": "result", "session_id": "browser-cli-session", "subtype": "success", "is_error": false, "result": envelope})
		}
		os.Exit(0)
	}
	var out sync.Mutex
	emit := func(v any) { out.Lock(); defer out.Unlock(); send(v) }
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		last := float64(0)
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				s := state("fixture/poll")
				n, _ := s["notification"].(float64)
				if n > last {
					last = n
					emit(map[string]any{"method": "account/login/completed", "params": map[string]any{"success": true}})
				}
			}
		}
	}()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage
			Method string
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(4)
		}
		if req.Method == "initialized" {
			continue
		}
		s := state(req.Method)
		result := any(map[string]any{})
		switch req.Method {
		case "initialize":
		case "account/read":
			if s["signedIn"] == true {
				result = map[string]any{"account": map[string]string{"type": "chatgpt", "planType": "plus"}}
			} else {
				result = map[string]any{"account": nil}
			}
		case "model/list":
			result = map[string]any{"data": []any{map[string]any{"id": "browser-model", "model": "browser-model", "displayName": "Browser fixture model", "isDefault": true}}, "nextCursor": nil}
		case "account/login/start":
			result = map[string]string{"type": "chatgpt", "loginId": "browser-login", "authUrl": "https://auth.openai.com/browser-fixture"}
		case "account/login/cancel", "account/logout":
		default:
			emit(map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": "unexpected fixture RPC"}})
			continue
		}
		emit(map[string]any{"id": req.ID, "result": result})
	}
	os.Exit(0)
}
