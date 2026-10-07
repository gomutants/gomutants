# Performance

_Last measured: 2026-08-10 for the in-repo three-tool comparison and
2026-05-12 for the broader real-world study, on a 10-core Apple M1 Pro.
Toolchain numbers shift with each Go release; treat these as snapshots, not
specifications._

Mutation testing on real Go packages. This page documents the methodology
and the numbers it produced on six targets — a tiny in-repo fixture
(`./testdata/simple/`), a small fast project (`google/uuid`), a medium
CLI (`spf13/cobra`), a single-package YAML diff tool
(`szhekpisov/diffyml`), a foundational package inside a large monorepo
(`prometheus/model/labels`), and a 4-package combined run inside the same
monorepo's tsdb layer
(`prometheus/{tsdb/chunkenc, tsdb/index, tsdb/chunks, tsdb/record}`, ~24k
LOC) — so you can reproduce them on your own hardware. The in-repo
harness at `benchmarks/` covers a couple of additional in-repo targets
(`./internal/mutator`).

## 2026-08-10 three-tool refresh

The current in-repo harness adds Mutago and refreshes Gremlins against the
v0.6.0-rc1 gomutants checkout. It copies the target source into an isolated
`go 1.25.7` module, uses 10 workers, disables gomutants's persistent cache,
warms each command, and reports three-run Hyperfine means. Mutago runs with
its opt-in coverage and per-test routing enabled.

| Scenario | gomutants | Gremlins 0.6.0 | Mutago 2.8.1 | Reported mutants (g / G / M) |
|---|---:|---:|---:|---:|
| `testdata/simple`, default catalogs¹ | 11.01 ± 0.06 s | **4.27 ± 0.15 s** | 11.33 ± 1.90 s | 68 / 20 / 63 |
| `testdata/simple`, matched four operators | 4.25 ± 0.06 s | **3.95 ± 0.03 s** | 4.06 ± 0.04 s | 18 / 19 / 18 |
| `internal/mutator`, matched four operators | **14.22 ± 0.13 s** | 23.16 ± 0.25 s | 22.22 ± 7.94 s² | 84 / 89 / 71 |

¹ Default catalogs deliberately do different work; this row measures the wait
from each product configuration, not equal engine throughput.

² Mutago's first measured medium run was 31.38 s and its next two were about
17.5 s, producing the large standard deviation. Three samples establish the
direction here, not a precise long-run estimate.

The matched set is arithmetic replacement, conditional boundary, conditional
negation, and unary-negative inversion—the transformations shared exactly by
all three tools. Mutant positions and viability filters still differ, so the
medium row should be read with both wall time and tested-mutant throughput:
gomutants used 173 ms per KILLED/LIVED mutant, Gremlins 260 ms, and Mutago
313 ms. Total wall time made gomutants 1.63× faster than Gremlins and 1.56×
faster than Mutago on that target. On the tiny matched target the ordering was
effectively tied: all three completed within 8%.

Pinned Gremlins v0.6.0 panics when executing these targets under Go 1.26.x,
while the current gomutants module itself requires Go 1.26.1. The isolated Go
1.25.7 fixture is therefore necessary to put all three tools on one working
toolchain. Gooze v1.0.1 remains excluded because the audited tag does not
compile. See the [generated result](../benchmarks/results.md) and
[harness documentation](../benchmarks/README.md) for outcome tables, raw-data
locations, exact flags, and reproduction instructions.

## Headlines from the May 2026 real-world study

If you only read one section:

- **Engine wall time** depends on workload size. Gomutants is **1.89×
  slower** than gremlins on the trivial in-repo `./testdata/simple/`
  (19 mutants), **0.93× on uuid** (~120 mutants), pulls ahead
  **1.55–1.78×** on single-package medium targets (model/labels, cobra,
  diffyml), and ties again on the 4-package tsdb target (~10% delta
  either way) where gomutants's one-shot multi-package setup balances
  against gremlins's per-subpackage setup paid 4×. The crossover is the
  point where there are enough mutants to amortize gomutants's one-time
  setup (coverage gather, baseline measurement, per-test coverage map).
- **Warm-cache rerun** is **~120–225× faster** than cold OOB on real-
  world targets with many mutants (cobra ~150×, model/labels ~120×,
  tsdb-4 ~145×, diffyml ~225×); ~24× on uuid and only ~3.5× on
  `./testdata/simple/` because the fixed pre-flight floor (`go list`,
  `go test -count=1 -coverprofile`, `go test -list`) is a larger
  fraction of tiny cold runs. Gremlins has no cache equivalent.
- **Adaptive timeouts shift more weight as targets get faster.** Gremlins
  with the default `--timeout-coefficient=20` lost mutants to its fixed
  ceiling on every fast target: 70% on `./testdata/simple/`, 30.7% on
  diffyml, 26% on uuid. Gomutants's adaptive per-mutant timeout sizes
  each deadline from per-test runtimes × margin and hit zero timeouts on
  the matched-operator scope of all three.
- **Go 1.25.7 vs 1.26.3 is mostly a wash for gomutants's engine**:
  identical on uuid and cobra (within run noise); real **+7.3% slowdown**
  on prometheus/model/labels (regex-heavy hot path, suspected stdlib
  change); on tsdb-4 the wall-time delta is confounded by sequential-run
  cache warming, but mutant outcomes shift (~30–60 mutants reclassify
  KILLED → LIVED — see caveats on noise).
- **Methodology gaps to know about up front**: cobra and tsdb-4 OOB rows
  are single-run (each takes 7–46 min, so a 3-run matrix wasn't
  affordable); the tsdb-4 gremlins row is sum-of-4-subpackage-invocations
  because `gremlins unleash` accepts only one target argument.

## Targets at a glance

| Target | LOC | Packages | Baseline `go test` | Mutants (gomutants OOB) |
|---|---:|---:|---:|---:|
| `./testdata/simple/` (in-repo) | ~70 | 1 | ~1.0 s | 36 |
| google/uuid (root) | ~2.3k | 1 | ~1.0 s | 464 |
| spf13/cobra (root) | ~6.1k | 1 | ~3.0 s | 1706 |
| szhekpisov/diffyml (`./pkg/diffyml/`) | ~8.0k | 1 | ~0.5 s | 2276 |
| prometheus/model/labels | ~4.0k | 1 | ~3.0 s | 1324 |
| prometheus tsdb-4 (combined chunkenc / index / chunks / record) | ~24k | 4 | ~5.0 s | 6155 |

The "Mutants (gomutants OOB)" column counts the full mutant set —
KILLED + LIVED + NOT_COVERED + TIMED_OUT + NOT_VIABLE — recovered by
walking the per-mutant JSON. gomutants's top-level `mutants_total` JSON
field excludes NOT_COVERED, so it reads lower than the walked total;
each per-target table below uses the walked total.

## Environment

| | |
|---|---|
| Host | macOS 26.3.1, Apple M1 Pro (10 cores) |
| Go | go1.25.7 darwin/arm64 (cross-comparison rows) and go1.26.3 darwin/arm64 (gomutants-only rows) — forced via `GOTOOLCHAIN`. See "Go 1.26.x compatibility" below. |
| gomutants | v0.2.2 |
| gremlins | v0.6.0 (release tarball) |
| Workers | 10 |
| Hyperfine | 3 runs + 1 warmup on uuid; single-run on cobra (8-min cold runs make repeats expensive) |
| Power | AC, no other CPU-bound load |

The M1 Pro mixes performance and efficiency cores and pipes them through
the same `runtime.NumCPU()` count, so 10 workers oversubscribe slightly
under sustained load. On a Linux x86 box with 10 homogeneous cores you
should expect somewhat lower per-mutant wall times and a slightly
different cold-cache penalty profile. Absolute numbers won't transfer;
the relative ordering between tools and the warm-cache-vs-cold ratios
should.

## Methodology

Two questions worth answering separately:

1. **Engine speed**: given the same set of mutants, how long does the test
   loop take?
2. **Out-of-the-box experience**: what does a user actually wait for when
   they run the tool with defaults?

The first needs a matched operator set. The second deliberately doesn't.

On `google/uuid` each scenario runs through hyperfine (3 measurements + 1
warmup) and the table reports mean ± stddev. On `spf13/cobra` each cold
scenario is a single timed run because each takes minutes; the warm-cache
scenario still uses hyperfine since each run finishes in seconds.

### Cold runs (cache off)

Both targets use the same three command shapes. The argument is `.` for
gremlins (it does not handle `./...` reliably on either target — see
caveats) and matched to `.` for gomutants on cobra so the comparison covers
the same packages. On `google/uuid` (single root package) `./...` and `.`
are equivalent for gomutants.

```bash
# 1. gremlins, 10 workers, default 5 operators
gremlins unleash --workers 10 --timeout-coefficient 20 \
  --silent --output gremlins.json .

# 2. gomutants out-of-box (10 workers, all default operators)
gomutants -workers 10 --cache=off -quiet \
  -output gomutants-oob.json .

# 3. gomutants restricted to gremlins's 5 default operators
gomutants -workers 10 --cache=off -quiet \
  -only=ARITHMETIC_BASE,CONDITIONALS_BOUNDARY,CONDITIONALS_NEGATION,\
INCREMENT_DECREMENT,INVERT_NEGATIVES \
  -output gomutants-l4l.json .
```

`--cache=off` is important: a partial cache hit would skew gomutants
favourably against a tool with no equivalent. Run 3 (like-for-like) is the
clean engine comparison; run 1 and run 2 together describe the user-facing
experience.

### Warm cache

To show the incremental-development behaviour:

```bash
gomutants -workers 10 -output prime.json .                  # primes cache
hyperfine --runs 3 'gomutants -workers 10 -output run.json .'  # warm runs
```

The cache is byte-keyed on source and tests; an unchanged tree skips the
test loop entirely.

### Computing efficacy

Both tools report a `test_efficacy` field, but they compute the denominator
differently. To make the column comparable, this page uses

```
efficacy = KILLED / (KILLED + LIVED + TIMED_OUT)
```

Including `TIMED_OUT` in the denominator avoids treating a timed-out mutant
as a free pass. Gremlins's JSON otherwise reports 100% in scenarios where a
quarter of its mutants timed out, which overstates the result.

## Results: ./testdata/simple/

- In-repo fixture used as a smoke test for both engines.
- Source size: ~70 LOC, single package with one trivial function and a
  small companion test file.
- Baseline `go test ./testdata/simple/`: ~1.0 s.

A deliberately tiny target. The point isn't engine throughput — there's
barely enough mutation work to compare — it's to show what gomutants's
fixed setup floor costs when there are too few mutants to amortize it.
Also exposes how gremlins's fixed-coefficient timeout behaves at the
fastest end of the spectrum.

### Wall time

| # | Tool | Mode | Wall time (mean ± σ, 3 runs) |
|---|---|---|---|
| 1 | gremlins (dev) | 10w, 5 default ops, `--timeout-coefficient=20` | 4.95 ± 0.08 s |
| 1a | gremlins (dev) | 10w, 5 default ops, `--timeout-coefficient=50` | 7.53 ± 0.48 s |
| 2 | gomutants v0.2.2 | out-of-box (10w, all ops, cache off) | 21.34 ± 0.47 s |
| 3 | gomutants v0.2.2 | like-for-like (10w, 5 ops, cache off) | **14.26 ± 1.49 s** |
| 4 | gomutants v0.2.2 | warm cache (10w, all ops, cache on, 2nd+ run) | **6.15 ± 2.02 s** |

### Mutant outcomes

| # | Total | KILLED | LIVED | NOT_COVERED | TIMED_OUT | NOT_VIABLE | Efficacy |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1 | 20 | 0 | 0 | 6 | **14** | 0 | 0.0% |
| 1a | 20 | 11 | 3 | 6 | 0 | 0 | 78.6% |
| 2 | 36 | 29 | 3 | 0 | 0 | 4 | 90.6% |
| 3 | 19 | 16 | 3 | 0 | 0 | 0 | 84.2% |
| 4 | 36 | 29 | 3 | 0 | 0 | 4 | 90.6% |

### Reading the testdata/simple results

- **Small-workload caveat is real.** Like-for-like (1a vs 3), gomutants
  is **1.89× slower** wall-clock than gremlins (14.26 vs 7.53 s) on this
  19-mutant target. Per-mutant cost (KILLED+LIVED): gomutants ~750 ms vs
  gremlins ~540 ms. Same direction as uuid (gomutants 0.93×) at smaller
  scale — gomutants's one-time setup (coverage gather, baseline timing,
  per-test coverage map) is fixed cost that only amortizes when many
  mutants share it.
- **Adaptive timeouts dominate at the default coefficient.** Row 1
  (gremlins, coef=20): 14 / 20 mutants timed out (70%); efficacy collapses
  to 0%. Row 1a bumps the coefficient to 50 to land a workable result. On
  fast targets, fixed-coefficient timeout sizing fails under any worker
  contention. Gomutants's adaptive per-mutant timeout hit 0 timeouts at
  the same `--timeout-coefficient=20` ceiling.
- **Warm cache is only ~3.5× faster here**, not the 120–225× the bigger
  targets show. Warm-rerun wall time is dominated by pre-flight toolchain
  calls (`go list`, `go test -count=1 -coverprofile`, `go test -list`) —
  see "Why warm-cache time doesn't track project size" below for the
  breakdown. On a 36-mutant target the cached short-circuit phase is
  effectively instant, but the setup floor (~3 s) is hard to undercut.
- **NOT_COVERED interpretation differs.** Gremlins flags 6 not-covered
  mutants; gomutants flags 0. Same lines: gomutants's per-test coverage
  finds tests that touch them, gremlins's package-level filter doesn't —
  the same delta documented for model/labels.

## Results: google/uuid

- Pinned commit: `2d3c2a9cc518326daf99a383f07c4d3c44317e4d`
- Source size: ~2.3k LOC, single root package, ~88% line coverage.
- Baseline `go test ./...`: ~1.0–1.2 s.

A small, self-contained package with no external services, no build-tag
gating on the test path, and no `go` directive in `go.mod`. Cold runs
finish in seconds.

### Wall time

| # | Tool | Mode | Go | Wall time (mean ± σ) |
|---|---|---|---|---|
| 1 | gremlins v0.6.0 | 10w, 5 default ops, `--timeout-coefficient=20` | 1.25.7 | 27.48 ± 0.23 s |
| 2 | gomutants v0.2.2 | out-of-box (10w, all ops, cache off) | 1.25.7 | 77.35 ± 0.30 s |
| 3 | gomutants v0.2.2 | like-for-like (10w, 5 ops, cache off) | 1.25.7 | 29.66 ± 0.28 s |
| 3a | gomutants v0.2.2 | like-for-like (10w, 5 ops, cache off) | **1.26.3** | **29.73 ± 0.11 s** |
| 4 | gomutants v0.2.2 | warm cache (10w, all ops, cache on, 2nd+ run) | 1.25.7 | **3.22 ± 0.02 s** |

### Mutant outcomes

| # | Total | KILLED | LIVED | NOT_COVERED | TIMED_OUT | NOT_VIABLE | Efficacy |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1 | 123 | 65 | 0 | 26 | **32** | 0 | 67.0% |
| 2 | 464 | 306 | 94 | 34 | 12 | 18 | 76.5% |
| 3 | 120 | 91 | 15 | 11 | **3** | 0 | 85.8% |
| 3a | 120 | 90 | 16 | 11 | 3 | 0 | 84.9% |
| 4 | 464 | 306 | 94 | 34 | 12 | 18 | 76.5% |

### Reading the uuid results

- **Engine throughput is roughly comparable on this small target.** On the
  matched operator set (runs 1 vs 3) the two engines finish within 8% of
  each other — 27.5 s vs 29.7 s for ~120 mutants. Mutant-discovery sets
  differ by ~3% (123 vs 120), which is normal AST-visitor variation, not
  an engine difference.

- **Adaptive timeouts dominate result quality.** Under identical workers
  and operator set, gremlins ran 32 / 123 (26%) of its mutants into the
  `--timeout-coefficient=20` ceiling; gomutants ran 3 / 120 (2.5%).
  Gomutants sizes per-mutant timeouts from the package's per-test
  runtimes plus a margin, so worker contention doesn't push tests past a
  fixed ceiling. With gremlins's default coefficient of 10, every covered
  mutant on this target timed out and the run was unusable; the table
  uses 20 as the lowest setting that yields a workable result.

- **More operators do more work.** Out-of-box gomutants (run 2) does
  ~3.8× the mutation work in ~2.8× the time of the matched gremlins run
  because gomutants ships additional operators — `STATEMENT_REMOVE`,
  `BRANCH_IF`, `INVERT_BITWISE`, etc. — that gremlins doesn't have.
  Whether you want that depends on the project; you can always restrict
  via `-only`.

- **Cache changes the loop.** Run 4 is the same workload as run 2 with
  the cache enabled on a re-run: 3.22 s, ~24× faster. This is the
  relevant number for the inner loop of "edit a test, re-run" — not for
  one-shot CI jobs, which should keep `--cache=off`.

## Results: spf13/cobra

- Pinned commit: `ad460ea8f249db69c943a365fb84f3a59042d54e`
- Source size: ~16.7k LOC across two packages (`cobra`, `cobra/doc`).
- Baseline `go test ./... -short`: ~3.0 s.

A larger target that exercises the engine's per-mutant overhead. The
benchmark targets the root `cobra` package only (`.`), to match what
gremlins can actually consume. A separate "full repo" measurement on
`gomutants ./...` is included for context.

Wall times here are 3-run medians for the like-for-like and gremlins rows
(each run takes 1–2 min, so 3 runs is affordable) and single-run for the
OOB rows (each takes ~7 min, so a 3-run hyperfine matrix would be 70+
minutes). Cobra's first run on each scenario is consistently 30–50%
slower than runs 2–3 because gomutants pre-builds the test binary and the
Go build cache is cold on first invocation; reporting the median of 3
factors that out. Single-run OOB rows include that cold-cache artifact;
treat them as upper bounds and ±15% rather than precise.

### Wall time

| # | Tool | Mode | Go | Target | Wall time |
|---|---|---|---|---|---|
| 1 | gremlins v0.6.0 | 10w, 5 default ops, `--timeout-coefficient=20` (median of 3) | 1.25.7 | `.` | 129.1 s |
| 2 | gomutants v0.2.2 | out-of-box (10w, all ops, cache off, single run) | 1.25.7 | `.` | 410.2 s |
| 3 | gomutants v0.2.2 | like-for-like (10w, 5 ops, cache off, median of 3) | 1.25.7 | `.` | **72.6 s** |
| 3a | gomutants v0.2.2 | like-for-like (10w, 5 ops, cache off, median of 3) | **1.26.3** | `.` | **73.0 s** |
| 4 | gomutants v0.2.2 | warm cache (10w, all ops, cache on, 2nd+ run, hyperfine 3×) | 1.25.7 | `.` | **2.73 ± 0.11 s** |
| — | gomutants v0.2.2 | full repo, OOB (10w, all ops, cache off, single run) | 1.25.7 | `./...` | 485.0 s (context only) |

### Mutant outcomes

| # | Total | KILLED | LIVED | NOT_COVERED | TIMED_OUT | NOT_VIABLE | Efficacy |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1 | 556 | 402 | 88 | 65 | 1 | 0 | 81.9% |
| 2 | 1706 | 1210 | 192 | 184 | 8 | 112 | 85.8% |
| 3 | 461 | 350 | 33 | 48 | 0 | 30 | 91.4% |
| 3a | 461 | 346 | 37 | 48 | 0 | 30 | 90.3% |
| 4 | 1706 | 1208 | 193 | 184 | 9 | 112 | 85.7% |

### Reading the cobra results

- **Engine ordering flips on bigger packages.** On the like-for-like row
  (1 vs 3), gomutants is **1.78× faster** in wall-clock — 72.6 s vs
  129.1 s — opposite of uuid where gremlins narrowly led. Per-mutant cost
  for gremlins on cobra is ~264 ms (KILLED+LIVED only; ignores
  NOT_COVERED); for gomutants it's ~190 ms. The gap comes from gomutants
  pre-building and reusing the test binary while gremlins fork-execs a
  fresh `go test` (with full compile) per mutant. That compile overhead
  amortizes badly on cobra's larger package.

- **Mutant counts diverge more than on uuid (556 vs 461).** Most of the
  delta is gomutants's pre-filtering: 30 mutants flagged `NOT_VIABLE`
  (won't compile or are syntactic no-ops) that gremlins doesn't filter
  and instead either skips silently or runs and reports as KILLED. The
  remainder (17) is AST-visitor coverage variation. Both KILLED counts
  represent real test work, so the wall-time comparison is still fair.

- **Timeouts are no longer the headline.** Cobra's slower test suite
  means worker contention doesn't push individual mutant runs past the
  20× ceiling — only 1 timeout for gremlins, 0 for gomutants L4L. The
  uuid timeout pattern is specific to fast-test packages where 10
  workers oversubscribe the CPU.

- **Cache lands hard on bigger workloads.** Run 4: 2.73 s vs the 410 s
  cold OOB — **~150× faster**. The full mutant set (1706) is unchanged,
  but the cache is byte-keyed on source and tests, so an unchanged tree
  skips the test loop entirely.

- **Out-of-box: 7 minutes vs 2 minutes.** OOB gomutants takes ~3.2× the
  wall time of OOB gremlins on the same root package, but discovers
  ~3.1× the mutants (1706 vs 556) by running 16 operator types vs 5.
  Throughput per mutant tested is essentially equal between OOB and L4L
  for gomutants (~290 ms vs ~190 ms); the wall-time delta is purely
  workload size. Restrict with `-only` if you don't want it.

- **Go 1.25.7 vs Go 1.26.3 (rows 3 and 3a) is a wash.** The like-for-like
  cobra run measures 72.6 s on Go 1.25.7 and 73.0 s on Go 1.26.3, with
  identical mutant counts (461) and KILLED/LIVED within run-to-run
  noise. uuid shows the same equivalence (29.66 ± 0.28 vs 29.73 ± 0.11).
  Whatever toolchain change broke gremlins on Go 1.26.x didn't measurably
  affect gomutants's `go test`-driven loop. See "Go 1.26.x compatibility"
  below.

## Results: diffyml

- Pinned commit: `1367f6df81bf1bf85360824682bcab96388c6b38`.
- Source size: ~8.0k LOC in a single package (`./pkg/diffyml/`), 100%
  statement coverage in tests.
- Baseline `go test ./pkg/diffyml/`: ~0.5 s.

A YAML diff tool. Single-package, fast test suite, fully covered — a
useful counterpoint to model/labels (where heavy regex tests dominate)
and cobra (where the test suite is larger and slower). Per-test wall
times are short enough that gremlins's fixed-coefficient timeout becomes
the dominant factor on the L4L scope.

**Methodology note.** diffyml's `go.mod` requires `go 1.26.2`, so this
target uses `GOTOOLCHAIN=go1.26.3` rather than the `go1.25.7` other
tables use. The cross-comparison rows use a locally-built
`gremlins version dev` — gremlins v0.6.0 release silently produces zero
mutants on Go 1.26.x (the same issue documented under "Go 1.26.x
compatibility" below); the dev build escapes that regression and
discovered 824 mutants on diffyml in dry-run. Numbers are therefore not
directly comparable to the gremlins-v0.6.0 release-tarball rows on
other targets, but the L4L ordering is consistent with cobra and
model/labels.

L4L is single-run-per-tool here, with gomutants L4L reported as a 3-run
median because run 1 paid cold-Go-build-cache cost (213 / 123 / 182 s
across the three runs).

### Wall time

| # | Tool | Mode | Wall time |
|---|---|---|---|
| 1 | gremlins dev | 10w, 5 default ops, `--timeout-coefficient=20`, single run | 317 s |
| 2 | gomutants v0.2.2 | like-for-like (10w, 5 ops, cache off, median of 3) | **182 s** |
| 3 | gomutants v0.2.2 | out-of-box (10w, all ops, cache off, single run) | 642 s (~10.7 min) |
| 4 | gomutants v0.2.2 | warm cache (10w, all ops, cache on, hyperfine 3×) | **2.85 ± 0.15 s** |

### Mutant outcomes

| # | Total | KILLED | LIVED | NOT_COVERED | TIMED_OUT | NOT_VIABLE | Efficacy |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1 | 824 | 551 | 0 | 20 | **253** | 0 | 68.5%¹ |
| 2 | 611 | 571 | 0 | 0 | 0 | 40 | **100%** |
| 3 | 2276 | 1683 | 247 | 1 | 19 | 326 | 87.2% |
| 4 | 2276 | 1683 | 247 | 1 | 19 | 326 | 87.2% |

¹ Gremlins's own JSON reports `test_efficacy: 100%` here — its formula
is `K / (K+L)` and there are 0 LIVED. The 68.5% in this table uses this
page's `K / (K+L+TO)` denominator, which folds in the 253 timed-out
mutants.

### Reading the diffyml results

- **L4L wall time: gomutants 1.74× faster.** 182 s median vs 317 s for
  gremlins. Per-mutant cost (KILLED+LIVED): gomutants ~319 ms vs gremlins
  ~575 ms (1.80× faster per mutant). Same pattern as cobra and
  model/labels — gomutants's pre-built test binary amortizes across the
  ~600-mutant L4L set while gremlins compiles `go test` fresh per
  mutant.
- **Adaptive timeouts are the headline at coef=20.** Gremlins burned
  253 / 824 mutants (30.7%) in the fixed timeout ceiling at the default
  coefficient — within the same regime as uuid (26%) and below
  `./testdata/simple/` (70%), where per-test wall times are even
  shorter. Gomutants's adaptive per-mutant timeout (deadline =
  `sum(selected test durations) × --timeout-margin`, clamped to
  `[--timeout-min, global ceiling]`) hit zero timeouts at L4L scope.
  Even at OOB scope across 16 operators on 2276 mutants, only 19
  timeouts surface (0.8%).
- **OOB runs 16 ops vs 5.** 2276 mutants in 642 s — per-K+L cost ~334 ms
  is close to the L4L number (319 ms), so the extra wall time is
  workload size, not engine drift on the wider operator set.
- **Warm cache is ~225× faster than cold OOB** (2.85 s vs 642 s) — the
  largest warm-rerun multiple on this page. Diffyml's baseline `go test`
  finishes in ~0.5 s, so the pre-flight `go test -count=1
  -coverprofile` (the dominant warm-cache cost per "Why warm-cache time
  doesn't track project size") is similarly short.
- **L4L kills 100% on this target.** Diffyml's test suite is
  well-instrumented enough that the matched 5-operator set finds no
  surviving mutants on its line-covered code. At OOB scope, 247 LIVED
  surface from gomutants's block-level operators (`STATEMENT_REMOVE`,
  `BRANCH_*`) exposing tests with weak assertions — the 87.2% OOB
  efficacy is the honest score on a wider operator surface.

## Results: prometheus/model/labels

- Pinned commit: `ecab2f45a8b7a1f12b8a16590a56590c96422f44`
- Source size: ~4.0k LOC, single package within a 245-package monorepo.
- Baseline `go test ./model/labels`: ~3.0 s (with 10 cores; user 8.9 s).

A foundational package from a real production codebase. Highly parallel
test suite (test-level parallelism saturates 10 cores), regex-heavy
matchers, and integration with the rest of the Prometheus monorepo via a
fat `go.mod`. Targets `./model/labels` from the repo root so both tools
resolve the package within the surrounding module.

Same methodology as cobra: 3-run medians for L4L and gremlins, single
run for OOB.

### Wall time

| # | Tool | Mode | Go | Target | Wall time |
|---|---|---|---|---|---|
| 1 | gremlins v0.6.0 | 10w, 5 default ops, `--timeout-coefficient=20` (median of 3) | 1.25.7 | `./model/labels` | 139.0 s |
| 2 | gomutants v0.2.2 | out-of-box (10w, all ops, cache off, single run) | 1.25.7 | `./model/labels` | 342.4 s |
| 3 | gomutants v0.2.2 | like-for-like (10w, 5 ops, cache off, median of 3) | 1.25.7 | `./model/labels` | **89.8 s** |
| 3a | gomutants v0.2.2 | like-for-like (10w, 5 ops, cache off, median of 3) | **1.26.3** | `./model/labels` | **96.4 s** |
| 4 | gomutants v0.2.2 | warm cache (10w, all ops, cache on, hyperfine 3×) | 1.25.7 | `./model/labels` | **2.84 ± 0.16 s** |

### Mutant outcomes

| # | Total | KILLED | LIVED | NOT_COVERED | TIMED_OUT | NOT_VIABLE | Efficacy |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1 | 859 | 352 | 86 | 419 | 2 | 0 | 80.0% |
| 2 | 1324 | 817 | 263 | 165 | 33 | 46 | 73.4% |
| 3 | 500 | 356 | 85 | 51 | 3 | 5 | 80.2% |
| 3a | 500 | 356 | 85 | 51 | 3 | 5 | 80.2% |
| 4 | 1324 | 817 | 263 | 165 | 33 | 46 | 73.4% |

### Reading the prometheus results

- **Engine throughput gap widens further on this target.** Like-for-like
  (1 vs 3): gomutants is **1.55× faster** wall-clock — 89.8 s vs 139.0 s.
  Per-mutant cost (KILLED+LIVED only): gremlins ~317 ms, gomutants
  ~204 ms. Same root cause as cobra — gomutants's pre-built test binary
  amortizes the test-binary compile across all mutants while gremlins
  re-pays it per mutant. The win is bigger here than on cobra (1.78×) but
  the absolute per-mutant gap is similar; what's different is gremlins is
  testing nearly twice as many mutants (438 vs 441 K+L is similar, but
  gremlins also serially attempts mutants on uncovered lines and counts
  them differently — see below).

- **NOT_COVERED interpretation differs sharply.** gremlins reports 419
  NOT_COVERED mutants on this target; gomutants reports 51. Both ran the
  same 5 operators against the same source. The gap is in how each tool
  defines "covered": gomutants uses per-test coverage (only counts a line
  as not-covered if no test in the suite touches it), while gremlins's
  package-level coverage flags lines that look uncovered by the
  test-utility files (`test_utils.go`) and a few less-exercised paths.
  Both KILLED counts (352 vs 356) are within run-to-run noise, so the
  meaningful comparison is unaffected.

- **OOB times out on 33 mutants.** The default adaptive timeout struggles
  on this target's heavier per-test workload — model/labels has slow
  regex tests that already approach the timeout floor under contention.
  Tuning `-timeout-margin` higher (e.g. 5×) or running fewer workers
  reduces this. The warm-cache row preserves the same 33 timeouts because
  the cache stores the timed-out status as-is; users rerunning to confirm
  flakes can pass `--cache=off`.

- **Go 1.25.7 → 1.26.3 shows a real ~7% slowdown here.** 89.79 s →
  96.38 s, with very tight per-version stddevs (0.34 / 0.21 across runs
  2–3). uuid and cobra didn't show this; prometheus/model/labels does.
  The mutant set and KILLED/LIVED counts are identical (500 mutants, 356
  killed, 85 lived), so this isn't an engine difference — it's the
  underlying `go test` getting slightly slower on this codebase under the
  newer toolchain. Cause not bisected; regex is a plausible suspect
  given model/labels's hot path, but I didn't verify.

- **Cache lands the same way as on cobra.** Run 4: 2.84 s vs 342 s OOB —
  **~120× faster**. The 33 cached timeouts and the heavy mutant set
  (1324) re-emerge instantly on second run; if you've fixed flake causes,
  pass `--cache=off` to re-test from scratch.

## Results: prometheus tsdb-4 (combined)

- Pinned commit: `ecab2f45a8b7a1f12b8a16590a56590c96422f44` (same as
  `prometheus/model/labels`).
- Target: 4 packages run as a single gomutants invocation —
  `./tsdb/chunkenc ./tsdb/index ./tsdb/chunks ./tsdb/record`.
- Source size: ~24k LOC across the 4 packages.
- Baseline `go test -short` on the combined target: ~5.0 s (wall;
  parallel across 10 cores).

The point of this target is to show gomutants on a workload that
straddles four packages with mixed test characters: `chunkenc` (XOR /
histogram chunk encoding, fast tests), `index` (b-tree posting lists,
~4 s tests), `chunks` (on-disk chunk format, ~4 s tests), `record`
(write-ahead record format, fast tests). It's the largest target on
this page and the one that exercises the per-test coverage map build
across multiple packages.

**Gremlins methodology note.** gremlins's `unleash` CLI accepts at most
one target argument, so it cannot replicate this multi-package run in
one invocation. The gremlins row is the **sum of 3 outer runs × 4
per-subpackage invocations** (12 gremlins calls total) — what a user
would actually wait for if they decided to mutation-test these 4
packages with gremlins today. This methodology disadvantages gremlins
on setup (it pays per-subpackage coverage gather, baseline, and AST
parse 4× per outer run) but is the honest user-experience comparison.
The gomutants rows pay multi-package setup once per invocation.

### Wall time

| # | Tool | Mode | Go | Wall time |
|---|---|---|---|---|
| 1 | gomutants v0.2.2 | out-of-box (10w, all ops, cache off, single run) | 1.25.7 | 2768 s (~46.1 min) |
| 2 | gomutants v0.2.2 | like-for-like (10w, 5 ops, cache off, median of 3) | 1.25.7 | 1272 s (~21.2 min) |
| 2a | gomutants v0.2.2 | like-for-like (10w, 5 ops, cache off, median of 3) | **1.26.3** | **855 s (~14.3 min)** |
| 3 | gomutants v0.2.2 | warm cache (10w, all ops, cache on, hyperfine 3×) | 1.25.7 | **19.0 ± 0.47 s** |
| 4 | gremlins v0.6.0 | per-subpkg × 4, 10w, 5 default ops, `--timeout-coefficient=20`, sum-of-medians | 1.25.7 | **951 s (~15.9 min)** |

The Go 1.25.7 and 1.26.3 L4L numbers are **not directly comparable** —
the 6 L4L runs were executed sequentially in order (1.25.7 r1, r2, r3,
then 1.26.3 r1, r2, r3) and the Go build cache warmed monotonically
across all 6: wall times went 1343 → 1272 → 1107 → 938 → 855 → 852 s,
so the 1.25.7 numbers paid more cold-cache cost. The steady-state L4L
wall on this target is **~850 s regardless of toolchain version**, with
the kind of run-1-of-each-batch overhead pattern documented in the
cobra caveats. See "Go 1.26.x compatibility" below for what *is* a real
toolchain difference here (test outcomes, not wall time).

### Mutant outcomes

| # | Total | KILLED | LIVED | NOT_COVERED | TIMED_OUT | NOT_VIABLE | Efficacy |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1 | 6155 | 3770 | 1199 | 735 | 186 | 265 | 73.0% |
| 2 | 2149 | 1571 | 334 | 191 | 45 | 8 | 80.6% |
| 2a | 2149 | 1514 | 389 | 191 | 47 | 8 | 77.6% |
| 3 | 6155 | 3771 | 1197 | 735 | 187 | 265 | 73.0% |
| 4 | 2172 | 1379 | 243 | 354 | 196 | 0 | 75.9% |

### Reading the tsdb-4 results

- **Engine-vs-engine wall time is essentially tied here.** L4L steady-state
  (gomutants Go 1.26.3 = 855 s) and gremlins sum-of-4 (951 s, median of
  3 outer runs) sit within ~10% of each other on this multi-package
  target. Much tighter than on single-package targets where gomutants's
  one-shot setup amortization pulled it ahead by 1.55–1.78× (model/labels
  and cobra). Why the gap closes here: gomutants pays multi-package setup
  cost once per invocation; gremlins pays per-subpackage setup ×4 per
  outer run. Those costs roughly cancel at this scope. **If you wanted
  gomutants's full advantage on tsdb-4, the warm-cache rerun (row 3,
  19 s) is the relevant comparison — gremlins has no cache equivalent.**

- **Gremlins still produces 4× more TIMED_OUT** (196 vs 45 / 47 in
  gomutants L4L). Consistent with the uuid finding: fixed
  `--timeout-coefficient` doesn't size per-mutant timeouts as well as
  gomutants's adaptive sizing under worker contention. The 196 timeouts
  are concentrated in `./tsdb/index` (177 of them), where the per-test
  cost is high enough that 20× the baseline gets hit on a non-trivial
  fraction of mutants.

- **Mutant discovery is close but not identical.** 2149 (gomutants) vs
  2172 (gremlins), 1% difference — within normal AST-visitor variation.
  But gremlins reports 0 NOT_VIABLE vs gomutants's 8, and 354 NOT_COVERED
  vs gomutants's 191 — same package-level-vs-per-test coverage definition
  difference documented for model/labels.

- **OOB is 46 min, warm-cache is 19 s — ~145× faster on re-run.** Same
  cache mechanism as the other targets, same byte-keyed invariants. On
  a real codebase of this size, that's the difference between "I'll
  rerun overnight" and "I'll rerun before my next git commit." Warm
  wall time is ~6× higher than on the smaller targets (3 s on uuid /
  cobra / model/labels) because the pre-flight `go test
  -count=1 -coverprofile` over 4 packages with ~5 s of cumulative
  cached-test time is correspondingly bigger — see "Why warm-cache time
  doesn't track project size" below for the breakdown logic.

- **Go 1.26.3 appears to shift test outcomes here, but the signal is
  noisy.** Row 2 (1.25.7 median): 1571 K / 334 L. Row 2a (1.26.3 median):
  1514 K / 389 L. The cross-toolchain delta is ~57 mutants K → L, but
  *within* a single toolchain the three runs already varied by ~22
  mutants (1.25.7: 1593 / 1571 / 1574; 1.26.3: 1516 / 1514 / 1515). The
  toolchain-attributable shift is therefore closer to **~30–60 mutants**
  (cross-toolchain delta minus intra-toolchain noise), not a clean 57.
  Distinct from prometheus/model/labels, where 1.26.3 changed wall time
  but not outcomes; here it's the opposite (outcomes appear to shift,
  wall time is confounded by cache warming). **Cause is unknown** — I
  did not bisect which mutants reclassify. Candidates worth checking if
  someone digs in are stdlib changes in 1.26 (regex, time, sync
  primitives are common suspects for tests that depend on undocumented
  runtime behavior), but that's conjecture, not finding.

- **NOT_VIABLE is dramatically lower than NOT_COVERED.** Across all
  rows, NV is 265 / 6155 in OOB (~4%) and 8 / 2149 in L4L (~0.4%). Most
  AST-level "would-have-been mutants" are syntactically valid here —
  good signal that gomutants's discovery operators are well-tuned for
  production Go.

- **TIMED_OUT count (186 in OOB) is 3% of the mutant set.** Higher than
  cobra (8 / 1706 = 0.5%) but lower than model/labels (33 / 1324 =
  2.5%). The index and chunks packages have a handful of slow
  table-driven tests near the per-mutant adaptive timeout ceiling. Most
  timeouts are stable across reruns (the warm-cache rerun reproduces
  187 of the 186 — same set ±1 from rerun variance), so they're a
  real test-suite signal, not just transient contention.

## Why warm-cache time doesn't track project size

Warm-cache wall times are remarkably similar across the three small-to-
medium targets despite radically different sizes — uuid 3.22 s, cobra
2.73 s, prometheus/model/labels 2.84 s — and cobra is *faster* than uuid
despite having ~3.7× more mutants and ~7× more LOC. That looks wrong
until you measure where the seconds actually go. The 4-package tsdb-4
target finally breaks the pattern at 19.0 s warm-cache wall time, for
reasons the breakdown below makes clear.

On a warm-cache run, gomutants's per-mutant work is genuinely free — every
mutant short-circuits to its cached status. The wall time is almost
entirely the pre-flight Go toolchain calls that run *before* the cache
lookup:

| Stage | uuid | cobra |
|---|---:|---:|
| `go list -json .` (package metadata) | 0.20 s | 0.09 s |
| `go test -count=1 -coverprofile` (fresh coverage) | 1.24 s | 0.91 s |
| `go test -count=1 .` (baseline timing for adaptive timeout) | 1.44 s | 1.51 s |
| `go test -list . .` (enumerate test fns) | 0.41 s | 0.46 s |
| `go list -f` | 0.05 s | 0.05 s |
| `go test -c -cover` (pre-build test binary) | 0.30 s | 0.35 s |

These were measured under a `go` wrapper, so the per-call durations are
inflated by the wrapper overhead and do not sum to the observed wall time
(some of these calls run in parallel). The ordering of relative cost is
what matters.

The dominant cost is **`go test -count=1 -coverprofile`** — gomutants
always re-collects a fresh coverage profile before consulting the cache,
and `-count=1` defeats Go's test result cache. That command's runtime is
set by the *test suite's wall-clock floor*, not the project's size.
Direct measurement, no wrapper:

```
uuid  go test -count=1 -cover .   real 1.24s   user 1.02s   sys 0.49s
cobra go test -count=1 -cover .   real 0.91s   user 0.67s   sys 0.69s
```

uuid's 59 tests run slower than cobra's 260 because uuid has
wall-clock-gated tests — `TestClockSeqRace`, `TestGetTime`,
`TestNewV6FromTimeGeneratesUniqueUUIDs`, `TestMonotonicTimeNow` — that
have small real-time floors which don't shrink with parallelism. Cobra's
tests are pure CPU-bound command-parsing unit tests that scale across
all 10 cores and finish in 0.91 s despite running ~4× more functions.

So warm-cache wall time is currently a function of test-suite character
**and number of packages**, not LOC. On single-package targets the
fixed setup is ~3 s. On the tsdb-4 multi-package target it scales to
~19 s because `go test -count=1 -coverprofile` is invoked once across
all 4 packages, the per-test coverage map build runs per-package, and
the baseline-timing call accumulates each package's test wall time. The
cached-mutant phase remains zero work; the linear-in-packages cost is
in the toolchain calls.

A future enhancement worth tracking: memoize the coverage profile on
the cache so warm-cache no-op runs skip the `-count=1` step entirely.
That would drop the single-package targets below ~1 s and the tsdb-4
target to single-digit seconds. Filed as
[issue #38](https://github.com/szhekpisov/gomutants/issues/38).

## Property-based suites: `--test-flags`

None of the targets on this page use property-based testing, so the
numbers above don't capture its cost — but it is the sharpest known
cliff. Every mutant on a covered line re-runs the whole property, and
these frameworks commonly default to 100 iterations, so a package's
mutation time scales with iterations × mutants rather than with mutants
alone.

`--test-flags` forwards flags to the inner `go test`, which makes the
iteration count a knob rather than a constant — via `-short`, or via
whatever flag the framework exposes for its own iteration count:

```bash
gomutants --changed-since main --test-flags '-short' ./...
```

The flags reach the per-mutant runs, the coverage run, and the baseline
run — never `go list` or the build steps, which is what makes this safe
where `GOFLAGS=-short` is not. The trade is explicit: fewer checks means
fewer chances to catch a mutant, so this belongs on a fast pre-push gate,
not on the run whose score you publish. Because the flags are part of the
cache identity, the two runs keep separate cache generations and neither
inherits the other's verdicts.

Two things bound the speedup:

- **The per-test timing phase is not covered.** It compiles with
  `go test -c` and drives the binary through `-test.*`-namespaced
  flags, so `-short` would need translating. It runs every test once
  at full cost no matter what `--test-flags` says. On a suite where
  that phase is a large share of wall clock, the end-to-end win is
  well short of the per-mutant ratio. Its durations also stop being
  usable as deadlines once the flags change what runs, so adaptive
  timeouts stand down and every mutant falls back to the global
  `baseline × --timeout-coefficient` ceiling — a second-order cost,
  since a hung mutant now occupies a worker for longer.
- **Only the mutant loop scales down.** Discovery, the coverage run,
  and compilation are unchanged, so the usual Amdahl ceiling applies.

Not benchmarked here — no target on this page exercises it.

## Go 1.26.x compatibility

The cross-comparison rows force `GOTOOLCHAIN=go1.25.7` because gremlins
v0.6.0 silently produces zero mutants on Go 1.26.x — coverage gathering
returns instantly, no error, no work done. To rule out a similar
regression in gomutants, the like-for-like rows are duplicated under
`GOTOOLCHAIN=go1.26.3` (rows 3a in both target tables).

| Target | Go 1.25.7 | Go 1.26.3 | Delta | Note |
|---|---:|---:|---:|---|
| google/uuid (L4L, hyperfine 3+1) | 29.66 ± 0.28 s | 29.73 ± 0.11 s | +0.2% (well below 1σ of combined run noise) | clean |
| spf13/cobra (L4L, median of 3) | 72.60 s | 72.96 s | +0.5% (run-to-run noise) | clean |
| prometheus/model/labels (L4L, median of 3) | 89.79 s | 96.38 s | **+7.3%** | real wall-time delta |
| prometheus tsdb-4 (L4L, median of 3) | 1272 s | 855 s | (confounded) | see below |

Mutant discovery is identical between toolchains across all four targets
(120 / 461 / 500 / 2149 mutants). KILLED/LIVED counts match exactly or
within run-to-run noise on uuid, cobra, and model/labels.

**The 7.3% slowdown on prometheus/model/labels** is a clean delta — tight
per-version stddevs (~0.3 s on each side), same mutant outcomes. Cause
not bisected; regex is a plausible suspect given model/labels's hot path,
but that's conjecture.

**The tsdb-4 row's wall-time difference is confounded** by cache warming
— the 6 L4L runs were sequential, so 1.25.7 paid cold-cache cost that
1.26.3 didn't. Steady-state L4L is ~850 s on both toolchains.

The tsdb-4 row also shows a **probable mutant-outcome shift under
1.26.3**: cross-toolchain median K is 57 lower (1571 → 1514). Intra-
toolchain variance is ~22 mutants across 3 runs, so the toolchain-
attributable delta is ~30–60 mutants — real but smaller than the raw
cross-median number suggests. Cause not bisected. uuid, cobra, and
model/labels don't show this shift.

## Mutant schemata (`--schemata`)

Measured on a **different host** from every other table on this page — Apple
M4 (10 cores), macOS, go1.26.2 darwin/arm64, 8 workers, `--cache=off` — so
these numbers are internally comparable but must not be read against the
M1 Pro rows above.

| Target | Mutants | Per-mutant builds | `--schemata` | Speedup | Schematized |
|---|---:|---:|---:|---:|---:|
| `./testdata/simple/` | 68 | 14.5 s | **1.7 s** | **8.5x** | 62 / 68 (91%) |
| `./internal/report/` | 348 | 59.0 s | **8.7 s** | **6.8x** | 298 / 348 (86%) |

Both runs produced byte-identical verdicts. For `internal/report` that is
305 killed, 12 lived, 28 not viable, 3 timed out and 96.21% efficacy on both
paths; `TestSchemataVerdictParity` asserts the same property mutant by
mutant on the bundled fixtures, which is stronger than comparing the
summary counts.

The speedup is the per-mutant compile and link disappearing. What is left
per mutant is process start plus the routed tests, so the ratio grows with
the number of mutants per package and shrinks as the tests themselves get
slower — a package whose tests dominate its build has less to gain.

The 10-15% of mutants that stay on the per-mutant path are the ones the
rewriter declines (constant contexts, mutations that would orphan a
declaration, guards that would leave a function without a terminating
statement) plus the ones the schema build rejects. They cost exactly what
they cost today.

## Caveats

- **Gremlins v0.6.0 silently produces zero mutants on Go 1.26.x** (the
  current homebrew default at time of writing). It runs `go test -cover`,
  reports a sub-second elapsed time, and exits with "No results to
  report." Forcing `GOTOOLCHAIN=go1.25.7` produces real numbers. This page
  uses 1.25.7 for both tools so they're benchmarked on the same toolchain;
  gomutants runs fine on 1.26 as well.
- **`./...` vs `.`**: gremlins on this build prints "no results" silently
  when given `./...` on either target and works only with `.`. The
  benchmarks use `.` throughout for gremlins. For uuid that's the entire
  module (one package); for cobra it's the root package only and the
  `cobra/doc` subpackage is excluded — the matched gomutants runs exclude
  it too. A separate gomutants `./...` row on the cobra table shows the
  full-repo number for context.
- **Wall time is sensitive to thermal state and background load.** These
  numbers were taken on AC power with no other CPU-bound work; rerun under
  the same conditions before drawing conclusions.
- **Cobra OOB rows are single-run; L4L and gremlins rows are 3-run
  medians.** Cold gomutants OOB on cobra is ~7 minutes per run, so the
  OOB-row matrix would be >70 minutes if hyperfined. The shorter L4L and
  gremlins runs are repeated 3 times and reported as medians because
  gomutants's first run is consistently 30–50% slower than runs 2–3 — an
  artifact of pre-building the test binary on a cold Go build cache.
  Gremlins doesn't share that artifact (each mutant is a fresh
  `go test`, so no warm-up effect; its 3 runs were 130/129/128 s). Treat
  the cobra OOB numbers as ±15% upper bounds rather than precise.
- **Coverage of external targets.** This page covers `google/uuid`,
  `spf13/cobra`, `prometheus/model/labels`, and a 4-package
  `prometheus/tsdb` slice. The in-repo `benchmarks/` harness covers two
  other targets (`./testdata/simple/` and `./internal/mutator`) and will
  give a different picture on different code.
- **Gremlins on tsdb-4 is a methodology compromise.** gremlins's
  `unleash` CLI accepts at most one target argument, so the row reports
  the sum of 4 per-subpackage invocations × 3 outer runs (12 calls
  total), median of the three outer-run sums. That's the realistic
  user-experience number, but the per-subpackage setup penalty (paid 4×
  per outer run) is a cost gomutants's multi-package mode doesn't have.
  Read the wall-time row as "what gremlins users would actually wait
  for," not "gremlins engine speed normalized to gomutants's scope." The
  per-subpackage data is in `/tmp/tsdb4-grem-*.json` if you want to
  inspect.
- **Cache state contaminates wall-time comparisons across batched
  runs.** Each `rm -f .gomutants-cache.json` clears gomutants's own cache
  but not Go's build cache, which warms monotonically across consecutive
  invocations. On tsdb-4 the 6 L4L runs went 1343 → 1272 → 1107 → 938 →
  855 → 852 s as the build cache filled. For fair toolchain-vs-toolchain
  comparison, interleave runs (1.25.7-r1, 1.26.3-r1, 1.25.7-r2, …) or
  pre-warm by running the full target once before measurement. The
  per-toolchain numbers in this doc were measured back-to-back; treat
  same-toolchain medians as steady-state and cross-toolchain wall-time
  deltas with care.

## Reproducing

```bash
# Pin Go versions
go install golang.org/dl/go1.25.7@latest && go1.25.7 download
go install golang.org/dl/go1.26.3@latest && go1.26.3 download

# gomutants from this repo:
go build -o ~/bin/gomutants .
# gremlins v0.6.0 release: download from go-gremlins/gremlins releases.

# --- google/uuid ---
git clone https://github.com/google/uuid /tmp/uuid && cd /tmp/uuid
git checkout 2d3c2a9cc518326daf99a383f07c4d3c44317e4d

# Go 1.25.7 (cross-comparison)
GOTOOLCHAIN=go1.25.7 hyperfine --warmup 1 --runs 3 \
  --prepare 'rm -f .gomutants-cache.json' \
  -n gremlins 'gremlins unleash --workers 10 --timeout-coefficient 20 --silent -o /tmp/g.json .' \
  -n gom-oob  'gomutants -workers 10 --cache=off -quiet -o /tmp/m1.json .' \
  -n gom-l4l  'gomutants -workers 10 --cache=off -quiet -only=ARITHMETIC_BASE,CONDITIONALS_BOUNDARY,CONDITIONALS_NEGATION,INCREMENT_DECREMENT,INVERT_NEGATIVES -o /tmp/m2.json .'

# Go 1.26.3 (gomutants L4L only — gremlins is broken on 1.26.x)
GOTOOLCHAIN=go1.26.3 hyperfine --warmup 1 --runs 3 \
  --prepare 'rm -f .gomutants-cache.json' \
  -n gom-l4l-1263 'gomutants -workers 10 --cache=off -quiet -only=ARITHMETIC_BASE,CONDITIONALS_BOUNDARY,CONDITIONALS_NEGATION,INCREMENT_DECREMENT,INVERT_NEGATIVES -o /tmp/m3.json .'

# --- spf13/cobra (3-run medians for L4L+gremlins; single-run for OOB) ---
git clone https://github.com/spf13/cobra /tmp/cobra && cd /tmp/cobra
git checkout ad460ea8f249db69c943a365fb84f3a59042d54e

# OOB single-run (each takes ~7 min)
rm -f .gomutants-cache.json
GOTOOLCHAIN=go1.25.7 time gomutants -workers 10 --cache=off -quiet -o /tmp/c-oob.json .

# L4L: 3 runs each, take median (run 1 includes a cold-cache artifact)
GREM_OPS=ARITHMETIC_BASE,CONDITIONALS_BOUNDARY,CONDITIONALS_NEGATION,INCREMENT_DECREMENT,INVERT_NEGATIVES
for v in 1.25.7 1.26.3; do
  for r in 1 2 3; do
    rm -f .gomutants-cache.json
    GOTOOLCHAIN=go$v time gomutants -workers 10 --cache=off -quiet -only=$GREM_OPS -o /tmp/c-l4l-$v-$r.json .
  done
done

# Gremlins: 3 runs (no toolchain auto-select; force 1.25.7)
for r in 1 2 3; do
  rm -f .gomutants-cache.json
  GOTOOLCHAIN=go1.25.7 time gremlins unleash --workers 10 --timeout-coefficient 20 --silent -o /tmp/c-grem-$r.json .
done

# Warm-cache run (prime once, then hyperfine):
rm -f .gomutants-cache.json
GOTOOLCHAIN=go1.25.7 gomutants -workers 10 -quiet -o /tmp/c-prime.json . >/dev/null
hyperfine --warmup 0 --runs 3 'GOTOOLCHAIN=go1.25.7 gomutants -workers 10 -quiet -o /tmp/c-warm.json .'

# --- prometheus/model/labels (run from repo root, target ./model/labels) ---
git clone https://github.com/prometheus/prometheus /tmp/prom && cd /tmp/prom
git checkout ecab2f45a8b7a1f12b8a16590a56590c96422f44
go mod download   # heavy: k8s, azure, gcp, etc.

# OOB single-run (~6 min)
rm -f .gomutants-cache.json
GOTOOLCHAIN=go1.25.7 time gomutants -workers 10 --cache=off -quiet -o /tmp/p-oob.json ./model/labels

# L4L 3-run median per Go version, gremlins 3-run median:
for v in 1.25.7 1.26.3; do
  for r in 1 2 3; do
    rm -f .gomutants-cache.json
    GOTOOLCHAIN=go$v time gomutants -workers 10 --cache=off -quiet -only=$GREM_OPS -o /tmp/p-l4l-$v-$r.json ./model/labels
  done
done
for r in 1 2 3; do
  rm -f .gomutants-cache.json
  GOTOOLCHAIN=go1.25.7 time gremlins unleash --workers 10 --timeout-coefficient 20 --silent -o /tmp/p-grem-$r.json ./model/labels
done

# Warm cache (~3 s):
rm -f .gomutants-cache.json
GOTOOLCHAIN=go1.25.7 gomutants -workers 10 -quiet -o /tmp/p-prime.json ./model/labels >/dev/null
hyperfine --warmup 0 --runs 3 'GOTOOLCHAIN=go1.25.7 gomutants -workers 10 -quiet -o /tmp/p-warm.json ./model/labels'

# --- prometheus tsdb-4 (combined 4-package multi-target) ---
# Same repo as above; targets: ./tsdb/chunkenc ./tsdb/index ./tsdb/chunks ./tsdb/record
TARGETS="./tsdb/chunkenc ./tsdb/index ./tsdb/chunks ./tsdb/record"

# OOB single-run (~46 min)
rm -f .gomutants-cache.json
GOTOOLCHAIN=go1.25.7 time gomutants -workers 10 --cache=off -quiet -o /tmp/t-oob.json $TARGETS

# L4L 3-run median per Go version (~15-22 min each, cache warms across runs)
for v in 1.25.7 1.26.3; do
  for r in 1 2 3; do
    rm -f .gomutants-cache.json
    GOTOOLCHAIN=go$v time gomutants -workers 10 --cache=off -quiet -only=$GREM_OPS -o /tmp/t-l4l-$v-$r.json $TARGETS
  done
done

# Warm cache: prime (~40 min) then hyperfine (~19 s each)
rm -f .gomutants-cache.json
GOTOOLCHAIN=go1.25.7 gomutants -workers 10 -quiet -o /tmp/t-prime.json $TARGETS >/dev/null
hyperfine --warmup 0 --runs 3 "GOTOOLCHAIN=go1.25.7 gomutants -workers 10 -quiet -o /tmp/t-warm.json $TARGETS"

# Gremlins per-subpkg × 3 outer runs (gremlins accepts only 1 target arg):
for r in 1 2 3; do
  for p in chunkenc index chunks record; do
    rm -f .gomutants-cache.json
    GOTOOLCHAIN=go1.25.7 time gremlins unleash --workers 10 --timeout-coefficient 20 \
      --silent -o /tmp/t-grem-$p-$r.json ./tsdb/$p
  done
done
# Sum each outer run's per-subpkg wall times; take the median of the 3 sums.
```
