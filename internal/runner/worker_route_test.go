package runner

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/szhekpisov/gomutants/internal/coverage"
	"github.com/szhekpisov/gomutants/internal/mutator"
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

// invocationsFor builds a worker around tm and returns the `go test`
// invocations it routes for a mutant in ownPkg at f.go:1.
func invocationsFor(t *testing.T, tm *coverage.TestMap, ownPkg string) [][]string {
	t.Helper()
	w := &Worker{testMap: tm}
	m := mutator.Mutant{Pkg: ownPkg, CoverageFile: "f.go", Line: 1}
	// Routing is asserted over the argv; the executable and working
	// directory are the same for every `go test` invocation.
	invs := w.testInvocations(m, false, time.Second)
	out := make([][]string, 0, len(invs))
	for _, inv := range invs {
		out = append(out, inv.args)
	}
	return out
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

// TestTestInvocations covers how a mutant is routed to per-package `go test`
// invocations: same-package, cross-package (own package ordered first), an
// importer-only mutant, and the no-routing fallback (whole own package, no
// -run filter). wantRuns[i]=="" means invocation i must carry no -run filter.
func TestTestInvocations(t *testing.T) {
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

	// Own package absent from the groups → just the sorted rest.
	wantRest := []string{"m/app", "m/bar", "m/zoo"}
	for range 50 {
		got := orderRoutePackages(map[string][]string{"m/zoo": {"T"}, "m/app": {"T"}, "m/bar": {"T"}}, "m/calc")
		if !slices.Equal(got, wantRest) {
			t.Fatalf("orderRoutePackages (own absent) = %v, want %v", got, wantRest)
		}
	}
}
