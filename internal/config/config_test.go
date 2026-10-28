package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	if cfg.Workers != DefaultWorkers() {
		t.Errorf("Workers=%d, want %d", cfg.Workers, DefaultWorkers())
	}
	if cfg.TimeoutCoefficient != 10 {
		t.Errorf("TimeoutCoefficient=%d, want 10", cfg.TimeoutCoefficient)
	}
	if cfg.Output != "mutation-report.json" {
		t.Errorf("Output=%q, want %q", cfg.Output, "mutation-report.json")
	}
	if cfg.CheckpointInterval != DefaultCheckpointInterval {
		t.Errorf("CheckpointInterval=%v, want %v", cfg.CheckpointInterval, DefaultCheckpointInterval)
	}
}

func TestLoadMissing(t *testing.T) {
	cfg, err := Load("/nonexistent/.gomutants.yml")
	if err != nil {
		t.Fatalf("Load of missing file should not error: %v", err)
	}
	if cfg.Workers != DefaultWorkers() {
		t.Errorf("Workers=%d, want default %d", cfg.Workers, DefaultWorkers())
	}
}

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gomutants.yml")

	yaml := `workers: 4
test-cpu: 2
timeout-coefficient: 20
coverpkg: "./pkg/..."
test-flags: "-rapid.checks=20 -short"
output: report.json
dry-run: true
verbose: true
quiet: true
disable:
  - BRANCH_IF
only:
  - ARITHMETIC_BASE
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Workers != 4 {
		t.Errorf("Workers=%d, want 4", cfg.Workers)
	}
	if cfg.TestCPU != 2 {
		t.Errorf("TestCPU=%d, want 2", cfg.TestCPU)
	}
	if cfg.TimeoutCoefficient != 20 {
		t.Errorf("TimeoutCoefficient=%d, want 20", cfg.TimeoutCoefficient)
	}
	if cfg.CoverPkg != "./pkg/..." {
		t.Errorf("CoverPkg=%q, want %q", cfg.CoverPkg, "./pkg/...")
	}
	if cfg.TestFlags != "-rapid.checks=20 -short" {
		t.Errorf("TestFlags=%q, want %q", cfg.TestFlags, "-rapid.checks=20 -short")
	}
	if cfg.Output != "report.json" {
		t.Errorf("Output=%q, want %q", cfg.Output, "report.json")
	}
	if !cfg.DryRun {
		t.Error("DryRun should be true")
	}
	if !cfg.Verbose {
		t.Error("Verbose should be true")
	}
	if !cfg.Quiet {
		t.Error("Quiet should be true")
	}
	if len(cfg.Disable) != 1 || cfg.Disable[0] != "BRANCH_IF" {
		t.Errorf("Disable=%v, want [BRANCH_IF]", cfg.Disable)
	}
	if len(cfg.Only) != 1 || cfg.Only[0] != "ARITHMETIC_BASE" {
		t.Errorf("Only=%v, want [ARITHMETIC_BASE]", cfg.Only)
	}
}

func TestLoadZeroValuesGetDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gomutants.yml")
	// Explicitly set fields to zero — should fall back to defaults.
	yaml := "workers: 0\ntimeout-coefficient: 0\noutput: \"\"\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Workers != DefaultWorkers() {
		t.Errorf("Workers=%d, want default %d", cfg.Workers, DefaultWorkers())
	}
	if cfg.TimeoutCoefficient != 10 {
		t.Errorf("TimeoutCoefficient=%d, want default 10", cfg.TimeoutCoefficient)
	}
	if cfg.Output != "mutation-report.json" {
		t.Errorf("Output=%q, want default", cfg.Output)
	}
}

func TestLoadCheckpointInterval(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"absent keeps default", "workers: 4\n", DefaultCheckpointInterval},
		{"explicit duration parses", "checkpoint-interval: 30s\n", 30 * time.Second},
		{"zero survives as disable", "checkpoint-interval: 0s\n", 0},
		{"negative reverts to default", "checkpoint-interval: -5s\n", DefaultCheckpointInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".gomutants.yml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CheckpointInterval != tt.want {
				t.Errorf("CheckpointInterval=%v, want %v", cfg.CheckpointInterval, tt.want)
			}
		})
	}
}

// TestApplyFlagsCheckpointIntervalZeroOverride pins the three-state merge:
// an explicit --checkpoint-interval=0 must override a non-zero YAML value
// (a plain "non-zero means user-set" check would silently drop it).
func TestApplyFlagsCheckpointIntervalZeroOverride(t *testing.T) {
	cfg := Default()
	cfg.CheckpointInterval = 30 * time.Second

	cfg.ApplyFlags(Flags{CheckpointInterval: CheckpointIntervalFlag{Set: true, Value: 0}})

	if cfg.CheckpointInterval != 0 {
		t.Errorf("CheckpointInterval=%v, want 0 (explicit --checkpoint-interval=0 must override YAML)", cfg.CheckpointInterval)
	}
}

// TestApplyFlagsIntegration pins the --integration toggle merge: passing the
// flag sets Integration; not passing it leaves the value untouched. Kills
// BRANCH_IF and STATEMENT_REMOVE on the `if f.Integration { c.Integration = true }`
// merge.
func TestApplyFlagsIntegration(t *testing.T) {
	cfg := Default()
	if cfg.Integration {
		t.Fatal("default Integration should be false")
	}

	cfg.ApplyFlags(Flags{Integration: true})
	if !cfg.Integration {
		t.Error("ApplyFlags(Integration: true) must set Integration; the merge guard or assignment was dropped")
	}

	// Omitting the flag must not turn Integration on from its zero value.
	other := Default()
	other.ApplyFlags(Flags{})
	if other.Integration {
		t.Error("ApplyFlags without Integration must leave it false")
	}
}

func TestLoadReadError(t *testing.T) {
	// Use a directory path as config file — will cause a read error (not IsNotExist).
	dir := t.TempDir()
	_, err := Load(dir) // dir exists but is a directory, not a file.
	if err == nil {
		t.Fatal("expected error when reading a directory as config file")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gomutants.yml")
	if err := os.WriteFile(path, []byte("{{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestApplyFlags(t *testing.T) {
	cfg := Default()

	cfg.ApplyFlags(Flags{
		Workers:            8,
		TestCPU:            4,
		TimeoutCoefficient: 15,
		TimeoutMargin:      4.5,
		TimeoutMin:         5 * time.Second,
		AdaptiveTimeout:    AdaptiveTimeoutFlag{Set: true, Value: false},
		CheckpointInterval: CheckpointIntervalFlag{Set: true, Value: 30 * time.Second},
		CoverPkg:           "./pkg/...",
		Tags:               "integration,debug",
		TestFlags:          "-rapid.checks=20",
		Output:             "out.json",
		Disable:            "BRANCH_IF,BRANCH_ELSE",
		Only:               "ARITHMETIC_BASE",
		ExcludeFiles:       "vendor/, _gen\\.go",
		ChangedSince:       "main",
		RunMutantID:        "a.go:F:ARITHMETIC_BASE#1",
		Cache:              "cache.json",
		DryRun:             true,
		Verbose:            true,
		Quiet:              true,
	})

	if cfg.Workers != 8 {
		t.Errorf("Workers=%d, want 8", cfg.Workers)
	}
	if cfg.TestCPU != 4 {
		t.Errorf("TestCPU=%d, want 4", cfg.TestCPU)
	}
	if cfg.TimeoutCoefficient != 15 {
		t.Errorf("TimeoutCoefficient=%d, want 15", cfg.TimeoutCoefficient)
	}
	if cfg.TimeoutMargin != 4.5 {
		t.Errorf("TimeoutMargin=%v, want 4.5", cfg.TimeoutMargin)
	}
	if cfg.TimeoutMin != 5*time.Second {
		t.Errorf("TimeoutMin=%v, want 5s", cfg.TimeoutMin)
	}
	if cfg.AdaptiveTimeoutEnabled() {
		t.Errorf("AdaptiveTimeoutEnabled=true, want false (CLI override)")
	}
	if cfg.CheckpointInterval != 30*time.Second {
		t.Errorf("CheckpointInterval=%v, want 30s", cfg.CheckpointInterval)
	}
	if cfg.CoverPkg != "./pkg/..." {
		t.Errorf("CoverPkg=%q", cfg.CoverPkg)
	}
	if cfg.Tags != "integration,debug" {
		t.Errorf("Tags=%q, want integration,debug", cfg.Tags)
	}
	if cfg.TestFlags != "-rapid.checks=20" {
		t.Errorf("TestFlags=%q, want -rapid.checks=20", cfg.TestFlags)
	}
	if cfg.Output != "out.json" {
		t.Errorf("Output=%q", cfg.Output)
	}
	if len(cfg.Disable) != 2 {
		t.Errorf("Disable=%v, want 2 entries", cfg.Disable)
	}
	if len(cfg.Only) != 1 || cfg.Only[0] != "ARITHMETIC_BASE" {
		t.Errorf("Only=%v", cfg.Only)
	}
	// Comma-split with surrounding whitespace trimmed, mirroring Disable/Only.
	if len(cfg.ExcludeFiles) != 2 || cfg.ExcludeFiles[0] != "vendor/" || cfg.ExcludeFiles[1] != "_gen\\.go" {
		t.Errorf("ExcludeFiles=%v, want [vendor/ _gen\\.go]", cfg.ExcludeFiles)
	}
	if cfg.ChangedSince != "main" {
		t.Errorf("ChangedSince=%q, want main", cfg.ChangedSince)
	}
	if cfg.RunMutantID != "a.go:F:ARITHMETIC_BASE#1" {
		t.Errorf("RunMutantID=%q", cfg.RunMutantID)
	}
	if cfg.Cache != "cache.json" {
		t.Errorf("Cache=%q, want cache.json", cfg.Cache)
	}
	if !cfg.DryRun {
		t.Error("DryRun should be true")
	}
	if !cfg.Verbose {
		t.Error("Verbose should be true")
	}
	if !cfg.Quiet {
		t.Error("Quiet should be true")
	}
}

func TestApplyFlagsZeroValuesNoOverride(t *testing.T) {
	cfg := Default()
	cfg.TestCPU = 7
	cfg.Tags = "integration" // e.g. set via YAML
	cfg.TestFlags = "-short" // e.g. set via YAML
	orig := cfg

	// Zero/empty values should not override defaults.
	cfg.ApplyFlags(Flags{})

	if cfg.Workers != orig.Workers {
		t.Errorf("Workers changed from %d to %d", orig.Workers, cfg.Workers)
	}
	if cfg.TestCPU != orig.TestCPU {
		t.Errorf("TestCPU changed from %d to %d", orig.TestCPU, cfg.TestCPU)
	}
	if cfg.TimeoutCoefficient != orig.TimeoutCoefficient {
		t.Errorf("TimeoutCoefficient changed")
	}
	// Pin the new adaptive-timeout knobs against CONDITIONALS_BOUNDARY
	// (`> 0` → `>= 0`) on their respective ApplyFlags guards. Without
	// these checks a `>= 0` mutation would silently overwrite the
	// default with the caller's zero.
	if cfg.TimeoutMargin != orig.TimeoutMargin {
		t.Errorf("TimeoutMargin changed from %v to %v — CONDITIONALS_BOUNDARY on `> 0` would let zero override the default", orig.TimeoutMargin, cfg.TimeoutMargin)
	}
	if cfg.TimeoutMin != orig.TimeoutMin {
		t.Errorf("TimeoutMin changed from %v to %v", orig.TimeoutMin, cfg.TimeoutMin)
	}
	if cfg.AdaptiveTimeout != orig.AdaptiveTimeout {
		t.Errorf("AdaptiveTimeout pointer changed; AdaptiveTimeoutFlag{Set:false} must be a no-op")
	}
	if cfg.CheckpointInterval != orig.CheckpointInterval {
		t.Errorf("CheckpointInterval changed from %v to %v; CheckpointIntervalFlag{Set:false} must be a no-op", orig.CheckpointInterval, cfg.CheckpointInterval)
	}
	if cfg.Output != orig.Output {
		t.Errorf("Output changed")
	}
	if cfg.Tags != orig.Tags {
		t.Errorf("Tags changed from %q to %q; an empty --tags must not clobber a YAML value", orig.Tags, cfg.Tags)
	}
	if cfg.TestFlags != orig.TestFlags {
		t.Errorf("TestFlags changed from %q to %q; an empty --test-flags must not clobber a YAML value", orig.TestFlags, cfg.TestFlags)
	}
}

// TestTestFlagFields pins the split contract every consumer depends on:
// the argv fragments appended to the inner `go test`. Unset must yield
// nothing appendable (not a one-element slice holding ""), which would
// otherwise reach `go test` as an empty argument and be read as a package
// pattern. Runs of whitespace collapse, and repeated --test-flags values
// (joined with a space by the CLI layer) come back in order.
func TestTestFlagFields(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"unset", "", nil},
		{"whitespace only", "   ", nil},
		{"single", "-short", []string{"-short"}},
		{"multiple", "-short -rapid.checks=20", []string{"-short", "-rapid.checks=20"}},
		{"collapses runs", "  -short   -race  ", []string{"-short", "-race"}},
		{"preserves order", "-a -b -c", []string{"-a", "-b", "-c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{TestFlags: tc.in}
			got := cfg.TestFlagFields()
			if !slices.Equal(got, tc.want) {
				t.Errorf("TestFlagFields(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestCanonicalTestFlags pins the form cache identity is computed from.
// Whitespace never reaches the runner and can be normalized, but order is
// preserved because arbitrary test-binary flags may interact while they
// are parsed. The unset case must stay "" so a flag-less run matches a
// flag-less cache.
func TestCanonicalTestFlags(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unset", "", ""},
		{"whitespace only", "   ", ""},
		{"already canonical", "-short", "-short"},
		{"leading and trailing space", "  -short  ", "-short"},
		{"collapses runs and preserves order", "-short    -race", "-short -race"},
		{"tabs and newlines", "-short\t-race\n-count=2", "-short -race -count=2"},
		// A detached value has to stay beside the flag it belongs to.
		{"detached value", "-gcflags  all=-N   -short", "-gcflags all=-N -short"},
		// Go takes the last occurrence, so these two orderings are different
		// runs and must keep different identities.
		{"repeated flag keeps order", "-count=2 -count=1", "-count=2 -count=1"},
		{"repeated flag other order", "-count=1 -count=2", "-count=1 -count=2"},
		// The `-test.` alias names the same flag, so its order also matters.
		{"aliased repeat keeps order", "-short -test.short", "-short -test.short"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{TestFlags: tc.in}
			if got := cfg.CanonicalTestFlags(); got != tc.want {
				t.Errorf("CanonicalTestFlags(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// The property the cache gate actually relies on: equal field slices
	// imply an equal canonical string, and different flags stay different.
	spaced := &Config{TestFlags: " -short   -race "}
	tight := &Config{TestFlags: "-short -race"}
	fewer := &Config{TestFlags: "-short"}
	if spaced.CanonicalTestFlags() != tight.CanonicalTestFlags() {
		t.Errorf("whitespace-only differences must canonicalize equal, got %q vs %q",
			spaced.CanonicalTestFlags(), tight.CanonicalTestFlags())
	}
	// Distinct custom flag names can still share state. Conservatively keep
	// every ordering separate instead of assuming that names commute: a test
	// binary can register both of these names against the same variable.
	cheapThenFull := (&Config{TestFlags: "-cheap=20 -full=100"}).CanonicalTestFlags()
	fullThenCheap := (&Config{TestFlags: "-full=100 -cheap=20"}).CanonicalTestFlags()
	if cheapThenFull == fullThenCheap {
		t.Errorf("order-only differences must keep distinct cache identities, both gave %q", cheapThenFull)
	}
	// A last-one-wins pair must not collapse either, or `-count=1 -count=2`
	// would replay the verdicts of a run that used the opposite value.
	lastWins := (&Config{TestFlags: "-count=1 -count=2"}).CanonicalTestFlags()
	firstWins := (&Config{TestFlags: "-count=2 -count=1"}).CanonicalTestFlags()
	if lastWins == firstWins {
		t.Errorf("repeated-flag orderings run differently and must not share a cache key, both gave %q", lastWins)
	}
	if spaced.CanonicalTestFlags() == fewer.CanonicalTestFlags() {
		t.Error("distinct flag sets must not canonicalize equal; the cache gate would stop discriminating")
	}
}

// TestFlagName pins the parse the CLI's managed-flag guard depends on. The
// `test.` case is the load-bearing one:
// `go test` forwards `-test.run` to the test binary, where it beats the
// `-run` gomutants set, so a guard that reads it as a flag named
// "test.run" would wave through exactly the override it exists to stop.
// The ok=false cases matter just as much — a value that happens to spell
// a managed name must not be mistaken for the flag itself.
func TestFlagName(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"-short", "short", true},
		{"--short", "short", true},
		{"-count=2", "count", true},
		{"--count=2", "count", true},
		{"-rapid.checks=20", "rapid.checks", true}, // only a leading `test.` is stripped
		{"-test.run=TestFoo", "run", true},
		{"--test.run=TestFoo", "run", true},
		{"-test.coverprofile=/tmp/c.out", "coverprofile", true},
		{"-testdata=x", "testdata", true}, // `test.` is a prefix with a dot, not `test`
		{"all=-N", "", false},             // a detached value
		{"./...", "", false},              // a package pattern
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, ok := FlagName(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("FlagName(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if name != tc.wantName {
				t.Errorf("FlagName(%q) = %q, want %q", tc.in, name, tc.wantName)
			}
		})
	}
}

// TestAdaptiveTimeoutEnabledNilDefault kills BRANCH_IF on the
// `if c.AdaptiveTimeout == nil { return true }` body. Without that
// early return the function dereferences a nil pointer and panics; the
// recover-then-fail wrapper distinguishes the panic from a clean
// `false` return that some mutations might also produce.
func TestAdaptiveTimeoutEnabledNilDefault(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("AdaptiveTimeoutEnabled panicked on nil pointer — BRANCH_IF on the nil-guard body lets execution fall through to *c.AdaptiveTimeout: %v", r)
		}
	}()
	c := Config{AdaptiveTimeout: nil}
	if !c.AdaptiveTimeoutEnabled() {
		t.Errorf("nil AdaptiveTimeout must default to true")
	}
}

// TestAdaptiveTimeoutEnabledExplicitFalse pins the *c.AdaptiveTimeout
// dereference path so a STATEMENT_REMOVE on the deref still has a
// targeted assertion.
func TestAdaptiveTimeoutEnabledExplicitFalse(t *testing.T) {
	f := false
	c := Config{AdaptiveTimeout: &f}
	if c.AdaptiveTimeoutEnabled() {
		t.Errorf("explicit false must propagate through AdaptiveTimeoutEnabled")
	}
}

// TestDefaultTimeoutMinValue pins DefaultTimeoutMin against
// ARITHMETIC_BASE (`*` → `/` collapses 2*time.Second to 0). Asserting
// the literal value ensures any arithmetic mutation on the constant
// shows up here.
func TestDefaultTimeoutMinValue(t *testing.T) {
	if DefaultTimeoutMin != 2*time.Second {
		t.Errorf("DefaultTimeoutMin = %v, want 2s — ARITHMETIC_BASE on `2 * time.Second` would change this", DefaultTimeoutMin)
	}
}

// TestDefaultCheckpointIntervalValue pins DefaultCheckpointInterval
// against ARITHMETIC_BASE on its initializer. `10 * time.Second` mutated
// to `10 / time.Second` collapses to 0 (integer division), which silently
// disables periodic checkpointing without any other test noticing because
// every other assertion compares against the const symbolically.
func TestDefaultCheckpointIntervalValue(t *testing.T) {
	if DefaultCheckpointInterval != 10*time.Second {
		t.Errorf("DefaultCheckpointInterval = %v, want 10s — ARITHMETIC_BASE on `10 * time.Second` would change this", DefaultCheckpointInterval)
	}
}

// TestLoadAppliesDefaultsForZeroAdaptiveFields kills BRANCH_IF and
// CONDITIONALS_NEGATION on the two `if cfg.TimeoutMargin == 0` /
// `cfg.TimeoutMin == 0` defaulting blocks in Load. We write a YAML
// that explicitly sets both to zero and assert the defaults take over.
// Without the guards (or with their condition negated) the user would
// run with TimeoutMargin=0 → per-mutant timeouts always clamp to Min,
// hiding genuine slowdowns.
func TestLoadAppliesDefaultsForZeroAdaptiveFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gomutants.yml")
	// timeout-min is time.Duration; YAML needs a string-compatible zero.
	// "0s" is the zero duration so the defaulting block in Load() must
	// still kick in (its trigger is the int64 zero, not the YAML token).
	body := []byte("timeout-margin: 0\ntimeout-min: 0s\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TimeoutMargin != DefaultTimeoutMargin {
		t.Errorf("TimeoutMargin=%v, want default %v — BRANCH_IF / CONDITIONALS_NEGATION on the zero-guard would skip the default", cfg.TimeoutMargin, DefaultTimeoutMargin)
	}
	if cfg.TimeoutMin != DefaultTimeoutMin {
		t.Errorf("TimeoutMin=%v, want default %v", cfg.TimeoutMin, DefaultTimeoutMin)
	}
}

func TestResolveCache(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty resolves to default path", "", ".gomutants-cache.json"},
		{"off disables", "off", ""},
		{"explicit path passes through", "/tmp/x.json", "/tmp/x.json"},
		{"relative path passes through", ".cache.json", ".cache.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Cache: tc.in}
			c.ResolveCache()
			if c.Cache != tc.want {
				t.Errorf("Cache=%q, want %q", c.Cache, tc.want)
			}
		})
	}
}

// TestLoadExampleFile asserts the in-repo .gomutants.yml.example matches
// the Config struct field-for-field. The production Load() path is
// permissive (yaml.v3 silently ignores unknown keys), so a separate
// strict decode with KnownFields(true) is what actually catches drift —
// e.g. a key removed from Config but still documented in the example,
// or a typo in the example that would silently no-op for users.
//
// Hard-fails (not Skip) on a missing file: the example is committed and
// referenced from the README; a missing file should break CI rather
// than be quietly tolerated.
func TestLoadExampleFile(t *testing.T) {
	path := filepath.Join("..", "..", ".gomutants.yml.example")

	// Permissive Load() must succeed.
	if _, err := Load(path); err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}

	// Strict decode catches keys that aren't on the Config struct.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("strict decode of %s failed — example contains keys absent from Config: %v", path, err)
	}
}

func TestSplitAndTrim(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b , c ", []string{"a", "b", "c"}}, // Kills STATEMENT_REMOVE on TrimSpace.
		{"single", []string{"single"}},
		{"  spaced  ", []string{"spaced"}}, // Explicit trimming check.
		{"", nil},
		{",,,", nil},
	}
	for _, tc := range tests {
		got := splitAndTrim(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("splitAndTrim(%q) = %v (len %d), want %v (len %d)",
				tc.input, got, len(got), tc.want, len(tc.want))
			continue
		}
		for i, g := range got {
			if g != tc.want[i] {
				t.Errorf("splitAndTrim(%q)[%d] = %q, want %q", tc.input, i, g, tc.want[i])
			}
		}
	}
}

// TestDetectEquivalentEnabled pins the three-state default: nil → false
// (opt-in), and the two explicit pointer states. Kills the nil-guard
// CONDITIONALS_NEGATION / BRANCH_IF in DetectEquivalentEnabled.
func TestDetectEquivalentEnabled(t *testing.T) {
	if (&Config{}).DetectEquivalentEnabled() {
		t.Error("nil DetectEquivalent should be false (opt-in default)")
	}
	tr, fa := true, false
	if !(&Config{DetectEquivalent: &tr}).DetectEquivalentEnabled() {
		t.Error("true pointer should report enabled")
	}
	if (&Config{DetectEquivalent: &fa}).DetectEquivalentEnabled() {
		t.Error("false pointer should report disabled")
	}
}

// TestApplyFlagsDetectEquivalent pins the three-state merge: a Set flag
// overrides, an unset flag leaves the YAML-loaded value intact. Kills the
// BRANCH_IF + STATEMENT_REMOVE on the applyToggleFlags merge branch.
func TestApplyFlagsDetectEquivalent(t *testing.T) {
	cfg := Default()
	cfg.ApplyFlags(Flags{DetectEquivalent: DetectEquivalentFlag{Set: true, Value: true}})
	if !cfg.DetectEquivalentEnabled() {
		t.Error("Set=true,Value=true not merged into config")
	}

	yes := true
	cfg2 := Config{DetectEquivalent: &yes}
	cfg2.ApplyFlags(Flags{DetectEquivalent: DetectEquivalentFlag{Set: false}})
	if !cfg2.DetectEquivalentEnabled() {
		t.Error("unset flag wrongly overrode the config value")
	}
}

// TestDefaultExcludeCallsIsIndependentCopy pins that callers can't grow or
// rewrite the package-level built-in set through the accessor — every run
// of a long-lived process must see the same defaults.
func TestDefaultExcludeCallsIsIndependentCopy(t *testing.T) {
	first := DefaultExcludeCalls()
	if len(first) == 0 {
		t.Fatal("built-in exclude-calls set is empty")
	}
	original := slices.Clone(first)

	first[0] = "mutated"

	if second := DefaultExcludeCalls(); !slices.Equal(second, original) {
		t.Errorf("writing to the returned slice leaked into the built-in set: %v, want %v", second, original)
	}
}

// TestDefaultExcludeCallsContents pins the two decisions the default set
// encodes: stdlib logging is covered out of the box, and the terminating
// log funcs stay mutable (deleting a log.Fatal IS a behaviour change).
func TestDefaultExcludeCallsContents(t *testing.T) {
	got := DefaultExcludeCalls()
	for _, want := range []string{"log.Print*", "slog.Info*"} {
		if !slices.Contains(got, want) {
			t.Errorf("built-in set %v missing %q", got, want)
		}
	}
	for _, unwanted := range []string{"log.Fatal*", "log.Panic*", "*.Info", "*.Error"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("built-in set must not contain %q (it is not unkillable-by-nature)", unwanted)
		}
	}
}

// TestExcludeCallsDefaultsEnabled pins the three-state default: nil → true
// (extend), and the two explicit pointer states. Kills the nil-guard
// CONDITIONALS_NEGATION / BRANCH_IF in ExcludeCallsDefaultsEnabled.
func TestExcludeCallsDefaultsEnabled(t *testing.T) {
	if !(&Config{}).ExcludeCallsDefaultsEnabled() {
		t.Error("nil ExcludeCallsDefaults should be true (extend by default)")
	}
	tr, fa := true, false
	if !(&Config{ExcludeCallsDefaults: &tr}).ExcludeCallsDefaultsEnabled() {
		t.Error("true pointer should report enabled")
	}
	if (&Config{ExcludeCallsDefaults: &fa}).ExcludeCallsDefaultsEnabled() {
		t.Error("false pointer should report disabled")
	}
}

func TestResolvedExcludeCallsExtendsDefaults(t *testing.T) {
	cfg := Config{ExcludeCalls: []string{"*.Debug", "mylog.*"}}
	got := cfg.ResolvedExcludeCalls()

	want := append(DefaultExcludeCalls(), "*.Debug", "mylog.*")
	if !slices.Equal(got, want) {
		t.Errorf("ResolvedExcludeCalls() = %v, want %v (built-ins first, then user entries)", got, want)
	}
}

func TestResolvedExcludeCallsReplacesWhenDefaultsOff(t *testing.T) {
	off := false
	cfg := Config{ExcludeCalls: []string{"mylog.*"}, ExcludeCallsDefaults: &off}
	got := cfg.ResolvedExcludeCalls()

	if !slices.Equal(got, []string{"mylog.*"}) {
		t.Errorf("ResolvedExcludeCalls() = %v, want only the user entries", got)
	}
}

func TestResolvedExcludeCallsEmptyCases(t *testing.T) {
	// No user entries: the built-ins alone, so a project with no config
	// still gets stdlib logging covered.
	if got := (&Config{}).ResolvedExcludeCalls(); !slices.Equal(got, DefaultExcludeCalls()) {
		t.Errorf("empty config resolved to %v, want the built-in set", got)
	}
	// Built-ins off with no user entries: nothing at all, which the
	// discover layer turns into a no-op excluder.
	off := false
	if got := (&Config{ExcludeCallsDefaults: &off}).ResolvedExcludeCalls(); len(got) != 0 {
		t.Errorf("defaults off with no user entries resolved to %v, want empty", got)
	}
}

// TestResolvedExcludeCallsDoesNotAliasConfig guards the append: resolving
// twice must not let the first result's spare capacity be overwritten by
// the second, and must not write through to the config's own slice.
func TestResolvedExcludeCallsDoesNotAliasConfig(t *testing.T) {
	cfg := Config{ExcludeCalls: []string{"mylog.*"}}
	first := cfg.ResolvedExcludeCalls()
	firstCopy := slices.Clone(first)

	second := cfg.ResolvedExcludeCalls()
	second[len(second)-1] = "rewritten"

	if !slices.Equal(first, firstCopy) {
		t.Errorf("second resolve mutated the first result: %v, want %v", first, firstCopy)
	}
	if cfg.ExcludeCalls[0] != "mylog.*" {
		t.Errorf("resolve wrote through to Config.ExcludeCalls: %v", cfg.ExcludeCalls)
	}
}

func TestLoadExcludeCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gomutants.yml")
	yaml := `exclude-calls:
  - "*.Debug"
  - "zap.*"
exclude-calls-defaults: false
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !slices.Equal(cfg.ExcludeCalls, []string{"*.Debug", "zap.*"}) {
		t.Errorf("ExcludeCalls=%v, want [*.Debug zap.*]", cfg.ExcludeCalls)
	}
	if cfg.ExcludeCallsDefaults == nil {
		t.Fatal("exclude-calls-defaults: false must unmarshal to a non-nil pointer")
	}
	if cfg.ExcludeCallsDefaultsEnabled() {
		t.Error("exclude-calls-defaults: false must disable the built-ins")
	}
	if !slices.Equal(cfg.ResolvedExcludeCalls(), []string{"*.Debug", "zap.*"}) {
		t.Errorf("ResolvedExcludeCalls()=%v, want the user list alone", cfg.ResolvedExcludeCalls())
	}
}

// TestLoadWithoutExcludeCallsKeepsDefaults pins that an existing config
// file with no exclude-calls keys still gets the built-in set.
func TestLoadWithoutExcludeCallsKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gomutants.yml")
	if err := os.WriteFile(path, []byte("workers: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ExcludeCalls != nil {
		t.Errorf("ExcludeCalls=%v, want nil", cfg.ExcludeCalls)
	}
	if !slices.Equal(cfg.ResolvedExcludeCalls(), DefaultExcludeCalls()) {
		t.Errorf("ResolvedExcludeCalls()=%v, want the built-in set", cfg.ResolvedExcludeCalls())
	}
}

func TestApplyFlagsExcludeCalls(t *testing.T) {
	// CLI replaces the YAML list (like --exclude-files) and is split and
	// trimmed on commas.
	cfg := Config{ExcludeCalls: []string{"fromyaml.*"}}
	cfg.ApplyFlags(Flags{ExcludeCalls: "log.Print* , *.Debug"})
	if !slices.Equal(cfg.ExcludeCalls, []string{"log.Print*", "*.Debug"}) {
		t.Errorf("ExcludeCalls=%v, want [log.Print* *.Debug]", cfg.ExcludeCalls)
	}

	// An unset flag leaves the YAML list intact.
	cfg2 := Config{ExcludeCalls: []string{"fromyaml.*"}}
	cfg2.ApplyFlags(Flags{})
	if !slices.Equal(cfg2.ExcludeCalls, []string{"fromyaml.*"}) {
		t.Errorf("ExcludeCalls=%v, want the YAML value untouched", cfg2.ExcludeCalls)
	}
}

// TestApplyFlagsExcludeCallsDefaults pins the three-state merge: a Set
// flag overrides, an unset flag leaves the YAML-loaded value intact.
func TestApplyFlagsExcludeCallsDefaults(t *testing.T) {
	cfg := Default()
	cfg.ApplyFlags(Flags{ExcludeCallsDefaults: ExcludeCallsDefaultsFlag{Set: true, Value: false}})
	if cfg.ExcludeCallsDefaultsEnabled() {
		t.Error("Set=true,Value=false not merged into config")
	}

	no := false
	cfg2 := Config{ExcludeCallsDefaults: &no}
	cfg2.ApplyFlags(Flags{ExcludeCallsDefaults: ExcludeCallsDefaultsFlag{Set: false, Value: true}})
	if cfg2.ExcludeCallsDefaultsEnabled() {
		t.Error("unset flag wrongly overrode the config value")
	}
}

func TestLoadIgnoresRunMutantID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gomutants.yml")
	// --run-mutant-id is CLI-only. applyStringFlags only overrides on a
	// non-empty flag, so a committed key would pin every later run to one
	// mutant with no CLI value that turns it back off.
	if err := os.WriteFile(path, []byte("run-mutant-id: \"a.go:F:ARITHMETIC_BASE#1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RunMutantID != "" {
		t.Errorf("RunMutantID=%q, want empty — the config key must be ignored", cfg.RunMutantID)
	}
}

func TestMemoryCapBytes(t *testing.T) {
	const (
		unit  = "want a size in MiB or GiB"
		whole = "want a positive whole number"
		most  = "must be at most 1024GiB"
	)
	tests := []struct {
		in      string
		want    int64
		wantErr string // "" for none
	}{
		{"", DefaultMemoryCap, ""},
		{"  ", DefaultMemoryCap, ""},
		{"off", NoMemoryCap, ""},
		{"OFF", NoMemoryCap, ""},
		{"64MiB", 64 << 20, ""},
		{"3072MiB", 3072 << 20, ""},
		{"4GiB", 4 << 30, ""},
		{" 4GiB ", 4 << 30, ""},
		{"4gib", 4 << 30, ""},
		{"4GIB", 4 << 30, ""},
		{"3072mib", 3072 << 20, ""},
		{"1GiB", 1 << 30, ""},
		{"1024GiB", 1 << 40, ""},
		{"1048576MiB", 1 << 40, ""},
		{"1025GiB", 0, most},
		{"1048577MiB", 0, most},
		// Over 1<<62, which parses as an int64 but not in 63 bits.
		{"9223372036854775807MiB", 0, most},
		// Over an int64: ParseInt's range error.
		{"9223372036854775808MiB", 0, most},
		// A MiB slip for GiB: under any build.
		{"2MiB", 0, "must be at least 64MiB (did you mean 2GiB?)"},
		{"63MiB", 0, "must be at least 64MiB (did you mean 63GiB?)"},
		{"4 GiB", 0, "write it without a space, as 4GiB"},
		{"3072\tmib", 0, "write it without a space, as 3072MiB"},
		{"x GiB", 0, whole},
		{"4096", 0, unit},
		{"4G", 0, unit},
		{"4GB", 0, unit},
		{"iB", 0, unit},
		{"nope", 0, unit},
		{"GiB", 0, whole},
		{"0GiB", 0, whole},
		{"04GiB", 0, whole},
		{"-1GiB", 0, whole},
		{"+4GiB", 0, whole},
		{"1.5GiB", 0, whole},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			c := Config{MemoryCap: tt.in}
			got, err := c.MemoryCapBytes()
			if got != tt.want {
				t.Errorf("MemoryCapBytes(%q)=%d, want %d", tt.in, got, tt.want)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("MemoryCapBytes(%q) err=%v, want none", tt.in, err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "memory-cap")):
				t.Errorf("MemoryCapBytes(%q) err=%v, want one naming memory-cap that says %q", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestDefaultMemoryCapValue(t *testing.T) {
	if DefaultMemoryCap != 2*1024*1024*1024 {
		t.Errorf("DefaultMemoryCap=%d, want 2 GiB", DefaultMemoryCap)
	}
	if NoMemoryCap != -1 {
		t.Errorf("NoMemoryCap=%d, want -1", NoMemoryCap)
	}
}

// TestMemoryCap pins the YAML key and the merge: --memory-cap overrides
// the file, and an empty flag leaves it alone.
func TestMemoryCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gomutants.yml")
	if err := os.WriteFile(path, []byte("memory-cap: 4GiB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MemoryCap != "4GiB" {
		t.Fatalf("MemoryCap=%q, want 4GiB from YAML", cfg.MemoryCap)
	}

	cfg.ApplyFlags(Flags{})
	if cfg.MemoryCap != "4GiB" {
		t.Errorf("MemoryCap=%q after an empty flag, want the YAML value kept", cfg.MemoryCap)
	}

	cfg.ApplyFlags(Flags{MemoryCap: "off"})
	if cfg.MemoryCap != "off" {
		t.Errorf("MemoryCap=%q, want off from the flag", cfg.MemoryCap)
	}
}
