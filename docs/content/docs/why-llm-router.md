---
title: Why llm-router
weight: 2
---

One narrow job — *rewrite one alias into different upstream model ids, spread
across providers, fail over* — done by a single static Go binary instead of a
control plane.

## LiteLLM vs llm-router

| | llm-router | LiteLLM |
| --- | --- | --- |
| Licence | none published yet | MIT |
| Performance | one ~17 MB static binary; measured idle at 5.9 MB RSS, 0% CPU, binds instantly | litellm 1.104 proxy measured idle at 94.8 MB RSS across its two processes, ~25 s from launch to first bind; a Python dependency tree spanning boto3, azure-*, polars, rq, redis and cryptography |
| Security | constant-time key check, inbound credentials stripped, secrets in env, cli upstreams store nothing, no bodies in logs — details below | boots only with a `LITELLM_MASTER_KEY`; virtual keys, budgets and teams need a Postgres database — a second credential store to run and protect |
| Alias → several providers | per-target model ids, weights, weight-0 failover-only targets | `model_name` with multiple deployments, weights and fallbacks |
| Wire translation | OpenAI Chat ↔ Anthropic Messages in both directions, streams translated incrementally without buffering | serves the OpenAI format and converts to provider SDKs; passthrough routes for some providers |
| Usage accounting | a SQLite file, one row per request, queryable over `/usage` with no database server | spend tracking through its database or external callbacks |
| Control plane (keys, budgets, teams, UI) | deliberately none | a core feature |

Both were measured idle on the same machine (macOS, arm64, one worker): llm-router
5.9 MB RSS, LiteLLM 94.8 MB — about 16× — and LiteLLM took 25 s to start serving
against llm-router's instant bind. CPU was 0% for both while idle. Under load the
difference is structural: the Go relay copies bytes; the Python proxy parses and
re-serialises.

![Measured idle footprint: llm-router 5.9 MB RSS vs LiteLLM 94.8 MB](/llm-router/perf-comparison.svg)

| Measured, idle (macOS, arm64, one worker) | llm-router | LiteLLM 1.104 proxy |
| --- | --- | --- |
| Idle RSS | 5.9 MB | 94.8 MB (parent + uvicorn worker) |
| Idle CPU | 0% | 0% |
| Launch → first bind | instant | ~25 s |

The verdict: if you want provider breadth, virtual keys, budgets and a team
console, LiteLLM is the more complete product and is MIT-licensed — use it. If
you want one small proxy you can read in an afternoon and run anywhere, llm-router
is the smaller tool for that job.

## The others

- [new-api](https://github.com/QuantumNous/new-api) — closest off-the-shelf fit (per-channel `ModelMapping`, priority/weight failover), but AGPL-3.0 plus attribution terms, ~1.4k open issues, and a multi-tenant admin console.
- [bifrost](https://github.com/maximhq/bifrost) — permissive and well-run; routing keys off a model name its own catalog recognises, so "same alias, different model id per provider" has no field. Adaptive load balancing is Enterprise.
- [GoModel](https://github.com/ENTERPILOT/GoModel) — right design (`virtual_models`, weights, affinity); still 0.1.x.
- [llm-gateway](https://github.com/genai-io/llm-gateway) — most on-point config of anything surveyed; 0 stars, 9 commits.
- Rust options (gproxy, oxllm, litellm-rs, the `llm_router` crate) — no tenure yet.

## Security

What the code does, verified:

- **Constant-time gateway key check.** `crypto/subtle.ConstantTimeCompare` in `internal/router/server.go`, applied to inference and `/usage` alike.
- **Client credentials are never relayed.** Inbound `Authorization`, `x-api-key` and `api-key` are dropped before dispatch; the target gets only its own configured key. An Anthropic-wire client's Claude key never reaches whichever provider the alias picks. Hop-by-hop headers are not forwarded.
- **Secrets live in the environment.** `apiKeyEnv` is preferred; no key is required in `router.yaml`. On Kubernetes they come from a Secret, not the ConfigMap.
- **cli upstreams store and proxy nothing.** A `kind: cli` upstream is rejected at load if it carries an `apiKey`. The unmodified Claude Code binary runs under your login and the router reads only its stdout — subscription credentials never pass through the proxy, which is also what Anthropic's policy requires.
- **Prompts stay off disk.** No request bodies in the logs.
- **Loopback by default.** Set `apiKey` before exposing the listener; the comparison is constant-time.
