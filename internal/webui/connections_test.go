package webui

import (
	"context"
	"net/http"
	"net/url"
	"testing"
)

func TestProviderAuthenticationRequiresSessionAndCSRF(t *testing.T) {
	signedIn := false
	h := New(Dependencies{GatewayKey: "gateway-secret", Connections: func() []Connection {
		return []Connection{{Name: "codex", Kind: "codex", SignedIn: signedIn}}
	}, Connect: func(_ context.Context, action ConnectionAction) error {
		signedIn = action.Kind == ConnectionLogin
		return nil
	}})
	defer h.Close()
	form := url.Values{"upstream": {"codex"}, "action": {"login"}}
	w := consoleRequest(h, "POST", "/ui/connections/action", form)
	if w.Code != http.StatusSeeOther || signedIn {
		t.Fatal("unauthenticated provider login was allowed")
	}
	cookie := consoleLogin(t, h)
	w = consoleRequest(h, "POST", "/ui/connections/action", form, cookie)
	if w.Code != http.StatusForbidden || signedIn {
		t.Fatal("provider login bypassed CSRF validation")
	}
	page := consoleRequest(h, "GET", "/ui/connections", nil, cookie)
	form.Set("csrf", csrfField(t, page.Body.String()))
	w = consoleRequest(h, "POST", "/ui/connections/action", form, cookie)
	if w.Code != http.StatusOK || !signedIn {
		t.Fatalf("authorized login failed: %d", w.Code)
	}
	form.Set("action", "logout")
	w = consoleRequest(h, "POST", "/ui/connections/action", form, cookie)
	if w.Code != http.StatusOK || signedIn {
		t.Fatal("authorized provider logout did not take effect")
	}
	if w = consoleRequest(h, "GET", "/ui/connections/action", nil, cookie); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("GET allowed account mutation")
	}
}

func TestCodexLoginURLRejectsUntrustedDestinations(t *testing.T) {
	for _, raw := range []string{"javascript:alert(1)", "http://auth.openai.com/login", "https://openai.com.evil.test/login", "https://openai.com@evil.test/login", "https://evil.test/?next=https://chatgpt.com", "https://auth.openai.com:8443/login"} {
		if officialLoginURL(raw) {
			t.Errorf("accepted unsafe login URL %q", raw)
		}
	}
	for _, raw := range []string{"https://auth.openai.com/authorize?state=random", "https://chatgpt.com/oauth/login"} {
		if !officialLoginURL(raw) {
			t.Errorf("rejected official login URL %q", raw)
		}
	}
}

func TestConnectionRefreshIsExplicitTypedAction(t *testing.T) {
	var actions []ConnectionAction
	h := New(Dependencies{GatewayKey: "gateway-secret", Connections: func() []Connection { return []Connection{{Name: "codex", Kind: "codex"}} }, Connect: func(_ context.Context, a ConnectionAction) error { actions = append(actions, a); return nil }})
	defer h.Close()
	cookie := consoleLogin(t, h)
	page := consoleRequest(h, "GET", "/ui/connections", nil, cookie)
	if len(actions) != 0 {
		t.Fatal("GET mutated connection")
	}
	form := url.Values{"upstream": {"codex"}, "action": {"refresh"}}
	if w := consoleRequest(h, "POST", "/ui/connections/action", form, cookie); w.Code != http.StatusForbidden {
		t.Fatal("refresh bypassed CSRF")
	}
	if len(actions) != 0 {
		t.Fatal("rejected refresh reached provider")
	}
	form.Set("csrf", csrfField(t, page.Body.String()))
	w := consoleRequest(h, "POST", "/ui/connections/action", form, cookie)
	if w.Code != http.StatusOK || len(actions) != 1 || actions[0].Kind != ConnectionRefresh || actions[0].Upstream != "codex" {
		t.Fatalf("refresh not dispatched: %d %+v", w.Code, actions)
	}
}
