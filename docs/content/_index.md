---
title: "llm-router"
linkTitle: "llm-router"
---

A small proxy that speaks **OpenRouter's wire format to the client** and its own
to each upstream. One model name, several interchangeable providers, and the
caller never learns which one served it.

```
agent ──POST /api/v1/chat/completions {"model":"anthropic/claude-opus-5.5"}──▶ llm-router
                                                        ├─ openrouter  anthropic/claude-opus-5.5  (weight 1)
                                                        └─ anthropic   claude-opus-5-5            (weight 0, failover only)
```

Point a client at `http://127.0.0.1:8787/api/v1` instead of
`https://openrouter.ai/api/v1`. Same paths, same `Authorization: Bearer`, same
`anthropic/claude-*` model strings. When a provider is down, throttled, or
saturated, the request moves on without the agent noticing.

{{< cards >}}
  {{< card link="running" title="Running" subtitle="Source, Docker, Kubernetes — and what daemon mode means here." >}}
  {{< card link="why-llm-router" title="Why llm-router" subtitle="Head to head with LiteLLM and the alternatives." >}}
  {{< card link="configuration" title="Configuration" subtitle="router.yaml: aliases, targets, patches, timeouts." >}}
  {{< card link="usage-history" title="Usage history" subtitle="Tokens and cost per served request, in one SQLite file." >}}
{{< /cards >}}
