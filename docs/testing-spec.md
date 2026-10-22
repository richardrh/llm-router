# Behavior-first TDD and test-suite specification

Status: **test suite implemented; shutdown race fixed after explicit user authorization.** The initial suite-writing task changed no production functionality and preserved product failures. The subsequent lifecycle fix and its verification are recorded in §12. The unrelated Codex denial-delivery behavior and failing assertions remain unchanged.

## 1. Goal and testing policy

Protect what an operator or client depends on: existing YAML configurations, executable startup/shutdown, HTTP request/response contracts, incremental streams, routing safety, agent conversations, accounting, and the browser console.

- No coverage-percentage target, test-count target, or mandated E2E/unit ratio. Coverage is a navigation aid, not an acceptance criterion.
- Prefer deterministic **process E2E** tests: real binary, YAML, environment, listener, routing, adapters, and SQLite. Replace only external providers with local protocol fixtures.
- Use a small browser suite for behaviors that need an actual browser. HTTP handler tests cannot prove cookies, embedded scripts, htmx swaps, or EventSource reconnection work together.
- Keep focused module tests at stable interfaces where they give a clearer, faster oracle: translation, usage normalization, stream decoding, and established lifecycle contracts. Do not unit-test each helper to improve a percentage.
- Fuzz bounded data seams; do not start the router or a provider process for every fuzz input.
- Default CI must need no provider key, subscription, installed Claude/Codex CLI, internet inference, or host tool execution. Local-provider E2E proves router behavior, not current vendor compatibility. A separately authorized live-provider smoke may check that, but is not a deterministic merge gate.
- The user authorized implementation at the seams below. Do not invent new exported interfaces solely to make private helpers testable.

## 2. Existing protection and the actual gaps

Existing means source assertions were inspected, not that the suite was executed for this document.

| Area | Existing protection | High-value addition |
| --- | --- | --- |
| Configuration | `internal/config/*_test.go`, plus substantial YAML/capability/shipped-config tests in `internal/router/router_test.go` and `cli_server_test.go` | Binary `-check`; historical input fixtures; configuration-to-observed-request behavior; no startup side effects during validation |
| Routing and wires | Handler-based tests for aliases, auth, shaping, retry statuses, saturation, translation, discovery, and usage | Real sockets and commitment; process startup; disconnect propagation; no replay after commitment |
| Agent adapters | Real fixture subprocesses in Claude/Codex tests; manager tests for continuation, cancellation, and capacity | HTTP disconnect through the running binary; YAML-selected commands; shutdown and restart semantics |
| Accounting | Store close/reopen, pruning, NULLs, grouping; reporting tests with directly seeded records | Inference → async writer → graceful process exit → restart → public `/usage` |
| Console/inference | Handler/state tests; typed inference tests for fragmented streams, tools, errors, and bounds | Actual browser auth, assets, streaming/reconnect, cancellation, and account-action journeys |
| Executable | No committed executable unit tests in the PR base; a pre-existing local help test was excluded with its unrelated CLI refactor | Process tests cover valid/invalid config exits, overrides, help, runtime errors, signals, and resource teardown |

Package coverage therefore understates some cross-package protection. In particular, config behavior is already exercised from the router package. Also, `TestStreamIsPassedThroughIncrementally` uses a completed recorder response: retain useful content assertions, but do not mistake them for proof of incremental delivery over TCP.

Sources: [router tests](../internal/router/router_test.go), [executable](../cmd/llm-router/main.go), [process config tests](../tests/e2e/config_test.go), [current CI](../.github/workflows/ci.yml).

## 3. Contracts to preserve

### Configuration and executable

1. Existing supported YAML remains accepted with the same operational meaning. Unknown schema keys fail rather than being silently ignored. `headers` and `bodyPatch` remain intentionally open maps; their arbitrary provider keys are not unknown schema fields.
2. Missing `kind` means HTTP; missing Claude CLI `mode` means one-shot. Omitted target model uses the alias; omitted target protocol uses the alias protocol. Protect these through requests, not a snapshot of every default field.
3. Positive target weights admit normal selection; weight zero means failover-only; negative weights and all-zero target sets fail validation. No exact randomized distribution is promised by a single scenario.
4. Alias/target protocol differences are supported only for Chat Completions ↔ Anthropic Messages. OpenAI Responses is supported as same-wire HTTP pass-through, not translation or subscription adaptation.
5. Model capability metadata drift is a warning, not fatal; unknown model IDs remain valid. Forbidden known capability patches are errors. Explicit configured metadata remains the discovery/accounting source of truth.
6. Credential ownership remains explicit: HTTP accepts `apiKey` **or** `apiKeyEnv`, not both; CLI/Codex own subscription credentials. UI requires a nonblank gateway key. Codex's additional protocol/control restrictions remain enforced.
7. `-check` validates and reports routes without binding a socket, contacting providers, starting agent commands, or opening the usage DB. An unset HTTP key environment variable is reported by check mode, not a configuration-schema error. Do not assert that check mode authenticates a provider.
8. `-config`, default `router.yaml` lookup, `-listen` override, successful help, nonzero invalid-config/startup exits, and graceful SIGTERM/SIGINT remain operator-facing behavior. Logs need useful context, not byte-identical wording or ordering.
9. Preserve empty-store-path disablement, `maxRows: 0` unlimited retention, and documented duration strings. Test explicit true/false overrides where they change routing, not an exhaustive table of incidental defaults.

Keep small immutable **input** compatibility fixtures for currently supported forms. Record their provenance when adding a release-derived fixture; do not call invented files historical release configs. Never regenerate compatibility inputs from the current Go structs. A deliberate breaking change needs an explicit migration/release decision and changed acceptance criteria, not silently rewritten fixtures. The shipped `router.yaml` must validate, but its current provider inventory/prices are not permanent test snapshots.

### Public HTTP interface

| Operation | Required observable behavior |
| --- | --- |
| `GET /healthz` | `200`, JSON `status: ok`, no gateway credential required; liveness, not a promise that a provider is ready |
| `GET /v1/models`, `/api/v1/models`, and model-ID suffixes | Authenticated alias discovery; required IDs/limits/capabilities usable by clients; slash-containing alias IDs resolve |
| `GET /model_group/info`, `/api/v1/model_group/info` | Configured model metadata/capabilities; no provider credential disclosure |
| POST `/v1` or `/api/v1` + `/chat/completions`, `/messages`, `/responses` | Correct configured wire, route/model mapping, and response semantics; Responses cases use HTTP only |
| Gateway key configured | Missing/wrong bearer credential → `401 unauthorized`, no provider request; provider/client credential headers are replaced or removed on dispatch |
| Malformed JSON / too-large body / unknown alias / wrong wire | Respectively `400 invalid_body`, `413 body_too_large`, `404 unknown_model`, `400 protocol_mismatch`; no provider request |
| Unsupported POST endpoint / non-POST inference | `404 unsupported_endpoint` / `405 method_not_allowed` |
| Retryable HTTP status | `401`, `402`, `403`, `429`, and `5xx` can fail over before commitment, within actual dispatch budget |
| Ordinary nonretryable provider error | Preserve provider status and applicable wire semantics; do not require a router-generated envelope |
| All dispatched targets fail / all targets busy | `502 all_targets_failed` / `503 targets_busy` in the corresponding controlled scenarios |
| `GET /usage` | Gateway authentication; disabled store → `404 usage_store_disabled`; invalid supported query values → `400`; valid filters/groups preserve reporting semantics |

Router-generated JSON errors require `error.code` (number), `error.type` (machine-readable string), and `error.message` (useful text). Do not pin human wording. Compare required response fields and types; tolerate additive fields unless security requires an allowlist. Do not snapshot IDs, timestamps, JSON key order, header capitalization, or entire discovery documents.

The current implementation accepts inference path suffixes beyond the two documented prefixes and accepts some additional authorization forms. Do not accidentally tighten production behavior while implementing tests; deciding whether to support or remove those forms is a separate compatibility decision. `/health` is not `/healthz`.

### Streaming, translation, sessions, and accounting

- **Commitment is the point of no return.** For HTTP, accepted response headers commit before a token arrives. A later disconnect, truncated body, or translation failure must not replay the request on a backup. Codex delays streaming commitment until output; Claude streaming currently commits eagerly. Test these separately.
- Same-wire SSE remains byte-transparent, including unknown fields, comments, and terminators; stream models are not rewritten. With configured nonstream rewrite, the response model is the alias. Routing headers retain the selected upstream/model.
- Translation is semantic and intentionally lossy. Upstream vocabulary is shaped **after** translation; upstream drop/patch precedes target drop/patch; patches replace top-level values rather than deep-merge. Unsupported fields do not imply universal round-trip identity.
- Sticky identity stays stable for an ongoing session; explicit session headers take precedence over prefix inference. Headerless identity includes the first user turn. Identical opening prefixes can share an identity; it is not a tenant isolation mechanism.
- Queued same-session cancellation must not kill the active turn. Active cancellation must release capacity. Claude may resume its saved session; Codex must not silently recreate an established lost conversation under the same key.
- Client-owned tool calls remain client-owned. The console displays them and does not execute them. Codex pending tool continuation requires the matching result; unsupported Codex controls fail explicitly rather than being ignored or silently routed elsewhere.
- Accounting is asynchronous and may drop under pressure; it is not a complete durable audit trail. Graceful close drains accepted queued records; SIGKILL has no such guarantee. Query settled state or restart after graceful exit.
- Missing token usage is `null`, reported zero is numeric zero where presence is reported. `cost_usd` is numeric; `cost_source: unreported` distinguishes an unknown dollar charge from measured zero. A Codex row can have measured tokens and unreported dollars. Aggregate `unreported` counts unreported **cost**, not necessarily missing tokens.
- `/usage` totals cover the filtered retained window, not only the returned recent-page limit. Time windows are `[since, until)`. Retention keeps newest inserted records; do not assume insertion order equals arbitrary supplied timestamps. A request rejected before service need not create a usage row.

Sources: [configuration loader](../internal/config/config.go), [server](../internal/router/server.go), [routing/shaping](../internal/router/router.go), [wire interface](../internal/wire/translate.go), [usage](../internal/usage/usage.go), [reporting](../internal/router/usage_http.go), [Codex protocol](codex-protocol.md).

## 4. Test seams and harness specification

| Seam | Catches | Does not establish |
| --- | --- | --- |
| Binary + YAML + TCP HTTP | Deployment/config composition, public contracts, streams, real cancellation, persistence, signals | Vendor behavior or browser integration |
| Browser → running binary | Cookies/origin/CSRF, embedded assets/CSP, htmx/SSE, visible operator journeys | Every parser edge, vendor OAuth, long expiry intervals |
| Exported module interface | Precise deterministic transformations, errors, accounting arithmetic, bounded parser/state properties | Correct executable wiring |
| Native fuzz target at an agreed module seam | Unexpected bytes/chunk boundaries and reproducible invariant failures | Correct composition or exhaustive safety |

### Suite locations

- `tests/e2e/*_test.go`: ordinary Go process tests, included in `go test ./...`.
- `tests/e2e/testdata/config/`: manually authored compatibility inputs; invalid variants are explicit test mutations.
- `tests/e2e/api_test.go` and `translation_test.go`: small independent provider wire fixtures, not output captured from the router under test.
- `tests/e2e/agent_fixture_test.go`: test-executable protocol fixture for Claude and Codex. No production test hooks or installed vendor CLI.
- `tests/browser/`: test-only Playwright Test + Chromium, pinned dependency/lockfile. Node is a testing prerequisite, not a frontend build/runtime dependency.
- `tests/e2e/browser_test.go` and `browser_fixture_test.go`: `browser` build-tagged Go entry point and external fixtures. Explicitly selecting this lane without its tooling fails rather than skipping.
- Existing `internal/<package>/*_test.go` and `testdata/fuzz/<FuzzName>/`: focused stable-interface regressions and native corpus files.

### Harness requirements

1. Build the binary once per suite, or accept a proposed `ROUTER_E2E_BINARY` absolute path. Always invoke that binary, not `runWith` or a replacement handler. The race job must provide a race-instrumented child binary: `go test -race` alone does not instrument a separately built subprocess.
2. Launch with `-listen 127.0.0.1:0`; obtain the bound address from the existing structured startup `addr` field, then GET `/healthz`. Keep readiness-log parsing in one helper, not product-log snapshot tests. Never reserve/release a port and hope it remains free.
3. Use fresh temp config/SQLite/HOME/CODEX_HOME/cwd, explicit fake credentials, and an allowlisted child environment. Provider URLs are loopback; command paths are fixture executables. Do not inherit subscription credentials or call real OAuth endpoints.
4. Provider fixtures validate relevant incoming wire fields independently and return literal replies. They expose test-only gates: request received, first event flushed, release completion, force disconnect, and cancellation observed. These controls belong to fixtures, never production endpoints.
5. Use bounded waits on events/observable state, not fixed sleeps or scheduler ordering. Browser polling/SSE makes “network idle” unsuitable. Deadlines are harness safeguards, not latency benchmarks.
6. Fresh process per stateful scenario unless a test explicitly tests persistence. Each process owns its store. Run safe independent cases concurrently only with separate fixtures/env/ports; never mutate process-global environment from parallel tests.
7. Own teardown even after failure: close clients, release gates, signal and wait for the router, verify fixture child termination, kill remaining test-owned processes after the deadline, delete temp files. Retain sanitized diagnostics on failure.
8. Verify through client responses and external provider observations. Provider dispatch counts are valid when they prove no duplicate execution or no unauthorized request; internal method-call counts are not a contract.
9. Use real SQLite and `/usage` in E2E; no direct SQL seeding to claim inference-to-accounting coverage. Keep precise store filter/queue-pressure cases at the existing store interface where necessary.
10. Browser tests use real forms and fresh contexts. Never replace `Dependencies.Run`, router selection, `fetch`, or EventSource. A local HTTPS reverse proxy preserving Host tests secure-cookie deployment; do not fake TLS by trusting `X-Forwarded-*`.

## 5. Prioritized acceptance suite

P0 protects shipped configuration, security, and the main inference path. P1 protects operational/stateful journeys. Priority is implementation order, not a reason to silently omit specified behavior.

| ID | Priority / seam | Scenario and acceptance |
| --- | --- | --- |
| C1 | P0 / process | Minimal HTTP, Anthropic auth style, translated target, Responses pass-through, one-shot CLI, persistent CLI, Codex, UI-enabled, and store-enabled config fixtures validate; representative fixtures actually serve their expected request |
| C2 | P0 / process + config | Invalid unknown keys, duration, reference, duplicate target, negative/all-zero weight, negative `maxAttempts`, forbidden model patch, contradictory credentials/CLI placeholders/Codex controls, UI without key, and negative store limits fail; diagnostics identify the bad setting. Raw `maxAttempts: 0` retains its existing default-sentinel meaning; the effective attempt budget must be positive. |
| C3 | P0 / process | `-check` has no runtime side effects; unset env key reports absence; secret values are absent from ordinary check output; shipped `router.yaml` validates without live credentials |
| C4 | P0 / process | Default config lookup and explicit `-config`; `-listen` wins; help exits successfully; missing config, occupied listener, and unusable store fail cleanly without surviving child/listener |
| C5 | P1 / config + process | Unknown models and metadata drift remain accepted; configured discovery values survive; known forbidden capability patches fail; nested target shaping and explicit false overrides retain meaning |
| H1 | P0 / process | All three HTTP wire paths and both documented prefixes; alias/model rewriting, discovery/list/detail, configured metadata, route headers, additive provider fields |
| H2 | P0 / process | Auth, credential scrubbing, malformed/oversized request, unknown alias, wrong wire, unsupported endpoint/method; assert exact status/type and no dispatch where applicable |
| H3 | P0 / process | Retryable/nonretryable status table, all-target failure, deterministic weight-zero backup, max-attempt budget, saturation skipped without consuming dispatch allowance |
| H4 | P0 / process | Gated first SSE event arrives before completion; exact same-wire bytes; post-header truncation never dispatches backup, including nonstream responses |
| H5 | P0 / process | Both translation directions, nonstream and streamed; tools/finish reasons/text; post-translation target shaping; independent literal wire examples |
| A1 | P1 / process | Claude full initial prompt, latest-turn continuation, isolated explicit identities, saved-session resume, client tool/result round-trip; incompatible streaming target skipped without spending attempt budget |
| A2 | P1 / process | Codex text/tool continuation, early versus late failure, unsupported controls before execution, unmatched tool result, lost-session refusal; host tools/approval requests fail closed |
| A3 | P1 / process + manager | Active versus queued cancellation isolation, reusable capacity, blocked child-input cancellation, no postcommit replay; retain manager-level scheduler edge tests |
| U1 | P0 / process | Measured, explicit-zero, and missing HTTP usage; hand-calculated cached-token price; graceful exit/restart preserves accepted rows |
| U2 | P1 / process + store | Subscription tokens with unknown dollars, reported CLI cost, filters/groups, page limit versus totals, retention after flush/restart, disabled history; queue pressure remains nonblocking at store seam |
| B1 | P0 / browser | Login/logout through real cookies, preauth/CSRF/cross-origin rejection, old-session revocation, HTTP and HTTPS-proxy cookie policy |
| B2 | P1 / browser | Routes/Usage navigation and htmx filtering against inference-generated rows; unknown usage presentation; safe read-only status |
| B3 | P0 / browser | Streamed two-turn chat in both supported playground wires; first delta visible while held; reload/reconnect does not dispatch again; inert text; no duplicated history |
| B4 | P1 / browser | Independent operator contexts; Stop/New; logout cancels active work; one operator cannot read/stop another conversation; old content does not enter a new conversation |
| B5 | P1 / browser | Explicit CLI consent; pending client tools displayed but never executed; failed/stopped/tool-pending turn requires New conversation; unsupported aliases unavailable |
| B6 | P1 / browser | Connections GET/polling is read-only; explicit refresh/login/cancel/logout via real protocol fixture; safe login link; account state shared, browser authentication independent |
| R1 | P1 / process | SIGTERM and SIGINT with active console/agent work close owned resources; graceful completed HTTP accounting flushes; restart invalidates browser sessions |
| R2 | P1 / process | Cooldown/half-open single probe, bounded actual attempts, sticky fallback, cache affinity subordinate to health/weights/pin; deterministic fixture gates, no exact stochastic ratios |
| P1 | P1 / packaged smoke | Built static binary/container starts with mounted YAML, serves a fixture request as nonroot, persists to a writable mounted store, and stops cleanly; image build alone is insufficient |

For bounds, test meaningful below/at/above cases: request `maxBodyBytes`, prompt/response limits, SSE observer line bound, and retention. Avoid multiplying every scenario across every protocol/backend: parameterize where semantics are shared, and keep distinct cases for the documented asymmetries.

## 6. Pseudocode: implementation slices

Notation below remains language-neutral design pseudocode. The executable implementation uses the shared helpers in `tests/e2e/harness_test.go`; these are test helpers, not new production APIs. Each independent row/case is a separate test.

### Slice C: compatibility inputs and validation

```text
for each supported_config_fixture:
    sandbox = freshSandbox(noRealCredentials=true)
    provider = localProtocolFixture(fixture.protocol)
    config = substituteOnlyRuntimePathsURLsAndFakeSecrets(fixture, sandbox, provider)
    result = exec(binary, ["-config", config.path, "-check"], sandbox.env)
    assert result.exit == 0
    assert provider.receivedRequests == []
    assert fixtureAgent.didNotStart
    assert not exists(config.storePath)
    assert fakeSecretValuesAbsent(result.output)
    # Selected representative fixtures additionally start and serve H1/A1/A2.

for each single_invalid_mutation, expected_setting in invalid_config_cases:
    config = mutateKnownValidLiteralFixture(single_invalid_mutation)
    result = exec(binary, ["-config", config.path, "-check"])
    assert result.exit != 0
    assert diagnosticIdentifies(result, expected_setting)  # not entire prose
    assert noProviderOrAgentExecution()

# Do not require unknown bodyPatch/header names to be rejected.
# Do not make the capability table an oracle for expected live provider prices.
```

### Slice H1/H2: YAML to observable routing and safety

```text
provider expects POST /v1/chat/completions:
    model == "provider-model"
    Authorization == "Bearer provider-fixture-key"
    no inbound x-api-key/api-key secret
    custom provider JSON field survives unless explicitly shaped
provider returns literal completion(content="hello", model="provider-model")
router = startRouter(literalYAML(alias="friendly", rewriteModel=true,
                               apiKey="gateway-fixture-key", apiKeyEnv="TEST_KEY"))

unauthorized = POST router /api/v1/chat/completions with no bearer key
assert unauthorized.status == 401
assert unauthorized.error.type == "unauthorized"
assert provider.receivedRequests == []

response = authorizedPOST(model="friendly", content="hi", inboundOtherSecrets=true)
assert response.status == 200
assert response.model == "friendly"
assert response.assistantText == "hello"
assert response.headers contain selected provider and "provider-model"
assert provider.validatedExactlyOneRequest
assert discoveryContainsAliasAndConfiguredMetadata(router, "friendly")
```

### Slice H3: failover and dispatch budget

```text
for status in [401, 402, 403, 429, 500, 502, 503]:
    primary replies status; backup replies literal "backup answer"
    router = freshRouter(primary.weight=1, backup.weight=0, maxAttempts=2)
    response = infer(router)
    assert response.text == "backup answer"
    assert externalDispatches == [primary, backup]

for status in [400, 404, 422]:
    primary replies status with literal provider error; backup is available
    assert infer(freshRouter()).status == status
    assert backup.receivedRequests == []

primary holds first request at capacity 1
router maxAttempts = 1
second = infer(router)
assert second.text == "backup answer"  # full primary was skipped, not attempted
release first request; assert first request finishes normally
```

### Slice H4: incremental bytes and no replay after commitment

```text
firstEvent = literalSSEWithCommentAndUnknownProviderField
rest = literalToolArgumentFragmentsAndDONE
primary.flushHeaders(200)
primary.writeAndFlush(firstEvent)
primary.waitFor(releaseCompletion)

client = startStreamingRequest(routerWithPrimaryAndBackup)
assert client.readExactly(firstEvent.length, deadline) == firstEvent
assert not primary.completionReleased
signal releaseCompletion; primary.writeAndFlush(rest); primary.finish()
assert client.remainingBytes == rest

# Separate case: headers/first event then connection drops or body is truncated.
# Client observes committed status/partial bytes or transport error, not backup text.
assert client.observedStatus == 200
assert backup.receivedRequests == []
assert noReplacementAnswerWasAppended

# Repeat post-header truncation for nonstream HTTP and active client cancellation.
# Codex: early failure can use backup; text then failure cannot.
# Claude: do not treat absence of text as proof that headers are uncommitted.
```

### Slice H5: translation is specified independently

```text
input = literalOpenAIConversation(
    system="Be concise", user="Find alpha",
    assistantToolCall=(id="call_1", name="lookup", args='{"q":"alpha"}'),
    toolResult=(id="call_1", text="found"))
expected = independentlyAuthoredAnthropicRequestForSameSupportedConversation
router target.api = anthropic-messages
router upstream.bodyPatch = {max_tokens: 100, metadata: {user_id: "upstream"}}
router target.bodyPatch = {max_tokens: 200, metadata: {user_id: "target"}}

send input through router
assert provider.path == "/v1/messages"
assert provider.essentialMessageAndToolFields == expected.essentialFields
assert provider.max_tokens == 200
assert provider.metadata == {user_id: "target"}  # shallow replacement
provider returns independentlyAuthoredAnthropicReplyAndUsage
assert client gets expected OpenAI text/tool ID/arguments/finish reason

# Separate reverse-direction fixture and incremental stream fixtures.
# Never calculate the expected output by invoking the inverse production mapper.
# Do not claim unsupported images/citations/structured outputs round-trip.
```

### Slice A: lifecycle and client-owned tools

```text
codex fixture requests declared tool lookup, call ID "call-1"
first = infer(session="s", declaredTools=[lookup])
assert first.toolCall == ("call-1", "lookup", {q: "alpha"})
second = infer(session="s", freshToolResult=("call-1", "found"))
assert second.text == "continued"
assert fixture continued same thread with matching result
assert no fixture host-tool execution

# Independent negative sessions: unmatched tool result, explicit max_tokens,
# changed established tool/model/instructions, lost process after established turn.
assert explicit supported-contract client error, not silently ignored controls

active = inferAsync(session="a"); wait fixture.activeTurn
queued = inferAsync(session="a"); cancel queued.clientContext
release active; assert active.text == "completed"
assert nextTurn(session="a").text == "continued"

# Separate active cancellation case: wait for actual partial output, disconnect.
eventually fixture observes cancellation/process exit
assert newSessionCanUseFreedCapacity()
assert noBackupExecutedCanceledCommittedTurn()
# Claude saved-session resume and Codex lost-session refusal have separate oracles.
```

### Slice U1: accounting oracle and restart

```text
ratesPerMillion = {input: 4, output: 20, cacheRead: 0.2, cacheWrite: 5}
provider replies to three sequential served requests:
    measured: usage={prompt_tokens:100, completion_tokens:20,
                     prompt_tokens_details:{cached_tokens:80}}
    zero:     usage={prompt_tokens:0, completion_tokens:0}
    unknown:  no usage field
router has a fresh on-disk store, sufficient queue capacity, and those rates
send all three; gracefullyStop(router); assert exit == 0
router = restartWithSameStore()
report = GET /usage

assert measured row == projection(input=20, output=20, cacheRead=80,
                                  cost=0.000496, costSource="estimated")
# Independent arithmetic: (20*4 + 20*20 + 80*0.2) / 1_000_000.
assert zero row has numeric zero tokens and costSource="estimated"
assert unknown row has null tokens and costSource="unreported"
assert report.totals.requests == 3
assert report.totals.unreported == 1
assert approximately(report.totals.cost_usd, 0.000496)
assert GET /usage?limit=1 has one recent row but totals.requests == 3

# Add separate Codex token-present/dollars-unreported and CLI reported-cost cases.
# Do not generalize HTTP zero-presence behavior to every CLI result encoding.
```

### Slices B1/B3/B4: real browser journeys

```text
browserContext A: open /ui/playground; expect login navigation
fill label "Gateway key"; click button "Sign in"
expect protected page; cookie is HttpOnly, SameSite=Strict, Path=/ui
raw same-session logout POST with missing CSRF -> 403
attacker-origin browser form submission -> rejected; A remains signed in

open Playground; select label "Route alias"; fill label "Message"
click "Send message"
fixture flushes partial text, holds completion
expect #active-turn displays literal partial text before fixture release
reload page
expect same active transcript; provider dispatch count unchanged
release completion; expect message input enabled and answer exactly once
send next turn; expect retained prior user/assistant history and stable session

browserContext B signs in separately and starts its own held turn
A clicks "Stop generation"
expect provider A cancellation; B still running
expect A cannot continue until "New conversation"
A clicks "New conversation"; sends new prompt
expect new conversation identity and no previous partial text
B clicks "Sign out"; expect provider B cancellation; A remains usable

# Render <script>/<img>-shaped provider text as visible inert text, not DOM nodes.
# Secure cookie case uses real local HTTPS proxy, public Host, secureCookies=true.
# Keep 30-minute/12-hour expiry, 128-session capacity, and stale callback races
# in deterministic handler/manager tests, not long-running browser scenarios.
```

### Slices B2/B5/B6: operations, tools, and Connections

```text
seed usage by sending known requests through the public inference interface
login; navigate "Routes" then "Usage"
fill "Alias"; select "Group by"; click "Apply filters"
expect intended filtered cells/group totals and working navigation after htmx swap
expect unknown token/cost presentation, not fabricated measured zero
GET /ui/api/status: no provider keys, commands, paths, or private header sentinels

select CLI route; Send without input[name=acknowledge_cli] -> no execution
acknowledge; fixture returns client-owned tool call
expect pending tool ID/name/arguments; no tool execution/continuation
expect New conversation required
new conversation through nonretryable HTTP 422 fixture -> failed presentation
new conversation through successful route -> usable again

open Connections; cached unknown account, no process start/RPC refresh
click "Refresh account and catalog" -> fixture account/catalog becomes visible
click "Sign in with ChatGPT" -> official HTTPS link; never visit real auth host
pending polling -> no additional account/catalog/login RPCs
click "Cancel sign-in" -> matching login canceled
start again; fixture emits login-complete notification -> visible signed-in state
second operator sees shared account state
click "Disconnect Codex account" -> account signed out, browser sessions still valid
unsafe fixture login URL -> no clickable unsafe destination
```

## 7. Native Go fuzzing specification

Use **`testing.F` / `go test -fuzz`**. It is built into the existing Go toolchain; no separate fuzz engine/dependency is needed. Coverage guidance chooses interesting mutations; it is not a test-suite coverage goal. Details, corpus semantics, flags, and primary-source citations: [fuzzing research](fuzzing-research.md), [official Go fuzzing guide](https://go.dev/doc/security/fuzz/).

The first four targets below are implemented. Add stateful generated traces only where the existing deterministic transition tests leave a concrete risk. No external property library or OSS-Fuzz deployment is required for this baseline.

| Target / seam | Inputs and seeds | Required oracle |
| --- | --- | --- |
| `FuzzConfigDecode` / `config.LoadConfig` | YAML bytes; valid compatibility inputs, unknown schema keys, duplicate keys, malformed durations, bad references, scalar/array roots | Malformed data returns an error rather than crashing/hanging; accepted config obeys named structural constraints; inserting a guaranteed-unknown top-level schema key into a valid fixture is rejected; cosmetic comments/key order do not alter fixture semantics |
| `FuzzWireSupportedMessages` / `wire.TranslateRequest`, `TranslateResponse`, `NewStreamTranslator` | Bounded supported text/tool conversations plus separate raw malformed JSON seeds; fragmented literal SSE | Valid translated JSON preserves the agreed text/tool identity/order/finish semantics; identity pair preserves bytes; partitioning stream input preserves event semantics; invalid shapes terminate without panic. No universal round-trip equivalence |
| `FuzzSSEFraming` / `inference.HTTPRunner` | Both wire formats, chunk-plan bytes, valid terminal events, comments, CRLF, UTF-8 splits, malformed JSON, truncated stream, fragmented tool arguments | Valid input yields the fixture's exact text/tools/usage once; arbitrary fragmentation gives equivalent results; incomplete valid prefix yields incomplete-response error with preserved partial text, not success; provider errors retain status |
| `FuzzUsageChunking` / `usage.Observer`, `UsageFromBody` | Literal OpenAI/Anthropic usage events, arbitrary partition plans, absent/zero usage, oversized junk line followed by valid event | Exact normalized literal values; partition invariance; later output totals replace rather than add; missing update preserves earlier fields; oversized junk recovers at newline; observer buffer never exceeds `usage.MaxSSELine` |
| Optional `FuzzConversationActions` / authenticated console handler | Bounded send/emit/stop/new/stale-ID/reconnect/logout actions; two sessions; deterministic controlled inference adapter at this module seam | Independent small state model: terminal turn cannot resume, reconnect never starts inference, stale work never enters new transcript, session A never mutates B. No private-setter-only fuzzing |

`LoadConfig` requires a path: write only the current input to an isolated scratch file and invoke the loader, never start a router. Its store-path validation may stat a path; it must not open the store or execute a configured command. Structured valid seeds keep all paths within the sandbox. Do not introduce a second YAML decoder implementation for tests. Config errors are currently textual, not a stable typed taxonomy; do not invent error categories in the fuzz oracle.

Fuzz harness limits are explicit search domains, not production promises: initially cap raw documents at 64 KiB, generated messages/tool arguments at modest bounded sizes, and action traces at 32 operations. Keep deterministic seeds just below/at/above the observer's 64 KiB line limit in a separate appropriately sized target/domain. Never silently discard the exact boundary being tested. Larger finite limits are covered by explicit examples.

```text
FuzzUsageChunking(f):
    f.Add(seedSelector="openai-cached", partitionPlan=[1, 2, 7])
    f.Add(seedSelector="anthropic-running-total", partitionPlan=[3, 1])
    f.Fuzz(selector, partitionPlan):
        fixture = chooseBoundedLiteralFixture(selector)
        observer = fresh usage.Observer
        for chunk in partitionWithoutDroppingOrReordering(fixture.bytes, partitionPlan):
            observer.Write(chunk)
            assert observer.BufferLen() <= usage.MaxSSELine
        assert observer.Result() == fixture.independentExpectedUsageAndPresence

FuzzSSEFraming(f):
    f.Fuzz(fixtureSelector, partitionPlan, truncateAt):
        fixture = chooseLiteralSupportedWireFixture(fixtureSelector)
        stream, expected = selectCompleteOrKnownIncompletePrefix(fixture, truncateAt)
        # HTTPRunner's supplied handler is the external response source at this seam.
        result, events, error = runThroughHTTPRunner(fragment(stream, partitionPlan))
        assert concatenatedText(events) == expected.text
        assert result.tools == expected.tools  # where a complete tool is expected
        assert result.hasUsage == expected.hasUsage
        assert errorCategory(error) == expected.errorCategory

FuzzConfigDecode(f):
    f.Fuzz(rawBytes):
        enforceDocumentSearchBound(rawBytes)
        path = isolatedScratchFile(rawBytes)
        cfg, err = config.LoadConfig(path)
        if err != nil: return  # parsing/validation rejection is an allowed result
        assert cfg has at least one upstream and alias
        assert each target references an existing upstream
        assert each alias has a positive-weight target and no negative target weight
        assert UI enabled implies nonblank gateway key
        assert known alias/target protocol pair is allowed by the written contract
        # Do not call cfg.validate() again and call that an independent oracle.
```

Go fuzz arguments are primitive values/bytes/strings, not arbitrary fixture structs. The pseudocode selectors become integers/strings and partition plans become `[]byte`. Use separate raw malformed-input and structured valid-input domains; otherwise mutations often test only the first parser rejection.

Partition invariance is a useful metamorphic property but insufficient alone: a parser that always returns empty would satisfy it. Anchor valid seeds to independent expected text, IDs, arguments, usage, and terminal/error outcomes. Normalize only nondeterministic generated timestamps/IDs when comparing translated outputs; never normalize meaningful loss away.

No swallowed panics, shared mutable state, global `Setenv`, real credentials, network providers, random wall-clock seeds, or spawned CLI per fuzz iteration. Bounded memory assertions apply to interfaces that actually promise them: the usage observer's line bound does not imply every translator has the same bound.

## 8. TDD implementation sequence and regression discipline

1. Select **one** acceptance ID and public seam. Write the concrete input and independent expected behavior before editing implementation.
2. Add one failing test that reaches that seam. Observe why it fails. For existing supported behavior, a new characterization test may already pass: do not manufacture a production bug to create a red phase.
3. For a new requirement/bug, establish red on the unchanged code, then implement only that behavior and obtain green. Build the E2E harness incrementally with the first real scenario, not a speculative framework.
4. Add a focused module test only if it isolates an uncertain edge/invariant that the E2E case cannot describe clearly. Keep existing valuable manager/decoder tests; do not mirror every E2E row at every layer.
5. Run the affected real scenario and relevant existing suite. Review/refactor separately without changing observable expectations. Update operator documentation for intentional contract changes.
6. For fuzz failures, preserve and replay the minimized input, decide the violated contract, fix it, and check in the corpus regression. Add a named E2E case only if the failure also exposes a composition risk.

Recommended progression: C1/C3 + H1 tracer → C2/C4/H2 → H3/H4 → H5/U1 → B1/B3 → agent/session and remaining operations → focused fuzz targets seeded from those fixtures. Add fuzz targets as their seam/oracles become clear; do not write the whole test suite before learning from the first vertical slice.

When replacing weak tests, remove obsolete checks rather than re-pin them: exact help prose, private struct layouts, generated HTML snapshots, exact current provider inventory/prices, or assertions that merely restate a copied default. Keep behavior such as usable omitted settings, correct rejection, disabled history, and genuine safety boundaries. Existing tests were retained during this test-only implementation.

**Sensitivity check:** for each important scenario, name the plausible defect it catches (e.g. dropping `KnownFields`, forwarding a gateway credential, buffering until EOF, retrying after headers, double-counting cached tokens). During implementation use a temporary targeted mutation where practical and confirm the intended test fails; restore it before delivery. Do not set an arbitrary mutation-score target or claim mutation results not run.

## 9. CI and execution specification

Keep current generation-drift, vet, race, static-build, and container-build checks. Extend them with executable behavior, not coverage gates. Test on the `go.mod` minimum toolchain; a newer local toolchain is not minimum-version evidence.

| Lane | Execution | Failure policy |
| --- | --- | --- |
| Every PR | Existing tests + deterministic process cases + checked-in fuzz seeds; Chromium browser journeys in their own job | Behavior failure blocks merge; missing selected harness dependency fails; no automatic retry turning a flaky first failure green |
| Race | Go suite with race-instrumented router/fixture child where relevant | Data race fails; normal subprocess build is not sufficient |
| Targeted PR fuzz | Small bounded mutation run for affected parser targets, e.g. 30 seconds each | Failure blocks; preserve corpus/logs; execution time is a resource budget, not a quality score |
| Scheduled fuzz | One target/package per job, e.g. 10 minutes, constrained workers; retain generated cache | Failure opens a reproducible regression issue; promote corpus to normal tests |
| Packaged/release | Actual static binary/container against local fixture, mounted config/store, nonroot and signal path | Startup/inference/persistence failure blocks release |
| Optional live provider | Explicit opt-in credentials and authorization, pinned supported CLI version | Separate compatibility signal, never required for an offline contributor PR |

Execution commands for the implemented suite:

```sh
# CI runner: instrument the subprocess as well as the Go tests.
go build -race -o "$RUNNER_TEMP/llm-router-race" ./cmd/llm-router
ROUTER_E2E_BINARY="$RUNNER_TEMP/llm-router-race" go test -race ./... -count=1

# Browser prerequisites: run these from tests/browser.
npm ci
npx playwright install --with-deps chromium
# Then from repo root; tagged entry point owns router/fixtures and invokes Playwright.
go test -tags=browser ./tests/e2e -run='^TestBrowserJourneys$' -count=1

# One package and exactly one fuzz target per active invocation.
go test ./internal/config -run='^$' -fuzz='^FuzzConfigDecode$' -fuzztime=30s -fuzzminimizetime=10s -parallel=2 -timeout=2m
go test ./internal/inference -run='^$' -fuzz='^FuzzSSEFraming$' -fuzztime=10m -fuzzminimizetime=30s -parallel=2 -timeout=15m

# Replay committed corpus normally; active exploration is NOT required for replay.
go test ./... -count=1
# Replay a discovered input using the target/hash command printed by Go.
```

Ordinary `go test` runs seeds in `f.Add` and checked-in `testdata/fuzz`, not all generated mutation-cache inputs. Preserve minimized failure files even when CI fails; cache generated exploration separately by OS/architecture/toolchain. Do not use `-fuzz=.` with `./...` as a multi-target runner. Native minimization is attempted, not guaranteed globally minimal. See the [cited commands and corpus policy](fuzzing-research.md).

Diagnostics: scenario ID, sanitized config, process exit/status, bounded logs, fixture protocol transcript, and browser failure trace/screenshot where safe. Never publish real keys, cookies, CSRF tokens, OAuth query parameters, or user prompts. Fixed sleeps, unbounded waits, order dependence, and hidden rerun-to-green policies are harness defects.

## 10. Completion criteria and known limits

The implementation is complete when each acceptance ID has an executable test at its assigned seam (or an existing test explicitly mapped to that behavior), all P0/P1 scenarios run in their intended lane, and the command paths above are wired into CI. A scaffold, a passing build, or a new coverage percentage is not completion.

A retained test must answer: which user-visible failure would make it fail; what independent oracle defines success; which side effects are controlled; which semantic differences are deliberately excluded? Confirm fixtures cannot contact real providers or execute host tools. Contract changes require documentation and migration decisions rather than changing snapshots until green.

**Review risks, not reproduced findings:** source inspection suggests special attention to Claude write failures after eager stream commitment, Claude all-zero usage presence, malformed wire translation shapes, and cache-count conversion between response formats. Add red examples for the intended contract before changing those paths; do not freeze accidental behavior or promise that all adapters already share identical semantics.

### Validation performed for this specification

A temporary real binary was built using the installed Go 1.27.0 toolchain. Against a local HTTP fixture and temporary YAML/store, the following were exercised:

- Valid `-check` exited 0; an unknown top-level YAML field exited 1 with the offending field in the diagnostic. Validation made no fixture request and did not create the usage DB.
- The shipped `router.yaml -check` exited 0 without live provider credentials.
- `/healthz` returned 200; unauthenticated model discovery returned 401; authenticated discovery and `/api/v1/chat/completions` returned 200.
- The provider received `real-model` and the fixture provider credential; the client received `spec-alias` and routing headers.
- Graceful SIGTERM exited 0. Restarting with the same SQLite file exposed one recorded request through `/usage`: uncached input 20, cached input 80, output 20, estimated cost **0.000496 USD**, matching the independent arithmetic in U1.

This initial design smoke did not execute permanent tests, fuzzing, browser journeys, race checks, or container smoke. The implementation results below supersede that limitation. Temporary design-smoke artifacts were removed.

## 11. Implemented suite and execution results

### Acceptance mapping

| Requirements | Executable tests |
| --- | --- |
| C1–C5 configuration/executable | [`config_test.go`](../tests/e2e/config_test.go), [literal YAML inputs](../tests/e2e/testdata/config/http.yaml); omitted-mode CLI runtime in `agent_test.go`; alias sticky override in `routing_test.go` |
| H1–H2 public HTTP | [`api_test.go`](../tests/e2e/api_test.go) |
| H3–H4 routing and commitment; R2 selection/recovery | [`routing_test.go`](../tests/e2e/routing_test.go) |
| H5 translation | [`translation_test.go`](../tests/e2e/translation_test.go) |
| A1–A3 agents/tools/cancellation; U2 agent charges; R1 child teardown | [`agent_test.go`](../tests/e2e/agent_test.go), [`agent_fixture_test.go`](../tests/e2e/agent_fixture_test.go) |
| U1–U2 accounting/reporting/retention | [`accounting_test.go`](../tests/e2e/accounting_test.go); existing `TestUsageStoreDropsRatherThanBlocks` retains the queue-pressure seam |
| R1 signals and browser-session restart | [`lifecycle_test.go`](../tests/e2e/lifecycle_test.go) |
| B1–B6 browser journeys | [`journeys.spec.ts`](../tests/browser/journeys.spec.ts), [`browser_test.go`](../tests/e2e/browser_test.go), [`browser_fixture_test.go`](../tests/e2e/browser_fixture_test.go) |
| P1 packaged runtime | [`packaged_test.go`](../tests/e2e/packaged_test.go) |
| Four fuzz properties | [`config`](../internal/config/config_fuzz_test.go), [`wire`](../internal/wire/translate_fuzz_test.go), [`inference`](../internal/inference/http_fuzz_test.go), [`usage`](../internal/usage/usage_fuzz_test.go) |

The existing router test `TestCacheAffinityDisabledLeavesWeightedSelection` retains the disabled-affinity case. Existing manager tests retain precise queued-cancellation and expiry transitions. The process queue case has a bounded noncompletion observation before canceling, and the idle-expiry case waits beyond its explicitly configured lease: these are real-time process tests, not proof of every possible scheduler interleaving. Optional state-machine fuzzing and live-provider checks are not required or implemented.

### Observed validation

Local Go checks used Go 1.27.0. The existing Dockerfile separately built its static binary with its Go 1.26 image, then the actual image was exercised under rootless Podman. No real provider or subscription account was contacted.

| Run | Observed result |
| --- | --- |
| `go vet ./...` | Passed |
| Race-built router + `go test -race ./... -count=1 -timeout=5m` | Initial test-only run failed: Codex denial-delivery assertions and active-console shutdown race/panic below. A too-strict wire fixture assertion was subsequently corrected and exercised by active wire fuzzing. See §12 for targeted shutdown-fix verification; no claim of a green full race suite. |
| `go test -tags=browser ./tests/e2e -run='^TestBrowserJourneys$' -count=1 -timeout=12m` | Passed after fixing fixture array encoding and waiting for the observable New-conversation form before choosing a route. All seven Chromium cases ran, including both playground wires and HTTPS cookies. |
| Existing Dockerfile build + `ROUTER_CONTAINER_RUNTIME=podman ROUTER_CONTAINER_IMAGE=<built-image> go test -tags=packaged ./tests/e2e -run='^TestPackagedRuntime$' -count=1 -timeout=5m` | Passed: nonroot startup, authenticated inference, mounted SQLite, graceful stop/restart, persisted report |
| Each of the four native fuzz targets with `-fuzztime=15s -fuzzminimizetime=5s -parallel=2 -timeout=90s` | Passed after correcting the wire oracle to accept an empty assistant text block without weakening tool identity/argument assertions. This was bounded exploration, not exhaustive proof. |

Harness corrections did not modify production functionality: literal YAML spelling/weights, correct queued-request admission, real cookie path, a non-nil fixture array, HTMX completion synchronization, and a semantic rather than exact-block-count wire oracle. Raw `maxAttempts: 0` remains an accepted default sentinel; its test proves actual primary-to-backup dispatch rather than demanding a new validation rule.

### Failures found by the test-only implementation

1. **Active-console shutdown raced accounting; subsequently fixed in §12.** `TestLifecycleSignalsCancelConsoleAndInvalidateLogin/SIGTERM` originally observed a nonzero router exit, race-detector evidence between `usage.(*Store).Record` (`internal/usage/store.go:251`) and `Store.Close` (`:419`), and `panic: send on closed channel`. The active playground goroutine reached `Server.finishLog` while executable teardown closed the store. The store implementation itself remains unchanged.
2. **Codex denial response is not observable before process retirement.** Both `TestAgentCodexDeniesHostApprovals/approval` and `/host-tool` received HTTP rejection, but the external protocol fixture never recorded the expected JSON-RPC error. [The documented contract](codex-protocol.md) says every unexpected server request receives an error and retires the conversation. `[INFERENCE]` Immediate process retirement can kill the peer before it reads the written denial. This is a protocol-delivery discrepancy, **not evidence that a host action was approved or executed**. The assertions remain failing; no permissive behavior, expected-failure wrapper, or skip was added.

Reproduction using a race-instrumented subprocess (important: the subprocess must be instrumented too):

```sh
work=$(mktemp -d)
go build -race -o "$work/llm-router-race" ./cmd/llm-router
ROUTER_E2E_BINARY="$work/llm-router-race" go test -race ./tests/e2e \
  -run='^(TestLifecycleSignalsCancelConsoleAndInvalidateLogin|TestAgentCodexDeniesHostApprovals)$' \
  -count=1 -timeout=2m
```

The original shutdown failure was scheduler-dependent. Its fix is described below. The unrelated Codex assertions remain enabled and were not rerun merely to reconfirm their previously observed failure.

## 12. Authorized shutdown lifecycle fix

The fix changes ownership and ordering, not usage semantics:

- `webui.Handler` registers inference workers before launch under the same lock as its shutdown flag. `Close` revokes sessions, cancels their work, and joins **all** registered workers, including draining turns removed by New conversation or logout. Concurrent callers share one close completion barrier.
- The executable tracks HTTP connection lifetimes. On every exit it closes remaining connections, waits for `http.Server.Serve` to stop accepting/registering connections, and joins the existing handlers before closing the usage store.
- Console cancellation runs alongside the HTTP graceful drain. A 20-second drain timeout still reports an error, but forced connection closure and handler completion now precede SQLite flushing. The store's nonblocking record queue is unchanged; there is no panic recovery, sleep-based fix, or added lock on every usage write.

Regressions:

- [`internal/webui/shutdown_test.go`](../internal/webui/shutdown_test.go) uses controlled runner completion and `testing/synctest` to prove that `Close` cannot return after mere cancellation. Active, replaced, and logged-out conversation cases all failed before the fix and pass afterward. Admission after closure is also rejected.
- `TestLifecycleHTTPDrainTimeoutPersistsFinalUsage` in [`tests/e2e/lifecycle_test.go`](../tests/e2e/lifecycle_test.go) holds an actual TCP stream past the graceful deadline, then restarts the binary and reads `/usage`. Before the executable fix the report contained zero rows. Afterward it contains the final measured usage and independently calculated charge.

Observed after the fix:

- `go test -race ./cmd/llm-router ./internal/... -count=1 -timeout=2m` passed.
- With a separately race-built router, `go test -race ./tests/e2e -run='^TestLifecycle' -count=1 -timeout=2m` passed, including both signals and the real 20-second drain-timeout path.
- `go test -tags=browser ./tests/e2e -run='^TestBrowserJourneys$' -count=1 -timeout=12m` passed.
- `go vet ./...` passed.

The remaining Codex denial-delivery discrepancy is outside this fix. Its tests are neither skipped nor weakened in the repository or CI.

Additional race-built process checks passed: five repetitions of
`TestLifecycleSignalsCancelConsoleAndInvalidateLogin`, and the accounting tests
plus agent active-cancellation/shutdown, blocked-input cancellation, session
retirement, and queued-cancellation regressions. These checks excluded the
unrelated Codex denial-delivery cases without modifying or skipping those tests
in the suite.

### Pull request isolation

The PR branch starts from the merged `main` tree. It excludes the pre-existing
local CLI refactor/help unit test, request-body extraction refactor, and Grafana
comparison document. The shutdown additions were applied directly to the
committed executable without those unrelated changes. Original working-tree
changes remain untouched.

The isolated PR tree passed `go vet ./...`, all seven Chromium journeys, and a
race-built router plus `go test -race ./... -skip='^TestAgentCodexDeniesHostApprovals$' -count=1 -timeout=5m`.
That explicit local command-line exclusion avoids reconfirming the known
unrelated failure; the tests themselves and unfiltered CI remain enabled.
