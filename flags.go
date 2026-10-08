package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/szhekpisov/gomutants/internal/config"
	"github.com/szhekpisov/gomutants/internal/discover"
)

// cliOptions is the parsed command line. flags is merged over the config
// file by (*Config).ApplyFlags; the remaining fields are CLI-only and never
// come from .gomutants.yml.
type cliOptions struct {
	flags      config.Flags
	configPath string
	// packages is the positional package patterns, defaulted to ./... .
	packages []string

	strykerOutput     string
	htmlOutput        string
	annotations       string
	thresholdEfficacy float64
	thresholdMcover   float64
	showVersion       bool
	listMutators      bool
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

	var (
		workers            int
		testCPU            int
		timeoutCoefficient int
		timeoutMargin      float64
		timeoutMin         time.Duration
		adaptiveTimeout    config.AdaptiveTimeoutFlag
		detectEquivalent   config.DetectEquivalentFlag
		checkpointInterval config.CheckpointIntervalFlag

		excludeCallsDefaults config.ExcludeCallsDefaultsFlag

		coverPkg          string
		tags              string
		testFlags         []string
		output            string
		configPath        string
		disable           string
		only              string
		excludeFiles      string
		excludeCalls      string
		changedSince      string
		runMutantID       string
		cachePath         string
		annotations       string
		strykerOutput     string
		htmlOutput        string
		thresholdEfficacy float64
		thresholdMcover   float64
		dryRun            bool
		verbose           bool
		quiet             bool
		integration       bool
		showVersion       bool
		listMutators      bool
	)

	fs.IntVar(&workers, "workers", 0, "parallel workers (default: NumCPU)")
	fs.IntVar(&workers, "w", 0, "parallel workers (shorthand)")
	fs.IntVar(&testCPU, "test-cpu", 0, "value passed to inner go test -cpu per mutant (0 omits the flag; go test then uses GOMAXPROCS)")
	fs.IntVar(&timeoutCoefficient, "timeout-coefficient", 0, "multiply baseline test time for the global timeout ceiling (default: 10)")
	fs.Float64Var(&timeoutMargin, "timeout-margin", 0, fmt.Sprintf("scale per-test sums into the per-mutant adaptive timeout (default: %g)", config.DefaultTimeoutMargin))
	fs.DurationVar(&timeoutMin, "timeout-min", 0, fmt.Sprintf("floor for the per-mutant adaptive timeout (default: %s)", config.DefaultTimeoutMin))
	// BoolFunc lets us distinguish "user set --adaptive-timeout=false"
	// from "user did not pass the flag" — the merge layer in
	// (*Config).ApplyFlags relies on the .Set bit to override YAML.
	fs.BoolFunc("adaptive-timeout", "use per-test durations to size each mutant's timeout (default: true; pass =false to disable)", func(s string) error {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("--adaptive-timeout: %w", err)
		}
		adaptiveTimeout = config.AdaptiveTimeoutFlag{Set: true, Value: v}
		return nil
	})
	// Opt-in TCE pass. BoolFunc (like --adaptive-timeout) so ApplyFlags can
	// tell "not provided" from an explicit value via the .Set bit.
	fs.BoolFunc("detect-equivalent", "after testing, compile each surviving mutant with -gcflags=-S and mark it EQUIVALENT when the generated assembly is identical to the original (Trivial Compiler Equivalence; default false, adds one package compile per survivor)", func(s string) error {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("--detect-equivalent: %w", err)
		}
		detectEquivalent = config.DetectEquivalentFlag{Set: true, Value: v}
		return nil
	})
	// fs.Func (like the BoolFunc above) lets us distinguish "user set
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
		checkpointInterval = config.CheckpointIntervalFlag{Set: true, Value: d}
		return nil
	})
	fs.StringVar(&coverPkg, "coverpkg", "", "coverage package pattern")
	fs.StringVar(&tags, "tags", "", "comma-separated build tags forwarded as -tags to the inner go list/go test (gremlins-compat)")
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
	fs.StringVar(&output, "output", "", "JSON report path")
	fs.StringVar(&output, "o", "", "JSON report path (shorthand)")
	fs.StringVar(&configPath, "config", ".gomutants.yml", "config file path")
	fs.StringVar(&disable, "disable", "", "comma-separated mutator types to disable")
	fs.StringVar(&only, "only", "", "comma-separated mutator types to run (disables all others)")
	fs.StringVar(&excludeFiles, "exclude-files", "", "comma-separated regexps; skip mutating production files whose module-relative path matches any (e.g. \"vendor/,_gen\\\\.go$\")")
	fs.StringVar(&excludeCalls, "exclude-calls", "", "comma-separated selector globs; suppress mutants inside calls whose selector matches any (e.g. \"log.Print*,*.Debug\"). Extends the built-in stdlib-logging set")
	// BoolFunc (like --adaptive-timeout) so ApplyFlags can tell "not
	// provided" from an explicit value via the .Set bit — this default is
	// on, so the explicit =false has to be distinguishable.
	fs.BoolFunc("exclude-calls-defaults", "apply the built-in --exclude-calls set for Go's standard-library logging (default: true; pass =false to narrow or replace it)", func(s string) error {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("--exclude-calls-defaults: %w", err)
		}
		excludeCallsDefaults = config.ExcludeCallsDefaultsFlag{Set: true, Value: v}
		return nil
	})
	fs.StringVar(&changedSince, "changed-since", "", "only test mutants on lines changed vs git ref (e.g. main, HEAD~1)")
	// The back-quoted `id` is the value placeholder flag's UnquoteUsage
	// prints in --help; see the --test-flags comment above for why its
	// position within the string matters.
	fs.StringVar(&runMutantID, "run-mutant-id", "", "run only the mutant with this stable `id` (the id field of a JSON report entry; a unique prefix is accepted). Skips the incremental cache for that mutant so the verdict is always freshly measured")
	fs.StringVar(&cachePath, "cache", "", "path to incremental-analysis cache file; skips mutants whose source package and covering tests are byte-identical to the cached run. Default .gomutants-cache.json. Pass --cache=off to disable")
	fs.StringVar(&annotations, "annotations", "", "emit annotations for surviving mutants (values: github)")
	fs.StringVar(&strykerOutput, "stryker-output", "", "also write a Stryker mutation-testing-elements report at this path (HTML viewer / dashboard)")
	fs.StringVar(&htmlOutput, "html-output", "", "also write a self-contained interactive HTML mutation report at this path (Stryker mutation-testing-elements viewer, no network deps)")
	fs.Float64Var(&thresholdEfficacy, "threshold-efficacy", 0, "minimum test efficacy %% (KILLED/(KILLED+LIVED)); exit 10 if not met. 0 disables (gremlins-compat)")
	fs.Float64Var(&thresholdMcover, "threshold-mcover", 0, "minimum mutant coverage %% ((KILLED+LIVED)/(KILLED+LIVED+NOT_COVERED)); exit 11 if not met. 0 disables (gremlins-compat)")
	fs.BoolVar(&dryRun, "dry-run", false, "list mutants without testing")
	fs.BoolVar(&verbose, "verbose", false, "show each mutant as tested")
	fs.BoolVar(&verbose, "v", false, "verbose (shorthand)")
	fs.BoolVar(&quiet, "quiet", false, "suppress header, phase lines, and per-mutant progress; only the final summary prints (warnings still go to stderr)")
	fs.BoolVar(&quiet, "q", false, "quiet (shorthand)")
	fs.BoolVar(&integration, "integration", false, "cross-package mode: route each mutant to covering tests in any package that imports it (widens coverage + the per-test build to the reverse-dependency closure). Manages -coverpkg itself; passing --coverpkg too is an error")
	fs.BoolVar(&showVersion, "version", false, "print version and exit")
	fs.BoolVar(&listMutators, "list-mutators", false, "print every mutator type with its description and example, then exit")

	if err := fs.Parse(args); err != nil {
		return nil, usageError(err)
	}

	if testCPU < 0 {
		return nil, usageErrorf("--test-cpu must be >= 0, got %d", testCPU)
	}

	if quiet && verbose {
		return nil, usageErrorf("--quiet and --verbose cannot be used together")
	}

	switch annotations {
	case "", "github":
	default:
		return nil, usageErrorf("--annotations=%q not recognized (supported: github)", annotations)
	}

	packages := fs.Args()
	if len(packages) == 0 {
		packages = []string{"./..."}
	}

	return &cliOptions{
		flags: config.Flags{
			Workers:            workers,
			TestCPU:            testCPU,
			TimeoutCoefficient: timeoutCoefficient,
			TimeoutMargin:      timeoutMargin,
			TimeoutMin:         timeoutMin,
			AdaptiveTimeout:    adaptiveTimeout,
			DetectEquivalent:   detectEquivalent,
			CheckpointInterval: checkpointInterval,
			CoverPkg:           coverPkg,
			Tags:               tags,
			TestFlags:          strings.Join(testFlags, " "),
			Output:             output,
			Disable:            disable,
			Only:               only,
			ExcludeFiles:       excludeFiles,
			ExcludeCalls:       excludeCalls,
			ChangedSince:       changedSince,
			RunMutantID:        runMutantID,
			Cache:              cachePath,
			DryRun:             dryRun,
			Verbose:            verbose,
			Quiet:              quiet,
			Integration:        integration,

			ExcludeCallsDefaults: excludeCallsDefaults,
		},
		configPath:        configPath,
		packages:          packages,
		strykerOutput:     strykerOutput,
		htmlOutput:        htmlOutput,
		annotations:       annotations,
		thresholdEfficacy: thresholdEfficacy,
		thresholdMcover:   thresholdMcover,
		showVersion:       showVersion,
		listMutators:      listMutators,
	}, nil
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
