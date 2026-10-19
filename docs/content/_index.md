---
title: "llm-router"
bookToC: false
---

# One model alias. Multiple providers.

A small Go proxy for routing LLM requests, translating wire formats, and failing
over when a provider cannot serve them. No control plane. No database server.

Point your client at `http://127.0.0.1:8787/api/v1`. Keep the same OpenRouter
model names and request format; the router picks an upstream and maps the model ID.

{{% columns %}}

- ### Start here

  [**Run the router →**]({{< relref "/docs/running" >}})

  Build from source or deploy with Docker or Kubernetes.

  [**Connect your client →**]({{< relref "/docs/pointing-clients" >}})

  Base URLs, authentication, Claude Code, and harness-owned tools.

- ### Make it yours

  [**Configure routing →**]({{< relref "/docs/configuration" >}})

  Aliases, weighted targets, failover, and request patches in `router.yaml`.

  [**Inspect usage →**]({{< relref "/docs/usage-history" >}})

  Track tokens and cost in a local SQLite file.

{{% /columns %}}

## Up and running

```bash
go build -o llm-router ./cmd/llm-router
export OPENROUTER_API_KEY=sk-or-...
./llm-router -check
./llm-router
```

Requires Go 1.26 or newer. Run from the repository root to use the included
`router.yaml`. The default listener is loopback-only; configure a gateway key
before exposing it to a network.

## How requests flow

1. Your client sends a request using a configured model alias.
2. The router chooses an eligible upstream and rewrites the model ID.
3. If that upstream fails before the response starts, the router tries another target.
4. The response streams back in the client's wire format.

Read [why llm-router]({{< relref "/docs/why-llm-router" >}}) for the tradeoffs,
or [design notes]({{< relref "/docs/design-notes" >}}) for the routing invariants.
