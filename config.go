package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a Go duration string ("30s", "2m").
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// APIProtocol is the wire protocol a client speaks to the router. Every target
// for an alias must speak the same protocol; the router does not translate.
type APIProtocol string

const (
	APIOpenAICompletions APIProtocol = "openai-completions"
	APIOpenAIResponses   APIProtocol = "openai-responses"
	APIAnthropicMessages APIProtocol = "anthropic-messages"
)

func (p APIProtocol) Path() (string, error) {
	switch p {
	case APIOpenAICompletions:
		return "/chat/completions", nil
	case APIOpenAIResponses:
		return "/responses", nil
	case APIAnthropicMessages:
		return "/messages", nil
	}
	return "", fmt.Errorf("unknown api %q", p)
}

type Config struct {
	Listen    string              `yaml:"listen"`
	APIKey    string              `yaml:"apiKey"`
	Defaults  Defaults            `yaml:"defaults"`
	Store     StoreConfig         `yaml:"store"`
	Upstreams map[string]Upstream `yaml:"upstreams"`
	Models    map[string]Alias    `yaml:"models"`

	// Warnings are non-fatal problems found while loading, such as metadata
	// that disagrees with the model capability table. They are reported to the
	// operator but do not stop the router.
	Warnings []string `yaml:"-"`
}

// StoreConfig configures the local usage database. An empty path disables it,
// and the router then keeps no record of what it served beyond the log.
type StoreConfig struct {
	// Path is the SQLite file. A missing parent directory is created. WAL mode
	// is enabled, so an external reader can query it while the router writes,
	// and `-wal`/`-shm` files sit alongside it.
	Path string `yaml:"path"`
	// QueueSize bounds how many records may wait to be written. Past it, records
	// are dropped and counted, so a slow disk can never slow down or fail a
	// request. Zero takes the default.
	QueueSize int `yaml:"queueSize"`
	// MaxRows caps how many records are kept, oldest deleted first, so the file
	// cannot grow without bound. Rows are near-uniform at roughly 80 bytes, so
	// 130000 is about 10 MB; /usage reports the real size. Zero keeps everything.
	MaxRows int64 `yaml:"maxRows"`
}

type Defaults struct {
	// MaxAttempts bounds actual upstream dispatches per client request. The
	// remaining configured targets stay available on the next request.
	MaxAttempts int `yaml:"maxAttempts"`
	// Sticky pins a session to one target so provider-side prompt caches hit.
	Sticky bool `yaml:"sticky"`
	// StickyByPrefix derives the session key from the conversation prefix when
	// the client sends no session header, which desktop agents do not by
	// default. Consecutive turns of one conversation share a stable opening
	// prompt, so hashing it pins the whole conversation to one target.
	StickyByPrefix bool `yaml:"stickyByPrefix"`
	// FirstByteTimeout bounds time-to-response-headers for one attempt.
	FirstByteTimeout Duration `yaml:"firstByteTimeout"`
	// StreamIdleTimeout aborts a stream with no bytes flowing for this long.
	StreamIdleTimeout Duration `yaml:"streamIdleTimeout"`
	// MaxStreamDuration bounds a single streaming response end to end.
	MaxStreamDuration Duration `yaml:"maxStreamDuration"`
	// RewriteModel rewrites the upstream model id in responses back to the alias.
	RewriteModel bool `yaml:"rewriteModel"`
	// InjectStreamUsage adds stream_options.include_usage when a client streams.
	InjectStreamUsage bool `yaml:"injectStreamUsage"`
	// MaxBodyBytes bounds a buffered client request body.
	MaxBodyBytes int64 `yaml:"maxBodyBytes"`
	// BreakerFailures trips an upstream after this many consecutive failures.
	BreakerFailures int `yaml:"breakerFailures"`
	// BreakerCooldown is how long a tripped upstream stays out of rotation.
	BreakerCooldown Duration `yaml:"breakerCooldown"`
	// RateLimitCooldown is the fallback window when a 429 has no Retry-After.
	RateLimitCooldown Duration `yaml:"rateLimitCooldown"`
	// CacheAffinity prefers the target whose prompt cache is warm for a
	// conversation's prefix, learned from observed cache reads, whenever no
	// session pin applies. Defaults to true.
	CacheAffinity *bool `yaml:"cacheAffinity"`
	// CacheAffinityTTL is how long an observation stays valid. Providers expire
	// their prompt caches, so an observation that outlived them would route to a
	// target whose cache is already cold.
	CacheAffinityTTL Duration `yaml:"cacheAffinityTTL"`
}

// AuthStyle is how an upstream key is presented on the wire.
type AuthStyle string

const (
	// AuthBearer is the OpenAI convention: Authorization: Bearer <key>.
	AuthBearer AuthStyle = "bearer"
	// AuthAnthropic additionally sends x-api-key and an anthropic-version
	// header, which the Claude API requires. Both spellings of the key are
	// sent because the native /v1/messages endpoint reads x-api-key while the
	// OpenAI-compatible /v1/chat/completions endpoint reads Authorization.
	AuthAnthropic AuthStyle = "anthropic"
)

// UpstreamKind is how an upstream is reached.
type UpstreamKind string

const (
	// UpstreamHTTP posts to BaseURL over HTTP using an API key. The default.
	UpstreamHTTP UpstreamKind = "http"
	// UpstreamCLI runs a local command and reads its output. This exists so a
	// subscription-backed agent CLI can be used without the router ever holding
	// its credentials: the command runs under the operator's own login and owns
	// its own token store, and the router only sees the text it prints.
	UpstreamCLI UpstreamKind = "cli"
)

// defaultAnthropicVersion is the API version header Claude expects. It is
// applied only when the upstream config does not set one itself.
const defaultAnthropicVersion = "2023-06-01"

type Upstream struct {
	BaseURL string `yaml:"baseUrl"`
	// APIKey is a literal secret. Prefer APIKeyEnv.
	APIKey string `yaml:"apiKey"`
	// APIKeyEnv names an environment variable holding the secret.
	APIKeyEnv string `yaml:"apiKeyEnv"`
	// AuthStyle selects the credential headers: "bearer" (the default) or
	// "anthropic".
	AuthStyle AuthStyle `yaml:"authStyle"`
	// Headers are added to every upstream request, overriding client headers.
	Headers map[string]string `yaml:"headers"`
	// BodyPatch is shallow-merged into the top level of the JSON request body.
	BodyPatch map[string]any `yaml:"bodyPatch"`
	// BodyDrop removes top-level JSON body fields before dispatch.
	BodyDrop []string `yaml:"bodyDrop"`
	// MaxConcurrency caps in-flight requests. Full targets are skipped, not queued.
	MaxConcurrency int `yaml:"maxConcurrency"`

	// Kind selects how this upstream is reached: "http" (the default) posts to
	// BaseURL, "cli" runs Command as a local subprocess.
	Kind UpstreamKind `yaml:"kind"`
	// Command is the argv for a cli upstream, for example
	// ["claude", "-p", "{prompt}", "--output-format", "stream-json", "--verbose",
	// "--include-partial-messages"]. It must contain "{prompt}" exactly once;
	// that element is replaced with the rendered conversation. The command must
	// print Claude Code's stream-json NDJSON on stdout.
	Command []string `yaml:"command"`
	// Timeout bounds one cli invocation. Defaults to maxStreamDuration.
	Timeout Duration `yaml:"timeout"`
}

// kind reports how the upstream is reached, defaulting to HTTP so that an
// existing config needs no change.
func (u Upstream) kind() UpstreamKind {
	if u.Kind == "" {
		return UpstreamHTTP
	}
	return u.Kind
}

type Alias struct {
	// API is the protocol clients speak for this alias.
	API APIProtocol `yaml:"api"`
	// Sticky overrides Defaults.Sticky for this alias.
	Sticky *bool `yaml:"sticky"`
	// Targets are tried in order after the first pick. Weight 0 means
	// failover-only: never picked while a weighted target is healthy.
	Targets []Target `yaml:"targets"`
	// ContextWindow and MaxOutputTokens are advertised to clients and used for
	// cost estimation. They do not change what the upstream accepts.
	ContextWindow   int `yaml:"contextWindow"`
	MaxOutputTokens int `yaml:"maxOutputTokens"`
	// Cost is per-million-token USD, advertised through the LiteLLM-shaped
	// metadata endpoint and used for request cost estimates.
	Cost Cost `yaml:"cost"`
}

type Cost struct {
	Input      float64 `yaml:"input" json:"input"`
	Output     float64 `yaml:"output" json:"output"`
	CacheRead  float64 `yaml:"cacheRead" json:"cacheRead"`
	CacheWrite float64 `yaml:"cacheWrite" json:"cacheWrite"`
}

type Target struct {
	Upstream string `yaml:"upstream"`
	// Model is the id sent upstream. Empty reuses the alias name.
	Model string `yaml:"model"`
	// Weight is the relative selection weight. Negative is rejected.
	Weight int `yaml:"weight"`
	// API is the wire protocol this upstream speaks for this target. Empty
	// means it speaks whatever the client does, so nothing needs translating.
	// Setting a different value makes the router translate between the two
	// wires, which is what lets one alias reach a native Anthropic endpoint
	// from an OpenAI-shaped client, or the reverse.
	API APIProtocol `yaml:"api"`
	// BodyPatch sets top-level JSON body fields for this target only, after the
	// upstream's own patch. This is how one alias speaks OpenRouter's
	// vocabulary to OpenRouter and Anthropic's to Claude.
	BodyPatch map[string]any `yaml:"bodyPatch"`
	// BodyDrop removes top-level JSON body fields for this target only, after
	// the upstream's own drops.
	BodyDrop []string `yaml:"bodyDrop"`
}

func (t Target) weight() int {
	if t.Weight < 0 {
		return 0
	}
	return t.Weight
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8787"
	}
	d := &c.Defaults
	if d.MaxAttempts == 0 {
		d.MaxAttempts = 3
	}
	if d.FirstByteTimeout == 0 {
		d.FirstByteTimeout = Duration(120 * time.Second)
	}
	if d.StreamIdleTimeout == 0 {
		d.StreamIdleTimeout = Duration(180 * time.Second)
	}
	if d.MaxStreamDuration == 0 {
		d.MaxStreamDuration = Duration(45 * time.Minute)
	}
	if d.MaxBodyBytes == 0 {
		d.MaxBodyBytes = 32 << 20
	}
	if d.BreakerFailures == 0 {
		d.BreakerFailures = 4
	}
	if d.BreakerCooldown == 0 {
		d.BreakerCooldown = Duration(30 * time.Second)
	}
	if d.RateLimitCooldown == 0 {
		d.RateLimitCooldown = Duration(20 * time.Second)
	}
	if d.CacheAffinityTTL == 0 {
		// Providers' default prompt cache lifetime is five minutes, so an
		// observation older than that describes a cache that no longer exists.
		d.CacheAffinityTTL = Duration(5 * time.Minute)
	}
	// Assign through the map key: a range variable is a copy, so mutating it
	// would leave every upstream at zero concurrency.
	for name, u := range c.Upstreams {
		if u.MaxConcurrency == 0 {
			u.MaxConcurrency = 8
		}
		if u.AuthStyle == "" {
			u.AuthStyle = AuthBearer
		}
		c.Upstreams[name] = u
	}
}

func (c *Config) validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	// Warnings are non-fatal findings. They are collected in a set so a problem
	// with an alias is reported once rather than once per target, and sorted so
	// the output is stable.
	warns := map[string]bool{}
	warn := func(format string, args ...any) { warns[fmt.Sprintf(format, args...)] = true }

	// A directory in store.path would otherwise surface as an opaque failure
	// from the SQLite driver at startup.
	if c.Store.QueueSize < 0 {
		add("store.queueSize must not be negative")
	}
	if c.Store.MaxRows < 0 {
		add("store.maxRows must not be negative")
	}
	if c.Store.Path != "" {
		if info, err := os.Stat(c.Store.Path); err == nil && info.IsDir() {
			add("store.path %q is a directory; name a file for the router to create", c.Store.Path)
		}
	}

	if len(c.Upstreams) == 0 {
		add("at least one upstream is required")
	}
	for name, u := range c.Upstreams {
		if strings.TrimSpace(name) == "" {
			add("upstream name must not be empty")
			continue
		}
		if u.BaseURL == "" && u.kind() == UpstreamHTTP {
			add("upstream %q: baseUrl is required", name)
		}
		if u.APIKey != "" && u.APIKeyEnv != "" {
			add("upstream %q: set apiKey or apiKeyEnv, not both", name)
		}
		switch u.kind() {
		case UpstreamHTTP:
			if len(u.Command) > 0 {
				add("upstream %q: command is only meaningful for a cli upstream", name)
			}
		case UpstreamCLI:
			if u.BaseURL != "" {
				add("upstream %q: baseUrl is not used by a cli upstream", name)
			}
			if len(u.Command) == 0 {
				add("upstream %q: command is required for a cli upstream", name)
			} else {
				placeholders := 0
				for _, arg := range u.Command {
					placeholders += strings.Count(arg, "{prompt}")
				}
				if placeholders != 1 {
					add("upstream %q: command must contain exactly one {prompt} placeholder, found %d", name, placeholders)
				}
			}
			// The whole point of a cli upstream is that the command already owns
			// a credential; giving it one too would be a contradiction, and the
			// router must never hold a subscription token.
			if u.APIKey != "" || u.APIKeyEnv != "" {
				add("upstream %q: a cli upstream manages its own credentials; drop apiKey and apiKeyEnv", name)
			}
		default:
			add("upstream %q: unknown kind %q (want %q or %q)", name, u.Kind, UpstreamHTTP, UpstreamCLI)
		}
		switch u.AuthStyle {
		case AuthBearer, AuthAnthropic:
		default:
			add("upstream %q: unknown authStyle %q (want %q or %q)", name, u.AuthStyle, AuthBearer, AuthAnthropic)
		}
		for k := range u.BodyPatch {
			if k == "model" {
				add("upstream %q: bodyPatch may not set \"model\"; model ids come from each target", name)
			}
		}
	}

	if len(c.Models) == 0 {
		add("at least one model alias is required")
	}
	for name, a := range c.Models {
		if strings.TrimSpace(name) == "" {
			add("model alias must not be empty")
			continue
		}
		if _, err := a.API.Path(); err != nil {
			add("model %q: %v", name, err)
		}
		if len(a.Targets) == 0 {
			add("model %q: needs at least one target", name)
		}
		weighted := 0
		seen := map[string]bool{}
		for i, t := range a.Targets {
			if t.Upstream == "" {
				add("model %q target %d: upstream is required", name, i)
				continue
			}
			if _, ok := c.Upstreams[t.Upstream]; !ok {
				add("model %q target %d: unknown upstream %q", name, i, t.Upstream)
			}
			if t.Weight < 0 {
				add("model %q target %d: weight must be >= 0", name, i)
			}
			if t.API != "" {
				if _, err := t.API.Path(); err != nil {
					add("model %q target %d: %v", name, i, err)
				} else if !translatable(a.API, t.API) {
					add("model %q target %d: no translation from client %s to upstream %s", name, i, a.API, t.API)
				}
			}
			// A cli upstream yields text and nothing else, so there is no
			// renderer for a Responses-API client to consume.
			if up, ok := c.Upstreams[t.Upstream]; ok && up.kind() == UpstreamCLI && a.API == APIOpenAIResponses {
				add("model %q target %d: a cli upstream cannot serve an openai-responses client", name, i)
			}
			// A patch on "model" would be silently undone by the router's own
			// rewrite, and it reads as if it selects a model. Point at the
			// field that does.
			for k := range t.BodyPatch {
				if k == "model" {
					add("model %q target %d: bodyPatch may not set \"model\"; use the target's model field", name, i)
				}
			}
			checkTargetCapabilities(name, a, i, t, warn, add)
			key := t.Upstream + "\x00" + t.Model
			if seen[key] {
				add("model %q target %d: duplicate upstream/model %q", name, i, t.Upstream+"/"+t.Model)
			}
			seen[key] = true
			weighted += t.weight()
		}
		if weighted == 0 && len(a.Targets) > 0 {
			add("model %q: every target has weight 0, so nothing is ever selected; give at least one target a positive weight", name)
		}
	}

	if c.Defaults.MaxAttempts < 1 {
		add("defaults.maxAttempts must be >= 1")
	}

	if len(warns) > 0 {
		c.Warnings = make([]string, 0, len(warns))
		for w := range warns {
			c.Warnings = append(c.Warnings, w)
		}
		sort.Strings(c.Warnings)
	}

	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
}

// stickyFor reports whether session stickiness applies to an alias.
func (c *Config) stickyFor(a *Alias) bool {
	if a.Sticky != nil {
		return *a.Sticky
	}
	return c.Defaults.Sticky
}

// cacheAffinityOn reports whether cache-aware selection is enabled. It defaults
// to on: it only ever breaks ties among equally eligible targets, so there is
// nothing for an operator to opt into.
func (c *Config) cacheAffinityOn() bool {
	if c.Defaults.CacheAffinity == nil {
		return true
	}
	return *c.Defaults.CacheAffinity
}

// upstreamsInOrder gives a deterministic name list for logs and validation output.
func (c *Config) upstreamsInOrder() []string {
	names := make([]string, 0, len(c.Upstreams))
	for n := range c.Upstreams {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
