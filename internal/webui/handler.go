package webui

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

//go:embed assets/*
var assets embed.FS

type Handler struct {
	deps     Dependencies
	mu       sync.Mutex
	sessions map[[32]byte]*browserSession
	secret   [32]byte
	stop     chan struct{}
	done     chan struct{}
	closed   bool
	// Worker registration and the closed flag share mu, so Close can wait
	// without racing a new Add, including work from discarded conversations.
	workers   sync.WaitGroup
	closeOnce sync.Once
	assets    http.Handler
}

func New(deps Dependencies) *Handler {
	h := &Handler{deps: deps, sessions: make(map[[32]byte]*browserSession), stop: make(chan struct{}), done: make(chan struct{})}
	if _, err := rand.Read(h.secret[:]); err != nil {
		panic("webui: secure randomness unavailable")
	}
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	h.assets = http.StripPrefix("/ui/assets/", http.FileServer(http.FS(sub)))
	go h.reap()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if h.deps.GatewayKey == "" {
		http.Error(w, "Console requires a gateway key", http.StatusServiceUnavailable)
		return
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		http.Error(w, "Console closed", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.sameOrigin(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/ui/assets/") && r.Method == http.MethodGet {
		h.assets.ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form", http.StatusBadRequest)
			return
		}
	}
	if r.URL.Path == "/ui/login" {
		h.login(w, r)
		return
	}
	s := h.session(r)
	if s == nil {
		authRedirect(w, r)
		return
	}
	if r.Method == http.MethodPost {
		if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(s.csrf)) != 1 {
			http.Error(w, "Invalid CSRF token", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/ui/logout" {
			c, _ := r.Cookie(sessionCookie)
			h.mu.Lock()
			delete(h.sessions, sha256.Sum256([]byte(c.Value)))
			h.mu.Unlock()
			h.revokeSession(s)
			h.cookie(w, r, sessionCookie, "", -1)
			authRedirect(w, r)
			return
		}
	}
	h.servePage(w, r, s)
}
