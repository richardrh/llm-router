package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// upstreamState is the per-upstream runtime: credentials, a concurrency gate,
// and the health state that keeps a dead provider out of rotation.
type upstreamState struct {
	name string
	cfg  Upstream
	key  string

	sem chan struct{}

	// threshold is how many consecutive failures trip the breaker.
	threshold int

	mu           sync.Mutex
	consecFails  int
	openUntil    time.Time
	probeRunning bool
}

func newUpstreamState(name string, cfg Upstream, key string, threshold int) *upstreamState {
	if threshold < 1 {
		threshold = 1
	}
	concurrency := cfg.MaxConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	return &upstreamState{
		name:      name,
		cfg:       cfg,
		key:       key,
		sem:       make(chan struct{}, concurrency),
		threshold: threshold,
	}
}

// acquire takes a concurrency slot without blocking. A saturated upstream is
// skipped rather than queued: a router that waits behind one slow provider
// cannot fail over to the others.
func (u *upstreamState) acquire() bool {
	select {
	case u.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (u *upstreamState) release() { <-u.sem }

// available reports whether the upstream is currently out of rotation. It is a
// pure read: callers may evaluate a target many times per request, so the
// decision must not consume the one-shot recovery probe.
func (u *upstreamState) available(now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.openUntil.IsZero() || !now.Before(u.openUntil)
}

// claimProbe gates the single request allowed to test a recovered upstream.
// Without it the whole herd would pile onto a provider at the instant its
// cooldown expires and knock it straight back over.
func (u *upstreamState) claimProbe(now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.consecFails == 0 || u.openUntil.IsZero() || now.Before(u.openUntil) {
		return true
	}
	if u.probeRunning {
		return false
	}
	u.probeRunning = true
	return true
}

func (u *upstreamState) recordSuccess() {
	u.mu.Lock()
	u.consecFails = 0
	u.openUntil = time.Time{}
	u.probeRunning = false
	u.mu.Unlock()
}

// recordFailure trips the breaker once the failure threshold is met. until,
// when set, overrides the computed cooldown (used to honour Retry-After).
func (u *upstreamState) recordFailure(cooldown time.Duration, until time.Time) {
	if until.IsZero() {
		until = time.Now().Add(cooldown)
	}
	u.mu.Lock()
	u.consecFails++
	if u.consecFails >= u.threshold {
		// Each further failure pushes the cooldown out, so a provider that has
		// just come back does not immediately re-enter rotation under load.
		u.openUntil = until
	}
	u.probeRunning = false
	u.mu.Unlock()
}

// rateLimit records a 429 without counting it as a health failure: a throttled
// upstream is healthy, just busy.
func (u *upstreamState) rateLimit(until time.Time, fallback time.Duration) {
	if until.IsZero() {
		until = time.Now().Add(fallback)
	}
	u.mu.Lock()
	u.openUntil = until
	u.probeRunning = false
	u.mu.Unlock()
}

// retryableStatus reports whether a status means "this provider could not serve
// it, try another". Ordinary client errors are the client's bug and would fail
// identically on every target.
func retryableStatus(code int) bool {
	switch {
	case code == http.StatusTooManyRequests:
		return true
	case code == http.StatusUnauthorized,
		code == http.StatusForbidden,
		code == http.StatusPaymentRequired:
		// Credential or quota problem specific to this upstream.
		return true
	case code >= 500:
		return true
	}
	return false
}

// parseRetryAfter understands both the delay-seconds and HTTP-date forms.
func parseRetryAfter(v string, now time.Time) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return time.Time{}
		}
		return now.Add(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return t
	}
	return time.Time{}
}

const dialTimeout = 10 * time.Second

func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			// Streaming responses are long-lived and already chunked; a
			// transport that negotiates compression here would buffer the SSE
			// body and defeat incremental delivery.
			DisableCompression: true,
			ForceAttemptHTTP2:  true,
			WriteBufferSize:    64 << 10,
			ReadBufferSize:     64 << 10,
		},
	}
}

// idleReader cancels an attempt context when no bytes arrive for the idle
// window, so a wedged upstream cannot hold the client open forever.
type idleReader struct {
	r      io.Reader
	idle   time.Duration
	cancel context.CancelFunc
}

func newIdleReader(r io.Reader, idle time.Duration, cancel context.CancelFunc) *idleReader {
	return &idleReader{r: r, idle: idle, cancel: cancel}
}

func (ir *idleReader) Read(p []byte) (int, error) {
	if ir.idle <= 0 {
		return ir.r.Read(p)
	}
	timer := time.AfterFunc(ir.idle, ir.cancel)
	n, err := ir.r.Read(p)
	timer.Stop()
	return n, err
}

func (ir *idleReader) Close() error {
	if c, ok := ir.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
