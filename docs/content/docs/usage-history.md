---
title: Usage history
weight: 6
---

Point `store.path` at a file and the router keeps a row per served request: the
alias, the upstream that answered, the tokens, the latency and the cost.

```yaml
store:
  path: ~/.llm-router/usage.db
  queueSize: 1024
  maxRows: 130000        # about 10 MB, keeping the newest 130k requests
```

Query it through the router:

```bash
curl -H "Authorization: Bearer $ROUTER_KEY" \
  'http://127.0.0.1:8787/usage?group_by=alias&since=7d'
```

or open the file directly, even while the router runs (WAL is enabled):

```bash
sqlite3 ~/.llm-router/usage.db \
  'SELECT upstream, count(*), sum(cost_usd) FROM requests GROUP BY 1'
```

## What it is

**Usage and cost are observed, not invented.** The relay feeds the bytes it has
already copied to a line-oriented observer, so token counts and cache reads are
recorded without parsing, buffering or rewriting the stream: the client still
receives the upstream's bytes exactly, terminator included. Both wires are
normalised, because they disagree — the OpenAI wire's `prompt_tokens` includes
cached tokens, while Anthropic's `input_tokens` excludes its separate cache
fields — so cache reads are never billed twice. Each served request logs
`input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens` and
`cost_usd` from the alias's declared rates. When a provider reports no usage, the
log says `usage=unreported` rather than claiming a spend that was never measured.

## What it is not

The store is a local record of what the router served, in a file any SQLite
client can read. It is deliberately not a control plane: no keys, no budgets, no
tenancy. It answers "what did this router do, and what did it cost" without
requiring a database server, and the file is the whole state.

- **Retention is a ring.** Records are pruned as they are written — within one
  flush — so `maxRows` is the actual cap.
- **A request whose provider reported nothing stores NULL, not zero.** The token
  columns are nullable precisely so that `SUM` cannot count spend that was never
  measured; `/usage` reports those requests separately under `unreported`.
- **Writing never applies back-pressure.** Records pass through a bounded queue
  to a single writer. If the disk cannot keep up they are dropped and counted in
  `dropped_records`, because accounting must never be the reason an inference
  call is slow, or the reason one fails.
- **The file is readable while the router runs.** WAL mode is on, which is also
  why `-wal` and `-shm` files appear beside it.
- **Only served requests are recorded.** When every target failed there is no
  usage, no cost and no upstream that answered, so that request is logged rather
  than stored.
