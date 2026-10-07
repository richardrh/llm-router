---
title: Configuration
weight: 5
---

`router.yaml` is the whole config, commented and ready to run. The keys worth
knowing:

| Key | Meaning |
| --- | --- |
| `listen` | Bind address. Loopback by default. |
| `apiKey` | Shared secret clients must present. `""` accepts any. |
| `defaults.*` | Retry budget, timeouts, sticky affinity, breaker thresholds. |
| `store.path` | SQLite file for usage history. Empty keeps no history. |
| `store.queueSize` | Records waiting to be written before they are dropped. |
| `store.maxRows` | Records kept, oldest deleted first. `0` keeps everything. |
| `upstreams.<name>.baseUrl` | Provider root; the protocol's path is appended. |
| `upstreams.<name>.apiKeyEnv` | Env var holding the key. Preferred over `apiKey`. |
| `upstreams.<name>.kind` | `http` (the default) or `cli`. |
| `upstreams.<name>.mode` | `one-shot` (default) or `persistent` for a long-lived Claude Code session. |
| `upstreams.<name>.toolMode` | `claude` (default) or `client`; `client` returns harness tool calls and requires non-streaming requests. |
| `upstreams.<name>.command` | argv for a `cli` upstream; one-shot commands contain `{prompt}`, persistent commands may contain `{model}`. |
| `upstreams.<name>.cwd` | Working directory for a CLI process; Claude Code discovers project configuration here. |
| `upstreams.<name>.timeout` | Bounds one CLI turn. Defaults to `maxStreamDuration`. |
| `upstreams.<name>.maxSessions` | Maximum live persistent CLI processes. Defaults to `maxConcurrency`. |
| `upstreams.<name>.sessionIdleTimeout` | Closes inactive persistent CLI processes. Zero keeps them alive until shutdown. |
| `upstreams.<name>.authStyle` | `bearer` (the default) or `anthropic`. |
| `upstreams.<name>.headers` | Extra headers per upstream, overriding the client's. |
| `upstreams.<name>.bodyDrop` | Top-level JSON fields removed before dispatch. |
| `upstreams.<name>.bodyPatch` | Top-level JSON fields forced. |
| `upstreams.<name>.maxConcurrency` | In-flight turn cap. At the limit: skipped, never queued. |
| `models.<alias>.api` | Wire protocol clients speak for this alias. |
| `models.<alias>.contextWindow`, `maxOutputTokens`, `cost` | Advertised to clients for budgeting; not enforced upstream. |
| `models.<alias>.targets[]` | Where the alias may be served: upstream, model id, weight. |

## Choosing a model version, provider and thinking mode

These are set per **target** — per provider route — because the same logical
model needs different fields on each:

| Decided at | Key |
| --- | --- |
| Which model version or variant | `targets[].model` |
| Which provider serves it | `targets[].bodyPatch` — OpenRouter's `provider` object |
| How hard it thinks | `targets[].bodyPatch` — `reasoning` on OpenRouter, `thinking`/`output_config` on Claude |
| A field a provider must not receive | `targets[].bodyDrop` |

**Which OpenRouter provider** serves a request is the `provider` object:
`order`/`only`/`ignore` select providers by slug, `allow_fallbacks` decides
whether OpenRouter may fall back to another, and `sort`, `require_parameters`,
`data_collection`, `zdr` and `max_price` constrain the choice further.

## Translating between wires

A target may name a different wire protocol than its alias. The router then
translates the request on the way out, and the response and its stream on the
way back.

**What translates:** messages (system hoisting, role coalescing, tool calls and
results, images), tools and tool choice, token limits, stop sequences and
reasoning controls are mapped. Native structured outputs, citations, computer
use, server-side tools and PDF input are not. Fields with no equivalent are
dropped rather than guessed at, because the Messages API rejects unknown
parameters instead of ignoring them.

Streaming is translated incrementally with no whole-stream buffering, and the
usage observer still reads the upstream's own events — so accounting works
identically whether or not the wire changed.

## The capability table

`capabilities.json` is an embedded snapshot of the Claude models' limits,
pricing and capability flags, transcribed from the providers' published figures
and cross-checked against LiteLLM's model map. Loading a config uses it to warn
about metadata that disagrees with the table, and to refuse thinking modes a
model does not support.
