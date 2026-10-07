---
title: Why llm-router
weight: 2
---

The thing it does is one narrow operation — *rewrite one alias into different
upstream model ids, spread across providers, fail over* — and it does that, with
a footprint you can hold in your head, instead of also being a control plane.

## LiteLLM vs llm-router

[LiteLLM](https://github.com/BerriAI/litellm) is the most-deployed gateway of
this kind, so it is the fair comparison:

| | llm-router | LiteLLM |
| --- | --- | --- |
| Language & runtime | Go 1.26; one static binary, no runtime dependencies | Python; a proxy server plus its dependency tree |
| Footprint | ~17 MB binary + one YAML file; no database required | Python app; keys, budgets and teams want a Postgres database, admin UI on top |
| Licence | none published yet | MIT |
| Provider breadth | whatever you configure: any OpenAI- or Anthropic-compatible base URL, plus cli upstreams | 100+ provider integrations, maintained upstream |
| Alias → several providers | per-target model ids, weights, weight-0 failover-only targets | `model_name` with multiple deployments, weights and fallbacks — its closest feature match |
| Wire translation | OpenAI Chat ↔ Anthropic Messages in both directions, streams translated incrementally without buffering | serves the OpenAI format and converts to provider SDKs; native passthrough routes for some providers |
| Session & cache affinity | session pins, prefix-derived pins, and routing toward warm prompt caches learned from provider-reported cache reads | several routing strategies (least-busy, latency, usage, cost); prompt-cache affinity is not one of them |
| Usage & cost accounting | a SQLite file, one row per served request, cost from declared rates, queryable over `/usage` while running | spend tracking through its database or callbacks to external systems |
| Subscription-backed upstreams | `kind: cli` runs the unmodified Claude Code / codex binary under your own login | not offered; proxies hold API keys |
| Control plane (keys, budgets, teams, UI) | deliberately none | a core feature |

The honest verdict: if you want provider breadth, virtual keys, budgets and a
team console, LiteLLM is the more complete product and is MIT-licensed — use it.
If you want one small proxy whose whole configuration is a readable YAML file,
that runs as a single static binary anywhere — laptop, container, pod — and that
translates wires and tracks cache-aware usage without a database, llm-router is
the smaller tool for that job.

## The other candidates

Surveyed against the actual repos (2026-10-05):

| Project | Language / licence | Verdict for this job |
| --- | --- | --- |
| [QuantumNous/new-api](https://github.com/QuantumNous/new-api) | Go, **AGPL-3.0** + attribution terms, ~49k stars | Closest off-the-shelf fit: per-channel `ModelMapping` is exactly alias rewriting, and priority/weight/auto-ban is exactly failover. SQLite, no Redis needed. Downsides: AGPL, ~1.4k open issues, and a whole multi-tenant admin console to run on your laptop. |
| [maximhq/bifrost](https://github.com/maximhq/bifrost) | Go, Apache-2.0, ~8.5k stars | Permissive and well-run, but its routing keys off a **model name its own catalog recognises**. There is no field for "client asks `anthropic/claude-opus-5.5`, send that to OpenRouter and `claude-opus-5-5` to the Claude API". Adaptive load balancing is Enterprise. |
| [ENTERPILOT/GoModel](https://github.com/ENTERPILOT/GoModel) | Go, MIT, ~1.2k stars | Has `virtual_models` with weights, failover, and session affinity — the design is right. Very young (still `0.1.x`), so assume API churn. |
| [genai-io/llm-gateway](https://github.com/genai-io/llm-gateway) | Go, Apache-2.0, **0 stars, 9 commits** | Architecturally the most on-point config of anything surveyed. Not trustworthy as a dependency yet — useful as a design reference. |
| Rust options ([gproxy](https://github.com/LeenHawk/gproxy), [oxllm](https://github.com/planetf1/oxllm), [openproxy](https://github.com/x5iu/openproxy), litellm-rs, `llm_router` crate) | Rust | None has the tenure. `oxllm` is 10 stars and ~4 months old; the `llm_router` crate has a placeholder repo URL and 5 recent downloads. |

So: ~600 lines of Go you can read in one sitting, versus adopting an AGPL
multi-tenant gateway or an Enterprise-gated one to get one feature.
