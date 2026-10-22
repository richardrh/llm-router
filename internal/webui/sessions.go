package webui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookie   = "omp_ui_session"
	preauthCookie   = "omp_ui_preauth"
	maxSessions     = 128
	sessionIdle     = 30 * time.Minute
	sessionLifetime = 12 * time.Hour
	preauthLifetime = 10 * time.Minute
)

type browserSession struct {
	csrf          string
	mu            sync.Mutex
	play          *conversation
	closed        bool // Guarded by mu; prevents work starting after revocation.
	created, last time.Time
}

// revokeSession prevents new work before canceling the session's conversation.
// Do not hold s.mu while closing the playground, which acquires it itself.
func (h *Handler) revokeSession(s *browserSession) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	h.closePlayground(s)
}

func (h *Handler) Close() {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		close(h.stop)
	}
	stale := h.sessions
	h.sessions = make(map[[32]byte]*browserSession)
	h.mu.Unlock()
	for _, s := range stale {
		h.revokeSession(s)
	}
	<-h.done
}

func (h *Handler) reap() {
	defer close(h.done)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-h.stop:
			return
		case now := <-tick.C:
			var stale []*browserSession
			h.mu.Lock()
			for key, s := range h.sessions {
				if expired(s, now) {
					delete(h.sessions, key)
					stale = append(stale, s)
				}
			}
			h.mu.Unlock()
			for _, s := range stale {
				h.revokeSession(s)
			}
		}
	}
}
func expired(s *browserSession, now time.Time) bool {
	return now.Sub(s.created) >= sessionLifetime || now.Sub(s.last) >= sessionIdle
}
func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("webui: secure randomness unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func (h *Handler) session(r *http.Request) *browserSession {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	key := sha256.Sum256([]byte(c.Value))
	now := time.Now()
	h.mu.Lock()
	s := h.sessions[key]
	if s != nil && expired(s, now) {
		delete(h.sessions, key)
	} else if s != nil {
		s.last = now
		h.mu.Unlock()
		return s
	}
	h.mu.Unlock()
	if s != nil {
		h.revokeSession(s)
	}
	return nil
}
