package main

// capabilities.json holds per-model facts: input/output limits, pricing per
// million tokens, and capability flags.
//
// It is a curated snapshot of the Claude models this router fronts, transcribed
// from the providers' published limits and pricing and cross-checked against
// LiteLLM's model map (model_prices_and_context_window.json), which tracks the
// same facts for the same models. Prices are per million tokens, matching the
// units in router.yaml.
//
// The table exists so a configuration can be checked against reality at load
// time instead of failing as an opaque 400 from a provider. It is deliberately
// conservative about what it enforces: an unknown model is never an error, and
// limits and pricing are only ever reported as warnings. Capability flags are
// the one exception, because asking a model for a thinking mode it rejects is a
// guaranteed failure rather than a judgement call. A stale snapshot must never
// block a setup that works.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

//go:embed capabilities.json
var capabilityData []byte

// Capability flag names, as they appear in capabilities.json.
const (
	flagAdaptiveThinking = "adaptiveThinking"
	flagThinkingAlwaysOn = "thinkingAlwaysOn"
	flagLegacyThinking   = "legacyThinking"
	flagOutputConfig     = "outputConfig"
	flagXhighEffort      = "xhighEffort"
	flagMaxEffort        = "maxEffort"
	flagPromptCaching    = "promptCaching"
	flagNativeStructured = "nativeStructuredOutput"
	flagVision           = "vision"
	flagFunctionCalling  = "functionCalling"
	flagReasoning        = "reasoning"
	flagFastMode         = "fastMode"
)

// ModelFacts is what the router knows about one model.
type ModelFacts struct {
	MaxInputTokens  int             `json:"maxInputTokens"`
	MaxOutputTokens int             `json:"maxOutputTokens"`
	Cost            Cost            `json:"cost"`
	Flags           map[string]bool `json:"capabilities"`
}

// Supports reports whether the model carries a capability flag.
func (f ModelFacts) Supports(flag string) bool { return f.Flags[flag] }

// capabilityTable indexes ModelFacts by normalised model id.
type capabilityTable struct {
	byID map[string]ModelFacts
}

// loadCapabilities parses the embedded table once. A failure is a build-time
// defect in the embedded file, which TestCapabilityTableParses guards, so it is
// surfaced rather than panicked.
var loadCapabilities = sync.OnceValues(func() (*capabilityTable, error) {
	var byID map[string]ModelFacts
	if err := json.Unmarshal(capabilityData, &byID); err != nil {
		return nil, fmt.Errorf("capabilities.json: %w", err)
	}
	return &capabilityTable{byID: byID}, nil
})

// Lookup resolves any spelling of a model id to its facts. The boolean is false
// for a model the table does not know, which callers treat as "nothing to
// check" rather than as an error.
func (t *capabilityTable) Lookup(modelID string) (ModelFacts, bool) {
	facts, ok := t.byID[normalizeModelID(modelID)]
	return facts, ok
}

// LookupAlias resolves the first target model the table recognises. An alias's
// targets are spellings of one model across providers, so any of them identifies
// it.
func (t *capabilityTable) LookupAlias(a Alias) (ModelFacts, bool) {
	for _, target := range a.Targets {
		if facts, ok := t.Lookup(target.Model); ok {
			return facts, true
		}
	}
	return ModelFacts{}, false
}

// supportedEfforts lists the output_config.effort levels a model accepts. The
// three base levels are available wherever effort is supported at all; xhigh and
// max are individually gated, which is why they are derived from flags rather
// than assumed.
func supportedEfforts(f ModelFacts) []string {
	if !f.Supports(flagOutputConfig) {
		return nil
	}
	efforts := []string{"low", "medium", "high"}
	if f.Supports(flagXhighEffort) {
		efforts = append(efforts, "xhigh")
	}
	if f.Supports(flagMaxEffort) {
		efforts = append(efforts, "max")
	}
	return efforts
}

// normalizeModelID maps every spelling of a model onto the table's key, so the
// OpenRouter slug, its floating and variant forms, and Claude's own id all
// resolve to one entry:
//
//	anthropic/claude-opus-5.5        -> claude-opus-5-5
//	anthropic/claude-opus-5.5:nitro  -> claude-opus-5-5
//	~anthropic/claude-opus-latest    -> claude-opus-latest
//	claude-opus-5-5                  -> claude-opus-5-5
//
// OpenRouter spells versions with dots where Claude uses dashes, so dots become
// dashes. A trailing variant tag is dropped because it selects a routing
// preference within one model, not a different model.
func normalizeModelID(id string) string {
	id = strings.TrimPrefix(id, "~")
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.Index(id, ":"); i >= 0 {
		id = id[:i]
	}
	return strings.ReplaceAll(id, ".", "-")
}

// checkTargetCapabilities compares one target against the model capability table
// and reports findings through the caller's warn and fail sinks.
//
// Limits and pricing are warnings only. The table is a snapshot of published
// facts and a stale entry must never stop a setup that works: the operator is
// told, and decides.
//
// Capability mismatches are errors, because they are not judgement calls. Asking
// a model for a thinking mode it rejects fails at request time with an opaque
// 400 from the provider, and no amount of failover fixes it.
func checkTargetCapabilities(alias string, declared Alias, i int, t Target, warn, fail func(string, ...any)) {
	table, err := loadCapabilities()
	if err != nil {
		fail("model %q target %d: %v", alias, i, err)
		return
	}
	facts, known := table.Lookup(t.Model)
	if !known {
		// An unknown model is not a mistake; the table has nothing to say.
		return
	}

	for _, f := range []struct {
		label    string
		declared float64
		known    float64
	}{
		{"cost.input", declared.Cost.Input, facts.Cost.Input},
		{"cost.output", declared.Cost.Output, facts.Cost.Output},
		{"cost.cacheRead", declared.Cost.CacheRead, facts.Cost.CacheRead},
		{"cost.cacheWrite", declared.Cost.CacheWrite, facts.Cost.CacheWrite},
	} {
		if costDiffers(f.declared, f.known) {
			warn("model %q: %s is %g but %s costs %g per MTok", alias, f.label, f.declared, t.Model, f.known)
		}
	}
	if declared.ContextWindow != 0 && declared.ContextWindow != facts.MaxInputTokens {
		warn("model %q: contextWindow %d disagrees with %s's %d", alias, declared.ContextWindow, t.Model, facts.MaxInputTokens)
	}
	if declared.MaxOutputTokens != 0 && declared.MaxOutputTokens != facts.MaxOutputTokens {
		warn("model %q: maxOutputTokens %d disagrees with %s's %d", alias, declared.MaxOutputTokens, t.Model, facts.MaxOutputTokens)
	}

	// thinking.type: which forms the model accepts. The two generations are
	// exact inverses, which is why neither value is safe as a default.
	if thinking, ok := t.BodyPatch["thinking"].(map[string]any); ok {
		kind, _ := thinking["type"].(string)
		switch kind {
		case "adaptive":
			if !facts.Supports(flagAdaptiveThinking) {
				fail("model %q target %d: %s rejects thinking.type=adaptive; it takes the explicit {type: enabled, budget_tokens: N} form", alias, i, t.Model)
			}
		case "enabled", "disabled":
			if facts.Supports(flagThinkingAlwaysOn) {
				fail("model %q target %d: thinking is always on for %s and thinking.type=%s is rejected; use {type: adaptive}", alias, i, t.Model, kind)
			}
		}
	}

	// output_config.effort exists only on the generations that accept it, and
	// its top two levels are individually gated.
	if oc, ok := t.BodyPatch["output_config"].(map[string]any); ok {
		effort, _ := oc["effort"].(string)
		switch {
		case effort == "":
		case !facts.Supports(flagOutputConfig):
			fail("model %q target %d: %s does not support output_config.effort", alias, i, t.Model)
		case effort == "xhigh" && !facts.Supports(flagXhighEffort):
			fail("model %q target %d: %s does not support effort %q", alias, i, t.Model, effort)
		case effort == "max" && !facts.Supports(flagMaxEffort):
			fail("model %q target %d: %s does not support effort %q", alias, i, t.Model, effort)
		}
	}
}

// costDiffers reports whether a declared price contradicts a known one. An
// undeclared or unknown price is not a disagreement.
func costDiffers(declared, known float64) bool {
	if declared == 0 || known == 0 {
		return false
	}
	diff := declared - known
	if diff < 0 {
		diff = -diff
	}
	return diff > 1e-6
}
