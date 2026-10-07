---
title: Running
weight: 3
---

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
the router derives a key from the conversation prefix.

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

It starts a fresh agent per request and flattens the conversation. Client
supplied `tools` are not forwarded in either mode; Claude Code owns and
executes its own tools inside the local process.

For an ordinary completion API, use an [OpenRouter](https://openrouter.ai/keys)
key for the one-endpoint many-provider case, or an Anthropic Console key for
Claude direct.
