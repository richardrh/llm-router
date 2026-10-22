package webui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (h *Handler) connections(w http.ResponseWriter, r *http.Request, s *browserSession) {
	if (r.URL.Path == "/ui/connections" && r.Method != http.MethodGet) || (r.URL.Path == "/ui/connections/action" && r.Method != http.MethodPost) {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	message := ""
	if r.Method == http.MethodPost {
		action := ConnectionAction{Upstream: r.PostForm.Get("upstream"), Kind: ConnectionActionKind(r.PostForm.Get("action")), LoginID: r.PostForm.Get("login_id")}
		switch action.Kind {
		case ConnectionLogin, ConnectionCancel, ConnectionLogout, ConnectionRefresh:
			if h.deps.Connect == nil {
				message = "Provider authentication is unavailable"
			} else if err := h.deps.Connect(ctx, action); err != nil {
				message = err.Error()
			}
		default:
			http.Error(w, "Unknown authentication action", http.StatusBadRequest)
			return
		}
	}
	var connections []Connection
	if h.deps.Connections != nil {
		connections = h.deps.Connections()
	}
	for i := range connections {
		if connections[i].LoginURL != "" && !officialLoginURL(connections[i].LoginURL) {
			connections[i].LoginURL = ""
			connections[i].Error = "Codex returned an unrecognized sign-in address; use codex login on the router host."
		}
	}
	body := ProviderConnections(s.csrf, connections, message)
	if r.Header.Get("HX-Request") == "true" {
		render(w, r, body)
	} else {
		render(w, r, Page("Connections", s.csrf, "connections", body))
	}
}

func officialLoginURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "openai.com" || strings.HasSuffix(host, ".openai.com") || host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")
}

func pendingLogin(connections []Connection) bool {
	for _, connection := range connections {
		if connection.LoginURL != "" && !connection.SignedIn {
			return true
		}
	}
	return false
}
