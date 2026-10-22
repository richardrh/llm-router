---
title: Running
weight: 3
---

# Running

## Daemon mode

llm-router runs as a **long-lived foreground server**: it binds its listen
address, serves requests until it receives `SIGTERM` or `SIGINT`, then drains
in-flight streams for up to 20 seconds before exiting. There is no
self-daemonizing flag on purpose — detaching, logging and restart-on-crash are
owned by whatever supervises it (Docker, Kubernetes, `systemd`, `launchd`,
`tmux` on a laptop). Kill signals and restarts are handled cleanly.

```bash
./llm-router -config router.yaml      # -config defaults to ./router.yaml
./llm-router -listen 0.0.0.0:8787     # override the bind address
./llm-router -check                   # validate config, print resolved routes, exit
```

For `systemd`, use `Restart=on-failure`; the process expects to be supervised.

## From source

```bash
go build -o llm-router ./cmd/llm-router   # requires Go 1.26 or newer
export OPENROUTER_API_KEY=sk-or-...        # at least one provider
export ANTHROPIC_API_KEY=sk-ant-...

./llm-router -check                        # validate, print resolved routes
./llm-router                               # listen on 127.0.0.1:8787
```

Then talk to it exactly as you would to OpenRouter:

```bash
curl http://127.0.0.1:8787/api/v1/chat/completions \
  -H "Authorization: Bearer $OPENROUTER_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"anthropic/claude-opus-5.5","messages":[{"role":"user","content":"hi"}]}'
```

`apiKey: ""` in the config accepts any credential, so the header only has to be
present for clients that insist on sending one.

Responses carry the routing decision:

```
X-Router-Upstream: openrouter
X-Router-Upstream-Model: anthropic/claude-opus-5.5
```

## Web console

Enable the embedded console in `router.yaml` and restart:

```yaml
apiKey: "replace-with-a-long-random-gateway-key"
ui:
  enabled: true
  secureCookies: false
```

Open `http://127.0.0.1:8787/ui/` and sign in with the gateway key. A nonempty key
is required; API clients must send the same key as a bearer token.
Overview shows upstream capacity and cooldowns. Routes is read-only. Usage
supports time, alias, upstream, and grouping filters over retained records.
Recorded requests are not a count of all incoming traffic or every failure;
missing provider usage is shown as unknown.

The playground supports OpenAI Chat Completions and Anthropic Messages, streams
responses, and offers Stop and New conversation controls. It assigns an explicit
conversation ID. CLI targets require acknowledgement before each request because
they can execute tools on the router host. Claude Code client-owned tool routes are
non-streaming; returned tool calls await an external harness and are never
executed by the console. Pending tool calls, stopped turns, and failed turns
require a new conversation.
For OpenAI models using a ChatGPT subscription, see
[Codex subscription]({{< relref "/docs/codex" >}}). Codex's native client-tool
calls support streaming and are still never executed by the playground.
Connections reads cached provider state only. Use **Refresh account and catalog**
to explicitly inspect or reload a Codex account; merely opening the page does
not launch or restart Codex. Pending-login notifications update the snapshot.
The playground restores its transcript on reconnect without resubmitting
inference, and incremental updates replace only the active turn.

Sessions/transcripts are bounded and held in server memory, not browser storage.
Logout or expiry cancels active work; restarting discards browser sessions.
Sessions expire after 30 minutes without requests or 12 hours total, with at most
128 sessions. Prompts are capped at 32 KiB, responses at 1 MiB, and conversations
at 50 turns plus a transcript-size bound. Claude Code may retain its own
on-disk sessions independently.
Completed CLI processes still follow the upstream's `maxSessions` and
`sessionIdleTimeout`; use a positive idle timeout so abandoned playground
conversations can release process capacity.

Use HTTP only on loopback. For a TLS-terminating reverse proxy, set
`ui.secureCookies: true`, preserve the public `Host`, disable buffering for
`/ui/playground/stream`, and prevent direct external access to the backend.
The router does not trust forwarded headers for cookie/origin security.
The console uses HttpOnly/SameSite=Strict cookies, CSRF tokens, same-origin checks,
and allowlisted operational snapshots that omit credentials, commands, and paths.

All HTML is rendered by templ; only the vendored htmx and official SSE extension
execute in the browser. There is no handwritten application JavaScript or CDN
dependency. Template changes require `go generate ./internal/webui`; ordinary
builds use the checked-in generated Go and embedded assets.

## Docker

The binary is static (the SQLite driver is pure Go), so the image is a
multi-stage build onto a distroless base: no shell, no package manager, running
as `nonroot`. Only the config is mounted; keys come in through the environment.

```bash
docker build -t llm-router .
docker run --rm -p 8787:8787 \
  -v "$PWD/router.yaml":/etc/llm-router/router.yaml:ro \
  -e OPENROUTER_API_KEY -e ANTHROPIC_API_KEY \
  llm-router -config /etc/llm-router/router.yaml
```

`docker run` never creates files it wasn't given, so with the usage store on a
bind-mounted file (`store.path`) the SQLite history survives the container.

## Kubernetes

`deploy/kubernetes.yaml` ships a working minimal config (one alias served by
OpenRouter), a Deployment and a Service:

```bash
kubectl apply -f deploy/kubernetes.yaml
kubectl create secret generic llm-router-keys \
  --from-literal=OPENROUTER_API_KEY=sk-or-...
kubectl port-forward svc/llm-router 8787:8787
# now the curl from "From source" works against 127.0.0.1:8787
```

For the full multi-provider config, replace the ConfigMap with your own
`router.yaml`:

```bash
kubectl create configmap llm-router-config \
  --from-file=router.yaml --dry-run=client -o yaml | kubectl apply -f -
```

Two constraints are load-bearing, both stated in the manifest: the usage store
lives on an `emptyDir`, so it dies with the pod unless you swap in a PVC; and
keep `replicas: 1`, because each replica keeps its own SQLite file, session pins
and cache-affinity observations — running several would fork the accounting and
the pinning without any of them being wrong.

To be precise about "Kubernetes-native": there is no operator, no CRDs, no
HPA — `replicas: 1` is a real constraint, not a placeholder. What makes it fit
the platform is what it does not need: no database, no sidecar, no service mesh,
one static binary probing on `/healthz`, config from a ConfigMap, keys from a
Secret. `kubectl apply -f` and it runs.

## Credentials

An **http** upstream authenticates with an API key, taken from the environment
(`apiKeyEnv`) or a literal in the config. A **cli** upstream (`kind: cli`) has no
credential at all: it runs a local command that manages its own login.

**Subscriptions cannot be replayed from a proxy, but they can be used through
one.** The distinction matters.

- Anthropic's [Claude Code
  policy](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use)
  states that developers "may not collect, store, or intermediate Claude.ai
  credentials or session tokens", and that sign-in "must complete through
  Anthropic's own flow". A proxy holding your OAuth token does exactly the
  forbidden thing — and would additionally have to impersonate the CLI
  (`user-agent: claude-cli/…`, `x-app: cli`, the `You are Claude Code…` system
  prompt), because the API rejects those tokens without it.
- The same policy expressly does **not** prevent "an end user signing in to the
  unmodified Claude Code binary with their own Claude subscription".

So the router supports the second shape and refuses the first. Point a
`kind: cli` upstream at the real binary: it runs under your login, and the router
sees only its stdout — never a token, a credential file, or a keychain entry. A
cli upstream carrying an `apiKey` is rejected at load, because there is nothing
to give it.

```yaml
upstreams:
  claude-code:
    kind: cli
    mode: persistent
    command: [claude, -p, --input-format, stream-json,
              --output-format, stream-json, --verbose,
              --include-partial-messages, --model, "{model}"]
    cwd: /path/to/project
    maxSessions: 2
    sessionIdleTimeout: 30m
```

Persistent mode keeps one unmodified Claude Code process per client session.
Later turns are sent over its stream-JSON stdin protocol, so Claude Code keeps
its normal system prompt, `CLAUDE.md`, hooks, skills, plugins, MCP servers,
tools, subagents, context management and subscription login. Send a stable
`X-OMP-Session` (or `X-Session-Id`/`X-Conversation-Id`) header; without one,
the router hashes the opening prefix through the first user message before
choosing an upstream. Later assistant replies do not change that key. Identical
opening prefixes share a session; use distinct headers for independent conversations.

`sessionIdleTimeout` releases idle processes when another request arrives.
Active and queued turns are protected from expiration. When a released session
has a saved Claude Code session ID, the router resumes it and sends only the
new turn or tool result, not the full conversation again. Cancellation or a
failed process retires the session so a later request can start or resume a
usable process.

The target's `model` is substituted when the process starts and cannot change
within that session. `cwd` controls project discovery. Leave `--bare` out:
bare mode skips subscription login and most normal project configuration.

The legacy one-shot form remains supported:

```yaml
upstreams:
  claude-code:
    kind: cli
    command: [claude, -p, "{prompt}", --output-format, stream-json,
              --verbose, --include-partial-messages]
```

It starts a fresh agent per request and flattens the conversation. Persistent
mode normally lets Claude Code own its tools. Set `toolMode: client` on a
persistent upstream to return validated `client_tool_request` calls as standard
OpenAI `tool_calls` or Anthropic `tool_use` responses. The harness executes the
tool and sends its result on the next non-streaming turn. Streaming requests skip
client-tool CLI targets without consuming an attempt; compatible targets remain eligible.

For an ordinary completion API, use an [OpenRouter](https://openrouter.ai/keys)
key for the one-endpoint many-provider case, or an Anthropic Console key for
Claude direct.
