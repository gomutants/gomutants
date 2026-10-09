#!/usr/bin/env bash
# Benchmark harness: compares gomutants with pinned Gremlins and Mutago builds.
#
# Run from the repository root: bash benchmarks/run.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

SUMMARIZE_ONLY=0
if [[ "${1:-}" == "--summarize-only" ]]; then
  SUMMARIZE_ONLY=1
fi

GOMUTANTS="$REPO_ROOT/bin/gomutants"
PINNED_BIN_DIR="$REPO_ROOT/benchmarks/bin"

find_competitor() {
  local name="$1" pinned="$PINNED_BIN_DIR/$1"
  if [[ -x "$pinned" ]]; then
    printf '%s\n' "$pinned"
  else
    command -v "$name" || true
  fi
}

GREMLINS="${GREMLINS:-$(find_competitor gremlins)}"
MUTAGO="${MUTAGO:-$(find_competitor mutago)}"
GO_TOOLCHAIN="${GO_TOOLCHAIN:-go1.25.7}"
WORKERS="${WORKERS:-10}"
RUNS="${RUNS:-3}"
# Gremlins needs a larger coefficient on very fast packages to avoid turning
# worker contention into TIMED_OUT results. Use the same ceiling for all tools.
TIMEOUT_COEF="${TIMEOUT_COEF:-50}"

if (( SUMMARIZE_ONLY == 0 )); then
  # Always rebuild so a stale binary from an old branch cannot skew results.
  echo "Building gomutants..."
  go build -o "$GOMUTANTS" .
fi

[[ -n "$GREMLINS" ]] || { echo "gremlins not found" >&2; exit 1; }
[[ -n "$MUTAGO" ]] || { echo "mutago not found" >&2; exit 1; }

for binary in hyperfine jq go; do
  command -v "$binary" >/dev/null || { echo "$binary required" >&2; exit 1; }
done

OUT_DIR="$REPO_ROOT/benchmarks/out"
mkdir -p "$OUT_DIR"

WORK_DIR=""
prepare_fixture() {
  WORK_DIR="$(mktemp -d "$OUT_DIR/work.XXXXXX")"
  mkdir -p "$WORK_DIR/testdata" "$WORK_DIR/internal"
  cp -R "$REPO_ROOT/testdata/simple" "$WORK_DIR/testdata/simple"
  cp -R "$REPO_ROOT/internal/mutator" "$WORK_DIR/internal/mutator"
  (
    cd "$WORK_DIR"
    go mod init github.com/gomutants/gomutants >/dev/null
    go mod edit -go="${GO_TOOLCHAIN#go}"
    env GOTOOLCHAIN="$GO_TOOLCHAIN" go test ./... >/dev/null
  )
}

cleanup_fixture() {
  case "$WORK_DIR" in
    "$OUT_DIR"/work.*) rm -rf -- "$WORK_DIR" ;;
    *) ;;
  esac
}

if (( SUMMARIZE_ONLY == 0 )); then
  prepare_fixture
  trap cleanup_fixture EXIT
fi

# These four operators have the same token substitutions in all three tools.
# Mutago does not implement Gremlins/gomutants's ++/-- swap, so that fifth
# Gremlins default operator is intentionally excluded from matched scenarios.
MATCHED_GOMUTANTS="ARITHMETIC_BASE,CONDITIONALS_BOUNDARY,CONDITIONALS_NEGATION,INVERT_NEGATIVES"
MATCHED_MUTAGO_KEEP="arithmetic/base arithmetic/negate conditional/negated expression/comparison"

mutago_disable_flags() {
  local mutator keep disabled=""
  while IFS= read -r mutator; do
    keep=0
    for included in $MATCHED_MUTAGO_KEEP; do
      if [[ "$mutator" == "$included" ]]; then
        keep=1
        break
      fi
    done
    if (( keep == 0 )); then
      disabled="$disabled --disable=$mutator"
    fi
  done < <("$MUTAGO" --list-mutators)
  printf '%s\n' "$disabled"
}

MUTAGO_MATCHED_DISABLE="$(mutago_disable_flags)"

# Spec format: label|description|gomutants path|competitor path|mode
SCENARIOS=(
  "small-defaults|./testdata/simple with each tool's default mutator catalog|./testdata/simple/...|./testdata/simple|defaults"
  "small-matched|./testdata/simple with the matched 4-operator set|./testdata/simple/...|./testdata/simple|matched"
  "mutator-matched|./internal/mutator with the matched 4-operator set|./internal/mutator/...|./internal/mutator|matched"
)

cpu_info() {
  if [[ "$(uname)" == "Darwin" ]]; then
    sysctl -n machdep.cpu.brand_string 2>/dev/null || uname -m
  else
    grep -m1 "model name" /proc/cpuinfo | cut -d: -f2- | sed 's/^ *//'
  fi
}

module_version() {
  local binary="$1"
  go version -m "$binary" 2>/dev/null | awk '$1 == "mod" { print $2 " " $3; exit }'
}

run_warmup() {
  local tool="$1" command="$2" log="$3"
  if ! eval "$command" >"$log" 2>&1; then
    echo "$tool warm-up failed:" >&2
    cat "$log" >&2
    exit 1
  fi
}

run_scenario() {
  local label="$1" desc="$2" gom_path="$3" competitor_path="$4" mode="$5"
  local gom_extra="" gre_extra="" mut_extra=""
  if [[ "$mode" == "matched" ]]; then
    gom_extra="--only $MATCHED_GOMUTANTS"
    gre_extra="--increment-decrement=false"
    mut_extra="$MUTAGO_MATCHED_DISABLE"
  fi

  echo
  echo "===== Scenario: $label ====="
  echo "$desc"

  local gom_json="$OUT_DIR/${label}-gomutants.json"
  local gre_json="$OUT_DIR/${label}-gremlins.json"
  local mut_json="$OUT_DIR/${label}-mutago.json"
  local hf_json="$OUT_DIR/${label}-hyperfine.json"

  # Every tool runs against the same isolated source snapshot and Go toolchain.
  # Cache is disabled for gomutants; neither competitor has an equivalent
  # persistent mutant-verdict cache.
  local gom_cmd="env GOTOOLCHAIN=$GO_TOOLCHAIN \"$GOMUTANTS\" --workers $WORKERS --timeout-coefficient $TIMEOUT_COEF --cache=off --quiet $gom_extra --output \"$gom_json\" $gom_path"
  local gre_cmd="env GOTOOLCHAIN=$GO_TOOLCHAIN \"$GREMLINS\" unleash --silent --workers $WORKERS --timeout-coefficient $TIMEOUT_COEF $gre_extra --output \"$gre_json\" $competitor_path"
  # Coverage and per-test routing are opt-in in Mutago. Enable both so the
  # comparison gives its engine the strongest native execution mode.
  local mut_cmd="env GOTOOLCHAIN=$GO_TOOLCHAIN \"$MUTAGO\" --noop --coverage --per-test --workers=$WORKERS --timeout-coefficient=$TIMEOUT_COEF --quiet --no-diffs --logger-summary-json $mut_extra $competitor_path"

  echo "Warming..."
  (
    cd "$WORK_DIR"
    run_warmup gomutants "$gom_cmd" "$OUT_DIR/${label}-gomutants-warmup.log"
    run_warmup gremlins "$gre_cmd" "$OUT_DIR/${label}-gremlins-warmup.log"
    run_warmup mutago "$mut_cmd" "$OUT_DIR/${label}-mutago-warmup.log"
  )

  echo "Running hyperfine ($RUNS runs each)..."
  (
    cd "$WORK_DIR"
    hyperfine --warmup 0 --runs "$RUNS" --export-json "$hf_json" \
      -n gomutants "$gom_cmd" \
      -n gremlins "$gre_cmd" \
      -n mutago "$mut_cmd"
    cp mutago-summary.json "$mut_json"
  )
}

status_count() {
  local report="$1" status="$2"
  jq --arg status "$status" '[.files[].mutations[].status | select(.==$status)] | length' "$report"
}

relative_line() {
  local competitor="$1" gom_mean="$2" competitor_mean="$3"
  if awk "BEGIN{exit !($gom_mean < $competitor_mean)}"; then
    local ratio
    ratio="$(awk "BEGIN{printf \"%.2f\", $competitor_mean / $gom_mean}")"
    printf 'gomutants was %s× faster than %s' "$ratio" "$competitor"
  else
    local ratio
    ratio="$(awk "BEGIN{printf \"%.2f\", $gom_mean / $competitor_mean}")"
    printf '%s was %s× faster than gomutants' "$competitor" "$ratio"
  fi
}

summarize_scenario() {
  local label="$1" desc="$2" mode="$3"
  local gom_json="$OUT_DIR/${label}-gomutants.json"
  local gre_json="$OUT_DIR/${label}-gremlins.json"
  local mut_json="$OUT_DIR/${label}-mutago.json"
  local hf_json="$OUT_DIR/${label}-hyperfine.json"

  local gom_mean gre_mean mut_mean gom_std gre_std mut_std
  gom_mean="$(jq -r '.results[] | select(.command=="gomutants") | .mean' "$hf_json")"
  gre_mean="$(jq -r '.results[] | select(.command=="gremlins") | .mean' "$hf_json")"
  mut_mean="$(jq -r '.results[] | select(.command=="mutago") | .mean' "$hf_json")"
  gom_std="$(jq -r '.results[] | select(.command=="gomutants") | .stddev' "$hf_json")"
  gre_std="$(jq -r '.results[] | select(.command=="gremlins") | .stddev' "$hf_json")"
  mut_std="$(jq -r '.results[] | select(.command=="mutago") | .stddev' "$hf_json")"

  local gom_killed gom_lived gom_nc gom_nv gom_to gom_err gom_total gom_exec gom_per
  gom_killed="$(status_count "$gom_json" "KILLED")"
  gom_lived="$(status_count "$gom_json" "LIVED")"
  gom_nc="$(status_count "$gom_json" "NOT COVERED")"
  gom_nv="$(status_count "$gom_json" "NOT VIABLE")"
  gom_to="$(status_count "$gom_json" "TIMED OUT")"
  gom_err="$(jq '[.files[].mutations[].status | select(.=="INFRA ERROR" or .=="ERROR")] | length' "$gom_json")"
  gom_total="$(jq '[.files[].mutations[]] | length' "$gom_json")"
  gom_exec=$((gom_killed + gom_lived))
  gom_per="n/a"
  if (( gom_exec > 0 )); then
    gom_per="$(awk "BEGIN{printf \"%.0f\", ($gom_mean * 1000) / $gom_exec}")"
  fi

  local gre_killed gre_lived gre_nc gre_nv gre_to gre_err gre_total gre_exec gre_per
  gre_killed="$(status_count "$gre_json" "KILLED")"
  gre_lived="$(status_count "$gre_json" "LIVED")"
  gre_nc="$(status_count "$gre_json" "NOT COVERED")"
  gre_nv="$(status_count "$gre_json" "NOT VIABLE")"
  gre_to="$(status_count "$gre_json" "TIMED OUT")"
  gre_err="$(status_count "$gre_json" "ERROR")"
  gre_total="$(jq '[.files[].mutations[]] | length' "$gre_json")"
  gre_exec=$((gre_killed + gre_lived))
  gre_per="n/a"
  if (( gre_exec > 0 )); then
    gre_per="$(awk "BEGIN{printf \"%.0f\", ($gre_mean * 1000) / $gre_exec}")"
  fi

  local mut_killed mut_lived mut_nc mut_err mut_skipped mut_total mut_exec mut_per
  mut_killed="$(jq -r '.killedCount' "$mut_json")"
  mut_lived="$(jq -r '.escapedCount' "$mut_json")"
  mut_nc="$(jq -r '.notCoveredCount' "$mut_json")"
  mut_err="$(jq -r '.errorCount' "$mut_json")"
  mut_skipped="$(jq -r '.skippedCount' "$mut_json")"
  mut_total="$(jq -r '.totalMutantsCount' "$mut_json")"
  mut_exec=$((mut_killed + mut_lived))
  mut_per="n/a"
  if (( mut_exec > 0 )); then
    mut_per="$(awk "BEGIN{printf \"%.0f\", ($mut_mean * 1000) / $mut_exec}")"
  fi

  local qualifier="default catalogs; workloads differ"
  if [[ "$mode" == "matched" ]]; then
    qualifier="matched operator semantics"
  fi

  cat <<EOF
### $label — $desc

| Metric | gomutants | Gremlins | Mutago |
|---|---:|---:|---:|
| Wall-clock mean ± σ (s) | $(printf "%.2f ± %.2f" "$gom_mean" "$gom_std") | $(printf "%.2f ± %.2f" "$gre_mean" "$gre_std") | $(printf "%.2f ± %.2f" "$mut_mean" "$mut_std") |
| Mutants reported | $gom_total | $gre_total | $mut_total |
| Killed | $gom_killed | $gre_killed | $mut_killed |
| Lived / escaped | $gom_lived | $gre_lived | $mut_lived |
| Not covered | $gom_nc | $gre_nc | $mut_nc |
| Not viable | $gom_nv | $gre_nv | n/a |
| Timed out | $gom_to | $gre_to | n/a |
| Other errors | $gom_err | $gre_err | ${mut_err}¹ |
| Skipped | 0 | 0 | $mut_skipped |
| Tested mutants (killed + lived) | $gom_exec | $gre_exec | $mut_exec |
| Time per tested mutant (ms) | $gom_per | $gre_per | $mut_per |

**Pairwise wall clock ($qualifier):** $(relative_line Gremlins "$gom_mean" "$gre_mean"); $(relative_line Mutago "$gom_mean" "$mut_mean").

EOF
}

if (( SUMMARIZE_ONLY == 0 )); then
  for spec in "${SCENARIOS[@]}"; do
    IFS='|' read -r label desc gom_path competitor_path mode <<<"$spec"
    run_scenario "$label" "$desc" "$gom_path" "$competitor_path" "$mode"
  done
fi

RESULTS_MD="$REPO_ROOT/benchmarks/results.md"
{
  echo "# Benchmark Results: gomutants vs Gremlins and Mutago"
  echo
  echo "_Generated: $(date -u +'%Y-%m-%d')_"
  echo
  echo "| | |"
  echo "|---|---|"
  echo "| Host | $(uname -sm) |"
  echo "| CPU | $(cpu_info) |"
  echo "| Target Go | $(env GOTOOLCHAIN="$GO_TOOLCHAIN" go version | awk '{print $3, $4}') |"
  echo "| gomutants | $("$GOMUTANTS" --version 2>&1 | head -1) |"
  echo "| Gremlins | $(module_version "$GREMLINS") |"
  echo "| Mutago | $(module_version "$MUTAGO") |"
  echo "| workers | $WORKERS |"
  echo "| timeout coefficient | $TIMEOUT_COEF |"
  echo "| hyperfine runs per scenario | $(jq '.results[0].times | length' "$OUT_DIR/small-defaults-hyperfine.json") |"
  echo
  echo "Raw hyperfine output, warm-up logs, and final tool reports are in \`benchmarks/out/\` (gitignored)."
  echo
  for spec in "${SCENARIOS[@]}"; do
    IFS='|' read -r label desc gom_path competitor_path mode <<<"$spec"
    summarize_scenario "$label" "$desc" "$mode"
  done
  cat <<'EOF'
¹ Mutago combines compile failures, execution errors, and timeouts in one
`errored` outcome; its timeout count cannot be separated from the summary.

## Reading the results

- `small-defaults` measures the product-facing default mutator catalogs, not
  equal work. Mutago coverage and per-test routing are enabled because they are
  its strongest native execution mode; gomutants uses its default routing and
  Gremlins always starts with package coverage.
- The matched scenarios use the four operator semantics shared exactly by all
  three tools: arithmetic replacement, conditional boundary, conditional
  negation, and unary-negative inversion. `INCREMENT_DECREMENT` is excluded
  because Mutago does not implement the same `++`/`--` swap.
- The medium matched scenario is the cleanest engine comparison. Total counts
  should still be read alongside wall time: discovery and viability filters can
  differ even when operator transformations are the same.
- Time per tested mutant normalizes wall time by KILLED + LIVED/escaped only.
  It excludes uncovered, non-viable, timed-out, errored, and skipped mutants
  because no completed test verdict exists for them.

## Caveats

- The harness copies the two source targets into an isolated temporary module
  with `go 1.25.7`. Pinned Gremlins v0.6.0 panics while executing these targets
  under Go 1.26.x; running every tool through the pinned compatible toolchain
  keeps the comparison usable.
- Gomutants's persistent cache is disabled. Gremlins and Mutago do not provide
  an equivalent mutant-verdict cache, so this page compares cold execution.
- Gomutants performs one-time coverage, baseline, and per-test-map setup. That
  fixed cost is visible on the tiny fixture and amortizes as mutant count grows.
- The coefficient is 50 because lower Gremlins deadlines misclassify mutants on
  fast packages under worker contention. All tools receive the same value.
- Gooze v1.0.1 is excluded: the audited tag does not compile because its source
  refers to missing coverage-index definitions, so it cannot produce a valid
  end-to-end timing.
- Wall-clock results are sensitive to CPU load, thermal state, and Go build
  cache state. Re-run under quiet conditions before quoting the numbers.
EOF
} >"$RESULTS_MD"

echo
echo "Wrote $RESULTS_MD"
