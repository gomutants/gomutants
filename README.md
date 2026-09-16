[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/szhekpisov/gomutants/badge)](https://scorecard.dev/viewer/?uri=github.com/szhekpisov/gomutants)
[![codecov](https://codecov.io/gh/szhekpisov/gomutants/graph/badge.svg?token=XNXMEJDGV2)](https://codecov.io/gh/szhekpisov/gomutants)
[![Mutation testing badge](https://img.shields.io/endpoint?style=flat&url=https%3A%2F%2Fbadge-api.stryker-mutator.io%2Fgithub.com%2Fszhekpisov%2Fgomutants%2Fmain)](https://dashboard.stryker-mutator.io/reports/github.com/szhekpisov/gomutants/main)
[![Quality Gate Status](https://sonarcloud.io/api/project_badges/measure?project=szhekpisov_gomutants&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=szhekpisov_gomutants)
[![Security & Static Analysis](https://github.com/szhekpisov/gomutants/actions/workflows/security.yml/badge.svg?branch=main)](https://github.com/szhekpisov/gomutants/actions/workflows/security.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/szhekpisov/gomutants.svg)](https://pkg.go.dev/github.com/szhekpisov/gomutants)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

# gomutants

**The best mutation testing tool for the Go development loop.**

**Find the gap. Fix the test. Verify the mutant. Repeat in seconds.**

Coverage tells you a line ran. gomutants tells you whether a test would catch it breaking, then gives you the controls to close that gap while you are still developing: changed-code scope, exact covering-test routing, stable single-mutant reruns, source overlays, and a checkpointed content-addressed cache.

gomutants earns that position through the complete path from a code change to a verified test fix. The advantage is the workflow, not any one checkbox: other tools have more mutators or win particular cold benchmarks, but they do not combine the same development-loop controls in one tool. [See the comparison.](#how-it-compares)

## The Go development loop

```bash
# 1. Find test gaps introduced by this branch.
gomutants --changed-since main -o mutants.json ./...

# 2. Add or strengthen the test for a surviving mutant.

# 3. Verify that exact mutant instead of rerunning the package.
gomutants --run-mutant-id \
  'internal/cli/cli.go:parseArgs:CONDITIONALS_BOUNDARY#2' \
  --threshold-efficacy 100 ./...

# 4. Rerun the changed scope. Unaffected verdicts come from the cache.
gomutants --changed-since main ./...
```

The targeted command bypasses the old cached verdict, runs the tests that cover that mutant, and writes the fresh result back to the cache. Exit `0` means the test killed it; exit `10` means it still lived; exit `1` means the rerun produced no verdict at all (`NOT COVERED`, `NOT VIABLE`, `TIMED OUT`, `EQUIVALENT`, or `INFRA ERROR`), so treat only `0` as a confirmed kill. Mutations are applied with `go test -overlay`, so the source tree is never rewritten.

## Why gomutants leads this loop

- **Work on the change, not the repository.** `--changed-since <ref>` uses the merge base and includes uncommitted edits to tracked files, keeping local and pull-request runs focused on the code being developed. A brand-new file is invisible to git diff until it is staged, so `git add` it before trusting the count.
- **Run only the relevant tests.** Per-test coverage routing maps each mutant to the tests that cover its line; `--integration` extends that routing across importing packages when the real assertion lives downstream.
- **Verify one fix directly.** Stable mutant IDs and unique prefixes turn “did this test kill it?” into one targeted run rather than another full mutation pass.
- **Keep completed work.** The default content-addressed cache reuses a verdict only while the mutated file, the rest of its package, and the tests that could exercise it all remain byte-identical. Mid-run checkpoints also let interrupted runs resume instead of starting over.
- **Make the workflow repeatable.** An auto-loaded [`.gomutants.yml`](#configuration-file) keeps operators, exclusions, thresholds, routing, timeouts, and cache behavior with the project; CLI flags can override it for one run.
- **Keep the working tree clean.** Byte-level overlays preserve Go syntax, including generics, without copying or editing source files.
- **Get an actionable result.** Terminal diffs, JSON, Stryker, self-contained HTML, GitHub annotations, and the Claude Code plugin put each survivor where a developer or coding agent can act on it.

### Measured feedback time

On the published Apple M1 Pro benchmark, unchanged out-of-the-box reruns complete **120–150× faster** on the larger targets because cached mutants do not execute again:

| Target | Cold OOB run | Warm rerun | Speedup |
|---|---:|---:|---:|
| spf13/cobra | 410 s | **2.7 s** | **~150×** |
| prometheus/model/labels | 342 s | **2.8 s** | **~120×** |
| prometheus tsdb-4 | 2768 s (~46 min) | **19 s** | **~145×** |

These are unchanged-tree reruns, not a claim that all edits are free: editing a package's source or its tests invalidates that package's cached verdicts. The cold-OOB column runs the full mutator set and is a single run on these targets, so it is not comparable to the matched-operator, 3-run-median engine timings below. Cold-engine ordering also depends on the target; gremlins is faster on the smallest measured external project, while gomutants leads the measured medium single-package targets. See the [benchmark snapshot](#benchmark-snapshot) and [`docs/performance.md`](docs/performance.md) for methodology and reproduction commands.

## Table of Contents

- [The Go development loop](#the-go-development-loop)
- [Why gomutants leads this loop](#why-gomutants-leads-this-loop)
  - [Measured feedback time](#measured-feedback-time)
- [Installation](#installation)
  - [Go Install](#go-install)
  - [GitHub Action](#github-action)
  - [Direct binary download](#direct-binary-download)
  - [From Source](#from-source)
  - [Verifying Releases](#verifying-releases)
- [More ways to run](#more-ways-to-run)
- [Where gomutants is—and isn't—the fit](#where-gomutants-isand-isntthe-fit)
- [How It Compares](#how-it-compares)
  - [Mutator-set equivalence](#mutator-set-equivalence)
  - [Benchmark snapshot](#benchmark-snapshot)
- [Features](#features)
- [Usage](#usage)
  - [PR-Scoped Mode](#pr-scoped-mode)
  - [Cross-Package Mode](#cross-package-mode)
  - [Stryker-format Reports](#stryker-format-reports)
  - [HTML Reports](#html-reports)
  - [Exit Codes & CI Integration](#exit-codes--ci-integration)
  - [Claude Code Plugin](#claude-code-plugin)
  - [Inline Ignore Directives](#inline-ignore-directives)
  - [Call-Site Exclusion](#call-site-exclusion)
  - [Configuration File](#configuration-file)
  - [Mutators](#mutators)
  - [All Flags](#all-flags)
- [Mutant schemata](#mutant-schemata)
- [How It Works](#how-it-works)
- [Self-efficacy (gomutants on itself)](#self-efficacy-gomutants-on-itself)
- [Security & Code Quality](#security--code-quality)
- [Contributing](#contributing)
- [License](#license)

## Installation

### Go Install

```bash
go install github.com/szhekpisov/gomutants@5741a097e347d75afdd7894464e8c2f612281dd4 # v0.5.0
```

Make sure `$GOPATH/bin` is in your `PATH`:

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
```

The minimum supported version of Go for gomutants is **1.26**, both for building gomutants itself and for the project under test (gomutants shells out to `go test` in your project's toolchain). macOS and Linux on amd64/arm64 are tested in CI; Windows works wherever `go` does, though it isn't covered by automated tests.

### GitHub Action

gomutants is published as a composite action:

```yaml
- uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
  with:
    fetch-depth: 0  # required so --changed-since can reach the base ref
- uses: szhekpisov/gomutants@5741a097e347d75afdd7894464e8c2f612281dd4 # v0.5.0
  with:
    args: --changed-since origin/${{ github.base_ref }} ./...
```

Each LIVED mutant on a changed line is emitted as a `::warning file=...,line=...::` workflow command, which GitHub renders inline on the "Files changed" view. The action fails on any LIVED mutant by default (`threshold-efficacy: 100`); set `threshold-efficacy: ""` to surface annotations without failing the job.

| Input | Default | Description |
|---|---|---|
| `args` | _required_ | Arguments forwarded to `gomutants`. The action appends `--annotations=github` automatically. |
| `version` | `latest` | gomutants version to install. `latest` resolves to the newest **stable** release; release candidates (`v0.5.1-rc0`) must be requested by tag. With `version: latest` the action keeps a pre-installed binary on PATH; with any pinned tag/branch/SHA it always re-installs so what runs matches what was requested. |
| `threshold-efficacy` | `100` | Minimum test efficacy `%` (`KILLED/(KILLED+LIVED)`). Below threshold → exit 10. Default `100` fails the step on any LIVED mutant; set to `""` to disable. |
| `threshold-mcover` | _empty_ | Minimum mutant coverage `%` (`(KILLED+LIVED)/(KILLED+LIVED+NOT_COVERED)`). Below threshold → exit 11. Empty disables. |
| `working-directory` | `.` | Directory containing `go.mod`. |
| `cache` | `.gomutants-cache.json` | Path to the incremental-analysis cache file. Set to `off` to disable. Pair with [`actions/cache`](https://github.com/actions/cache) to persist across CI runs. |

See [`action.yml`](action.yml) for the full composite definition.

### Direct binary download

Binaries for Linux and macOS (amd64 and arm64) are attached to every [release](https://github.com/szhekpisov/gomutants/releases):

```bash
VERSION=0.5.0  # check the releases page for the latest stable
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
curl -fL "https://github.com/szhekpisov/gomutants/releases/download/v${VERSION}/gomutants_${VERSION}_${OS}_${ARCH}.tar.gz" \
  | tar -xz
sudo mv gomutants /usr/local/bin/
```

See [Verifying Releases](#verifying-releases) below to check signatures and provenance before installing.

### From Source

```bash
git clone https://github.com/szhekpisov/gomutants.git
cd gomutants
go build ./...
./gomutants --version
```

### Verifying Releases

Published release artifacts are append-only and signed. Every release includes:

- **Checksums** (`checksums.txt`) — SHA256 hashes for all archives
- **Cosign signature** (`checksums.txt.sigstore.json`) — keyless Sigstore signature
- **SBOMs** (`*.spdx.json`) — SPDX Software Bill of Materials for each archive
- **SLSA provenance** — Level 3 provenance attestation

<details>
<summary>Verification commands</summary>

**Verify the checksums signature:**

```bash
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/szhekpisov/gomutants/' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'

# Linux
sha256sum --check checksums.txt --ignore-missing
# macOS
shasum -a 256 --check checksums.txt --ignore-missing
```

**Verify SLSA provenance:**

```bash
gh attestation verify gomutants_<VERSION>_linux_amd64.tar.gz \
  --repo szhekpisov/gomutants
```

</details>

<a name="quick-start"></a>

## More ways to run

```bash
# Run on the whole module.
gomutants ./...

# Run only on lines this PR changes.
gomutants --changed-since origin/main ./...

# Near drop-in for gremlins users:
gomutants unleash ./...

# Use in CI — exit code 10 if efficacy falls below the threshold:
gomutants --threshold-efficacy 80 ./...
```

<a name="where-gomutants-isnt-the-fit"></a>

## Where gomutants is—and isn't—the fit

gomutants is strongest on active codebases with meaningful test suites, where developers repeatedly find, fix, and verify gaps. For a one-off scan, a very small package, or a thin test suite (roughly below 70% line coverage), its up-front coverage collection, baseline measurement, and per-test map may cost more than the later cache and routing can save.

## How It Compares

**The position:** gomutants is the Go mutation tester built for the complete development loop. It does not have the largest operator catalog (Mutago does), the richest standalone LLM payload (Mutago), or native sharding and OCI report transport (Gooze). It does combine change scoping, exact test-impact selection, a stable fresh single-mutant rerun, mutant-level caching, checkpoint/resume, adaptive deadlines, overlays, conservative outcomes, and CI-ready reports in one workflow.

| Development-loop capability | gomutants | [Mutago 2.8.1](https://github.com/quality-gates/mutago) | [Gremlins 0.6.0](https://github.com/go-gremlins/gremlins/releases/tag/v0.6.0) | [Gooze 1.0.1](https://github.com/gooze-dev/gooze) |
|---|---|---|---|---|
| Changed-line scope | merge base + working tree | merge base | git ref | no native support found |
| Stable single-mutant rerun | yes, forced fresh | yes | no | no |
| Persistent incremental results | mutant/test-aware | no | no | file-level |
| Mid-run checkpoint/resume | yes | no | no | no |
| Test-impact selection | covering tests, default | covering tests, opt-in | package or full suite | companion test file |
| Source isolation | `go test -overlay` | `go test -overlay` | temporary worktree | temporary project copy |
| Timeout policy | adaptive from selected tests | clean-suite coefficient | package coefficient | fixed seconds |
| Equivalent-mutant detection | compiler TCE, opt-in | no | no | no |
| Main report surfaces | JSON, Stryker, HTML, GitHub | JSON, agentic JSON, HTML, GitHub/GitLab | JSON, terminal diff | TUI, YAML, OCI |
| Native sharding | no | no | no | yes |

The feature matrix was reviewed on 10 August 2026 against the linked releases and documentation. “No” means no documented native capability was found, not that the workflow cannot be reproduced with external scripting.

This comparison is deliberately about development-loop capabilities, not an absolute score. Competitors can be the better choice when their distinct strength is the priority: Mutago for operator breadth or agent-oriented JSON, Gremlins for a simpler cold run, Gooze for distributed execution, and [Ooze 0.2.0](https://github.com/gtramontina/ooze) for custom domain-specific mutation operators.

### Mutator-set equivalence

gomutants is a strict superset of [ooze](https://github.com/gtramontina/ooze) v0.2.0 and [gremlins](https://github.com/go-gremlins/gremlins) v0.6.0: more mutators overall, and on the mutators they share it generates the same positions (or more, for ooze). Position-level reports: ooze on [uuid](docs/equivalence/ooze/uuid.md); gremlins on [uuid](docs/equivalence/gremlins/uuid.md) and [cobra](docs/equivalence/gremlins/cobra.md).

### Benchmark snapshot

Current three-tool run on Apple M1 Pro 10-core: gomutants v0.6.0-rc1,
Gremlins v0.6.0, and Mutago v2.8.1; 3-run Hyperfine means, 10 workers,
cache off, and `GOTOOLCHAIN=go1.25.7`. The matched rows use the four operator
semantics implemented identically by all three tools.

| Scenario | gomutants | Gremlins | Mutago | Mutants reported (g / G / M) |
|---|---:|---:|---:|---:|
| Tiny fixture, default catalogs¹ | 11.01 s | **4.27 s** | 11.33 s | 68 / 20 / 63 |
| Tiny fixture, matched operators | 4.25 s | **3.95 s** | 4.06 s | 18 / 19 / 18 |
| `internal/mutator`, matched operators | **14.22 s** | 23.16 s | 22.22 s | 84 / 89 / 71 |

¹ Default catalogs do different amounts of work and are not an engine-speed
comparison. On the medium matched target, gomutants was 1.63× faster than
Gremlins and 1.56× faster than Mutago in total wall time; per tested mutant it
used 173 ms, versus 260 ms and 313 ms respectively. See the
[full dated results](benchmarks/results.md) for standard deviations, outcome
counts, pinned versions, Go 1.26 compatibility, and reproduction details.

**Broader Gremlins study (May 2026):**

Four real-world Go projects on Apple M1 Pro 10-core, gomutants v0.2.2 vs gremlins v0.6.0, matched 5-operator set (gremlins' defaults), `workers=10`, `--cache=off`, `GOTOOLCHAIN=go1.25.7` (gremlins is broken on Go 1.26.x). Engine and gremlins rows are 3-run medians; cold-OOB rows on the larger targets are single-run.

**Engine wall-clock (cold cache, like-for-like operators):**

| Target | gremlins | gomutants | Speedup |
|---|---:|---:|---:|
| google/uuid (~2.3k LOC, 1 pkg) | 27.5 s | 29.7 s | 0.93× |
| spf13/cobra (~6k LOC, 1 pkg) | 129 s | **73 s** | **1.78×** |
| prometheus/model/labels (~4k LOC, 1 pkg) | 139 s | **90 s** | **1.55×** |
| prometheus tsdb-4 (~24k LOC, 4 pkgs) | 951 s¹ | 855 s | 1.11× |

¹ gremlins's `unleash` accepts only one target argument, so its tsdb-4 row sums 4 per-subpackage invocations; gomutants's row is a single multi-package run.

**Warm-cache rerun (full out-of-the-box workload, cache on)** — the inner edit/test loop where gomutants short-circuits unchanged mutants via the content-addressed cache. gremlins has no equivalent.

| Target | Cold OOB | Warm rerun | Speedup |
|---|---:|---:|---:|
| google/uuid | 77 s | 3.2 s | ~24× |
| spf13/cobra | 410 s | **2.7 s** | **~150×** |
| prometheus/model/labels | 342 s | **2.8 s** | **~120×** |
| prometheus tsdb-4 | 2768 s (~46 min) | **19 s** | **~145×** |

**Reading the numbers:**

- **Engine ordering depends on package size.** Roughly tied on uuid (~120 mutants), 1.5–1.8× faster on medium single-package targets where gomutants's pre-built test binary amortizes across many mutants, tied again on the 4-package multi-target where one-shot setup balances against gremlins's per-subpackage setup paid 4×.
- **Adaptive per-mutant timeouts win on contended runs.** Gremlins ran 26% of uuid mutants into its `--timeout-coefficient=20` ceiling under worker contention; gomutants ran 2.5%. Same pattern on tsdb-4 (196 vs 45 timeouts).

See [`docs/performance.md`](docs/performance.md) for full per-target tables,
NOT_COVERED interpretation differences, Go compatibility notes, and
reproduction commands.

## Features

- **`--changed-since <ref>`** — scope mutation testing to lines changed vs a git ref. Fast enough to gate every PR.
- **Per-test coverage routing** — each mutant runs only the tests whose coverage touches the mutated line, not the whole suite.
- **Incremental cache** — content-addressed; warm reruns skip mutants whose package sources and covering tests are byte-identical to the previous run (120–150× speedup on warm reruns).
- **Resumable runs** — the cache is checkpointed mid-run, so a run killed by an OOM, a CI timeout, or a double Ctrl-C resumes from the last checkpoint instead of starting over.
- **Adaptive per-mutant timeouts** — deadlines sized from recorded per-test durations × margin, so fast tests don't wait out a multi-minute global ceiling.
- **Byte-level patching via `go test -overlay`** — generics and all Go syntax survive intact; source tree never modified.
- **28 mutators including block-level** — `BRANCH_IF`, `BRANCH_ELSE`, `BRANCH_CASE`, `EXPRESSION_REMOVE`, `STATEMENT_REMOVE`, `LOOP_CONDITION`, `RANGE_BREAK` and the return-value set `RETURN_ERROR_NIL`, `RETURN_ZERO`, `RETURN_TRUE`, `RETURN_FALSE` on top of 17 token-level operators (arithmetic, bitwise, comparison, logical, loop control, literal increment/decrement, logical-negation removal, error-wrap downgrade).
- **OOM-safe** — each `go test` child runs in its own process group with a 2 GiB RSS cap; output capped at 1 MiB per stream.
- **Multiple report formats** — gremlins-compatible JSON (default), [Stryker `mutation-testing-elements` v2](https://github.com/stryker-mutator/mutation-testing-elements) JSON, and a self-contained interactive HTML report.
- **Conservative outcomes** — compile-failing mutants surface as `NOT_VIABLE`, while recognized host resource and I/O failures surface as `INFRA ERROR`; neither inflates efficacy or becomes a cached verdict.
- **Equivalent-mutant detection (opt-in)** — `--detect-equivalent` recompiles each survivor with `-gcflags=-S` and reclassifies it as `EQUIVALENT` when the generated assembly matches the original (Trivial Compiler Equivalence). Such mutants can't be killed by any test, so they drop out of the efficacy denominator instead of failing the gate. Sound: a killable mutant is never marked equivalent.
- **Cross-package routing (opt-in)** — `--integration` extends per-test routing across package boundaries so a mutant is killed by a covering test in *any* importing package (cross-package/E2E tests), not just its own. See [Cross-Package Mode](#cross-package-mode).
- **Inline ignore directives** — `// gomutants:disable*` comments suppress specific mutants by line, function, or regex.
- **Call-site exclusion** — `--exclude-calls` drops mutants inside calls matching a selector glob, so operators in logging arguments stop counting as test gaps. Covers Go's standard-library logging out of the box; the Go analogue of PITest's `avoidCallsTo`. See [Call-Site Exclusion](#call-site-exclusion).
- **GitHub Action** — surfaces surviving mutants as inline annotations on the PR diff.
- **Claude Code plugin** — `/gomutants:mutants` slash command runs gomutants on changed code and proposes concrete `*_test.go` cases that would kill each surviving mutant.

## Usage

```bash
gomutants [flags] <package patterns>
```

### PR-Scoped Mode

`--changed-since <ref>` scopes a run to mutants on lines added or modified since the given git ref:

```bash
# CI: gate every PR on changed lines only
gomutants --changed-since origin/main ./...

# Local: see what changed since the last commit
gomutants --changed-since HEAD~1 ./...
```

The flag runs `git diff --unified=0 --merge-base <ref>` and keeps only mutants on added/modified lines. Diffing from the **merge base** — the commit your branch forked from — rather than from the ref's tip is what makes the scope match the set of lines the pull request shows: on a branch that is behind its base, diffing against the tip also reports every line that landed on the base after you forked, failing your gate on someone else's work. Uncommitted working-tree edits are included, so a local pre-push run sees them. If no common ancestor is reachable — almost always a shallow clone — the run fails with a hint rather than silently widening the scope; `fetch-depth: 0` on `actions/checkout` fixes it. Combine with `--threshold-efficacy 100` to fail on any LIVED mutant on changed lines. A typical setup runs `--changed-since` per PR and the full tree post-merge; see [`.github/workflows/mutation.yml`](.github/workflows/mutation.yml) for an example.

### Cross-Package Mode

By default each mutant runs only against the tests in **its own package** whose coverage touches the mutated line. That's the fast path, and usually the right one: a mutant that only a downstream test kills means the mutated package's own tests are weak, and the `LIVED` report is the nudge to strengthen them.

But some architectures legitimately assert behavior across package boundaries — thin wiring/glue packages, generated code, or a black-box/E2E suite that tests through the public API. There, a mutant's only killer lives in an *importing* package, and per-package routing reports it `LIVED` (a false survivor) or `NOT COVERED`.

`--integration` closes that gap:

```bash
gomutants --integration ./...
```

It routes each mutant to the covering tests in **any** package that imports it. Concretely it:

- computes the reverse-dependency closure of the target packages (every package whose imports *or test imports* reach a target) and widens coverage collection and the per-test build to that set;
- pins `-coverpkg` to the target packages so importing tests record coverage on the mutated code (passing `--coverpkg` as well is an error);
- runs each covering package's tests in its own `go test` invocation, short-circuiting on the first kill.

Trade-offs:

- **Slower.** The per-test coverage build expands to the reverse-dependency closure, and `-coverpkg` instrumentation adds overhead. The closure keeps this bounded to packages that can actually reach a target, but on large modules it's a real cost.
- **Scores aren't comparable** to a non-integration run: mutants flip `LIVED`/`NOT COVERED` → `KILLED`, raising efficacy and mutant coverage. Pick one mode per gate.
- Default (per-package) routing remains the recommended path; reach for `--integration` only when your suite deliberately asserts behavior across package boundaries.

### Stryker-format Reports

```bash
gomutants --stryker-output stryker-report.json ./...
```

Writes a [mutation-testing-elements v2](https://github.com/stryker-mutator/mutation-testing-elements) report alongside the gremlins-format JSON. The same file feeds:

- The [`<mutation-test-report-app>`](https://www.npmjs.com/package/mutation-testing-elements) web component, which renders an interactive HTML view when embedded in a page with `src="stryker-report.json"`.
- The [Stryker Dashboard](https://stryker-mutator.io/docs/General/dashboard/), which hosts the report and serves a mutation-score badge:

```bash
curl -X PUT \
  -H "X-Api-Key: $STRYKER_DASHBOARD_KEY" \
  -H "Content-Type: application/json" \
  --data @stryker-report.json \
  "https://dashboard.stryker-mutator.io/api/reports/github.com/<org>/<repo>/<branch-or-sha>"
```

Once registered on `dashboard.stryker-mutator.io`, your project gets a `mutationScoreBadge` URL you can drop in this README — the same surface PIT, Stryker (JS/.NET/Scala), and Infection PHP plug into.

### HTML Reports

```bash
gomutants --html-output mutation-report.html ./...
```

Writes a single self-contained HTML file. Open it in any browser — no web server, no network access, no companion JSON file. The page bundles the [`<mutation-test-report-app>`](https://www.npmjs.com/package/mutation-testing-elements) web component and the report data into one document, so it works as a CI artifact you can upload from a job and link to from a PR check.

Inside, you get a per-file efficacy sidebar and click-through annotated source: each mutated line is highlighted with the mutator name, status (KILLED / SURVIVED / NO_COVERAGE / TIMEOUT / COMPILE_ERROR / RUNTIME_ERROR), and the original-vs-replacement diff.

If you already publish to the [Stryker Dashboard](https://stryker-mutator.io/docs/General/dashboard/) you don't need this flag — the dashboard renders the same report with history and a hosted badge. `--html-output` is for local viewing and CI artifacts, especially in air-gapped environments where uploading to a third-party dashboard isn't an option.

### Exit Codes & CI Integration

| Exit code | Meaning |
|-----------|---------|
| `0` | Success |
| `1` | Runtime, build, or target error |
| `2` | Usage or configuration error (unknown/invalid flags, conflicting options, invalid config) |
| `10` | Below `--threshold-efficacy` (gremlins-compat) |
| `11` | Below `--threshold-mcover` (gremlins-compat) |

```bash
gomutants --threshold-efficacy 80 --threshold-mcover 90 ./...
```

`test_efficacy = killed / (killed + lived)` — excludes `not_viable`, `not_covered`, `timed_out`, `equivalent`, and `infra_error`.
The JSON `mutations_coverage` field is `(mutants_total - mutants_not_covered) / mutants_total`, so infrastructure errors remain visible in that run-level coverage measure. The `--threshold-mcover` gate retains its gremlins-compatible `(killed + lived) / (killed + lived + not_covered)` formula.

### Claude Code Plugin

This repo ships a [Claude Code](https://claude.com/claude-code) plugin that exposes a `/gomutants:mutants` slash command. It runs gomutants on changed code, parses the JSON report, and proposes concrete `*_test.go` cases that would kill each surviving mutant — without editing any files. It also writes a self-contained interactive HTML report (the same one `--html-output` produces) to `/tmp/gomutants-report.html` for click-through inspection.

Install:

```text
/plugin marketplace add szhekpisov/gomutants
/plugin install gomutants@gomutants
```

Use:

```text
/gomutants:mutants                    # default: --changed-since main ./...
/gomutants:mutants ./internal/foo     # scope to a package
/gomutants:mutants --since HEAD~1     # scope by git ref
```

The plugin assumes `gomutants` is on `PATH` (`go install github.com/szhekpisov/gomutants@latest`), and falls back to `go run github.com/szhekpisov/gomutants@latest` otherwise. Plugin sources live under [`plugin/`](plugin/); the marketplace manifest is at [`.claude-plugin/marketplace.json`](.claude-plugin/marketplace.json).

### Inline Ignore Directives

Annotate Go source with `// gomutants:disable*` comments to silence specific mutants. Suppressed mutants are dropped from the run entirely — they don't appear in any status bucket and don't affect `test_efficacy` or `mutations_coverage`. The aggregate count surfaces as `mutants_suppressed` in the JSON report and on the terminal summary.

For a project-wide policy rather than a per-site annotation — "never mutate inside logging calls" — see [Call-Site Exclusion](#call-site-exclusion).

Four forms:

```go
// Same line — suppress every (or one) mutator on the line of the directive.
return a + b // gomutants:disable
return a + b // gomutants:disable ARITHMETIC_BASE reason="commutative"
return a + b // gomutants:disable ARITHMETIC_BASE,INVERT_NEGATIVES

// Next line — suppress mutators on the first non-blank, non-comment line that follows.
// gomutants:disable-next-line CONDITIONALS_NEGATION reason="branch always taken in prod"
if debugMode { ... }

// Function — when placed as the doc-comment of a func, suppresses every mutant in the body.
// gomutants:disable-func reason="generated code"
func gen() { ... }

// Regexp — anywhere in the file; suppresses mutants on lines whose source text matches.
// gomutants:disable-regexp ^\s*log\. reason="logging is not behaviour"
```

<details>
<summary>Grammar and edge cases</summary>

```text
DIRECTIVE  = "// gomutants:" KIND [ WS PATTERN ] [ WS MUTATORS ] [ WS "reason=" QUOTED ]
KIND       = "disable" | "disable-next-line" | "disable-func" | "disable-regexp"
PATTERN    = present only for "disable-regexp"; first whitespace-delimited token after the kind, RE2 syntax
MUTATORS   = ( MUTATOR ("," MUTATOR)* ) | "*"   // upper-case mutator type names; "*" = all
QUOTED     = any Go-quoted string ("...", `...`, or 'c') with standard escape handling
```

- Omitting `MUTATORS` (or supplying `*`) suppresses every mutator at the directive's target.
- `reason="..."` is optional; recommended for self-documentation. Reasons surface to stderr under `--verbose`.
- Unknown mutator name → warning to stderr, that name is dropped, the rest of the directive still applies. If *every* named mutator is unknown, the directive is dropped entirely with a summary warning — a typo like `TYPP_O` must not silently disable every mutator on the line. Forward-compatible across mutator renames (rename one mutator at a time; stale names are individually skipped).
- `disable-func` placed on a non-function comment → warning, directive ignored.
- `disable-regexp` with an invalid pattern → warning, directive ignored.
- `disable-next-line` on the last line of a file (or with only blanks/comments after it) → warning, directive ignored.
- Patterns with whitespace are not supported in v1; use `\s` instead.
- Multiple `// gomutants:` directives on a single physical line are not supported (Go treats them as one comment). Combine with a comma list (`disable A,B`) or `*` instead.

</details>

### Call-Site Exclusion

Some mutants are real code changes that no reasonable test can catch. The canonical case is arithmetic inside a logging argument:

```go
log.Printf("imported %d/%d rows (%.1f%%)", done, total, float64(done)/float64(total)*100)
```

Every operator there gets an `ARITHMETIC_BASE` mutant. No test asserts the log text, so they LIVE forever, dragging `test_efficacy` down and failing gates for code whose only observable effect is a log line. They aren't equivalent either — the emitted text genuinely differs — so `--detect-equivalent` correctly leaves them in the denominator.

`--exclude-calls` closes the gap between file-level exclusion and per-line directives: mutants inside a matching call are dropped before any test runs, and counted in `mutants_suppressed`.

**This is on by default for Go's standard-library logging** — no setup needed for the common case:

```
log.Print*   log.Output
slog.Debug*  slog.Info*  slog.Warn*  slog.Error*  slog.Log*
```

Add your own logger or telemetry wrappers; your list *extends* the built-ins:

```yaml
# .gomutants.yml
exclude-calls:
  - "*.Debug"          # any receiver's Debug method
  - "metrics.*"        # a whole package
  - "tracer.Record"    # one exact call
```

```bash
gomutants --exclude-calls 'log.Print*,*.Debug' ./...

# Narrow or replace the built-ins instead of extending them:
gomutants --exclude-calls 'mylog.*' --exclude-calls-defaults=false ./...
```

Patterns are **globs, not regexps** — every character is literal except `*`, which matches any run of characters. Matching is anchored, so `log.Print` matches only `log.Print`, and `log.Print*` is what also catches `log.Printf` / `log.Println`.

<details>
<summary>What a pattern is matched against, and what gets suppressed</summary>

The pattern is matched against the call's selector as written in the source:

| Call | Selector | Matched by |
|------|----------|------------|
| `log.Printf(…)` | `log.Printf` | `log.Print*`, `log.*` |
| `logger.Debug(…)` | `logger.Debug` | `*.Debug` |
| `s.log.Errorf(…)` | `s.log.Errorf` | `*.Errorf` |
| `getLogger().Info(…)` | `_.Info` | `*.Info` (the receiver has no name to match) |
| `println(…)` | `println` | `println` |

- **The whole call expression is suppressed, not just the argument list.** That includes `STATEMENT_REMOVE` of a bare `log.Printf(...)` statement — deleting a log line is as unkillable as mutating what it prints — and anything nested inside the arguments.
- **`log.Fatal*` and `log.Panic*` are deliberately not in the default set.** They exit or panic, so deleting one is a real behavioural change your tests can and should catch. Same for `slog.New` / `slog.SetDefault` / `slog.With`, which configure rather than emit.
- **Method-shaped patterns like `*.Info` are not defaults either** — they would reach `err.Error()` and any domain method sharing the name. Opt in per project.
- **Matching is syntactic, with no type resolution.** An aliased import (`import stdlog "log"`) renders under its alias, so add `stdlog.*` if you use one. A local variable named `log` would also match `log.*`.
- Suppressed mutants leave every count and both denominators, exactly like directive-suppressed ones. The terminal summary and `mutants_suppressed_by_calls` in the JSON report break out how many came from here rather than from `// gomutants:disable*`.
- A pattern of only asterisks is rejected: it would suppress nearly every mutant in the module, which is never intended and would otherwise pass silently.

</details>

### Configuration File

`.gomutants.yml` in the project root:

```yaml
workers: 10
test-cpu: 0             # 0 = let go test use GOMAXPROCS
timeout-coefficient: 10
adaptive-timeout: true  # per-test adaptive sizing; set false for single global timeout
timeout-margin: 3.0     # multiplier on per-test sums (only when adaptive)
timeout-min: 2s         # floor on per-mutant adaptive timeout
coverpkg: "./pkg/mypackage/..."
tags: ""                # build tags forwarded to the inner go list/go test (e.g. "integration,debug")
test-flags: ""          # flags forwarded to the inner go test only (e.g. "-short")
output: mutation-report.json
changed-since: ""       # set to e.g. "main" to scope runs by default
integration: false      # cross-package routing; manages -coverpkg itself (don't also set coverpkg)
cache: ""               # path to incremental-analysis cache; "" = .gomutants-cache.json, "off" = disabled
checkpoint-interval: 10s # how often to flush the cache mid-run; 0s disables (final flush still runs)
disable: []
only: []
exclude-calls: []            # selector globs; extends the built-in stdlib-logging set
exclude-calls-defaults: true # false = drop the built-ins and use only the list above
```

Priority: built-in defaults < config file < CLI flags. See [`.gomutants.yml.example`](.gomutants.yml.example) for a complete reference.

### Mutators

`gomutants --list-mutators` prints the authoritative catalog for the installed
binary, one mutator per line with a description and representative example. It
does not require a Go module or configuration file, so it can be used anywhere
to check names before adding them to `--only`, `--disable`, or an inline
directive:

```console
$ gomutants --list-mutators | grep RETURN
RETURN_ERROR_NIL            Swallow a propagated error            return nil, err -> return nil, nil
RETURN_FALSE                Force a boolean return false          return x > 0 -> return false
RETURN_TRUE                 Force a boolean return true           return x > 0 -> return true
RETURN_ZERO                 Return the zero value instead         return count -> return 0
```

**Token-level:**

| Type | Description | Example |
|------|-------------|---------|
| `ARITHMETIC_BASE` | Swap arithmetic operators | `+` <-> `-`, `*` <-> `/`, `%` <-> `*` |
| `CONDITIONALS_BOUNDARY` | Relax/tighten boundaries | `<` <-> `<=`, `>` <-> `>=` |
| `CONDITIONALS_NEGATION` | Negate comparisons | `==` <-> `!=`, `<` <-> `>=`, `>` <-> `<=` |
| `INCREMENT_DECREMENT` | Swap increment/decrement | `++` <-> `--` |
| `INVERT_NEGATIVES` | Invert negation | `-x` -> `+x`, `a - b` -> `a + b` |
| `INVERT_ASSIGNMENTS` | Swap arithmetic compound assignments | `+=` <-> `-=`, `*=` <-> `/=`, `%=` -> `*=` |
| `INVERT_BITWISE` | Swap bitwise binary operators | `&` <-> `\|`, `^` -> `&`, `<<` <-> `>>` |
| `INVERT_BITWISE_ASSIGNMENTS` | Swap bitwise compound assignments | `&=` <-> `\|=`, `^=` -> `&=`, `<<=` <-> `>>=` |
| `INVERT_LOGICAL` | Swap logical operators | `&&` <-> `\|\|` |
| `INVERT_LOOP_CTRL` | Swap loop control | `break` <-> `continue` |
| `REMOVE_SELF_ASSIGNMENTS` | Drop op from compound assignment | `x += y` -> `x = y` |
| `REMOVE_LOGICAL_NOT` | Drop `!` from a logical negation | `if !ok` -> `if ok` |
| `ERRORF_WRAP` | Downgrade the error-wrapping verb | `fmt.Errorf("load: %w", err)` -> `fmt.Errorf("load: %v", err)` |
| `INTEGER_INCREMENT` | Increment integer literal | `42` -> `43`, `0xFF` -> `256` |
| `INTEGER_DECREMENT` | Decrement integer literal | `42` -> `41`, `0` -> `-1` |
| `FLOAT_INCREMENT` | Increment float literal | `1.5` -> `2.5`, `0.0` -> `1.0` |
| `FLOAT_DECREMENT` | Decrement float literal | `1.5` -> `0.5`, `1e2` -> `99.0` |

`ERRORF_WRAP` leaves the formatted message byte-for-byte identical and only breaks the error chain, so `errors.Is` and `errors.As` against the cause stop matching. It survives exactly when a test asserts on `err.Error()` and never unwraps — the gap it exists to report. Matching is syntactic and covers any `Errorf` selector, so an aliased import or your own error package is reached without configuration.

`REMOVE_LOGICAL_NOT` deliberately skips a negated comparison (`!(a == b)`): dropping the `!` there produces the same behaviour as the `CONDITIONALS_NEGATION` mutant on the inner operator, and two mutants that live and die together would dilute the efficacy denominator without measuring anything. It applies to everything else — `!ok`, `!strings.HasPrefix(s, p)`, `!(a && b)` — which no other mutator reaches.

**Block-level:**

| Type | Description | Example |
|------|-------------|---------|
| `BRANCH_IF` | Empty if/else-if body | `if x { doStuff() }` -> `if x { _ = 0 }` |
| `BRANCH_ELSE` | Empty else body | `else { doStuff() }` -> `else { _ = 0 }` |
| `BRANCH_CASE` | Empty case body | `case 1: doStuff()` -> `case 1: _ = 0` |
| `EXPRESSION_REMOVE` | Remove boolean operand | `a && b` -> `true && b` / `a && true` |
| `STATEMENT_REMOVE` | Remove statement effect | `x = expr` -> `_ = expr`, `f()` -> `_ = 0` |
| `LOOP_CONDITION` | Force for-loop condition to false | `for i := 0; i < n; i++ {}` -> `for i := 0; false; i++ {}` |
| `RANGE_BREAK` | Insert early break in for…range body | `for _, v := range xs { f(v) }` -> `for _, v := range xs { break; f(v) }` |

**Return values:**

Each return slot is claimed by exactly one of these, based on the type declared in the function's signature, so they never mutate the same value twice.

| Type | Description | Example |
|------|-------------|---------|
| `RETURN_ERROR_NIL` | Swallow a propagated error | `return nil, err` -> `return nil, nil` |
| `RETURN_ZERO` | Return the zero value instead | `return count` -> `return 0`, `return name` -> `return ""`, `return d` -> `return *new(time.Duration)` |
| `RETURN_TRUE` | Force a boolean return true | `return x > 0` -> `return true` |
| `RETURN_FALSE` | Force a boolean return false | `return x > 0` -> `return false` |

`RETURN_ZERO` on a single-expression formatting helper — `return fmt.Sprintf(...)` with no branching — is a known low-signal survivor. Killing it means asserting on the exact formatted string, which pins the wording of output that has no logic behind it. Drop these with [`--exclude-calls`](#call-site-exclusion) instead — `gomutants --exclude-calls 'fmt.Sprintf' ./...` suppresses them before any test runs, on the same reasoning that keeps logging arguments out of the count.

**Mutant statuses:**

| Status | Meaning |
|--------|---------|
| KILLED | Test failed — mutant detected |
| LIVED | Tests passed — **test gap** |
| NOT COVERED | No test covers the mutated line |
| NOT VIABLE | Mutation causes a compile error (filtered, not counted as a kill) |
| TIMED OUT | Test execution exceeded the per-mutant timeout |
| INFRA ERROR | A recognized host resource or I/O failure prevented a reliable verdict |

### All Flags

<details>
<summary>Complete flag reference</summary>

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--workers` | `-w` | NumCPU | Parallel workers |
| `--test-cpu` | | 0 (omit) | Value passed to inner `go test -cpu` per mutant; 0 lets go test use `GOMAXPROCS` |
| `--timeout-coefficient` | | 10 | Multiplier applied to baseline test time for the **global timeout ceiling** (also the per-mutant timeout when `--adaptive-timeout=false`) |
| `--adaptive-timeout` | | true | Use the per-test durations recorded during the coverage build to size each mutant's timeout. Pass `=false` to fall back to the single global ceiling. |
| `--timeout-margin` | | 3.0 | When adaptive: `per-mutant timeout = sum(selected test durations) × this`, clamped to `[--timeout-min, --timeout-coefficient × baseline]` |
| `--timeout-min` | | 2s | Floor for the per-mutant adaptive timeout. Absorbs cold-start, child fork, and GC pause overhead that doesn't scale with the underlying test work. |
| `--coverpkg` | | | Coverage package pattern (forwarded to `go test -coverpkg`) |
| `--tags` | | | Comma-separated build tags forwarded as `-tags` to every inner `go list` / `go test` (including `go test -c`/`-list`), so mutation testing reaches code behind `//go:build` constraints (gremlins-compat) |
| `--test-flags` | | | Flags forwarded verbatim to the inner `go test` runs and to nothing else. Whitespace-separated and repeatable. Part of the cache identity, so changing the value discards cached verdicts rather than replaying them. Setting it also stands adaptive timeouts down to the global ceiling, since the per-test timings are measured without these flags. Use it to trade mutation fidelity for speed on property-based suites — see [Speeding up property-based suites](#speeding-up-property-based-suites) |
| `--output` | `-o` | `mutation-report.json` | JSON report path |
| `--config` | | `.gomutants.yml` | Config file path |
| `--disable` | | | Comma-separated mutator types to disable |
| `--only` | | | Comma-separated mutator types to run (disables all others) |
| `--exclude-files` | | | Comma-separated regexps; skip mutating production files whose module-relative path matches any. Unanchored (e.g. `vendor/` hits anywhere). Excluded files produce no mutants and are never parsed. |
| `--exclude-calls` | | | Comma-separated selector globs; suppress mutants inside calls whose selector matches any (e.g. `log.Print*,*.Debug`). Anchored; `*` is the only metacharacter. Extends the built-in stdlib-logging set. Suppressed mutants are dropped before any test runs. See [Call-Site Exclusion](#call-site-exclusion). |
| `--exclude-calls-defaults` | | true | Apply the built-in `--exclude-calls` set for Go's standard-library logging (`log.Print*`, `slog.Info*`, …). Pass `=false` to narrow or fully replace it. |
| `--changed-since` | | | Only test mutants on lines changed vs the merge base of the given git ref and `HEAD` (e.g. `main`, `HEAD~1`); requires a git repo |
| `--run-mutant-id` | | | Run only the mutant with this stable `id` (the `id` field of a report entry); a unique prefix is accepted. Skips the incremental cache for that mutant. See [Re-running a single mutant](#re-running-a-single-mutant) |
| `--cache` | | `.gomutants-cache.json` | Path to incremental-analysis cache file. Skips mutants whose source package and covering tests are byte-identical to the cached run — invalidation is package-scoped, so editing one file re-runs its package. Pass `--cache=off` to disable. |
| `--checkpoint-interval` | | 10s | How often to flush completed mutant outcomes to the cache mid-run, so a hard kill (OOM, CI timeout, SIGKILL) loses at most this much progress and the next run resumes from the last checkpoint. `0` disables periodic checkpointing (the cache is then written only once, at the end). Ignored when `--cache=off`. |
| `--detect-equivalent` | | false | After testing, recompile each surviving mutant with package-scoped `-gcflags=-S` and reclassify it as `EQUIVALENT` when the generated assembly is identical to the original (Trivial Compiler Equivalence). Equivalent mutants can't be killed by any test, so they're dropped from the efficacy denominator. Adds one package compile per survivor. |
| `--schemata` | | false | **Experimental.** Compile each package once with every mutant present but inert, then run mutants against the prebuilt test binary instead of recompiling per mutant — roughly a 7x cold-run speedup on the bundled fixtures. Mutants the rewriter cannot prove safe keep the per-mutant path, so verdicts are unchanged. Ignored with `--test-flags` or `--run-mutant-id`. See [Mutant schemata](#mutant-schemata) |
| `--integration` | | false | Route each mutant to covering tests in *any* package that imports it, not just its own. Widens coverage and the per-test build to the reverse-dependency closure of the target packages and manages `-coverpkg` itself (passing `--coverpkg` too is an error). Lets a mutant be killed by a cross-package/E2E test. See [Cross-Package Mode](#cross-package-mode). |
| `--annotations` | | | Emit annotations for LIVED mutants. Supported: `github` (workflow-command warnings on stdout). |
| `--stryker-output` | | | Also write a [Stryker mutation-testing-elements](https://github.com/stryker-mutator/mutation-testing-elements) report at this path (for the HTML viewer and Stryker Dashboard). |
| `--html-output` | | | Also write a self-contained interactive HTML mutation report at this path (Stryker mutation-testing-elements viewer bundled inline; no network access required to open). |
| `--threshold-efficacy` | | 0 | Minimum test efficacy (KILLED/(KILLED+LIVED)). Below threshold → exit 10 (gremlins-compat). 0 disables. |
| `--threshold-mcover` | | 0 | Minimum mutant coverage ((KILLED+LIVED)/(KILLED+LIVED+NOT_COVERED)). Below threshold → exit 11 (gremlins-compat). 0 disables. |
| `--dry-run` | | false | List mutants without testing |
| `--verbose` | `-v` | false | Stream each mutant as tested |
| `--quiet` | `-q` | false | Suppress header, phase lines, and per-mutant progress; only the final summary lands on stdout (warnings still go to stderr). Mutually exclusive with `--verbose`. |
| `--list-mutators` | | false | Print every registered mutator type with its description and a representative example, then exit. Requires no module, package argument, or configuration file. |
| `--version` | | | Print version and exit |

</details>

Common invocations:

```bash
# Default: run on all packages with NumCPU workers.
gomutants ./...

# Faster CI: only mutants on lines this PR changes.
gomutants --changed-since origin/main ./...

# Local exploration: see what would be tested without running.
gomutants --dry-run ./...

# Verbose stream of every mutant as it completes.
gomutants -v ./...

# Quiet for CI: only the final summary on stdout (exit code still gates).
gomutants -q --threshold-efficacy 80 ./...

# Inspect, select, or exclude mutators.
gomutants --list-mutators
gomutants --only ARITHMETIC_BASE,CONDITIONALS_NEGATION ./...
gomutants --disable BRANCH_IF,BRANCH_ELSE ./...

# Skip generated code and vendored deps.
gomutants --exclude-files 'vendor/,_gen\.go$' ./...

# Teach it about your own logger on top of the built-in stdlib set.
gomutants --exclude-calls '*.Debug,*.Infof' ./...

# Tune for memory-tight runners.
gomutants --workers=2 ./...

# Give each go test more CPU lanes (paired with low --workers).
gomutants --workers=1 --test-cpu=8 ./...
```

`gomutants unleash ./...` is accepted unchanged for gremlins-compat scripts.

### Speeding up property-based suites

Property-based tests are the worst case for mutation testing: every mutant on
a covered line re-runs the whole property. At the iteration counts these
frameworks default to — commonly 100 — the run becomes dominated by iteration
rather than by mutants.

`--test-flags` forwards flags to the inner `go test`, so you can dial that
down explicitly, with `-short` or with whatever flag your property framework
exposes for its iteration count:

```bash
# Cheaper per-mutant runs; composes with --changed-since for a pre-push gate.
gomutants --changed-since main --test-flags '-short' ./...
```

Four things to know:

- **`go test` only.** The flags reach the per-mutant runs, the coverage run,
  and the baseline run — never `go list` or the build steps. This is why
  `GOFLAGS` is not a workaround. Go applies a GOFLAGS entry only "when the
  given flag is known by the current command" (`go help environment`), so
  you cannot say which invocations a flag reaches: `-short` is silently
  ignored by `go list` and `go test -c`, while `-race` is honored by them.
  GOFLAGS also bypasses the managed-flag check below — `GOFLAGS=-run=TestFoo`
  narrows the coverage run and every per-mutant run with no diagnostic,
  which `--test-flags -run=TestFoo` rejects outright.
- **`-short` narrows coverage too, deliberately.** The coverage run sees the
  same flags, so a test that `-short` skips is not recorded as covering. Its
  lines report as `NOT_COVERED` rather than as false survivors — an honest
  gap instead of a misleading one.
- **Changing the value invalidates the cache.** `--test-flags` is part of the
  cache's identity, so alternating between a cheap gate run and a full scoring
  run does not replay one's verdicts as the other's. The cost is that each
  value keeps its own cache generation: switching back and forth re-runs from
  cold rather than resuming. That is the intended trade — a mutant that
  survived 20 property iterations says nothing about whether it survives 100.
  Whitespace-only differences don't count as a change. Flag order does:
  arbitrary test-binary flags can interact while they are parsed, so the
  cache conservatively keeps `-race -short` and `-short -race` separate.
- **The per-test timing phase does not see the flags,** so adaptive timeouts
  stand down. That phase compiles with `go test -c` and runs the binary
  directly with `-test.*`-namespaced flags, where `-short` would need
  translating. Its durations therefore describe a run your flags have
  changed, which is unusable as a deadline in either direction — under
  `-race` a deadline measured without it would fire early, turning survivors
  into `TIMED_OUT` and quietly dropping them out of the efficacy
  denominator. With `--test-flags` set, every mutant gets the global
  `baseline × --timeout-coefficient` ceiling instead (the baseline *is*
  measured with your flags), exactly as under `--adaptive-timeout=false`.
  This also caps the speedup: the timing phase runs each test once at full
  cost regardless, so a suite dominated by it improves less than the
  per-mutant arithmetic suggests.

Your flags are placed **after** the package argument, which is what lets
flags belonging to the *test binary* rather than to `go test` work at all.
`go test` goes on claiming its own flags past one it does not recognise,
but that first unrecognised flag marks the package list as already seen —
so a package named after it is forwarded to the test binary as a
positional argument and the package list falls back to `.`. With
`-rapid.checks=100` ahead of the package, the working directory would be
tested instead. Your order is preserved both on argv and in the cache
identity. Go reads the last occurrence of a repeated flag wherever the
package sits, and distinct custom flags can also share state or otherwise
interact while they are parsed.

Before an `-args` boundary, flags gomutants manages itself (`-overlay`,
`-run`, `-timeout`, `-coverprofile`, `-coverpkg`, `-c`, `-o`, `-exec`) are
rejected rather than silently honored. These conflicts can fail quietly; for
example, replacing `-overlay` means no mutant is ever applied and every one
"survives". The `-test.`-prefixed spellings are rejected too — `go test` hands `-test.run`
straight to the test binary, where it beats the `-run` filter gomutants
computed.

`-args` is supported for the rarer case where a custom test-binary flag has
the same name as a flag owned by the Go command. Put Go-owned flags before it
and raw test-binary flags after it, for example
`--test-flags '-race -args -x'`. Gomutants' package and managed flags are
already ahead of this boundary, so `-args` cannot swallow them. A managed
`-test.*` spelling after `-args` is still rejected because it would override
the corresponding flag in the test binary itself.

One wrinkle, and it only bites if you pass a *managed* name after the
boundary. `go test -bench -args …` binds `-args` to `-bench` as its value,
so the fields after it are read by the Go command after all — and an
`-overlay` among them would silently replace the mutation overlay. Rather
than track which Go flags take values, gomutants declines to treat `-args`
or `--` as a boundary when the field before it is a flag with no inline
`=` value. Write that value inline (`-bench=. -args -run=custom`) if you
need the relaxation there. Names gomutants does not manage are unaffected,
so `--test-flags '-race -args -x'` works either way.

## Mutant schemata

`--schemata` is an experimental execution mode. Instead of recompiling the
package and relinking the test binary once per mutant, it compiles each
package **once** with every mutant already present but inert, each one behind
a guard keyed to the `GOMUTANTS_ACTIVE` environment variable, and then runs
the prebuilt test binary once per mutant.

```bash
gomutants --schemata ./...
```

That removes the dominant cost of a cold run. On this repository's own
`internal/report` package, 348 mutants take 59s on the per-mutant path and
8.7s with `--schemata` — the same 305 killed, 12 lived, 28 not viable.

This is the architecture Stryker.NET, StrykerJS and mutmut 3 use. Go makes it
harder than those languages do: there is no conditional expression, and
gomutants deliberately carries no type checker, so the rewriter has to prove
from syntax alone that a mutation can be made switchable. It uses four
strategies:

| | Shape | Covers |
|---|---|---|
| Bool wrap | `func() bool { if on(41) { return a >= b }; return a > b }()` | Conditions and logical operators, whose type is bool whatever the operands are |
| Statement wrap | `if on(42) { return x - y } else { return x + y }` | Arithmetic, literals, return values, assignment operators |
| Block guard | `if !on(43) { … }` | The branch-emptying mutators, with no duplicated source at all |
| Fallback | today's per-mutant `-overlay` run | Everything else |

**Verdicts do not change.** Anything the rewriter cannot prove safe falls back
to the per-mutant path rather than guessing, and so does anything the schema
build rejects. The declined set includes constant contexts (a closure is not a
constant), `recover()` inside a condition (wrapping it stops it recovering),
mutations that would orphan a declaration or an import, and guards that would
leave a function without a terminating statement. Because verdicts are
interchangeable, `--schemata` is deliberately **not** part of the cache
identity: results recorded under one path replay correctly under the other.

Two known costs, which are why it is opt-in:

- The guards perturb inlining and escape analysis, so benchmark-shaped or
  timing-sensitive tests may behave differently than on the per-mutant path.
- `GOMUTANTS_ACTIVE` is visible to the tests, which matters only to a test
  asserting on an exact environment.

`--test-flags` and `--run-mutant-id` switch it off automatically: the former
because those are `go test` arguments with no general translation to a test
binary's own flags, the latter because a single mutant costs one compile
either way.

## How It Works

1. **Resolve packages** via `go list -json`.
2. **Collect coverage** with `go test -coverprofile`. Mutants on uncovered lines are filtered upfront as `NOT_COVERED`.
3. **Measure baseline test time** to set the global timeout ceiling (`baseline × --timeout-coefficient`). With `--adaptive-timeout=false` this also becomes every mutant's deadline.
4. **Discover mutants** by walking the AST and emitting byte-level patches. Address-of `&` is recognised and skipped; unary `-` is emitted by exactly one mutator.
5. **Build per-test coverage map.** Test binaries are compiled once; each test runs in isolation with `-test.run=<one>` to record the lines it covers — and its wall-time, used for adaptive per-mutant timeouts.
6. **Test mutants** in parallel:
   - Each worker owns a stable temp source file + overlay JSON.
   - Mutations are applied as byte-level patches; the original tree is never written to.
   - The mutant's covered tests are looked up; only those run via `go test -overlay -run=<regex>`.
   - Each `go test` child runs in its own process group with a 2 GiB RSS cap; output is capped at 1 MiB per stream.
   - Recognized resource and I/O failures in test output, command startup, or gomutants' per-mutant temp-file writes are reported as `INFRA ERROR`, not as false kills. Detection is scoped by how far the input can be trusted, so a real kill is never laundered into a non-result:
     - gomutants' own failed syscalls are matched on the errno (`ENOSPC`, `ENOMEM`, `EMFILE`, …), which survives a wrapper rewriting the message and can't be produced by the code under test.
     - If any test reported a failure of its own (`--- FAIL:`), or the binary aborted on a `panic:`, the mutation was detected: `KILLED`, whatever else the output holds. The panic case is what catches a mutation that provokes a host error indirectly — dropping a `defer f.Close()` until a background goroutine panics with `too many open files`.
     - Otherwise the phase decides which wordings count. Before the test binary runs, `[build failed]`/`[setup failed]` output is entirely toolchain-authored, so generic phrasings are unambiguous there (`go: fork/exec …/compile: resource temporarily unavailable`, `ld: out of memory allocating …`). Once tests are running, `go test` merges their output into the same stream, so only phrasings a test is unlikely to print itself count — `fatal error: out of memory`, never a bare `out of memory`.
     - A `signal: killed` gomutants did not send is `INFRA ERROR`: the RSS monitor's own kill and the per-mutant deadline are both classified as `TIMED OUT` before this is reached, so what is left is an outside hand — typically a cgroup limit below the monitor's 2 GiB ceiling, which the kernel enforces before the monitor's 1s poll ever sees it. It is read both from the process exit error (the `go` process was signalled) and from a `signal: killed` line on stdout (only the test binary was, so `go test` survived to report it and exited 1). The stdout reading counts at the start of a line only, where `go test` writes it.
7. **Detect equivalent mutants** (only with `--detect-equivalent`). Each surviving mutant is recompiled with package-scoped `-gcflags=-S` under the same overlay mechanism; the reference (original) package is compiled once per package. When the normalized assembly matches the original, the mutation was folded away by the compiler and is reclassified `EQUIVALENT` — provably unkillable, so it leaves the efficacy denominator rather than failing the gate. The comparison is one-sided: any real difference in generated code diverges the hash, so a killable mutant is never marked equivalent.

Performance optimizations layered on top:

- **Per-mutant adaptive timeout.** Each mutant's deadline is `clamp(sum(selected test durations) × --timeout-margin, --timeout-min, global ceiling)`. A 50ms unit test gets a 2s floor instead of waiting out a multi-minute whole-suite ceiling, so infinite-loop mutants on fast packages trip in seconds rather than minutes. Falls back to the per-package sum when no per-test set is known, then to the global ceiling. Disable with `--adaptive-timeout=false`.
- **`GOMAXPROCS=NumCPU/workers` per child.** Without this, `--workers=10` on a 10-core box would have each child also assume 10 cores, oversubscribing 100×. With it, each child compiles + tests within its share.
- **Sort pending mutants by `(Pkg, File, Offset)` before dispatch.** The first mutant in a package pays the cold compile; subsequent ones reuse the build cache for deps and stdlib. This sort alone was a 17% wall-clock reduction.
- **`-vet=off` on the inner `go test`.** Vet runs in the user's CI on clean source; re-running it for every mutant is wasted work. Measured 17–39% per-mutant wall-clock reduction on representative packages.
- **Incremental cache.** A verdict is reused only while everything that could change it is byte-identical to the prior run: the mutated file; every other non-test file in its package, plus that package's `//go:embed` inputs; every test file in its package; and, when the run instruments beyond the package under test (`--integration`, or an explicit `--coverpkg`) and the coverage map places a covering test in another package, that package's test and production sources too. Without such a scope a foreign package is left alone: the coverage map's test names carry no package, so a name two packages happen to share would otherwise drag an unrelated package's sources into the key. Invalidation is package-scoped on purpose — a mutant compiles as part of its whole package, and tests share their package's helpers and fixtures, so file-level invalidation would replay stale verdicts. A CI run that touches one file therefore pays for that file's package, not the repository. The memoized coverage profile is keyed the same way, embedded data included, so a fixture edit that changes which lines are covered re-runs `go test -coverprofile` instead of replaying a stale profile. The one gap left is a change in a package the mutant's package *imports*; closing it needs the forward import closure, whose churn would invalidate most of a repository whenever a widely imported package changes. `INFRA ERROR` is never written to or reused from the cache, so transient host failures are retried.

### JSON report

Compatible with the gremlins JSON format:

```json
{
  "go_module": "github.com/example/project",
  "test_efficacy": 100,
  "mutations_coverage": 97.16,
  "mutants_total": 792,
  "mutants_killed": 772,
  "mutants_lived": 0,
  "mutants_not_viable": 0,
  "mutants_not_covered": 20,
  "mutants_suppressed": 5,
  "elapsed_time": 159.84,
  "files": [...]
}
```

Each entry under `files[].mutations[]` looks like this:

```json
{
  "id": "internal/runner/pool.go:(*Pool).dispatch:CONDITIONALS_BOUNDARY#2",
  "type": "CONDITIONALS_BOUNDARY",
  "status": "LIVED",
  "line": 142,
  "column": 9,
  "original": "<",
  "replacement": "<="
}
```

`id` is a stable handle for one mutant, formatted `file:function:TYPE#n`. The file segment is the path relative to the module root. Unlike the enclosing entry's `file_name`, which stays gremlins-compatible by dropping the import-path prefix shared by the packages in the run, it does not depend on which packages you asked for — so the same mutant carries the same `id` under `gomutants ./...` and under `gomutants ./internal/runner/`. The function segment is the enclosing declaration — `(*T).Method` for methods, empty for package-level declarations, and the enclosing function (not the literal) for mutants inside a closure. `n` counts mutants sharing those three fields, in source order.

Because the ID is anchored to a function rather than a line, it survives edits elsewhere in the file: adding imports, reformatting, or changing a neighbouring function all leave it untouched, so two runs' reports can be diffed entry by entry to see which mutants are *still* alive. Adding a *differently* named neighbour is one such edit; adding one that renders to the same name is the exception, covered below. IDs are unique within a report. The Stryker and HTML reports use the same string as their mutant `id`.

An ID does change when:

- the enclosing function is renamed, or its receiver type changes;
- a same-type mutation point is added or removed earlier in that same function;
- the file is renamed or moved;
- a same-type mutation point is added or removed earlier in the same file, for the package-level declarations that share the empty function segment. Mutants inside functions are unaffected;
- a declaration rendering to the same name as the enclosing one is added or removed earlier in the file — in practice, another `func init()`. See below.

Two declarations in one file that render to the same name — most often a second `func init()` — are told apart by an occurrence suffix: `init`, then `init~2`. They do not share a counter, so an edit inside the first leaves the second's IDs alone.

The suffix counts occurrences in source order, so inserting a third `func init()` *above* the other two shifts them to `init~2` and `init~3` and hands the new declaration the bare `init` anchor. Unlike a rename, this does not retire the old IDs: they still resolve, but to mutants in a different declaration, so a report diff across that edit reads them as the same mutant changing status. Compare by line as well when a file gains or loses an `init`. Nothing else collides — two same-named functions are a compile error, and a method's anchor carries its receiver type.

`mutants_suppressed` is omitted when zero; it counts mutants dropped by `// gomutants:disable*` directives or by [`--exclude-calls`](#call-site-exclusion), and is excluded from every other count. `mutants_suppressed_by_calls` (also omitted when zero) breaks out the `--exclude-calls` share of that total rather than adding to it.

`mutants_equivalent` is omitted when zero; it counts surviving mutants proven equivalent by `--detect-equivalent`. They stay in `mutants_total` but count as neither killed nor lived, so they drop out of the `test_efficacy` denominator.

`mutants_infra_error` is omitted when zero; it counts mutants whose tests could not produce a reliable verdict because gomutants recognized an environmental resource or I/O failure. They stay in `mutants_total` and `mutations_coverage`, but count as neither killed nor lived and are not cached. Per-mutant entries use the status `INFRA ERROR`; Stryker and HTML reports map it to `RuntimeError`.

Because those mutants leave both threshold denominators, a run that hit them is an *incomplete* measurement rather than a passing one. When the count is non-zero gomutants prints a warning on **stderr** before evaluating the gates, so a CI job that keeps only the exit code and the JSON report still sees that the run needs a rerun.

### Re-running a single mutant

`--run-mutant-id` narrows a run to exactly one mutant, named by the `id` from a previous report. That turns "did the test I just wrote kill it?" into one `go test` instead of the whole package's worth:

```sh
# 1. Full run. Pick a survivor's id out of the report.
gomutants -o report.json ./...
jq -r '.files[].mutations[] | select(.status == "LIVED") | .id' report.json

# 2. Write a test, then re-run just that mutant.
gomutants --run-mutant-id 'internal/cli/cli.go:parseArgs:CONDITIONALS_BOUNDARY#2' ./...
```

A unique prefix works too, so you rarely need the whole string. An exact `id` always wins over a prefix — necessary because `#1` is a prefix of `#10`. A prefix matching more than one mutant is an error that lists the candidates; an `id` matching none is an error too. So is naming a mutant that another filter then drops — a `// gomutants:disable` directive, [`--exclude-calls`](#call-site-exclusion), or a `--changed-since` range that excludes its line — because the alternative is a run that tests nothing and exits 0. So is combining it with `--dry-run`, which returns before anything is compiled or tested. All of those exit **2**, the same as any other bad invocation. The flag is CLI-only: there is no `run-mutant-id` config key, since a committed one would pin every later run to a single mutant.

Pair it with a threshold to get a scriptable answer:

```sh
gomutants --run-mutant-id "$ID" --threshold-efficacy 100 ./...   # exit 0 = killed, 10 = still alive
```

Three behaviors worth knowing:

- **The incremental cache is bypassed for that mutant.** A cache hit would replay the previous verdict rather than run anything, which is exactly the wrong answer when you have just edited a test. The fresh verdict is still written back to the cache.
- **Setup is not skipped.** Coverage collection, the baseline measurement and the per-test coverage map all still run — the saving is one mutant test run instead of N, which is the part that grows with the package. A bad `id` is the exception: it is resolved against the discovered set before those phases start, so a typo or a stale anchor fails immediately instead of after a full `go test -cover`.
- **A missing verdict exits 1, not 0.** `KILLED` and `LIVED` are the only two statuses that answer the question. If the named mutant comes back `NOT COVERED`, `NOT VIABLE`, `TIMED OUT`, `EQUIVALENT` or `INFRA ERROR`, the thresholds above would see an empty denominator, skip the gate and exit 0 — so a single-mutant run instead fails with the status it got. Ordinary whole-package runs keep the skip-the-gate behavior.

## Self-efficacy (gomutants on itself)

gomutants kills **96.01%** of mutants in its `./internal/...` library code (2478 killed, 103 survivors out of 2845 discovered). Statement coverage is 100%. The CI gate fails on any surviving mutant on changed lines per PR, so drift surfaces on the PR that introduces it. See [docs/MUTATION_COVERAGE.md](docs/MUTATION_COVERAGE.md) for the per-package breakdown and an analysis of why the remaining mutants survive.

The `main` package is excluded from mutation testing. Its mutants exercise the integration test suite (which forks gomutants subprocesses to test mutated overlays), each taking minutes; running them in CI under the same gate isn't tractable, and most surviving mutants are output-formatting drift the integration tests intentionally don't pin.

## Security & Code Quality

**Supply chain.** Releases are signed with [cosign](https://docs.sigstore.dev/) (keyless Sigstore), ship [SPDX](https://spdx.dev/) SBOMs for every artifact, and carry [SLSA Level 3](https://slsa.dev/spec/v1.0/levels#build-l3) build provenance. Published tags are immutable. Release candidates (`v0.5.1-rc0`) go through the identical signing, SBOM, and provenance pipeline and are marked as pre-releases. See [Verifying Releases](#verifying-releases) for verification commands. The repo is tracked by [OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/szhekpisov/gomutants) (badge above).

**Continuous checks.** Every push and PR is scanned by:

- [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) — known vulnerability detection
- [zizmor](https://github.com/zizmorcore/zizmor) — GitHub Actions workflow security scanning
- [golangci-lint](https://golangci-lint.run/) — multi-linter static analysis

**Test quality.** Unit + integration test suite (the integration suite forks gomutants subprocesses to test mutated overlays end-to-end). Mutation testing gated per-PR (no LIVED mutant on changed lines), with 96.01% efficacy on the full `./internal/...` library tree — the tool is dogfooded on itself, gated by its own CI gate.

**Reporting vulnerabilities.** Open a [private GitHub Security Advisory](https://github.com/szhekpisov/gomutants/security/advisories/new).

## Contributing

Found a bug or have a feature request? [Open an issue](https://github.com/szhekpisov/gomutants/issues/new).

Maintainers: see [RELEASING.md](RELEASING.md) for the release process.

<details>
<summary>Development setup</summary>

**Prerequisites:** Go 1.26+

```bash
git clone https://github.com/szhekpisov/gomutants.git
cd gomutants
go build ./...
```

**Useful commands:**

```bash
go test ./...                    # full test suite (unit + integration)
go test -race ./...              # race detector
./gomutants ./internal/<pkg>/    # mutation-test one package locally
```

**CI pipelines** (run on every push and PR):
- **Tests** — unit + integration tests with coverage
- **Security & Static Analysis** — govulncheck + zizmor + golangci-lint
- **OpenSSF Scorecard** — supply-chain best-practices scoring
- **Mutation Testing** — gomutants on itself, gated per-PR on changed lines

</details>

## License

[MIT](LICENSE).

---

If you find this project useful, please consider giving it a ⭐ — it helps others discover it.
