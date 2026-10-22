# Go fuzzing research for llm-router

## Recommendation and scope

- **Implemented baseline:** standard-library `testing.F` and native Go fuzzing; no new dependency.
- The four targets and execution results are mapped in [the test specification](testing-spec.md#11-implemented-suite-and-execution-results).
- Prioritize deterministic E2E examples for routing, HTTP/SSE behavior, configuration,
  persistence, and user-facing APIs; use fuzzing to explore their bounded data seams.
- Fuzzing does not replace an explicit behavioral oracle or a composed E2E scenario.
- Go's engine uses coverage feedback to retain interesting inputs [1]; this is an
  exploration mechanism, **not** a coverage percentage target or evidence that the
  suite protects every requirement. Assess the properties asserted, not a metric.
- This note records the tool research. The implementation was tested separately; consult the linked results rather than treating example budgets as measured runs.

## Native target contract

- Define `func FuzzXxx(f *testing.F)` in an `_test.go` file, calling `f.Fuzz` exactly
  once unless the test first skips or fails. Its callback starts with `*testing.T`
  and returns nothing. Seed argument types/order must match the callback [1][2].
- Supported values: `string`, `[]byte`, `bool`, `int`, `int8`, `int16`, `int32`
  (`rune`), `int64`, `uint`, `uint8` (`byte`), `uint16`, `uint32`, `uint64`,
  `float32`, and `float64`; arbitrary structs/maps are not native arguments [1][2].
- Use bytes/strings for serialized YAML, JSON, SSE, or command sequences; interpret
  them with a deterministic, bounded decoder when structured inputs are needed.
- Targets must be fast, deterministic, and independent of shared state. Do not
  retain mutable arguments or their pointers between calls, or mutate the engine's
  supplied byte backing storage; copy first if a tested operation mutates it [2].
- **Project isolation requirement:** never contact provider services, use real
  credentials, or mutate the process-global environment inside a fuzz callback.
  Workers execute callbacks concurrently and in nondeterministic order [1][3].
- Supply explicit per-case dependencies; use local in-memory data and, only where
  needed, isolated temporary files or a fresh local test server. Reset state per
  invocation. A separate deterministic E2E suite owns real HTTP composition.
- Parsing rejection is normally a valid result, not a failure; assert the agreed
  rejection behavior. Skip only inputs outside a documented property domain.

## Corpus, replay, and minimization

- Seed corpus entries come from `f.Add(...)` and package-local
  `testdata/fuzz/FuzzTargetName/` files. Ordinary `go test` executes those seeds,
  without generating new mutations [1][2].
- During active fuzzing the baseline includes both seed corpus and generated
  corpus; coverage-expanding inputs are cached for future fuzzing [1][4].
- Generated corpus is stored under the Go build cache, not automatically checked
  into the repository, and is not used by ordinary test runs [4].
- On a reproducible failing input the engine attempts minimization and writes a
  regression corpus file under `testdata/fuzz/<target>/<hash>`; its output provides
  a `go test -run=...` replay command [1]. Preserve that file with the fix.
- Minimization is an attempt, not a guarantee of a globally smallest input;
  `-fuzzminimizetime` bounds each attempt (default 60s), accepts durations or
  iteration counts such as `100x`, and `0` disables minimization [1][3].
- Corpus files use Go's versioned textual encoding (`go test fuzz v1`); their
  value types/order must match the callback. Raw JSON/YAML fixtures are not
  directly interchangeable with encoded corpus files [1].
- Keep representative valid/invalid seeds and every valuable fixed regression in
  version control. Review generated fixtures for secrets before publishing them.

## Executable target commands

Run from the repository root. The named config and inference targets are
implemented. Each active invocation selects one package and exactly one target;
`-fuzz` cannot fan out across `./...` [3].

```sh
# Deterministic suite plus all checked-in fuzz seeds; disable result-cache reuse.
go test ./... -count=1 -timeout=5m

# FuzzConfigDecode in internal/config: short local/PR exploration.
# -run='^$' avoids ordinary tests and other fuzz seed runs during this command.
go test ./internal/config -run='^$' -fuzz='^FuzzConfigDecode$' -fuzztime=30s -fuzzminimizetime=10s -parallel=2 -timeout=2m

# FuzzSSEFraming in internal/inference: longer scheduled exploration.
go test ./internal/inference -run='^$' -fuzz='^FuzzSSEFraming$' -fuzztime=10m -fuzzminimizetime=30s -parallel=2 -timeout=15m

# FuzzConfigDecode: execution-count budget, not a time guarantee.
go test ./internal/config -run='^$' -fuzz='^FuzzConfigDecode$' -fuzztime=10000x -parallel=2 -timeout=5m

# Failure replay: replace HASH with the basename printed by the engine.
HASH=replace_with_reported_corpus_hash
go test ./internal/config -run="^FuzzConfigDecode$/${HASH}$" -count=1 -timeout=1m
```

`-run` selects tests/examples/fuzz seed runs; `-count=1` prevents successful test
result-cache reuse. `-count` does not multiply an actively fuzzed target [3].
The explicit timeout values are proposed budgets, not measured project timings.

## Parallelism, deadlines, and CI policy

- `-parallel` limits concurrent fuzz worker subprocesses even without `t.Parallel`;
  its default is `GOMAXPROCS`. Excess workers can degrade performance through CPU
  contention. `-cpu` does not apply to the actively fuzzed target [3].
- Set an explicit small worker count per CI job and avoid oversubscribing multiple
  targets on the same runner. Prefer a target matrix or sequential commands.
- `-fuzztime` defaults to indefinite execution; specify a duration or `Nx` count.
  `-timeout` is a test-binary deadline (default 10m), distinct from mutation time
  and per-attempt minimization time. Leave budget for setup and minimization [3].
- Official fuzz documentation lists a one-second target execution timeout [1];
  Go 1.26 also defines a one-second worker responsiveness timeout [5]. Do not
  treat this as an application-level deadline, or use fuzzing for slow E2E cases.
- Bound input lengths, decoded operation counts, and resource sizes in each
  harness. Test application cancellation/timeouts separately with controlled
  dependencies; never recover panics merely to make a fuzz target pass.
- Proposed CI: always replay checked-in seeds; optionally run short bounded
  exploration on PRs and longer scheduled jobs, separately from E2E tests.
- Persist the Go build cache across exploration jobs when practical. `GOCACHE`
  must be an absolute path; `go env GOCACHE` reports its location [4]. Use an
  OS/architecture/toolchain-scoped cache policy rather than sharing blindly.
- Upload failure logs and newly written `testdata/fuzz` files even on job failure;
  preserve the full generated cache separately if desired. A cache is not a
  substitute for checked-in regression seeds.
- `go clean -fuzzcache` removes generated fuzz cache entries, not checked-in
  `testdata` files, and can temporarily reduce exploration effectiveness [4].

## Properties and optional structured/stateful generation

All properties below are **proposed requirements**; confirm semantics against the
behavior specification before implementing them, rather than blessing current output.

- Configuration: arbitrary YAML never panics; accepted configurations satisfy
  explicit invariants; rejected configurations return an error. The current
  loader has textual errors, not stable typed categories. Do not invent an
  encoder round-trip contract where no configuration encoder exists.
- SSE: arbitrary chunk boundaries preserve the same assembled event sequence as
  unchunked input; malformed/truncated frames terminate predictably without panic.
- Protocol conversion: preserve agreed roles, text/tool ordering, tool-call IDs,
  and finish reasons for supported messages; unsupported data follows explicit
  error/omission policy. Avoid assuming all provider formats are bijective.
- Optional **stdlib-only stateful generator**: interpret `[]byte` as bounded
  commands against a fresh routing/session instance and a small independent
  reference model. Compare results and observable state after each operation.
  Seed valid traces plus invalid transitions; preserve the bytes as the replay
  input. This is a harness design, not a built-in Go state-machine facility.
- Keep raw malformed-input targets alongside structured generators; constrain
  generation without eliminating meaningful invalid cases. Add an external
  property library only if its generators/shrinking demonstrably simplify a
  needed model; no additional dependency is recommended for the baseline.

## Primary sources

[1]: https://go.dev/doc/security/fuzz/ "Official Go fuzzing guide"
[2]: https://github.com/golang/go/blob/go1.26.0/src/testing/fuzz.go "Go 1.26 testing.F contract"
[3]: https://github.com/golang/go/blob/go1.26.0/src/cmd/go/alldocs.go#L3290-L3445 "Go 1.26 test flags"
[4]: https://github.com/golang/go/blob/go1.26.0/src/cmd/go/alldocs.go#L2323-L2356 "Go 1.26 build and fuzz cache"
[5]: https://github.com/golang/go/blob/go1.26.0/src/internal/fuzz/worker.go#L24-L31 "Go 1.26 worker timing constants"
