# Performance benchmarks

Compares the current gomutants checkout with pinned versions of its closest
buildable performance competitors:

- [Gremlins v0.6.0](https://github.com/go-gremlins/gremlins/releases/tag/v0.6.0)
- [Mutago v2.8.1](https://github.com/quality-gates/mutago/releases/tag/v2.8.1)

Gooze v1.0.1 is not timed because the audited tag does not compile.

## Prerequisites

Install the pinned competitor binaries:

```bash
bash benchmarks/install-tools.sh
```

Install `hyperfine` and `jq` with your system package manager. The harness also
needs the Go 1.25.7 toolchain; with Go toolchain auto-download enabled, the
first run obtains it automatically.

## Running

From the repository root:

```bash
bash benchmarks/run.sh

# Re-render results.md from existing JSON without rerunning hyperfine:
bash benchmarks/run.sh --summarize-only
```

The harness prefers binaries in `benchmarks/bin/`, then falls back to `PATH`.
Paths and measurement settings can be overridden explicitly:

```bash
GREMLINS=/path/to/gremlins \
MUTAGO=/path/to/mutago \
GO_TOOLCHAIN=go1.25.7 \
WORKERS=10 RUNS=3 TIMEOUT_COEF=50 \
bash benchmarks/run.sh
```

The script always rebuilds `bin/gomutants`, copies the benchmark targets into a
temporary isolated module, verifies its baseline tests, warms each command, and
then measures three runs with Hyperfine. Results are written to
[`results.md`](results.md); raw Hyperfine JSON, final reports, and warm-up logs
land in `benchmarks/out/` (gitignored).

## Scenarios

| Scenario | Target | Purpose |
|---|---|---|
| `small-defaults` | `./testdata/simple` | Product-facing fixed overhead with each tool's default mutator catalog. Workloads deliberately differ. |
| `small-matched` | `./testdata/simple` | Tiny-input engine comparison using four exactly shared operator semantics. |
| `mutator-matched` | `./internal/mutator` | Medium engine comparison where one-time setup can amortize across more mutants. |

The matched set contains arithmetic replacement, conditional boundary,
conditional negation, and unary-negative inversion. Gremlins' fifth default
operator, `INCREMENT_DECREMENT`, is excluded because Mutago does not implement
the same `++`/`--` transformation.

Mutago runs with `--coverage --per-test`, its strongest native execution mode.
Gomutants uses its default per-test routing, with its persistent result cache
disabled, and Gremlins uses its package-coverage workflow. All tools receive 10
workers and a timeout coefficient of 50.

## Why isolated Go 1.25 fixtures?

The main module now requires Go 1.26.10. Pinned Gremlins v0.6.0 panics while
executing these targets with Go 1.26.x, so directly benchmarking the live module
would fail before any useful timing. Copying the exact source and tests into a
temporary `go 1.25.7` module lets all three binaries execute the same target
under one compatible toolchain. The source tree is never modified.

## Reading the metrics

- Wall clock is the user-visible wait. Compare it with reported mutant counts;
  a lower time can represent less work.
- Time per tested mutant divides wall time by KILLED + LIVED/escaped. It excludes
  uncovered, non-viable, timed-out, errored, and skipped mutants.
- Mutago combines compile failures, execution errors, and timeouts into one
  `errored` count, so its timeout total cannot be reported separately.
- Absolute timings depend on machine load, temperature, and build-cache state.
  Re-run under quiet conditions before publishing the numbers.
