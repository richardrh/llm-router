package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

var (
	errBodyTooLarge = errors.New("request body exceeds configured limit")
	errBadJSON      = errors.New("request body is not a JSON object")
)

type Server struct {
	router *Router
	cfg    *Config
	log    *slog.Logger
	client *http.Client
	seq    atomic.Uint64
}

func NewServer(cfg *Config, router *Router, log *slog.Logger) *Server {
	return &Server{router: router, cfg: cfg, log: log, client: newHTTPClient()}
}

// Handler mounts the client surface. Discovery is registered under /api/v1 as
// well as /v1 so a client can be pointed at this router by changing only the
// host in the OpenRouter base URL, https://openrouter.ai/api/v1. Inference
// needs no such help: handleRoot matches on the path suffix, so
// /api/v1/chat/completions and /v1/chat/completions route identically.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, p := range []string{"/v1/models", "/api/v1/models"} {
		mux.HandleFunc(p, s.handleModels)
		mux.HandleFunc(p+"/", s.handleModels)
	}
	for _, p := range []string{"/model_group/info", "/api/v1/model_group/info"} {
		mux.HandleFunc(p, s.handleModelGroupInfo)
	}
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/", s.handleRoot)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
}

// handleRoot dispatches inference paths. Anything else is an explicit error
// rather than a pass-through, so an unsupported endpoint fails loudly.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}

	path := r.URL.Path
	var api APIProtocol
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		api = APIOpenAICompletions
	case strings.HasSuffix(path, "/responses"):
		api = APIOpenAIResponses
	case strings.HasSuffix(path, "/messages"):
		api = APIAnthropicMessages
	default:
		s.writeError(w, http.StatusNotFound, "unsupported_endpoint",
			fmt.Sprintf("%s is not served; use /v1/chat/completions, /v1/responses, /v1/messages or /v1/models", path))
		return
	}

	if !s.authorized(w, r) {
		return
	}
	s.handleInference(w, r, api)
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	want := s.cfg.APIKey
	if want == "" {
		return true
	}
	got := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(got, "Bearer "); ok {
		got = after
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "invalid gateway api key")
		return false
	}
	return true
}

type attemptOutcome struct {
	status   int
	bytes    int64
	streamed bool
	attempts int
	// usage is what the provider reported, when it reported anything. hasUsage
	// distinguishes "not reported" from "reported as zero", so a cost is never
	// invented for a response that carried no counts.
	usage    Usage
	hasUsage bool
}

func (s *Server) handleInference(w http.ResponseWriter, r *http.Request, api APIProtocol) {
	reqID := requestID(r.Method, r.URL.Path, s.seq.Add(1))
	start := time.Now()
	logger := s.log.With("request_id", reqID, "path", r.URL.Path)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.Defaults.MaxBodyBytes+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", errBodyTooLarge.Error())
			return
		}
		s.writeError(w, http.StatusBadRequest, "read_body_failed", err.Error())
		return
	}

	plan, err := newRequestPlan(body, s.cfg.Defaults.MaxBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", errBodyTooLarge.Error())
			return
		}
		s.writeError(w, http.StatusBadRequest, "invalid_body", errBadJSON.Error())
		return
	}

	var aliasName string
	if raw, ok := plan.fields["model"]; ok {
		_ = json.Unmarshal(raw, &aliasName)
	}
	alias, ok := s.router.cfg.Models[aliasName]
	if !ok {
		s.writeError(w, http.StatusNotFound, "unknown_model",
			fmt.Sprintf("no route for model %q; known aliases: %s", aliasName, strings.Join(s.aliasNames(), ", ")))
		return
	}
	plan.alias = aliasName
	plan.api = api
	if alias.API != api {
		s.writeError(w, http.StatusBadRequest, "protocol_mismatch",
			fmt.Sprintf("alias %q is served as %s, not %s", aliasName, alias.API, api))
		return
	}

	if _, err := api.Path(); err != nil {
		s.writeError(w, http.StatusBadRequest, "unsupported_api", err.Error())
		return
	}

	// The prefix hash serves both session pinning and cache observation, and
	// costs a hash of the opening turn, so it is computed only when one of them
	// will use it.
	var fingerprint string
	if s.cfg.Defaults.StickyByPrefix || s.cfg.cacheAffinityOn() {
		fingerprint = plan.cacheFingerprint()
	}
	stickyKey := stickyKeyFrom(r.Header)
	if stickyKey == "" && s.cfg.Defaults.StickyByPrefix {
		stickyKey = fingerprint
	}
	candidates := s.router.orderedCandidates(aliasName, &alias, stickyKey, fingerprint, time.Now())
	if len(candidates) == 0 {
		s.writeError(w, http.StatusServiceUnavailable, "no_targets", "every configured target is unavailable")
		return
	}

	var outcome attemptOutcome
	var lastErr error
	var lastStatus int

	// maxAttempts bounds upstream dispatches, not loop iterations: a target
	// skipped for saturation or a pending recovery probe must not use up the
	// request's failover budget.
	budget := s.cfg.Defaults.MaxAttempts
	dispatched := 0

	for _, c := range candidates {
		if dispatched >= budget {
			break
		}
		now := time.Now()
		if !c.upstream.claimProbe(now) {
			logger.Debug("skipping upstream awaiting recovery probe", "upstream", c.upstream.name)
			continue
		}
		if !c.upstream.acquire() {
			logger.Debug("skipping saturated upstream", "upstream", c.upstream.name)
			continue
		}
		dispatched++
		attemptStart := time.Now()
		res, took, err := s.attempt(w, r, plan, c, logger)
		attemptStatus := 0
		if res != nil {
			attemptStatus = res.StatusCode
		}
		c.upstream.release()

		if err == nil {
			outcome = took
			outcome.attempts = dispatched
			// Record what the provider reported about its cache, so a later turn
			// of this conversation can be sent back to it.
			if outcome.hasUsage {
				s.router.affinity.observe(fingerprint, c.upstream.name, outcome.usage, time.Now())
			}
			s.finishLog(logger, aliasName, c, outcome, start)
			return
		}
		lastErr = err
		lastStatus = attemptStatus
		logger.Warn("attempt failed, trying next target",
			"upstream", c.upstream.name,
			"upstream_model", c.target.Model,
			"status", attemptStatus,
			"attempt_ms", time.Since(attemptStart).Milliseconds(),
			"error", err.Error())
	}

	s.logFailure(logger, aliasName, candidates, start)
	if lastErr == nil {
		// Every target was skipped for saturation; there is no upstream error
		// to report, and 502 would misattribute it to the providers.
		s.writeError(w, http.StatusServiceUnavailable, "targets_busy",
			fmt.Sprintf("every target for model %q was at its concurrency limit", aliasName))
		return
	}
	if lastStatus != 0 && !retryableStatus(lastStatus) {
		s.writeError(w, lastStatus, "upstream_error", lastErr.Error())
		return
	}
	s.writeError(w, http.StatusBadGateway, "all_targets_failed",
		fmt.Sprintf("no target could serve model %q: %v", aliasName, lastErr))
}

func (s *Server) finishLog(logger *slog.Logger, aliasName string, c candidate, outcome attemptOutcome, start time.Time) {
	attrs := []any{
		"alias", aliasName,
		"upstream", c.upstream.name,
		"upstream_model", c.target.Model,
		"status", outcome.status,
		"attempts", outcome.attempts,
		"streamed", outcome.streamed,
		"bytes", outcome.bytes,
		"total_ms", time.Since(start).Milliseconds(),
	}
	if outcome.hasUsage {
		attrs = append(attrs,
			"input_tokens", outcome.usage.InputTokens,
			"output_tokens", outcome.usage.OutputTokens,
			"cache_read_tokens", outcome.usage.CacheReadTokens,
			"cache_write_tokens", outcome.usage.CacheWriteTokens,
			"cost_usd", s.cfg.Models[aliasName].Cost.Estimate(outcome.usage))
	} else {
		// No counts were reported, so no cost is claimed. An invented figure
		// would be worse than an absent one.
		attrs = append(attrs, "usage", "unreported")
	}
	logger.Info("served", attrs...)
}

func (s *Server) logFailure(logger *slog.Logger, aliasName string, candidates []candidate, start time.Time) {
	logger.Error("request failed",
		"alias", aliasName,
		"targets", len(candidates),
		"total_ms", time.Since(start).Milliseconds())
}

// attempt performs one upstream dispatch. It returns a non-nil error only when
// the attempt is safe to replay against another target, which is exclusively
// before any response byte has been committed to the client.
func (s *Server) attempt(
	w http.ResponseWriter,
	r *http.Request,
	plan *requestPlan,
	c candidate,
	logger *slog.Logger,
) (*http.Response, attemptOutcome, error) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	streamTimer := time.AfterFunc(s.cfg.Defaults.MaxStreamDuration.Duration(), cancel)
	defer streamTimer.Stop()

	// A target may speak a different wire than the client, so the path and the
	// body are decided per attempt rather than once per request.
	upstreamAPI := c.upstreamAPI(plan.api)
	apiPath, err := upstreamAPI.Path()
	if err != nil {
		return nil, attemptOutcome{}, err
	}
	upBody, err := plan.bodyFor(c, s.cfg.Defaults.InjectStreamUsage, upstreamAPI)
	if err != nil {
		return nil, attemptOutcome{}, err
	}

	endpoint := strings.TrimRight(c.upstream.cfg.BaseURL, "/") + apiPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(upBody)))
	if err != nil {
		return nil, attemptOutcome{}, err
	}
	req.ContentLength = int64(len(upBody))
	copyRequestHeaders(req.Header, r.Header)
	for k, v := range c.upstream.cfg.Headers {
		req.Header.Set(k, v)
	}
	applyUpstreamAuth(req, c.upstream)
	if plan.stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		c.upstream.recordFailure(s.cfg.Defaults.BreakerCooldown.Duration(), time.Time{})
		if r.Context().Err() != nil {
			return nil, attemptOutcome{}, fmt.Errorf("client disconnected")
		}
		return nil, attemptOutcome{}, err
	}

	now := time.Now()
	if retryableStatus(resp.StatusCode) {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), now)
		if resp.StatusCode == http.StatusTooManyRequests {
			c.upstream.rateLimit(retryAfter, s.cfg.Defaults.RateLimitCooldown.Duration())
		} else {
			c.upstream.recordFailure(s.cfg.Defaults.BreakerCooldown.Duration(), retryAfter)
		}
		statusErr := fmt.Errorf("upstream %s returned %d: %s", c.upstream.name, resp.StatusCode, readErrorSnippet(resp))
		drainAndClose(resp.Body)
		return nil, attemptOutcome{status: resp.StatusCode}, statusErr
	}

	c.upstream.recordSuccess()

	// Past this point the response belongs to the client; no failover.
	translating := upstreamAPI != plan.api
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Router-Upstream", c.upstream.name)
	w.Header().Set("X-Router-Upstream-Model", c.target.Model)
	if translating {
		// The body is about to change shape, so the upstream's length is no
		// longer the length of what will be sent, and a client trusting it would
		// truncate the response.
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(resp.StatusCode)

	outcome := attemptOutcome{status: resp.StatusCode}
	if isEventStream(resp) {
		outcome.streamed = true
		var observer usageObserver
		n, err := s.relayStream(w, resp.Body, ctx, cancel, logger, &observer,
			newStreamTranslator(plan.api, upstreamAPI, plan.alias))
		outcome.bytes = n
		outcome.usage, outcome.hasUsage = observer.usage, observer.found
		if err != nil && r.Context().Err() == nil {
			logger.Warn("stream interrupted", "upstream", c.upstream.name, "error", err.Error())
		}
		return resp, outcome, nil
	}

	buf, readErr := io.ReadAll(newIdleReader(resp.Body, s.cfg.Defaults.StreamIdleTimeout.Duration(), cancel))
	outcome.bytes = int64(len(buf))
	// Usage is read from the upstream's own bytes, before any conversion: it is
	// reported in the upstream's vocabulary, not the client's.
	if u, ok := usageFromBody(buf); ok {
		outcome.usage, outcome.hasUsage = u, true
	}
	switch {
	case translating:
		converted, cerr := translateResponse(plan.api, upstreamAPI, buf, plan.alias)
		if cerr != nil {
			// The status line is already committed, so the failure has to be
			// reported in the body. The operator gets the detail.
			logger.Error("response translation failed",
				"upstream", c.upstream.name, "error", cerr.Error())
			buf = errTranslationFailed
		} else {
			buf = converted
		}
	case s.cfg.Defaults.RewriteModel:
		buf = rewriteModelField(buf, plan.alias)
	}
	if _, werr := w.Write(buf); werr != nil {
		logger.Warn("client write failed", "upstream", c.upstream.name, "error", werr.Error())
	}
	if readErr != nil && r.Context().Err() == nil {
		logger.Warn("response body interrupted", "upstream", c.upstream.name, "error", readErr.Error())
	}
	// Status is already committed, so a truncated body must not trigger
	// failover: the client would see a corrupt response either way.
	return resp, outcome, nil
}

// relayStream copies SSE bytes through with a flush per chunk so the client sees
// tokens as they are produced rather than at end of turn.
//
// When the client and upstream share a wire the copy is byte-transparent, and
// the same bytes are fed to an observer so usage can be recorded without parsing,
// buffering or rewriting the stream. When they do not, the bytes are converted on
// the way out — and the observer still sees the upstream's own bytes, because
// usage is reported in the upstream's vocabulary, not the client's.
func (s *Server) relayStream(w http.ResponseWriter, body io.ReadCloser, ctx context.Context, cancel context.CancelFunc, logger *slog.Logger, observer *usageObserver, xlate streamTranslator) (int64, error) {
	flusher, _ := w.(http.Flusher)
	reader := newIdleReader(body, s.cfg.Defaults.StreamIdleTimeout.Duration(), cancel)
	defer body.Close()

	emit := func(b []byte) error {
		if len(b) == 0 {
			return nil
		}
		if _, werr := w.Write(b); werr != nil {
			return werr
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			observer.Write(buf[:n])
			out := buf[:n]
			if xlate != nil {
				out = xlate.Write(buf[:n])
			}
			if werr := emit(out); werr != nil {
				return total, werr
			}
			total += int64(len(out))
		}
		if err != nil {
			if xlate != nil {
				tail := xlate.Close()
				if werr := emit(tail); werr != nil {
					return total, werr
				}
				total += int64(len(tail))
			}
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			if errors.Is(err, context.Canceled) {
				select {
				case <-ctx.Done():
					return total, ctx.Err()
				default:
				}
			}
			return total, err
		}
	}
}

func (s *Server) aliasNames() []string {
	names := make([]string, 0, len(s.cfg.Models))
	for n := range s.cfg.Models {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": code, "code": status},
	})
}

func readErrorSnippet(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	if n == 0 {
		return ""
	}
	return strings.TrimSpace(string(buf[:n]))
}

func isEventStream(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "text/event-stream")
}

var hopByHop = map[string]bool{
	"connection":          true,
	"proxy-connection":    true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// clientCredentialHeaders are inbound secrets that belong to the gateway, not
// to any upstream. Forwarding them would hand a client's key to a provider it
// was never issued for: an Anthropic-wire client sends x-api-key, and the
// router must replace it rather than relay it to, say, OpenRouter.
var clientCredentialHeaders = map[string]bool{
	"authorization": true,
	"x-api-key":     true,
	"api-key":       true,
}

// copyRequestHeaders forwards end-to-end headers. Client credentials are
// dropped: they must never leak to an upstream.
func copyRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if hopByHop[lk] || clientCredentialHeaders[lk] || lk == "host" || lk == "content-length" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if hopByHop[lk] || lk == "content-length" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func applyUpstreamAuth(req *http.Request, u *upstreamState) {
	if u.key == "" {
		return
	}
	switch u.cfg.AuthStyle {
	case AuthAnthropic:
		req.Header.Set("x-api-key", u.key)
		req.Header.Set("Authorization", "Bearer "+u.key)
		// The Claude API rejects requests without an explicit version. The
		// upstream's own headers were applied already, so an explicit
		// anthropic-version in config still wins.
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", defaultAnthropicVersion)
		}
	default:
		req.Header.Set("Authorization", "Bearer "+u.key)
	}
}

// rewriteModelField makes a proxied response report the alias the client asked
// for, rather than the upstream's internal id.
func rewriteModelField(body []byte, alias string) []byte {
	if alias == "" {
		return body
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body
	}
	if _, ok := fields["model"]; !ok {
		return body
	}
	fields["model"] = mustMarshal(alias)
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}
