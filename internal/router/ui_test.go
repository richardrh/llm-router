package router

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestConsoleStatusDoesNotExposeProviderCredentials(t *testing.T) {
	up := newFakeUpstream(t, "provider")
	srv := newTestServer(t, map[string]*fakeUpstream{"provider": up}, map[string]int{"provider": 1}, func(c *Config, a *Alias) {
		c.APIKey = "gateway-secret"
		c.UI.Enabled = true
		u := c.Upstreams["provider"]
		u.APIKey = "upstream-secret"
		u.BaseURL += "/private-url-token"
		u.Headers = map[string]string{"X-Private": "private-header-token"}
		u.BodyPatch = map[string]any{"secret": "private-patch-token"}
		c.Upstreams["provider"] = u
	})
	t.Cleanup(srv.Close)
	handler := srv.Handler()
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest("GET", "http://console.test/ui/login", nil))
	token := regexp.MustCompile(`name="csrf"[^>]*value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if len(token) != 2 {
		t.Fatal("login did not provide CSRF token")
	}
	login := httptest.NewRequest("POST", "http://console.test/ui/login", strings.NewReader(url.Values{"key": {"gateway-secret"}, "csrf": {token[1]}}.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	login.Header.Set("Origin", "http://console.test")
	for _, cookie := range page.Result().Cookies() {
		login.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, login)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("login: %d %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest("GET", "http://console.test/ui/api/status", nil)
	for _, cookie := range response.Result().Cookies() {
		if cookie.MaxAge >= 0 {
			request.AddCookie(cookie)
		}
	}
	snapshot := httptest.NewRecorder()
	handler.ServeHTTP(snapshot, request)
	if snapshot.Code != http.StatusOK {
		t.Fatalf("status: %d %s", snapshot.Code, snapshot.Body.String())
	}
	for _, secret := range []string{"gateway-secret", "upstream-secret", "private-url-token", "private-header-token", "private-patch-token"} {
		if strings.Contains(snapshot.Body.String(), secret) {
			t.Fatalf("operational snapshot disclosed %s", secret)
		}
	}
	if up.hits() != 0 {
		t.Fatal("status reading contacted the provider")
	}
}
