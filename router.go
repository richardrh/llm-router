package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Router owns selection state: which target to try next, and which upstream a
// session is pinned to.
type Router struct {
	cfg       *Config
	upstreams map[string]*upstreamState
	log       *slog.Logger

	mu     sync.Mutex
	sticky map[string]*stickyEntry
	// affinity is nil when cache-aware selection is disabled.
	affinity *cacheAffinity
}

type stickyEntry struct {
	target Target
	expiry time.Time
}

const stickyTTL = 2 * time.Hour

// maxStickyEntries bounds memory for a long-running router. Eviction is
// approximate: the map is cleared wholesale once it grows past the cap.
const maxStickyEntries = 10_000

func NewRouter(cfg *Config, log *slog.Logger) (*Router, error) {
	r := &Router{
		cfg:       cfg,
		upstreams: make(map[string]*upstreamState, len(cfg.Upstreams)),
		log:       log,
		sticky:    make(map[string]*stickyEntry),
	}
	for name, u := range cfg.Upstreams {
		// A cli upstream holds no credential of its own: the command runs under
		// the operator's login and owns its own token store. Resolving a key for
		// it would be the very intermediation this design avoids.
		if u.kind() == UpstreamCLI {
			r.upstreams[name] = newUpstreamState(name, u, "", cfg.Defaults.BreakerFailures)
			continue
		}
		key := u.APIKey
		if u.APIKeyEnv != "" {
			key = strings.TrimSpace(os.Getenv(u.APIKeyEnv))
		}
		if u.APIKeyEnv != "" && key == "" {
			log.Warn("upstream has no credential; requests will be unauthenticated",
				"upstream", name, "env", u.APIKeyEnv)
		}
		r.upstreams[name] = newUpstreamState(name, u, key, cfg.Defaults.BreakerFailures)
	}
	if cfg.cacheAffinityOn() {
		r.affinity = newCacheAffinity(cfg.Defaults.CacheAffinityTTL.Duration(), maxAffinityEntries)
	}
	return r, nil
}

// candidate is one ordered attempt: which upstream, and which model id to ask
// it for.
type candidate struct {
	upstream *upstreamState
	target   Target
}

// orderedCandidates returns the attempt plan for an alias: the first pick,
// then every remaining target in configuration order so a later failure can
// still fail over.
//
// The first pick is decided by the session pin when there is one, otherwise by
// whichever upstream already holds this conversation's cached prefix, otherwise
// by weight. A pin wins because it is an explicit statement about where a
// session belongs; affinity only decides what a pin would otherwise decide
// blindly.
func (r *Router) orderedCandidates(aliasName string, alias *Alias, stickyKey, fingerprint string, now time.Time) []candidate {
	all := r.allCandidates(aliasName, alias)
	if len(all) == 0 {
		return nil
	}

	first := -1
	switch {
	case stickyKey != "" && r.cfg.stickyFor(alias):
		first = r.stickyIndex(aliasName, stickyKey, all, now)
	default:
		if first = r.affinityIndex(all, fingerprint, now); first < 0 {
			first = r.weightedIndex(all, now)
		}
	}
	if first < 0 {
		return nil
	}

	out := make([]candidate, 0, len(all))
	out = append(out, all[first])
	for i, c := range all {
		if i != first {
			out = append(out, c)
		}
	}
	return out
}

// affinityIndex picks among the candidates whose upstream is known to hold a
// warm cache for this prefix, respecting their weights. It returns -1 when
// nothing is warm, leaving the caller to choose normally.
func (r *Router) affinityIndex(all []candidate, fingerprint string, now time.Time) int {
	warm := r.affinity.warm(fingerprint, now)
	if len(warm) == 0 {
		return -1
	}
	subset := make([]int, 0, len(all))
	total := 0
	for i, c := range all {
		// Failover-only targets are never chosen proactively, warm or not.
		if c.target.weight() == 0 || !warm[c.upstream.name] || !c.upstream.available(now) {
			continue
		}
		subset = append(subset, i)
		total += c.target.weight()
	}
	if len(subset) == 0 {
		return -1
	}
	pick := rand.IntN(total)
	for _, idx := range subset {
		pick -= all[idx].target.weight()
		if pick < 0 {
			return idx
		}
	}
	return subset[len(subset)-1]
}

func (r *Router) allCandidates(aliasName string, alias *Alias) []candidate {
	out := make([]candidate, 0, len(alias.Targets))
	for _, t := range alias.Targets {
		up, ok := r.upstreams[t.Upstream]
		if !ok {
			continue
		}
		if t.Model == "" {
			t.Model = aliasName
		}
		out = append(out, candidate{upstream: up, target: t})
	}
	return out
}

// prefixStickyKey derives a stable conversation key when the client sends no
// session header. It hashes the opening messages rather than the whole body:
// a conversation is append-only, so its opening turn is identical across every
// later turn, which is exactly the span a provider prompt cache keys on.
func (p *requestPlan) prefixStickyKey() string {
	raw, ok := p.fields["messages"]
	if !ok {
		return ""
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil || len(messages) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(p.alias))
	h.Write([]byte{0})
	// Opening turn plus the first reply: together they identify a conversation
	// while staying byte-identical as the transcript grows.
	for _, m := range messages[:min(len(messages), 2)] {
		h.Write(truncate(m, 4096))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func truncate(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	return b[:max]
}

// weightedIndex picks a healthy target by weight. When every weighted target
// is unhealthy it falls back to declared order, so a request is still attempted
// rather than refused outright.
func (r *Router) weightedIndex(all []candidate, now time.Time) int {
	healthy := make([]int, 0, len(all))
	total := 0
	for i, c := range all {
		if c.target.weight() > 0 && c.upstream.available(now) {
			healthy = append(healthy, i)
			total += c.target.weight()
		}
	}
	if len(healthy) == 0 {
		return declaredOrderIndex(all)
	}
	pick := rand.IntN(total)
	for _, idx := range healthy {
		pick -= all[idx].target.weight()
		if pick < 0 {
			return idx
		}
	}
	return healthy[len(healthy)-1]
}

// declaredOrderIndex is the fallback when no weighted target is currently
// healthy. Preference order matters: a declared-order weighted target first,
// then any target that is still reachable (including a weight-0 failover-only
// backup), and only as a last resort a target we already know is down.
func declaredOrderIndex(all []candidate) int {
	now := time.Now()
	for i, c := range all {
		if c.target.weight() > 0 && c.upstream.available(now) {
			return i
		}
	}
	for i, c := range all {
		if c.upstream.available(now) {
			return i
		}
	}
	for i := range all {
		if all[i].target.weight() > 0 {
			return i
		}
	}
	return 0
}

func (r *Router) stickyIndex(aliasName, stickyKey string, all []candidate, now time.Time) int {
	r.mu.Lock()
	entry, ok := r.sticky[aliasName+"\x00"+stickyKey]
	if ok && now.After(entry.expiry) {
		delete(r.sticky, aliasName+"\x00"+stickyKey)
		ok = false
	}
	r.mu.Unlock()

	if ok {
		for i, c := range all {
			if c.target.Upstream == entry.target.Upstream && c.target.Model == entry.target.Model {
				if c.upstream.available(now) {
					return i
				}
				break
			}
		}
		// Pinned target is unhealthy: fall through to normal selection.
	}

	idx := r.weightedIndex(all, now)
	r.bindSticky(aliasName, stickyKey, all[idx].target, now.Add(stickyTTL))
	return idx
}

func (r *Router) bindSticky(aliasName, key string, target Target, expiry time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sticky) >= maxStickyEntries {
		r.sticky = make(map[string]*stickyEntry, maxStickyEntries/4)
	}
	r.sticky[aliasName+"\x00"+key] = &stickyEntry{target: target, expiry: expiry}
}

func stickyKeyFrom(h http.Header) string {
	for _, name := range []string{"x-omp-session", "x-session-id", "x-conversation-id", "x-sticky-key"} {
		if v := h.Get(name); v != "" {
			return v
		}
	}
	return ""
}

// requestPlan is the buffered, per-target view of a client request. Buffering is
// what makes pre-commit failover possible at all: the body has to survive to be
// replayed against the next upstream.
type requestPlan struct {
	fields map[string]json.RawMessage
	stream bool
	alias  string
	// api is the wire protocol the client spoke, which decides whether
	// OpenAI-only conveniences like stream_options may be injected.
	api APIProtocol
	// fingerprint caches the conversation's prefix hash, computed once and
	// shared by session pinning and cache observation.
	fingerprint    string
	fingerprintSet bool
}

// cacheFingerprint returns the conversation's cacheable prefix hash, computing
// it at most once per request.
func (p *requestPlan) cacheFingerprint() string {
	if !p.fingerprintSet {
		p.fingerprint = p.prefixStickyKey()
		p.fingerprintSet = true
	}
	return p.fingerprint
}

func newRequestPlan(body []byte, maxBytes int64) (*requestPlan, error) {
	if int64(len(body)) > maxBytes {
		return nil, errBodyTooLarge
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, errBadJSON
	}
	p := &requestPlan{fields: fields}
	p.stream = boolField(fields, "stream")
	return p, nil
}

// bodyFor renders the request body for one target: the alias is replaced with
// the target's model id, the body is converted to the upstream's wire if that
// differs from the client's, and then the upstream's field shaping is applied
// and finally the target's.
//
// Translation happens before shaping on purpose. bodyPatch and bodyDrop
// configure the request the upstream receives, so on a translating target they
// speak the upstream's vocabulary: a native Anthropic target takes
// `thinking: {type: adaptive}`, not whatever OpenAI would call it.
func (p *requestPlan) bodyFor(c candidate, injectUsage bool, upstreamAPI APIProtocol) ([]byte, error) {
	fields := make(map[string]json.RawMessage, len(p.fields)+len(c.upstream.cfg.BodyPatch)+len(c.target.BodyPatch)+2)
	for k, v := range p.fields {
		fields[k] = v
	}
	fields["model"] = mustMarshal(c.target.Model)

	if upstreamAPI != p.api {
		rendered, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		converted, err := translateRequest(p.api, upstreamAPI, rendered)
		if err != nil {
			return nil, err
		}
		fields = make(map[string]json.RawMessage, len(c.upstream.cfg.BodyPatch)+len(c.target.BodyPatch)+2)
		if err := json.Unmarshal(converted, &fields); err != nil {
			return nil, fmt.Errorf("translated request is not a JSON object: %w", err)
		}
	}

	if err := applyBodyShaping(fields, c.upstream.cfg.BodyDrop, c.upstream.cfg.BodyPatch); err != nil {
		return nil, err
	}
	if err := applyBodyShaping(fields, c.target.BodyDrop, c.target.BodyPatch); err != nil {
		return nil, err
	}
	// stream_options is an OpenAI-wire field. The Anthropic Messages API has no
	// equivalent, rejects unknown parameters, and reports usage in its own stream
	// events, so it is never sent there. That is why the condition is about the
	// upstream's wire rather than the client's.
	if injectUsage && p.stream && upstreamAPI != APIAnthropicMessages {
		fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}
	return json.Marshal(fields)
}

// applyBodyShaping removes then sets top-level fields. A patched value replaces
// whatever the client sent under that key rather than merging into it, so what
// the config names is exactly what the upstream receives.
func applyBodyShaping(fields map[string]json.RawMessage, drop []string, patch map[string]any) error {
	for _, k := range drop {
		delete(fields, k)
	}
	for k, v := range patch {
		encoded, err := json.Marshal(v)
		if err != nil {
			return err
		}
		fields[k] = encoded
	}
	return nil
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}

func boolField(fields map[string]json.RawMessage, key string) bool {
	raw, ok := fields[key]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false
	}
	return b
}

// requestID derives a stable per-request log id without hashing any prompt
// content.
func requestID(method, path string, seq uint64) string {
	h := fnv.New64a()
	h.Write([]byte(method))
	h.Write([]byte(path))
	h.Write([]byte{'#'})
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], seq)
	h.Write(buf[:])
	return strconv.FormatUint(h.Sum64(), 36)
}

// drainAndClose consumes a rejected response so the connection can be reused.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, body, 64<<10)
	_ = body.Close()
}
