package webui

import (
	"crypto/sha256"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func consoleRequest(h *Handler, method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var body string
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, "http://console.test"+path, strings.NewReader(body))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://console.test")
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func responseCookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("missing cookie %s", name)
	return nil
}
func csrfField(t *testing.T, body string) string {
	t.Helper()
	match := regexp.MustCompile(`name="csrf"[^>]*value="([^"]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("missing CSRF form field")
	}
	return match[1]
}
func consoleLogin(t *testing.T, h *Handler) *http.Cookie {
	t.Helper()
	page := consoleRequest(h, "GET", "/ui/login", nil)
	pre := responseCookie(t, page, preauthCookie)
	w := consoleRequest(h, "POST", "/ui/login", url.Values{"csrf": {csrfField(t, page.Body.String())}, "key": {"gateway-secret"}}, pre)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login status %d: %s", w.Code, w.Body.String())
	}
	return responseCookie(t, w, sessionCookie)
}
func newTestConsole(t *testing.T) *Handler {
	t.Helper()
	h := New(Dependencies{GatewayKey: "gateway-secret", Snapshot: func() Status { return Status{Aliases: []Alias{{Name: "private-model"}}} }})
	t.Cleanup(h.Close)
	return h
}

func TestConsoleRequiresKeyAndSession(t *testing.T) {
	disabled := New(Dependencies{})
	defer disabled.Close()
	if w := consoleRequest(disabled, "GET", "/ui/login", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing key status %d", w.Code)
	}
	h := newTestConsole(t)
	for _, path := range []string{"/ui/", "/ui/routes", "/ui/usage", "/ui/playground", "/ui/api/status"} {
		w := consoleRequest(h, "GET", path, nil)
		if w.Code != http.StatusSeeOther || strings.Contains(w.Body.String(), "private-model") {
			t.Fatalf("unauthenticated response for %s: %d", path, w.Code)
		}
	}
	session := consoleLogin(t, h)
	w := consoleRequest(h, "GET", "/ui/api/status", nil, session)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "private-model") {
		t.Fatalf("authenticated status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "gateway-secret") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive response policy violated")
	}
}
func TestConsoleRejectsCrossOriginAndLoginCSRF(t *testing.T) {
	h := newTestConsole(t)
	if w := consoleRequest(h, "POST", "/ui/login", url.Values{"key": {"gateway-secret"}}); w.Code != 403 {
		t.Fatalf("missing preauth status %d", w.Code)
	}
	page := consoleRequest(h, "GET", "/ui/login", nil)
	pre := responseCookie(t, page, preauthCookie)
	form := url.Values{"key": {"gateway-secret"}, "csrf": {csrfField(t, page.Body.String())}}
	r := httptest.NewRequest("POST", "http://console.test/ui/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.test")
	r.AddCookie(pre)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-origin login status %d", w.Code)
	}
	session := consoleLogin(t, h)
	r = httptest.NewRequest("GET", "http://console.test/ui/api/status", nil)
	r.Header.Set("Origin", "https://evil.test")
	r.AddCookie(session)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("foreign read status %d", w.Code)
	}
}
func TestConsoleSessionIsolationLogoutAndExpiry(t *testing.T) {
	h := newTestConsole(t)
	first := consoleLogin(t, h)
	second := consoleLogin(t, h)
	firstPage := consoleRequest(h, "GET", "/ui/", nil, first)
	firstCSRF := csrfField(t, firstPage.Body.String())
	if w := consoleRequest(h, "POST", "/ui/logout", url.Values{"csrf": {firstCSRF}}, second); w.Code != 403 {
		t.Fatalf("cross-session csrf status %d", w.Code)
	}
	if w := consoleRequest(h, "POST", "/ui/logout", url.Values{"csrf": {firstCSRF}}, first); w.Code != 303 {
		t.Fatalf("logout status %d", w.Code)
	}
	if w := consoleRequest(h, "GET", "/ui/api/status", nil, first); w.Code != 303 {
		t.Fatalf("logged out session status %d", w.Code)
	}
	if w := consoleRequest(h, "GET", "/ui/api/status", nil, second); w.Code != 200 {
		t.Fatalf("other session status %d", w.Code)
	}
	h.mu.Lock()
	h.sessions[sha256.Sum256([]byte(second.Value))].last = time.Now().Add(-sessionIdle)
	h.mu.Unlock()
	r := httptest.NewRequest("GET", "http://console.test/ui/api/status", nil)
	r.AddCookie(second)
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || w.Header().Get("HX-Redirect") != "/ui/login" {
		t.Fatalf("expired htmx response %d %v", w.Code, w.Header())
	}
	third := consoleLogin(t, h)
	h.mu.Lock()
	h.sessions[sha256.Sum256([]byte(third.Value))].created = time.Now().Add(-sessionLifetime)
	h.mu.Unlock()
	if w := consoleRequest(h, "GET", "/ui/api/status", nil, third); w.Code != 303 {
		t.Fatalf("absolute expiry status %d", w.Code)
	}
}
func TestConsoleSessionLimitAndCookieTransport(t *testing.T) {
	h := newTestConsole(t)
	for range maxSessions {
		consoleLogin(t, h)
	}
	page := consoleRequest(h, "GET", "/ui/login", nil)
	pre := responseCookie(t, page, preauthCookie)
	w := consoleRequest(h, "POST", "/ui/login", url.Values{"csrf": {csrfField(t, page.Body.String())}, "key": {"gateway-secret"}}, pre)
	if w.Code != 503 {
		t.Fatalf("session limit status %d", w.Code)
	}
	r := httptest.NewRequest("GET", "https://console.test/ui/login", nil)
	r.TLS = &tls.ConnectionState{}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	secure := responseCookie(t, w, preauthCookie)
	if !secure.Secure || !secure.HttpOnly || secure.SameSite != http.SameSiteStrictMode {
		t.Fatalf("insecure HTTPS cookie: %+v", secure)
	}
	r = httptest.NewRequest("GET", "http://console.test/ui/login", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if responseCookie(t, w, preauthCookie).Secure {
		t.Fatal("trusted arbitrary forwarded protocol")
	}
}

func TestConsoleExplicitHTTPSProxyPolicy(t *testing.T) {
	h := New(Dependencies{GatewayKey: "gateway-secret", SecureCookies: true})
	defer h.Close()
	r := httptest.NewRequest("GET", "http://console.test/ui/login", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	pre := responseCookie(t, w, preauthCookie)
	if !pre.Secure {
		t.Fatal("proxy deployment must issue secure cookies")
	}
	form := url.Values{"key": {"gateway-secret"}, "csrf": {csrfField(t, w.Body.String())}}
	for _, origin := range []string{"http://console.test", "https://foreign.test", "https://console.test"} {
		r = httptest.NewRequest("POST", "http://console.test/ui/login", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", origin)
		r.AddCookie(pre)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if origin == "https://console.test" {
			if w.Code != http.StatusSeeOther || !responseCookie(t, w, sessionCookie).Secure {
				t.Fatalf("HTTPS proxy login: %d", w.Code)
			}
		} else if w.Code != http.StatusForbidden {
			t.Fatalf("untrusted origin %s accepted: %d", origin, w.Code)
		}
	}
}

func TestConsoleRevokesAuthenticationOnSessionRemoval(t *testing.T) {
	for _, reason := range []string{"logout", "idle expiry", "absolute expiry", "shutdown"} {
		t.Run(reason, func(t *testing.T) {
			h := newTestConsole(t)
			cookie := consoleLogin(t, h)
			key := sha256.Sum256([]byte(cookie.Value))
			h.mu.Lock()
			s := h.sessions[key]
			switch reason {
			case "idle expiry":
				s.last = time.Now().Add(-sessionIdle)
			case "absolute expiry":
				s.created = time.Now().Add(-sessionLifetime)
			}
			h.mu.Unlock()

			switch reason {
			case "logout":
				w := consoleRequest(h, http.MethodPost, "/ui/logout", url.Values{"csrf": {s.csrf}}, cookie)
				if w.Code != http.StatusSeeOther {
					t.Fatalf("logout status %d", w.Code)
				}
			case "shutdown":
				h.Close()
			default:
				w := consoleRequest(h, http.MethodGet, "/ui/api/status", nil, cookie)
				if w.Code != http.StatusSeeOther {
					t.Fatalf("expired session status %d", w.Code)
				}
			}

			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed {
				t.Fatal("removed session remains available for starting work")
			}
			h.mu.Lock()
			_, registered := h.sessions[key]
			h.mu.Unlock()
			if registered {
				t.Fatal("removed session remains registered")
			}
		})
	}
}
