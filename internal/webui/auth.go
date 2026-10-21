package webui

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (h *Handler) cookie(w http.ResponseWriter, r *http.Request, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/ui", HttpOnly: true, Secure: h.deps.SecureCookies || r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: age})
}

func (h *Handler) sameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		if !h.matchingOrigin(r, origin) {
			return false
		}
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if r.Method == http.MethodPost && r.Header.Get("Origin") == "" {
		if ref := r.Header.Get("Referer"); ref != "" {
			return h.matchingOrigin(r, ref)
		}
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	return true
}
func (h *Handler) matchingOrigin(r *http.Request, raw string) bool {
	u, err := url.Parse(raw)
	scheme := "http"
	if h.deps.SecureCookies || r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.User == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host)
}
func (h *Handler) preauth() string {
	payload := strconv.FormatInt(time.Now().Unix(), 10) + "." + randomToken()
	mac := hmac.New(sha256.New, h.secret[:])
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}
func (h *Handler) validPreauth(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	stamp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	age := time.Since(time.Unix(stamp, 0))
	if age < 0 || age > preauthLifetime {
		return false
	}
	mac := hmac.New(sha256.New, h.secret[:])
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := hex.DecodeString(parts[2])
	return err == nil && hmac.Equal(sig, mac.Sum(nil))
}
func authRedirect(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/ui/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	message := ""
	if r.Method == http.MethodPost {
		c, err := r.Cookie(preauthCookie)
		token := r.PostForm.Get("csrf")
		if err != nil || !h.validPreauth(token) || subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) != 1 {
			http.Error(w, "Invalid CSRF token", http.StatusForbidden)
			return
		}
		provided := sha256.Sum256([]byte(r.PostForm.Get("key")))
		expected := sha256.Sum256([]byte(h.deps.GatewayKey))
		if subtle.ConstantTimeCompare(provided[:], expected[:]) == 1 {
			token := randomToken()
			s := &browserSession{csrf: randomToken(), created: time.Now(), last: time.Now()}
			h.mu.Lock()
			if h.closed || len(h.sessions) >= maxSessions {
				h.mu.Unlock()
				http.Error(w, "Console session limit reached", http.StatusServiceUnavailable)
				return
			}
			h.sessions[sha256.Sum256([]byte(token))] = s
			h.mu.Unlock()
			h.cookie(w, r, sessionCookie, token, int(sessionLifetime.Seconds()))
			h.cookie(w, r, preauthCookie, "", -1)
			http.Redirect(w, r, "/ui/", http.StatusSeeOther)
			return
		}
		message = "Invalid gateway key"
	}
	token := h.preauth()
	h.cookie(w, r, preauthCookie, token, int(preauthLifetime.Seconds()))
	render(w, r, Page("Sign in", "", "", Login(token, message)))
}
