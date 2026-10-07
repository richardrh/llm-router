---
title: Design notes
weight: 7
---

**Failover happens strictly before the first response byte is committed.** The
request body is buffered (bounded by `maxBodyBytes`) so it can be replayed
against the next target. Once a status line is written, the response belongs to
the client: a truncated or failed stream is logged and returned, never
re-dispatched, because the client cannot un-see it.

**Retried statuses.** 429, 401, 402, 403, 5xx. Ordinary 4xx are the client's
bug and are returned unchanged instead of being fired at every provider. A 429
parks the upstream without counting against its health — it is busy, not broken.

**Streaming is byte-transparent.** No parsing, no re-serialisation: tool-call
argument fragments and reasoning deltas pass through exactly as the upstream
emitted them, with a flush per chunk. The transport disables compression so the
SSE body is never buffered.

**Sticky affinity exists for prompt caching.** Successive turns of one
conversation are pinned to a single target so the provider's cache keeps
hitting. Worth knowing when reading `router.yaml`: Claude's
OpenAI-compatibility layer does not implement prompt caching, while OpenRouter's
Claude models do, so the OpenAI-wire aliases send traffic to OpenRouter first
and keep the direct Claude API as failover. The native `/v1/messages` aliases,
where caching works properly, go direct.

**Cache affinity makes that assumption observable.** A session pin is a guess
about where a cache lives. Cache affinity replaces the guess with evidence: the
usage reports say whether a prefix was read from cache or written to it, so the
router records which upstream actually holds each conversation's prefix, with a
TTL matching the provider's own cache lifetime. When no pin applies — a new
session, or one whose target failed and was replaced — the conversation goes
back to the cache that holds it rather than to a weighted coin toss. It is a
preference, not a strategy: it only ever chooses among targets that are already
equally eligible, so it cannot override a pin, weights, or health.

**Usage on streams is only requested on the OpenAI wire.** `injectStreamUsage`
adds `stream_options`. The Anthropic Messages API has no such field and reports
usage in its own stream events, so the router never injects it there.

**Model rewriting is not applied to streams.** Rewriting JSON mid-stream would
mean parsing incremental structured data; the response headers carry
`X-Router-Upstream-Model` instead. Non-streaming responses have their `model`
field rewritten back to the alias so the client displays what it asked for.

**Client credentials never reach an upstream.** Inbound `Authorization`,
`x-api-key` and `api-key` are dropped and replaced with the target's own key, so
an Anthropic-wire client's Claude key is never relayed to whichever provider the
alias happens to pick. Hop-by-hop headers are not forwarded.

**The listener is loopback by default.** Set `apiKey` if anything else can reach
it; the comparison is constant-time.

## What this deliberately does not do

- **Translation is limited to two pairs.** A target may name a different wire
  than its alias: OpenAI Chat Completions to and from Anthropic Messages, in
  either direction. Every other combination is refused at startup rather than
  half-supported. An alias whose targets all share its wire stays a
  byte-transparent pass-through.
- **The translation covers what a coding agent needs, and no more.** Messages
  (system hoisting, role coalescing, tool calls and results, images), tools and
  tool choice, token limits, stop sequences and reasoning controls are mapped.
  Native structured outputs, citations, computer use, server-side tools and PDF
  input are not. Fields with no equivalent are dropped rather than approximated,
  because the Messages API rejects unknown parameters outright.
- **The compatibility layer is optional now.** Claude's OpenAI-compatibility
  layer supports tools and streaming but ignores `strict`, `reasoning_effort`
  and `response_format`, and does not implement prompt caching. Pointing a target
  at `api: anthropic-messages` reaches the native endpoint through the
  translator instead, so an OpenAI-shaped client can use Claude's own `thinking`
  and `output_config` controls.
- **No translation between reasoning vocabularies.** `reasoning` (OpenRouter) and
  `thinking`/`output_config` (Claude) are different fields with different accepted
  values, and they change with the model generation. The router forwards whatever
  each target's `bodyPatch` sets and never maps one provider's spelling onto
  another's — a mapping table would be wrong the moment a model version changed.
- **No `/v1/embeddings`, images, audio, or batch.** Unsupported endpoints return
  404 rather than being proxied blindly.
- **No request bodies in the logs.** Prompts and code stay out of your disk.
- **No prefix-similarity scheduling for self-hosted KV caches.** Cache affinity
  tracks which *provider* holds a warm prompt cache, learned from the usage it
  reports. It does not do prefix-tree matching across a fleet of self-hosted
  pods, where the cache lives in GPU memory and has to be probed quite
  differently.
- **`contextWindow` is advisory.** It tells the client how to budget its
  context. It does not stop an upstream from rejecting a longer prompt, and it
  does not make different upstreams behave identically — OpenRouter and the
  Claude API may serve a different revision, or apply different limits, under
  the same alias.

## Tests

```bash
go test -race ./...
```

Covers alias rewriting per target, weighted distribution, weight-0 failover-only
semantics, sticky affinity (with and without a client session header), breaker
rotation and single-probe recovery, pre-commit failover, no-failover on client
errors, SSE passthrough, credential scrubbing, Anthropic credential and version
headers, `stream_options` suppression on the Anthropic wire, the OpenRouter-shaped
`/api/v1` paths, per-target body shaping, the model capability table, usage
accounting, cache affinity, wire translation in both directions, CLI-backed
upstreams, the usage store, the `/usage` endpoint, and the YAML-on-disk path
through `LoadConfig`.
