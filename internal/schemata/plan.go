package schemata

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Plan is what a schemata build produced: the set of mutants on the fast
// path, and the prebuilt test binaries to run them against.
type Plan struct {
	opts     Options
	overlay  string
	active   map[int]bool
	declined []Fallback

	mu   sync.Mutex
	bins map[string]*binary
	dirs map[string]string
}

// binary is one package's compiled test binary, built at most once.
type binary struct {
	once sync.Once
	path string
	dir  string
	err  error
}

func newPlan(opts Options, overlay string, states []*fileState, declined []Fallback) *Plan {
	active := map[int]bool{}
	dirs := map[string]string{}
	for _, st := range states {
		for _, m := range st.mutants {
			active[m.ID] = true
		}
		dirs[st.pkg.ImportPath] = st.pkg.Dir
	}
	return &Plan{
		opts:     opts,
		overlay:  overlay,
		active:   active,
		declined: declined,
		bins:     map[string]*binary{},
		dirs:     dirs,
	}
}

// Schematized reports whether a mutant runs against a prebuilt binary. A
// nil plan schematizes nothing, so callers need no separate feature check.
func (p *Plan) Schematized(id int) bool {
	if p == nil {
		return false
	}
	return p.active[id]
}

// Count returns how many mutants are on the fast path.
func (p *Plan) Count() int {
	if p == nil {
		return 0
	}
	return len(p.active)
}

// Declined returns the mutants that stayed on the per-mutant overlay path,
// with the reason for each, in mutant-id order.
func (p *Plan) Declined() []Fallback {
	if p == nil {
		return nil
	}
	out := slices.Clone(p.declined)
	slices.SortFunc(out, func(a, b Fallback) int { return a.MutantID - b.MutantID })
	return out
}

// Binary returns the test binary for a package and the directory to run it
// in, compiling it on first use.
//
// Building lazily matters: a test binary is tens of megabytes, a large repo
// has many test packages, and a package whose mutants all came from the
// cache never needs one at all. The memo mirrors the per-package reference
// compile in internal/tce.
func (p *Plan) Binary(ctx context.Context, importPath string) (string, string, error) {
	if p == nil {
		return "", "", fmt.Errorf("schemata: no plan")
	}
	p.mu.Lock()
	b, ok := p.bins[importPath]
	if !ok {
		b = &binary{}
		p.bins[importPath] = b
	}
	p.mu.Unlock()

	b.once.Do(func() { b.path, b.dir, b.err = p.compileTest(ctx, importPath) })
	return b.path, b.dir, b.err
}

func (p *Plan) compileTest(ctx context.Context, importPath string) (string, string, error) {
	dir, err := p.dirOf(ctx, importPath)
	if err != nil {
		return "", "", err
	}
	out := filepath.Join(p.opts.TmpDir, "schema-"+sanitize(importPath)+".test")

	// -vet=off for the same reason the per-mutant path uses it: vet runs on
	// clean source in the user's CI, and re-running it here is pure cost.
	// No -cover either — coverage was collected once, before any mutation.
	args := []string{"test", "-c", "-vet=off", "-o", out, "-overlay=" + p.overlay}
	if p.opts.Tags != "" {
		args = append(args, "-tags="+p.opts.Tags)
	}
	args = append(args, importPath)

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = p.opts.ProjectDir
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("schemata: building test binary for %s: %w: %s", importPath, err, strings.TrimSpace(string(b)))
	}
	// A package with no test files compiles fine and produces nothing, so
	// the caller has to fall back for mutants routed only to it.
	if _, err := os.Stat(out); err != nil {
		return "", "", fmt.Errorf("schemata: %s produced no test binary", importPath)
	}
	return out, dir, nil
}

// dirOf resolves the directory a package's tests must run in. Target
// packages are already known; a covering package found only through
// cross-package routing is looked up once.
func (p *Plan) dirOf(ctx context.Context, importPath string) (string, error) {
	p.mu.Lock()
	dir, ok := p.dirs[importPath]
	p.mu.Unlock()
	if ok {
		return dir, nil
	}

	cmd := exec.CommandContext(ctx, "go", "list", "-f", "{{.Dir}}", importPath)
	cmd.Dir = p.opts.ProjectDir
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("schemata: locating %s: %w", importPath, err)
	}
	dir = strings.TrimSpace(string(b))

	p.mu.Lock()
	p.dirs[importPath] = dir
	p.mu.Unlock()
	return dir, nil
}
