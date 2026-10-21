package webui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"
)

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = c.Render(r.Context(), w)
}
func (h *Handler) servePage(w http.ResponseWriter, r *http.Request, s *browserSession) {
	if r.URL.Path == "/ui/playground" || strings.HasPrefix(r.URL.Path, "/ui/playground/") {
		h.servePlayground(w, r, s)
		return
	}
	if r.URL.Path == "/ui/connections" || r.URL.Path == "/ui/connections/action" {
		h.connections(w, r, s)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status := func() Status {
		if h.deps.Snapshot != nil {
			return h.deps.Snapshot()
		}
		return Status{}
	}
	usage := func(q url.Values) UsageData {
		if q.Get("since") == "" {
			q.Set("since", "24h")
		}
		if h.deps.Usage == nil {
			return UsageData{Since: q.Get("since")}
		}
		data, err := h.deps.Usage(q)
		if err != nil {
			data.Error = err.Error()
		}
		return data
	}
	switch r.URL.Path {
	case "/ui", "/ui/":
		render(w, r, Page("Overview", s.csrf, "overview", Overview(status(), usage(url.Values{}))))
	case "/ui/overview":
		render(w, r, Overview(status(), usage(url.Values{})))
	case "/ui/routes":
		body := Routes(status())
		if r.Header.Get("HX-Request") == "true" {
			render(w, r, body)
		} else {
			render(w, r, Page("Routes", s.csrf, "routes", body))
		}
	case "/ui/usage":
		body := Usage(usage(r.URL.Query()))
		if r.Header.Get("HX-Request") == "true" {
			render(w, r, body)
		} else {
			render(w, r, Page("Usage", s.csrf, "usage", body))
		}
	case "/ui/api/status":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status())
	default:
		http.NotFound(w, r)
	}
}
