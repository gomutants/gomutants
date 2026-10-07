package config

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type MutatorConfig struct {
	Enabled *bool `yaml:"enabled"`
}

type Config struct {
	Workers            int `yaml:"workers"`
	TestCPU            int `yaml:"test-cpu"`
	TimeoutCoefficient int `yaml:"timeout-coefficient"`
	// TimeoutMargin scales per-mutant adaptive timeouts (sum of selected
	// per-test durations × this). Default 3.0 — wide enough to absorb GC
	// pauses, scheduler jitter, and mutated-code slowdowns without false
	// TIMED OUT classifications. Only used when AdaptiveTimeout is true.
	TimeoutMargin float64 `yaml:"timeout-margin"`
	// TimeoutMin floors the per-mutant adaptive timeout. Default 2s —
	// covers child-process fork + cold-start cost on tests that measure
	// in single-digit milliseconds. Only used when AdaptiveTimeout is true.
	TimeoutMin time.Duration `yaml:"timeout-min"`
	// AdaptiveTimeout enables per-mutant adaptive timeout selection.
	// Pointer so YAML can distinguish "user opted in/out" from "default"
	// — without it, a YAML `adaptive-timeout: false` is indistinguishable
	// from the zero value during ApplyFlags merging. Use
	// AdaptiveTimeoutEnabled() in callers; that handles the default.
	AdaptiveTimeout *bool  `yaml:"adaptive-timeout"`
	CoverPkg        string `yaml:"coverpkg"`
	// Tags is forwarded verbatim as `-tags=<value>` to every inner `go`
	// command (list, test, test -c) so mutation testing reaches code
	// guarded by `//go:build` constraints. Go parses the comma-separated
	// list itself, so we pass the raw string through unchanged.
	Tags string `yaml:"tags"`
	// TestFlags is forwarded verbatim to the inner `go test` invocations
	// (per-mutant runs, the coverage run, the baseline run) and to nothing
	// else. That scoping is the point, and is why GOFLAGS is not a usable
	// workaround: Go honors a GOFLAGS entry only "when the given flag is
	// known by the current command", so it silently reaches whichever of
	// `go list`/`go test -c`/`go test` happen to accept it, and it slips
	// past the managed-flag guard in main.go that rejects e.g. -run.
	// Whitespace-separated; see TestFlagFields.
	TestFlags string   `yaml:"test-flags"`
	Output    string   `yaml:"output"`
	DryRun    bool     `yaml:"dry-run"`
	Verbose   bool     `yaml:"verbose"`
	Quiet     bool     `yaml:"quiet"`
	Disable   []string `yaml:"disable"`
	Only      []string `yaml:"only"`
	// ExcludeFiles holds regexps matched (unanchored) against each
	// production file's module-relative path; matching files are skipped
	// entirely, producing no mutants. Test files are never mutated and so
	// are unaffected.
	ExcludeFiles []string `yaml:"exclude-files"`
	// ExcludeCalls holds selector globs (`log.Print*`, `*.Debug`) matched
	// against the rendered selector of every call. Mutants inside a
	// matching call are suppressed — the Go analogue of PITest's
	// avoidCallsTo, for operators inside logging and telemetry arguments
	// that no test can reasonably assert on. Extends
	// DefaultExcludeCalls unless ExcludeCallsDefaults says otherwise;
	// read the merged list through ResolvedExcludeCalls.
	ExcludeCalls []string `yaml:"exclude-calls"`
	// ExcludeCallsDefaults controls whether the built-in stdlib-logging
	// set is in effect. Pointer for the same three-state reason as
	// AdaptiveTimeout; when unset the default is true (extend). Set it
	// false to narrow or fully replace the built-ins.
	ExcludeCallsDefaults *bool  `yaml:"exclude-calls-defaults"`
	ChangedSince         string `yaml:"changed-since"`
	// RunMutantID narrows the run to the single mutant with this stable
	// id (the `id` field of a JSON report entry); a unique prefix is
	// accepted. Sits beside ChangedSince because both are mutant filters,
	// but unlike every other field here it is deliberately not YAML-backed:
	// applyStringFlags only overrides on a non-empty flag, so a committed
	// `run-mutant-id` would pin every later run to one mutant with no way
	// to switch it off from the CLI. It is set from --run-mutant-id only.
	RunMutantID string `yaml:"-"`
	Cache       string `yaml:"cache"`
	// CheckpointInterval is how often completed mutant outcomes are
	// flushed to the cache file mid-run, so a hard kill (OOM, CI timeout,
	// SIGKILL) loses at most this much progress. 0 disables periodic
	// checkpointing — the cache is then written only once, at the end of
	// the run. Negative values are nonsensical and revert to the default.
	CheckpointInterval time.Duration `yaml:"checkpoint-interval"`
	// DetectEquivalent enables the post-test Trivial Compiler Equivalence
	// pass that reclassifies provably-equivalent survivors as EQUIVALENT.
	// Pointer for the same three-state reason as AdaptiveTimeout; default
	// is OFF (see DetectEquivalentEnabled), because it adds a package
	// compile per survivor.
	DetectEquivalent *bool                     `yaml:"detect-equivalent"`
	Mutants          map[string]*MutatorConfig `yaml:"mutants"`
	// Integration enables cross-package per-test routing: a mutant is
	// routed to (and killed by) covering tests in any package that imports
	// it, not just tests in its own package. Coverage instrumentation and
	// the per-test map widen to the reverse-dependency closure of the
	// target packages. Off by default; see main.go for the -coverpkg
	// conflict guard.
	Integration bool `yaml:"integration"`
	// Schemata enables mutant schemata: every schematizable mutant of a
	// package is compiled into one test binary, inert behind a guard, and
	// selected at run time by environment variable. That removes the
	// per-mutant compile and link, which dominates a cold run. Mutants the
	// rewriter cannot prove safe fall back to the per-mutant overlay path,
	// so verdicts are unchanged either way — which is also why this is
	// deliberately absent from the cache identity. Off by default while the
	// feature is experimental.
	Schemata bool `yaml:"schemata"`
}

// Default values for adaptive-timeout knobs. Exposed as package-level
// constants so the CLI flag descriptions can quote the same numbers.
const (
	DefaultTimeoutMargin = 3.0
	DefaultTimeoutMin    = 2 * time.Second
	// DefaultCheckpointInterval is the default cadence for mid-run cache
	// checkpointing. Cheap relative to per-mutant `go test` cost, and
	// bounds worst-case lost work on a hard kill to ~this duration.
	DefaultCheckpointInterval = 10 * time.Second
)

// AdaptiveTimeoutEnabled returns whether per-mutant adaptive timeout
// selection is in effect. The pointer field allows three states (set to
// true, set to false, unset); when unset the default is true.
func (c *Config) AdaptiveTimeoutEnabled() bool {
	if c.AdaptiveTimeout == nil {
		return true
	}
	return *c.AdaptiveTimeout
}

// DetectEquivalentEnabled reports whether the Trivial Compiler Equivalence
// pass should run. The pointer field allows three states; when unset the
// default is false (opt-in — the opposite of AdaptiveTimeout).
func (c *Config) DetectEquivalentEnabled() bool {
	if c.DetectEquivalent == nil {
		return false
	}
	return *c.DetectEquivalent
}

// defaultExcludeCalls is the built-in call-exclusion set: Go's standard
// library logging, and nothing else. Deliberately conservative, since it
// applies to every project with no opt-in.
//
// Three families are left out on purpose:
//   - log.Fatal*/log.Panic* — they exit or panic, so deleting one is a
//     real behavioural change a test can and should catch. Keeping them
//     mutable is what makes suppressing a whole call expression (rather
//     than only its argument list) safe.
//   - slog.New/SetDefault/With — logger construction and configuration,
//     not emission.
//   - Method-shaped globs like `*.Info` or `*.Error` — they would reach
//     err.Error() and any domain method that shares a name. A project
//     wanting them for its own logger adds them itself.
//
// Attribute constructors (slog.String, slog.Int, …) need no entry: they
// appear as arguments to a matching call, and so already sit inside its
// suppressed span.
var defaultExcludeCalls = []string{
	"log.Print*",
	"log.Output",
	"slog.Debug*",
	"slog.Info*",
	"slog.Warn*",
	"slog.Error*",
	"slog.Log*",
}

// DefaultExcludeCalls returns a copy of the built-in call-exclusion set,
// so a caller appending to it can't grow the package-level slice.
func DefaultExcludeCalls() []string {
	return slices.Clone(defaultExcludeCalls)
}

// ExcludeCallsDefaultsEnabled reports whether the built-in stdlib-logging
// set is in effect. The pointer field allows three states; when unset the
// default is true — the user list extends the built-ins rather than
// replacing them.
func (c *Config) ExcludeCallsDefaultsEnabled() bool {
	if c.ExcludeCallsDefaults == nil {
		return true
	}
	return *c.ExcludeCallsDefaults
}

// ResolvedExcludeCalls is the call-exclusion list actually applied: the
// built-in set followed by the user's entries, or the user's entries
// alone when the built-ins are switched off. Centralized here so the CLI
// and any other caller can't disagree about what "extend" means.
func (c *Config) ResolvedExcludeCalls() []string {
	if !c.ExcludeCallsDefaultsEnabled() {
		return slices.Clone(c.ExcludeCalls)
	}
	return append(DefaultExcludeCalls(), c.ExcludeCalls...)
}

// TestFlagFields splits TestFlags into the argv fragments appended to each
// inner `go test`. Whitespace-separated, so a flag whose value contains a
// space cannot be expressed — repeat the flag or use its `=` form instead.
// Centralized here so every consumer (worker, coverage run, baseline run,
// cache key) splits identically; a per-call-site strings.Fields would let
// them drift.
func (c *Config) TestFlagFields() []string {
	return strings.Fields(c.TestFlags)
}

// FlagName extracts the flag name from one `go test` argv fragment:
// leading dashes and any `=value` suffix stripped, plus the `test.`
// prefix `go test` accepts as an alias for its own flags (`-test.run` and
// `-run` are the same flag by the time the test binary parses them, and
// the aliased spelling wins where both appear). ok is false for a field
// that isn't a flag at all — a value sitting in its own field, as in
// `-gcflags all=-N`.
//
// Exported for the CLI's managed-flag guard, which would otherwise miss
// `-test.run`.
func FlagName(field string) (string, bool) {
	if !strings.HasPrefix(field, "-") {
		return "", false
	}
	name, _, _ := strings.Cut(strings.TrimLeft(field, "-"), "=")
	return strings.TrimPrefix(name, "test."), true
}

// CanonicalTestFlags is TestFlags reduced to the form cache identity is
// computed from: the split fields rejoined by single spaces. Whitespace
// can be normalized because it never reaches argv, but field order must be
// preserved. Arbitrary test-binary flags can be aliases for the same
// variable or otherwise interact while parsing, so even distinct names are
// not guaranteed to commute. Collapsing two orders onto one key could replay
// coverage and mutant verdicts from a behaviorally different run.
func (c *Config) CanonicalTestFlags() string {
	return strings.Join(c.TestFlagFields(), " ")
}

// DefaultWorkers returns the default worker count: NumCPU. Floored at 1.
// Use --workers / -w to override.
func DefaultWorkers() int {
	return max(1, runtime.NumCPU())
}

func Default() Config {
	return Config{
		Workers:            DefaultWorkers(),
		TimeoutCoefficient: 10,
		TimeoutMargin:      DefaultTimeoutMargin,
		TimeoutMin:         DefaultTimeoutMin,
		CheckpointInterval: DefaultCheckpointInterval,
		Output:             "mutation-report.json",
	}
}

func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config %s: %w", path, err)
	}

	// Preserve defaults for zero-value fields.
	if cfg.Workers == 0 {
		cfg.Workers = DefaultWorkers()
	}
	if cfg.TimeoutCoefficient == 0 {
		cfg.TimeoutCoefficient = 10
	}
	// Treat negative as nonsensical and revert to the default. ApplyFlags
	// already screens out non-positive CLI values; doing the same here
	// closes the YAML side. A negative Margin or Min would silently
	// collapse adaptive selection to the floor or to a negative scaled
	// value (later max'd up to Min) — never useful, never what the user
	// meant.
	if cfg.TimeoutMargin <= 0 {
		cfg.TimeoutMargin = DefaultTimeoutMargin
	}
	if cfg.TimeoutMin <= 0 {
		cfg.TimeoutMin = DefaultTimeoutMin
	}
	// Only negative values revert to the default — 0 is a meaningful
	// "disable periodic checkpointing" choice and must survive unmarshal.
	if cfg.CheckpointInterval < 0 {
		cfg.CheckpointInterval = DefaultCheckpointInterval
	}
	if cfg.Output == "" {
		cfg.Output = "mutation-report.json"
	}

	return cfg, nil
}

// ResolveCache materializes the cache path from the loaded config:
// "off" disables caching (Cache=""), an empty Cache enables it at the
// default path, and any other value passes through. Call after Load and
// ApplyFlags so YAML and CLI inputs are merged before the default
// kicks in.
func (c *Config) ResolveCache() {
	switch c.Cache {
	case "off":
		c.Cache = ""
	case "":
		c.Cache = ".gomutants-cache.json"
	}
}

// AdaptiveTimeoutFlag captures the `--adaptive-timeout` CLI flag value.
// Used as a parameter to ApplyFlags so the CLI layer can express three
// states ("set to true", "set to false", "not provided") that a plain
// bool cannot.
type AdaptiveTimeoutFlag struct {
	Set   bool
	Value bool
}

// DetectEquivalentFlag captures the `--detect-equivalent` CLI flag value.
// Like AdaptiveTimeoutFlag, the Set bit lets ApplyFlags distinguish "not
// provided" from an explicit `--detect-equivalent=false`.
type DetectEquivalentFlag struct {
	Set   bool
	Value bool
}

// ExcludeCallsDefaultsFlag captures the `--exclude-calls-defaults` CLI
// flag value. Like AdaptiveTimeoutFlag, the Set bit lets ApplyFlags tell
// "not provided" from an explicit `--exclude-calls-defaults=false`.
type ExcludeCallsDefaultsFlag struct {
	Set   bool
	Value bool
}

// CheckpointIntervalFlag captures the `--checkpoint-interval` CLI flag
// value. Like AdaptiveTimeoutFlag, it carries a Set bit so ApplyFlags can
// tell "not provided" from an explicit `--checkpoint-interval=0`; a plain
// duration can't, because 0 is both the zero value and a valid choice.
type CheckpointIntervalFlag struct {
	Set   bool
	Value time.Duration
}

// Flags bundles the CLI flag values consumed by ApplyFlags. Bundling
// them keeps the per-flag merge semantics ("non-zero / non-empty / Set=true
// overrides YAML") at the call site explicit by named field, which a
// positional signature can't sanely express past a handful of args.
type Flags struct {
	Workers            int
	TestCPU            int
	TimeoutCoefficient int
	TimeoutMargin      float64
	TimeoutMin         time.Duration
	AdaptiveTimeout    AdaptiveTimeoutFlag
	DetectEquivalent   DetectEquivalentFlag
	CheckpointInterval CheckpointIntervalFlag
	CoverPkg           string
	Tags               string
	TestFlags          string
	Output             string
	Disable            string
	Only               string
	ExcludeFiles       string
	ExcludeCalls       string
	ChangedSince       string
	RunMutantID        string
	Cache              string
	DryRun             bool
	Verbose            bool
	Quiet              bool
	Integration        bool
	Schemata           bool
	// ExcludeCallsDefaults is three-state (see the type), not a plain
	// bool: unlike the toggles above, its default is on, so "not provided"
	// has to stay distinguishable from an explicit =false.
	ExcludeCallsDefaults ExcludeCallsDefaultsFlag
}

// ApplyFlags merges CLI-provided flag values into c, with CLI winning
// over the YAML-loaded defaults already present. Grouped into three
// per-kind helpers so each method stays under the linter's cognitive
// complexity threshold.
func (c *Config) ApplyFlags(f Flags) {
	c.applyNumericFlags(f)
	c.applyStringFlags(f)
	c.applyToggleFlags(f)
}

func (c *Config) applyNumericFlags(f Flags) {
	if f.Workers > 0 {
		c.Workers = f.Workers
	}
	if f.TestCPU > 0 {
		c.TestCPU = f.TestCPU
	}
	if f.TimeoutCoefficient > 0 {
		c.TimeoutCoefficient = f.TimeoutCoefficient
	}
	if f.TimeoutMargin > 0 {
		c.TimeoutMargin = f.TimeoutMargin
	}
	if f.TimeoutMin > 0 {
		c.TimeoutMin = f.TimeoutMin
	}
	if f.AdaptiveTimeout.Set {
		v := f.AdaptiveTimeout.Value
		c.AdaptiveTimeout = &v
	}
	if f.CheckpointInterval.Set {
		c.CheckpointInterval = f.CheckpointInterval.Value
	}
}

func (c *Config) applyStringFlags(f Flags) {
	if f.CoverPkg != "" {
		c.CoverPkg = f.CoverPkg
	}
	if f.Tags != "" {
		c.Tags = f.Tags
	}
	if f.TestFlags != "" {
		c.TestFlags = f.TestFlags
	}
	if f.Output != "" {
		c.Output = f.Output
	}
	if f.Disable != "" {
		c.Disable = splitAndTrim(f.Disable)
	}
	if f.Only != "" {
		c.Only = splitAndTrim(f.Only)
	}
	if f.ExcludeFiles != "" {
		c.ExcludeFiles = splitAndTrim(f.ExcludeFiles)
	}
	// Replaces the YAML list, like --exclude-files. The built-in defaults
	// are a separate layer and still extend the result; turn them off with
	// --exclude-calls-defaults=false.
	if f.ExcludeCalls != "" {
		c.ExcludeCalls = splitAndTrim(f.ExcludeCalls)
	}
	if f.ChangedSince != "" {
		c.ChangedSince = f.ChangedSince
	}
	if f.RunMutantID != "" {
		c.RunMutantID = f.RunMutantID
	}
	if f.Cache != "" {
		c.Cache = f.Cache
	}
}

func (c *Config) applyToggleFlags(f Flags) {
	if f.DryRun {
		c.DryRun = true
	}
	if f.Verbose {
		c.Verbose = true
	}
	if f.Quiet {
		c.Quiet = true
	}
	if f.Integration {
		c.Integration = true
	}
	if f.Schemata {
		c.Schemata = true
	}
	if f.DetectEquivalent.Set {
		v := f.DetectEquivalent.Value
		c.DetectEquivalent = &v
	}
	if f.ExcludeCallsDefaults.Set {
		v := f.ExcludeCallsDefaults.Value
		c.ExcludeCallsDefaults = &v
	}
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
