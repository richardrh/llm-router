package main

import (
	"bytes"
	"encoding/json"
)

// Usage is what a provider reported for one request, normalised across the two
// wires the router speaks.
//
// InputTokens counts only the uncached input: the portion billed at the base
// input rate. This matters because the wires disagree. The OpenAI wire reports
// prompt_tokens inclusive of cached tokens, while the Anthropic wire reports
// input_tokens exclusive of its separate cache fields. Normalising here keeps
// cost arithmetic honest instead of quietly double-billing cache reads.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// TotalInput is everything sent upstream, cached or not.
func (u Usage) TotalInput() int { return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens }

// Total is every token the provider billed for.
func (u Usage) Total() int { return u.TotalInput() + u.OutputTokens }

// Estimate prices a request in USD from the alias's declared per-million-token
// rates. It is an estimate in the honest sense: it uses the provider's own token
// counts, and the configured rates, and nothing else.
func (c Cost) Estimate(u Usage) float64 {
	return (float64(u.InputTokens)*c.Input +
		float64(u.OutputTokens)*c.Output +
		float64(u.CacheReadTokens)*c.CacheRead +
		float64(u.CacheWriteTokens)*c.CacheWrite) / 1_000_000
}

// wireUsageFields is the union of the usage objects both providers emit. Every
// field is a pointer so an absent field is distinguishable from a zero: the
// Anthropic stream reports input and output counts in separate events, and a
// later event must not zero out what an earlier one established.
type wireUsageFields struct {
	// OpenAI wire.
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	PromptDetails    *struct {
		CachedTokens        *int `json:"cached_tokens"`
		CacheCreationTokens *int `json:"cache_creation_tokens"`
	} `json:"prompt_tokens_details"`

	// Anthropic wire.
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
}

// merge folds one usage object into the accumulated totals.
func (u *Usage) merge(f *wireUsageFields) {
	// Anthropic: input_tokens excludes the cache fields, so they add on top.
	if f.InputTokens != nil {
		u.InputTokens = *f.InputTokens
		if f.CacheReadInputTokens != nil {
			u.CacheReadTokens = *f.CacheReadInputTokens
		}
		if f.CacheCreationInputTokens != nil {
			u.CacheWriteTokens = *f.CacheCreationInputTokens
		}
	}
	// OpenAI: prompt_tokens includes cached tokens, so subtract to recover the
	// uncached portion.
	if f.PromptTokens != nil {
		cached := 0
		if f.PromptDetails != nil && f.PromptDetails.CachedTokens != nil {
			cached = *f.PromptDetails.CachedTokens
		}
		u.CacheReadTokens = cached
		u.InputTokens = max(0, *f.PromptTokens-cached)
		if f.PromptDetails != nil && f.PromptDetails.CacheCreationTokens != nil {
			u.CacheWriteTokens = *f.PromptDetails.CacheCreationTokens
		}
	}
	// Both wires report output the same way, and Anthropic's message_delta
	// carries the running total, so a later value replaces an earlier one.
	if f.OutputTokens != nil {
		u.OutputTokens = *f.OutputTokens
	}
	if f.CompletionTokens != nil {
		u.OutputTokens = *f.CompletionTokens
	}
}

// usageEnvelope covers every place a provider puts usage: top level in an OpenAI
// response or an Anthropic message_delta, and nested under the message in an
// Anthropic message_start.
type usageEnvelope struct {
	Usage   *wireUsageFields `json:"usage"`
	Message *struct {
		Usage *wireUsageFields `json:"usage"`
	} `json:"message"`
}

// mergeFromJSON folds any usage found in a JSON payload into the totals. It
// reports whether the payload carried any, so callers can distinguish "no usage
// reported" from "usage reported as zero".
func (u *Usage) mergeFromJSON(payload []byte) bool {
	var env usageEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return false
	}
	found := false
	if env.Usage != nil {
		u.merge(env.Usage)
		found = true
	}
	if env.Message != nil && env.Message.Usage != nil {
		u.merge(env.Message.Usage)
		found = true
	}
	return found
}

// usageFromBody extracts usage from a complete, non-streaming response body.
func usageFromBody(body []byte) (Usage, bool) {
	var u Usage
	if !u.mergeFromJSON(body) {
		return Usage{}, false
	}
	return u, true
}

// maxSSELine bounds the per-line buffer. A longer line is not a usage payload
// the router cares about, and retaining it would let a misbehaving upstream grow
// memory without limit.
const maxSSELine = 64 << 10

// usageObserver accumulates usage from a response without altering it. It is fed
// the same bytes the client receives, so a byte-transparent relay can report
// token counts and cost without parsing, buffering or rewriting the stream.
type usageObserver struct {
	partial  []byte
	skipping bool
	usage    Usage
	found    bool
}

// Write consumes a fragment of the response. It never fails and never blocks, so
// it can sit directly in the relay loop.
func (o *usageObserver) Write(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			o.consume(p, false)
			return
		}
		o.consume(p[:i], true)
		p = p[i+1:]
	}
}

// consume adds a fragment to the current line and, when the line ends, inspects
// it. An oversized line is abandoned rather than buffered.
func (o *usageObserver) consume(frag []byte, complete bool) {
	if !o.skipping {
		if len(o.partial)+len(frag) > maxSSELine {
			o.skipping = true
			o.partial = o.partial[:0]
		} else {
			o.partial = append(o.partial, frag...)
		}
	}
	if !complete {
		return
	}
	if !o.skipping {
		o.inspect(o.partial)
	}
	o.skipping = false
	o.partial = o.partial[:0]
}

// inspect looks for usage in one complete SSE line.
func (o *usageObserver) inspect(line []byte) {
	payload, ok := sseData(line)
	if !ok {
		return
	}
	// Cheap gate before unmarshalling: most chunks carry content, not usage.
	if !bytes.Contains(payload, []byte(`"usage"`)) {
		return
	}
	if o.usage.mergeFromJSON(payload) {
		o.found = true
	}
}

// sseData returns the payload of a "data:" line, ignoring comments, other
// fields, and the [DONE] sentinel.
func sseData(line []byte) ([]byte, bool) {
	line = bytes.TrimRight(line, "\r")
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false
	}
	return payload, true
}
