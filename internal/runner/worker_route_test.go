package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gomutants/gomutants/internal/coverage"
	"github.com/gomutants/gomutants/internal/mutator"
)

// routeMap builds a TestMap whose index routes one position to the given
// (pkg, name) references.
func routeMap(fileLine string, refs ...coverage.TestRef) *coverage.TestMap {
	return coverage.NewTestMapForTesting(nil, map[string][]coverage.TestRef{
		fileLine: refs,
	})
}

func lastArg(args []string) string { return args[len(args)-1] }

func runArg(args []string) (string, bool) {
	for _, a := range args {
		if strings.HasPrefix(a, "-run=") {
			return a, true
		}
	}
	return "", false
}

// routedInvocations returns the `go test` invocations of m's routed run
// (see Worker.Test).
func (w *Worker) routedInvocations(m mutator.Mutant, short bool, timeout time.Duration) [][]string {
	return w.invocations(w.routeGroups(m), m.Pkg, short, timeout)
}

// recheckInvocations returns the `go test` invocations of m's re-check
// (see Worker.Test), empty when it has none.
func (w *Worker) recheckInvocations(m mutator.Mutant) [][]string {
	return w.invocations(w.recheckGroups(m, w.routeGroups(m)), m.Pkg, false, time.Second)
}

// invocationsFor builds a worker around tm and returns the `go test`
// invocations it routes for a mutant in ownPkg at f.go:1.
func invocationsFor(t *testing.T, tm *coverage.TestMap, ownPkg string) [][]string {
	t.Helper()
	w := &Worker{testMap: tm}
	m := mutator.Mutant{Pkg: ownPkg, CoverageFile: "f.go", Line: 1}
	return w.routedInvocations(m, false, time.Second)
}

// checkInvocation asserts that invocation i targets wantPkg and carries the
// expected -run filter. wantRun=="" means the invocation must have no -run
// filter (the whole package runs).
func checkInvocation(t *testing.T, i int, inv []string, wantPkg, wantRun string) {
	t.Helper()
	if got := lastArg(inv); got != wantPkg {
		t.Errorf("invocation %d package = %q, want %q", i, got, wantPkg)
	}
	r, ok := runArg(inv)
	switch {
	case wantRun == "" && ok:
		t.Errorf("invocation %d has unexpected -run %q; whole package should run", i, r)
	case wantRun != "" && r != wantRun:
		t.Errorf("invocation %d -run = %q, want %q", i, r, wantRun)
	}
}

// TestRoutedInvocations covers how a mutant is routed to per-package `go test`
// invocations: same-package, cross-package (own package ordered first), an
// importer-only mutant, and the no-routing fallback (whole own package, no
// -run filter). wantRuns[i]=="" means invocation i must carry no -run filter.
func TestRoutedInvocations(t *testing.T) {
	const (
		calc = "m/calc"
		app  = "m/app"
	)
	cases := []struct {
		name     string
		tm       *coverage.TestMap
		ownPkg   string
		wantPkgs []string
		wantRuns []string
	}{
		{
			name:     "same package, one invocation",
			tm:       routeMap("f.go:1", coverage.TestRef{Pkg: calc, Name: "TestA"}),
			ownPkg:   calc,
			wantPkgs: []string{calc},
			wantRuns: []string{"-run=^(TestA)$"},
		},
		{
			name: "cross package orders own first",
			tm: routeMap("f.go:1",
				coverage.TestRef{Pkg: app, Name: "TestApp"},
				coverage.TestRef{Pkg: calc, Name: "TestCalc"}),
			ownPkg:   calc,
			wantPkgs: []string{calc, app},
			wantRuns: []string{"-run=^(TestCalc)$", "-run=^(TestApp)$"},
		},
		{
			name:     "importer only, never own package",
			tm:       routeMap("f.go:1", coverage.TestRef{Pkg: app, Name: "TestApp"}),
			ownPkg:   calc,
			wantPkgs: []string{app},
			wantRuns: []string{"-run=^(TestApp)$"},
		},
		{
			name:     "no routing runs whole own package",
			tm:       routeMap("other.go:9", coverage.TestRef{Pkg: calc, Name: "TestA"}),
			ownPkg:   calc,
			wantPkgs: []string{calc},
			wantRuns: []string{""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invs := invocationsFor(t, tc.tm, tc.ownPkg)
			if len(invs) != len(tc.wantPkgs) {
				t.Fatalf("got %d invocations, want %d: %v", len(invs), len(tc.wantPkgs), invs)
			}
			for i, inv := range invs {
				checkInvocation(t, i, inv, tc.wantPkgs[i], tc.wantRuns[i])
			}
		})
	}
}

// TestRecheckInvocations covers which suites re-check a mutant its routed
// run passed: every package that can kill it run in full, less those the
// routed run already ran in full. wantPkgs nil means no re-check.
func TestRecheckInvocations(t *testing.T) {
	const (
		calc = "m/calc"
		app  = "m/app"
		lib  = "m/lib"
	)
	suites := []coverage.Package{{ImportPath: calc}, {ImportPath: app}, {ImportPath: lib}}
	// app's tests link calc; lib's don't; calc's own are calc's.
	deps := map[string]map[string]bool{calc: {}, app: {calc: true}, lib: {}}
	cross := func(tm *coverage.TestMap) *coverage.TestMap { return tm.WithSuitesForTesting(true, deps, suites...) }
	cases := []struct {
		name     string
		tm       *coverage.TestMap
		pkg      string
		wantPkgs []string
	}{
		{"no map: the routed run was the whole package", nil, calc, nil},
		{"own package routed to a subset", routeMap("f.go:1", coverage.TestRef{Pkg: calc, Name: "TestA"}), calc, []string{calc}},
		{
			"own package routed to a subset, suites in scope",
			routeMap("f.go:1", coverage.TestRef{Pkg: calc, Name: "TestA"}).WithSuitesForTesting(false, nil, suites...),
			calc, []string{calc},
		},
		{"no covering tests: the whole own package already ran", routeMap("other.go:9", coverage.TestRef{Pkg: calc, Name: "TestA"}), calc, nil},
		{
			"cross-package, no covering tests: the linking importers",
			cross(routeMap("other.go:9", coverage.TestRef{Pkg: calc, Name: "TestA"})),
			calc, []string{app},
		},
		{
			"cross-package, routed to an importer: own package and the importer in full",
			cross(routeMap("f.go:1", coverage.TestRef{Pkg: app, Name: "TestApp"})),
			calc, []string{calc, app},
		},
		{
			"cross-package, links unknown: every suite",
			routeMap("f.go:1", coverage.TestRef{Pkg: calc, Name: "TestA"}).WithSuitesForTesting(true, nil, suites...),
			calc, []string{calc, app, lib},
		},
		{
			// m/types has no tests, so it isn't among the suites: running
			// it would build and link for "no test files".
			"cross-package, own package without tests: only the importer",
			routeMap("f.go:1", coverage.TestRef{Pkg: app, Name: "TestApp"}).WithSuitesForTesting(true,
				map[string]map[string]bool{calc: {}, app: {"m/types": true}, lib: {}}, suites...),
			"m/types", []string{app},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &Worker{testMap: tc.tm}
			invs := w.recheckInvocations(mutator.Mutant{Pkg: tc.pkg, CoverageFile: "f.go", Line: 1})
			if len(invs) != len(tc.wantPkgs) {
				t.Fatalf("got %d invocations, want %d: %v", len(invs), len(tc.wantPkgs), invs)
			}
			for i, inv := range invs {
				checkInvocation(t, i, inv, tc.wantPkgs[i], "")
			}
		})
	}
}

func TestOrderRoutePackages(t *testing.T) {
	// Go randomizes map iteration order, so a dropped slices.Sort surfaces as
	// a non-sorted result only on some iterations. Using several keys and
	// looping makes the "rest must be sorted" assertion reliably catch it
	// (the odds of the unsorted result accidentally matching every iteration
	// are vanishing).
	groups := map[string][]string{
		"m/calc": {"T"}, // own — must come first regardless of order
		"m/zoo":  {"T"},
		"m/app":  {"T"},
		"m/qux":  {"T"},
		"m/bar":  {"T"},
	}
	want := []string{"m/calc", "m/app", "m/bar", "m/qux", "m/zoo"}
	for range 50 {
		if got := orderRoutePackages(groups, "m/calc"); !slices.Equal(got, want) {
			t.Fatalf("orderRoutePackages = %v, want %v", got, want)
		}
	}

	// A whole-package (nil) entry for the own package still sorts first.
	if got := orderRoutePackages(map[string][]string{"m/zoo": {"T"}, "m/calc": nil}, "m/calc"); !slices.Equal(got, []string{"m/calc", "m/zoo"}) {
		t.Fatalf("orderRoutePackages (own runs whole) = %v, want [m/calc m/zoo]", got)
	}

	// Own package absent from the groups → just the sorted rest.
	wantRest := []string{"m/app", "m/bar", "m/zoo"}
	for range 50 {
		got := orderRoutePackages(map[string][]string{"m/zoo": {"T"}, "m/app": {"T"}, "m/bar": {"T"}}, "m/calc")
		if !slices.Equal(got, wantRest) {
			t.Fatalf("orderRoutePackages (own absent) = %v, want %v", got, wantRest)
		}
	}
}

// recheckModule writes a module whose Add is covered by a weak TestWeak
// and by testSrc's other tests, and returns a worker for it whose map
// routes Add's line to TestWeak alone, plus the `+`→`-` mutant on it.
func recheckModule(t *testing.T, testSrc string) (*Worker, mutator.Mutant) {
	t.Helper()
	dir := t.TempDir()
	src := "package testpkg\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
	for name, body := range map[string]string{
		"go.mod":      "module testmod\n\ngo 1.26\n",
		"add.go":      src,
		"add_test.go": "package testpkg\n\nimport \"testing\"\n\nfunc TestWeak(t *testing.T) { _ = Add(1, 2) }\n" + testSrc,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, "add.go")
	tm := routeMap("testmod/add.go:4", coverage.TestRef{Pkg: "testmod", Name: "TestWeak"})
	w, err := NewWorker(0, t.TempDir(), TimeoutPolicy{Global: 30 * time.Second}, map[string][]byte{file: []byte(src)}, dir, tm)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	plus := strings.Index(src, "+")
	return w, mutator.Mutant{
		ID: 1, File: file, Pkg: "testmod", CoverageFile: "testmod/add.go", Line: 4,
		StartOffset: plus, EndOffset: plus + 1, Replacement: "-",
		Status: mutator.StatusPending,
	}
}

// TestWorkerTestRecheckKills: a mutant its covering tests pass is re-run
// against its whole package, where a test the map didn't route it to
// kills it. Without the re-check it would be a false LIVED.
func TestWorkerTestRecheckKills(t *testing.T) {
	w, m := recheckModule(t, "\nfunc TestStrong(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n")
	got := w.Test(context.Background(), m)
	if got.Status != mutator.StatusKilled || !got.Rechecked {
		t.Errorf("Status=%v Rechecked=%v, want KILLED by the re-check", got.Status, got.Rechecked)
	}
}

// TestWorkerTestRecheckLives: a mutant no test of its package kills lives
// after the re-check, marked as re-checked.
func TestWorkerTestRecheckLives(t *testing.T) {
	w, m := recheckModule(t, "\nfunc TestOther(t *testing.T) { _ = Add(2, 2) }\n")
	got := w.Test(context.Background(), m)
	if got.Status != mutator.StatusLived || !got.Rechecked || got.Duration <= 0 {
		t.Errorf("Status=%v Rechecked=%v Duration=%v, want a re-checked LIVED with its duration", got.Status, got.Rechecked, got.Duration)
	}
}

// TestWorkerTestNoRecheckAfterKill: a mutant its covering tests kill is
// not re-checked.
func TestWorkerTestNoRecheckAfterKill(t *testing.T) {
	w, m := recheckModule(t, "")
	w.testMap = routeMap("testmod/add.go:4", coverage.TestRef{Pkg: "testmod", Name: "TestKill"})
	if err := os.WriteFile(filepath.Join(filepath.Dir(m.File), "kill_test.go"),
		[]byte("package testpkg\n\nimport \"testing\"\n\nfunc TestKill(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := w.Test(context.Background(), m)
	if got.Status != mutator.StatusKilled || got.Rechecked {
		t.Errorf("Status=%v Rechecked=%v, want KILLED by the routed run alone", got.Status, got.Rechecked)
	}
}

// TestWorkerTestRecheckCancelled: a parent cancel during the re-check
// leaves the mutant Pending with no duration and not marked re-checked,
// as a cancel during the routed run does.
func TestWorkerTestRecheckCancelled(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	t.Setenv("GOMUTANTS_RECHECK_MARKER", marker)
	w, m := recheckModule(t, `
func TestSlow(t *testing.T) {
	_ = os.WriteFile(os.Getenv("GOMUTANTS_RECHECK_MARKER"), nil, 0o644)
	time.Sleep(30 * time.Second)
}
`)
	// TestSlow needs os and time; the module's test file imports only testing.
	testFile := filepath.Join(filepath.Dir(m.File), "add_test.go")
	body, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "import \"testing\"", "import (\n\t\"os\"\n\t\"testing\"\n\t\"time\"\n)", 1))
	if err := os.WriteFile(testFile, body, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// TestSlow runs only in the re-check, so its marker means the
		// routed run has passed.
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	got := w.Test(ctx, m)
	if got.Status != mutator.StatusPending || got.Duration != 0 || got.Rechecked || got.ID != m.ID || got.File != m.File {
		t.Errorf("got %+v, want the untouched Pending mutant %+v", got, m)
	}
}
