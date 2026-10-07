---
title: Pointing clients at it
weight: 4
---

**Any OpenRouter client.** Change the base URL and nothing else:

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8787/api/v1   # was https://openrouter.ai/api/v1
```

**omp / pi.** In `~/.omp/agent/models.yml`, use `discovery.type: litellm` so
context windows and pricing come from `/model_group/info` rather than a guess:

```yaml
providers:
  my-router:
    baseUrl: http://127.0.0.1:8787/api/v1
    apiKey: ROUTER_KEY          # only if `apiKey:` is set in router.yaml
    authHeader: true
    api: openai-completions
    disableStrictTools: true
    discovery:
      type: litellm
```

Aliases then appear as `my-router/anthropic/claude-opus-5.5` and
`my-router/anthropic/claude-sonnet-5.5`:

```yaml
modelRoles:
  default: my-router/anthropic/claude-sonnet-5.5
  slow: my-router/anthropic/claude-opus-5.5
```

**Anthropic-wire clients** (Claude Code, the Anthropic SDKs) point
`ANTHROPIC_BASE_URL=http://127.0.0.1:8787` and use the bare alias
`claude-opus-5-5`. The router forwards those to Claude's native `/v1/messages`.

For a persistent `kind: cli` alias, send a stable conversation header so the
router can keep the same Claude Code process for every turn:

```text
X-OMP-Session: project-42
```

The router also accepts `X-Session-Id`, `X-Conversation-Id`, and
`X-Sticky-Key`. Without one, it derives a key from the opening conversation
prefix when the request contains messages.
