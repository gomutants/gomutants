package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gomutants/gomutants/internal/config"
	"github.com/gomutants/gomutants/internal/discover"
)

// cliOptions is the parsed command line. flags is merged over the config
// file by (*Config).ApplyFlags; the remaining fields are CLI-only and never
// come from .gomutants.yml.
type cliOptions struct {
	flags      config.Flags
	configPath string
	runOptions
	showVersion  bool
	listMutators bool
}

// runOptions are the CLI-only settings the mutation run itself reads. They
// are kept apart from flags so the run can't read a raw flag value that
// .gomutants.yml should have been merged with.
type runOptions struct {
	// packages is the positional package patterns, defaulted to ./... .
	packages []string

	strykerOutput     string
	htmlOutput        string
	annotations       string
	thresholdEfficacy float64
	thresholdMcover   float64
}

// boolFlagFunc registers a BoolFunc flag that parses its value and hands it
// to set. BoolFunc rather than BoolVar lets set record that the flag was
// given at all: the merge layer in (*Config).ApplyFlags relies on that .Set
// bit to let an explicit value (=false included) override YAML.
func boolFlagFunc(fs *flag.FlagSet, name, usage string, set func(bool)) {
	fs.BoolFunc(name, usage, func(s string) error {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("--%s: %w", name, err)
		}
		set(v)
		return nil
	})
}

// parseFlags parses args into cliOptions and runs the checks that need
// nothing but the flags themselves. A parse failure, including -h, comes
// back as a usage error wrapping the flag package's error, so callers can
// detect flag.ErrHelp with errors.Is.
func parseFlags(args []string) (*cliOptions, error) {
	// Strip "unleash" for gremlins CLI compat.
	if len(args) > 0 && args[0] == "unleash" {
		args = args[1:]
	}

	fs := flag.NewFlagSet("gomutants", flag.ContinueOnError)
	fs.SetOutput(stderr)

	o := &cliOptions{}
	f := &o.flags
	// --test-flags is repeatable; the values are joined once parsing ends.
	var testFlags []string

	fs.IntVar(&f.Workers, "workers", 0, "parallel workers (default: NumCPU)")
	fs.IntVar(&f.Workers, "w", 0, "parallel workers (shorthand)")
	fs.IntVar(&f.TestCPU, "test-cpu", 0, "value passed to inner go test -cpu per mutant (0 omits the flag; go test then uses GOMAXPROCS)")
	fs.IntVar(&f.TimeoutCoefficient, "timeout-coefficient", 0, "multiply baseline test time for the global timeout ceiling (default: 10)")
	fs.Float64Var(&f.TimeoutMargin, "timeout-margin", 0, fmt.Sprintf("scale per-test sums into the per-mutant adaptive timeout (default: %g)", config.DefaultTimeoutMargin))
	fs.DurationVar(&f.TimeoutMin, "timeout-min", 0, fmt.Sprintf("floor for the per-mutant adaptive timeout (default: %s)", config.DefaultTimeoutMin))
	boolFlagFunc(fs, "adaptive-timeout", "use per-test durations to size each mutant's timeout (default: true; pass =false to disable)", func(v bool) {
		f.AdaptiveTimeout = config.AdaptiveTimeoutFlag{Set: true, Value: v}
	})
	// Opt-in TCE pass.
	boolFlagFunc(fs, "detect-equivalent", "after testing, compile each surviving mutant with -gcflags=-S and mark it EQUIVALENT when the generated assembly is identical to the original (Trivial Compiler Equivalence; default false, adds one package compile per survivor)", func(v bool) {
		f.DetectEquivalent = config.DetectEquivalentFlag{Set: true, Value: v}
	})
	// fs.Func (like boolFlagFunc) lets us distinguish "user set
	// --checkpoint-interval" from "user did not pass the flag", which
	// (*Config).ApplyFlags needs because 0 is a valid value (disable) and
	// can't be told apart from the unset zero value otherwise.
	fs.Func("checkpoint-interval", fmt.Sprintf("how often to flush completed mutant outcomes to the cache mid-run so a hard kill (OOM, CI timeout, SIGKILL) loses at most this much progress; 0 disables (default: %s)", config.DefaultCheckpointInterval), func(s string) error {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("--checkpoint-interval: %w", err)
		}
		if d < 0 {
			return fmt.Errorf("--checkpoint-interval must be >= 0, got %s", d)
		}
		f.CheckpointInterval = config.CheckpointIntervalFlag{Set: true, Value: d}
		return nil
	})
	fs.StringVar(&f.CoverPkg, "coverpkg", "", "coverage package pattern")
	fs.StringVar(&f.Tags, "tags", "", "comma-separated build tags forwarded as -tags to the inner go list/go test (gremlins-compat)")
	// fs.Func rather than StringVar so repeated --test-flags accumulate
	// instead of the last one silently winning; a user building the value
	// up across a wrapper script and a CI invocation expects both to apply.
	// The back-quoted `flags` is deliberate and must come first: flag's
	// UnquoteUsage takes the first back-quoted token as the value
	// placeholder shown in --help. Any other backticks in this string
	// would be consumed instead, printing e.g. "-test-flags go test".
	fs.Func("test-flags", "`flags` forwarded verbatim to the inner go test runs (per-mutant, coverage, baseline) and to nothing else; whitespace-separated, repeatable. Placed after the package argument; use -args within flags for test-binary names that collide with go test flags (see README). Use to trade mutation fidelity for speed on property-based suites, e.g. --test-flags=-short", func(s string) error {
		testFlags = append(testFlags, s)
		return nil
	})
	fs.StringVar(&f.Output, "output", "", "JSON report path")
	fs.StringVar(&f.Output, "o", "", "JSON report path (shorthand)")
	fs.StringVar(&o.configPath, "config", ".gomutants.yml", "config file path")
	fs.StringVar(&f.Disable, "disable", "", "comma-separated mutator types to disable")
	fs.StringVar(&f.Only, "only", "", "comma-separated mutator types to run (disables all others)")
	fs.StringVar(&f.ExcludeFiles, "exclude-files", "", "comma-separated regexps; skip mutating production files whose module-relative path matches any (e.g. \"vendor/,_gen\\\\.go$\")")
	fs.StringVar(&f.ExcludeCalls, "exclude-calls", "", "comma-separated selector globs; suppress mutants inside calls whose selector matches any (e.g. \"log.Print*,*.Debug\"). Extends the built-in stdlib-logging set")
	// Default on, so the explicit =false has to be distinguishable.
	boolFlagFunc(fs, "exclude-calls-defaults", "apply the built-in --exclude-calls set for Go's standard-library logging (default: true; pass =false to narrow or replace it)", func(v bool) {
		f.ExcludeCallsDefaults = config.ExcludeCallsDefaultsFlag{Set: true, Value: v}
	})
	fs.StringVar(&f.ChangedSince, "changed-since", "", "only test mutants on lines changed vs git ref (e.g. main, HEAD~1)")
	// The back-quoted `id` is the value placeholder flag's UnquoteUsage
	// prints in --help; see the --test-flags comment above for why its
	// position within the string matters.
	fs.StringVar(&f.RunMutantID, "run-mutant-id", "", "run only the mutant with this stable `id` (the id field of a JSON report entry; a unique prefix is accepted). Skips the incremental cache for that mutant so the verdict is always freshly measured")
	fs.StringVar(&f.Cache, "cache", "", "path to incremental-analysis cache file; skips mutants whose source package and covering tests are byte-identical to the cached run. Default .gomutants-cache.json. Pass --cache=off to disable")
	fs.StringVar(&o.annotations, "annotations", "", "emit annotations for surviving mutants (values: github)")
	fs.StringVar(&o.strykerOutput, "stryker-output", "", "also write a Stryker mutation-testing-elements report at this path (HTML viewer / dashboard)")
	fs.StringVar(&o.htmlOutput, "html-output", "", "also write a self-contained interactive HTML mutation report at this path (Stryker mutation-testing-elements viewer, no network deps)")
	fs.Float64Var(&o.thresholdEfficacy, "threshold-efficacy", 0, "minimum test efficacy %% (KILLED/(KILLED+LIVED)); exit 10 if not met. 0 disables (gremlins-compat)")
	fs.Float64Var(&o.thresholdMcover, "threshold-mcover", 0, "minimum mutant coverage %% ((KILLED+LIVED)/(KILLED+LIVED+NOT_COVERED)); exit 11 if not met. 0 disables (gremlins-compat)")
	fs.BoolVar(&f.DryRun, "dry-run", false, "list mutants without testing")
	fs.BoolVar(&f.Verbose, "verbose", false, "show each mutant as tested")
	fs.BoolVar(&f.Verbose, "v", false, "verbose (shorthand)")
	fs.BoolVar(&f.Quiet, "quiet", false, "suppress header, phase lines, and per-mutant progress; only the final summary prints (warnings still go to stderr)")
	fs.BoolVar(&f.Quiet, "q", false, "quiet (shorthand)")
	fs.BoolVar(&f.Integration, "integration", false, "cross-package mode: route each mutant to covering tests in any package that imports it (widens coverage + the per-test build to the reverse-dependency closure). Manages -coverpkg itself; passing --coverpkg too is an error")
	fs.BoolVar(&o.showVersion, "version", false, "print version and exit")
	fs.BoolVar(&o.listMutators, "list-mutators", false, "print every mutator type with its description and example, then exit")

	if err := fs.Parse(args); err != nil {
		return nil, usageError(err)
	}
	f.TestFlags = strings.Join(testFlags, " ")

	if f.TestCPU < 0 {
		return nil, usageErrorf("--test-cpu must be >= 0, got %d", f.TestCPU)
	}

	if f.Quiet && f.Verbose {
		return nil, usageErrorf("--quiet and --verbose cannot be used together")
	}

	switch o.annotations {
	case "", "github":
	default:
		return nil, usageErrorf("--annotations=%q not recognized (supported: github)", o.annotations)
	}

	o.packages = fs.Args()
	if len(o.packages) == 0 {
		o.packages = []string{"./..."}
	}
	return o, nil
}

// runFilters are the user-supplied exclusion patterns, compiled once the
// config file and the flags have been merged.
type runFilters struct {
	excluder     *discover.Excluder
	callExcluder *discover.CallExcluder
}

// loadConfig merges the config file under the parsed flags and rejects
// combinations that are only invalid once both sources are known. Every
// failure here is a usage error: nothing has touched the project yet.
func loadConfig(opts *cliOptions) (config.Config, runFilters, error) {
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return cfg, runFilters{}, usageError(err)
	}
	cfg.ApplyFlags(opts.flags)
	cfg.ResolveCache()

	// Integration mode computes -coverpkg from the target packages so that
	// tests in importing packages record coverage on the mutated code. An
	// explicit --coverpkg would conflict with that computed value, so refuse
	// rather than silently pick one.
	if cfg.Integration && cfg.CoverPkg != "" {
		return cfg, runFilters{}, usageErrorf("--integration manages -coverpkg automatically; do not also pass --coverpkg")
	}

	// --run-mutant-id exists to answer "did the test I just wrote kill this
	// mutant?" from an exit code, and --dry-run returns before anything is
	// compiled or tested. The pair would print the mutant and exit 0 — which
	// a script reads as a kill. Checked after ApplyFlags because dry-run is
	// also a config-file key: a committed `dry-run: true` is invisible to the
	// caller and cannot be turned back off from the command line.
	if cfg.RunMutantID != "" && cfg.DryRun {
		return cfg, runFilters{}, usageErrorf("--run-mutant-id cannot be used with --dry-run: a dry run tests nothing, so there is no verdict to report")
	}

	// Checked after ApplyFlags so a value from .gomutants.yml is screened
	// too, not just the CLI one.
	if err := checkTestFlags(cfg.TestFlagFields()); err != nil {
		return cfg, runFilters{}, usageError(err)
	}

	// Compile user-supplied patterns before any project or Go-tool work.
	// These are configuration errors even when the selected target also
	// happens to be unbuildable, so configuration must win that race.
	excluder, err := discover.NewExcluder(cfg.ExcludeFiles)
	if err != nil {
		return cfg, runFilters{}, usageErrorf("--exclude-files: %w", err)
	}
	callExcluder, err := discover.NewCallExcluder(cfg.ResolvedExcludeCalls())
	if err != nil {
		return cfg, runFilters{}, usageErrorf("--exclude-calls: %w", err)
	}

	// Periodic checkpointing rides on the cache file; with --cache=off
	// there is nothing to flush. Warn rather than silently ignore so a
	// user who set --checkpoint-interval isn't misled about durability.
	if cfg.Cache == "" && opts.flags.CheckpointInterval.Set {
		fmt.Fprintln(stderr, "gomutants: --checkpoint-interval ignored: --cache is off")
	}

	return cfg, runFilters{excluder: excluder, callExcluder: callExcluder}, nil
}
