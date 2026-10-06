package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type modelCard struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	// Non-standard fields are how a generic `openai-models-list` discovery
	// client learns the real context and output limits of an alias.
	ContextLength int `json:"context_length,omitempty"`
	MaxModelLen   int `json:"max_model_len,omitempty"`

	Limits struct {
		MaxInputTokens  int `json:"max_input_tokens,omitempty"`
		MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	} `json:"limits,omitempty"`

	SupportedEndpointTypes []string `json:"supported_endpoint_types,omitempty"`
}

// handleModels advertises the configured aliases, not upstream inventory: a
// client must only ever see names the router can actually serve.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return
	}
	if !s.authorized(w, r) {
		return
	}

	id := modelIDFromPath(r.URL.Path)

	now := time.Now().Unix()
	names := s.aliasNames()

	if id != "" {
		alias, ok := s.cfg.Models[id]
		if !ok {
			s.writeError(w, http.StatusNotFound, "unknown_model", "no route for model "+id)
			return
		}
		card := s.card(id, &alias, now)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
		return
	}

	cards := make([]modelCard, 0, len(names))
	for _, n := range names {
		alias := s.cfg.Models[n]
		cards = append(cards, s.card(n, &alias, now))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": cards})
}

// modelIDFromPath pulls the id out of a ".../v1/models/<id>" request whatever
// base prefix precedes it. A client pointed at /api/v1 must resolve a single
// model the same way one pointed at /v1 does, so the prefix is matched rather
// than assumed.
func modelIDFromPath(path string) string {
	i := strings.LastIndex(path, "/v1/models")
	if i < 0 {
		return ""
	}
	return strings.Trim(path[i+len("/v1/models"):], "/")
}

func (s *Server) card(name string, alias *Alias, created int64) modelCard {
	c := modelCard{
		ID:      name,
		Object:  "model",
		Created: created,
		OwnedBy: "omp-router",
	}
	c.ContextLength = alias.ContextWindow
	c.MaxModelLen = alias.ContextWindow
	c.Limits.MaxInputTokens = alias.ContextWindow
	c.Limits.MaxOutputTokens = alias.MaxOutputTokens
	switch alias.API {
	case APIAnthropicMessages:
		c.SupportedEndpointTypes = []string{"anthropic"}
	case APIOpenAICompletions:
		c.SupportedEndpointTypes = []string{"openai"}
	case APIOpenAIResponses:
		c.SupportedEndpointTypes = []string{"openai", "responses"}
	}
	return c
}

// modelInfo is the LiteLLM-shaped metadata entry. Clients that use rich
// discovery read limits and pricing from here rather than guessing.
type modelInfo struct {
	MaxInputTokens  int     `json:"max_input_tokens"`
	MaxTokens       int     `json:"max_tokens"`
	Mode            string  `json:"mode"`
	LiteLLMProvider string  `json:"litellm_provider"`
	InputCostPerTok float64 `json:"input_cost_per_token"`
	OutputCostPerTk float64 `json:"output_cost_per_token"`
	CacheReadCost   float64 `json:"cache_read_input_token_cost"`
	CacheWriteCost  float64 `json:"cache_creation_input_token_cost"`

	// Providers are the upstreams this alias can be served from. The capability
	// flags come from the model capability table, so a client can tell what it
	// may ask for before sending a request rather than discovering it from a
	// 400.
	Providers                 []string `json:"providers,omitempty"`
	SupportsVision            bool     `json:"supports_vision"`
	SupportsFunctionCalling   bool     `json:"supports_function_calling"`
	SupportsReasoning         bool     `json:"supports_reasoning"`
	SupportsPromptCaching     bool     `json:"supports_prompt_caching"`
	SupportsAdaptiveThinking  bool     `json:"supports_adaptive_thinking"`
	SupportedReasoningEfforts []string `json:"supported_reasoning_efforts,omitempty"`
}

// aliasProviders lists the distinct upstreams an alias can be served from, in
// declaration order.
func aliasProviders(a Alias) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range a.Targets {
		if !seen[t.Upstream] {
			seen[t.Upstream] = true
			out = append(out, t.Upstream)
		}
	}
	return out
}

func (s *Server) handleModelGroupInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return
	}
	if !s.authorized(w, r) {
		return
	}

	table, err := loadCapabilities()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "capability_table", err.Error())
		return
	}

	entries := make([]map[string]any, 0, len(s.cfg.Models))
	for _, name := range s.aliasNames() {
		alias := s.cfg.Models[name]
		info := modelInfo{
			MaxInputTokens:  alias.ContextWindow,
			MaxTokens:       alias.MaxOutputTokens,
			Mode:            "chat",
			LiteLLMProvider: "openrouter",
			InputCostPerTok: alias.Cost.Input / 1_000_000,
			OutputCostPerTk: alias.Cost.Output / 1_000_000,
			CacheReadCost:   alias.Cost.CacheRead / 1_000_000,
			CacheWriteCost:  alias.Cost.CacheWrite / 1_000_000,
			Providers:       aliasProviders(alias),
		}
		// Declared metadata stays authoritative; the table only fills gaps and
		// supplies capabilities, so a stale snapshot can never override a
		// deliberate choice in router.yaml.
		if facts, ok := table.LookupAlias(alias); ok {
			if info.MaxInputTokens == 0 {
				info.MaxInputTokens = facts.MaxInputTokens
			}
			if info.MaxTokens == 0 {
				info.MaxTokens = facts.MaxOutputTokens
			}
			info.SupportsVision = facts.Supports(flagVision)
			info.SupportsFunctionCalling = facts.Supports(flagFunctionCalling)
			info.SupportsReasoning = facts.Supports(flagReasoning)
			info.SupportsPromptCaching = facts.Supports(flagPromptCaching)
			info.SupportsAdaptiveThinking = facts.Supports(flagAdaptiveThinking)
			info.SupportedReasoningEfforts = supportedEfforts(facts)
		}
		entries = append(entries, map[string]any{
			"model_name": name,
			"model_info": info,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": entries})
}
