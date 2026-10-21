---
title: Codex subscription
weight: 8
---

# Codex subscription

`kind: codex` connects the router to the official Codex app-server over stdio.
Codex owns ChatGPT sign-in, credential storage, and token refresh. The router
never reads or forwards subscription tokens. This is separate from OpenAI API
billing and from any Codex login stored by omp.

## Install and connect

The adapter is verified against Codex **0.163.0**. Install that release under the
same OS account that runs the router, for example:

```bash
npm install -g @openai/codex@0.163.0
codex --version
```

Add an upstream and alias to `router.yaml`:

```yaml
upstreams:
  codex:
    kind: codex
    command: [codex, app-server, --listen, "stdio://"]
    maxConcurrency: 4
    maxSessions: 8
    sessionIdleTimeout: 30m
    timeout: 30m

models:
  openai/codex:
    api: openai-completions
    targets:
      - upstream: codex
        model: default
        weight: 1
```

`default` resolves the installed Codex catalog's recommended model. To pin a
model, use an actual model ID shown under **Connections → Codex model catalog**.
Catalog presence does not guarantee that your account can use that model. Context
limits and dollar prices are not invented from the model name; configure context
metadata separately if needed. Subscription usage records tokens when supplied,
but keeps dollar cost unreported.

The default command above is supplied automatically if `command` is omitted.
Codex upstreams are always persistent and use client-owned tools. API keys,
HTTP base URLs, headers, request patches, and one-shot mode are rejected on this
upstream kind. The standard distroless router image does not contain Codex;
run on a host with Codex installed or supply a suitable custom runtime image.

## Authenticate

With the console enabled and a gateway key configured:

1. Open `/ui/` and sign in with the **router gateway key**.
2. Open **Connections**.
3. Under the Codex upstream, click **Sign in with ChatGPT**.
4. Click **Continue sign-in with OpenAI** and complete the official browser flow.
5. Login notifications update cached account state; the page polls that snapshot.

Opening Connections never starts or restarts Codex or fetches a model catalog.
Initial account state is **not checked**, not signed out. Click **Refresh account
and catalog** to load it explicitly. A pending browser flow keeps its process
and callback listener during refresh. If that process exits, the pending link
is cleared and an error is shown instead of polling indefinitely.

Alternatively, run `codex login` on the router host, then click **Refresh account
and catalog** in Connections.
Use the same `HOME`, `CODEX_HOME`, and credential-store environment as the router.
A browser callback must reach the router host's Codex listener; for a remote
host, use the official CLI's headless/device login flow instead. No account
credentials should be pasted into router YAML or chat.

**Cancel sign-in** cancels a pending flow. **Disconnect Codex account** logs out
of Codex on that host and terminates this upstream's conversations. All router
operators share this provider account; this is not multi-tenant authentication.

## Strict OpenAI Chat interface

Codex routes accept **OpenAI Chat Completions only**, with text messages and
client-owned function tools. Anthropic Messages and Responses API aliases are
rejected for this upstream kind; those protocols remain available for other
upstreams where supported.

Codex controls its own output limit. The adapter does **not** pretend to enforce
`max_tokens` or `max_completion_tokens`: requests containing either receive
HTTP 400. Explicit sampling, stop sequences, structured-output controls, and
other unsupported request controls are also rejected, not silently dropped.
`reasoning_effort` is forwarded to the native Codex turn; the selected model
must support the requested effort. `tool_choice` may be omitted or `auto`.

The accepted top-level fields are `model`, `messages`, `tools`,
`reasoning_effort`, `tool_choice`, `stream`, and `stream_options`.
Image/multimodal inputs are rejected explicitly.

For omp, keep the router provider configuration and add a model override for
each Codex alias you expose:

```yaml
providers:
  my-router:
    baseUrl: http://127.0.0.1:8787/api/v1
    apiKey: '!cat "$HOME/.local/state/llm-router/ui-key"'
    auth: apiKey
    authHeader: true
    api: openai-completions
    disableStrictTools: true
    discovery:
      type: litellm
    modelOverrides:
      openai/codex:
        omitMaxOutputTokens: true
        reasoning: true
        input: [text]
        compat:
          supportsStore: false
          supportsSamplingParams: false
          supportsPenaltyAndStopParams: false
          supportsForcedToolChoice: false
          supportsReasoningEffort: true
          thinkingFormat: openai
```

The key-file command assumes the local console setup described in this project;
otherwise use your own gateway credential source. `omitMaxOutputTokens` means
no output cap is requested; it does not bypass a requested cap. omp may display
budgets inferred from its own catalog, which are not limits enforced by this
adapter.

```bash
omp models my-router
omp --model my-router/openai/codex
```

## Tool and conversation ownership

The client sends function definitions; Codex's native dynamic-tool request is
returned as `tool_calls`. The client executes the tool and submits its matching
`role: tool` result. Codex then continues the same live turn. Streaming tool
calls are supported. The router never runs the client's tool implementation.

Built-in host environment access is disabled with `environments: []`, alongside
defense-in-depth shell/web/hook/delegation restrictions. Unexpected approval or
interactive requests fail closed rather than being auto-approved.

Send a stable `X-OMP-Session` header for each conversation. Without one, the router
uses its normal opening-prefix identity. Model/tool definitions and developer
instructions must remain stable within a session. A canceled, expired, evicted,
or failed process loses its live thread and pending tool RPCs; start a new
conversation instead of replaying a tool result into a different process.
Router restart also loses live Codex conversations. A zero idle timeout disables
idle expiry; capacity eviction only considers unreserved conversations.

## References

- [Official app-server protocol](https://learn.chatgpt.com/docs/app-server)
- [Official authentication guide](https://learn.chatgpt.com/docs/auth)
- [Version-specific implementation notes](https://github.com/richardrh/llm-router/blob/main/docs/codex-protocol.md)
