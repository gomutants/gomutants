package schemata

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

// SourceFile is one parsed production file of a package. The caller already
// read and parsed every file during discovery, so schemata takes the parse
// cache rather than doing it again.
type SourceFile struct {
	Path string // Absolute path, matching mutator.Mutant.File.
	Src  []byte
	AST  *ast.File
}

// Package is one package to schematize.
type Package struct {
	ImportPath string
	Dir        string
	Files      []SourceFile
}

// Options configures a schemata build.
type Options struct {
	ProjectDir string // Working directory for the inner go commands.
	TmpDir     string // Where generated sources, overlays and binaries go.
	Tags       string // Forwarded as -tags, as everywhere else.
}

// Fallback is a mutant schemata declined, with the reason why. The caller
// runs these on the per-mutant overlay path unchanged.
type Fallback struct {
	MutantID int
	Reason   string
}

// maxBuildRounds bounds the demote-and-retry loop. Each round costs one
// compile of the schematized packages, and in practice the first round
// clears everything the static rules did not: the bound only guards against
// a diagnostic this package cannot attribute.
const maxBuildRounds = 3

// Build schematizes every package that has pending mutants, compiles the
// result until it is clean, and returns a plan describing what ended up on
// the fast path.
//
// Mutants are only ever *removed* from the fast path, never reclassified:
// anything the compiler rejects is handed back as a Fallback so the
// existing per-mutant overlay path decides its verdict. That is what keeps
// a schemata run's verdicts identical to a plain run's.
func Build(ctx context.Context, opts Options, fset *token.FileSet, pkgs []Package, mutants []mutator.Mutant) (*Plan, error) {
	states, declined := newStates(fset, pkgs, mutants)
	if len(states) == 0 {
		return &Plan{opts: opts, declined: declined}, nil
	}

	overlayPath := filepath.Join(opts.TmpDir, "schemata-overlay.json")
	var lastOut string
	for round := 0; round < maxBuildRounds; round++ {
		more, err := regenerate(fset, states)
		if err != nil {
			return nil, err
		}
		declined = append(declined, more...)
		if err := writeOverlay(opts, overlayPath, states); err != nil {
			return nil, err
		}

		out, err := compile(ctx, opts, states)
		if err == nil {
			return newPlan(opts, overlayPath, states, declined), nil
		}
		lastOut = out
		dropped, unresolved := attribute(opts.ProjectDir, out, states)
		if len(dropped) == 0 {
			if unresolved != "" {
				lastOut = unresolved
			}
			break
		}
		declined = append(declined, dropped...)
	}

	// Every round still found errors it could not pin on a mutant. Rather
	// than guess, decline the whole thing: the caller falls back to the
	// per-mutant path, which is exactly today's behaviour.
	return nil, fmt.Errorf("schemata: could not reach a clean build: %s", firstLines(lastOut, 5))
}

// fileState tracks one file across the demote-and-retry rounds.
type fileState struct {
	pkg     *Package
	file    SourceFile
	mutants []mutator.Mutant // Still on the fast path.
	schema  FileSchema
	genPath string
	dirty   bool
}

// newStates groups pending mutants onto the files that hold them, dropping
// packages that cannot be schematized at all.
func newStates(fset *token.FileSet, pkgs []Package, mutants []mutator.Mutant) ([]*fileState, []Fallback) {
	byFile := map[string][]mutator.Mutant{}
	for _, m := range mutants {
		byFile[m.File] = append(byFile[m.File], m)
	}

	var states []*fileState
	var declined []Fallback
	for i := range pkgs {
		pkg := &pkgs[i]
		asts := make([]*ast.File, 0, len(pkg.Files))
		for _, f := range pkg.Files {
			asts = append(asts, f.AST)
		}
		if reason := CheckCollisions(asts); reason != "" {
			for _, f := range pkg.Files {
				declined = append(declined, fallbacksFor(byFile[f.Path], reason)...)
			}
			continue
		}
		for _, f := range pkg.Files {
			ms := byFile[f.Path]
			if len(ms) == 0 {
				continue
			}
			states = append(states, &fileState{pkg: pkg, file: f, mutants: ms, dirty: true})
		}
	}
	return states, declined
}

func fallbacksFor(ms []mutator.Mutant, reason string) []Fallback {
	out := make([]Fallback, 0, len(ms))
	for _, m := range ms {
		out = append(out, Fallback{MutantID: m.ID, Reason: reason})
	}
	return out
}

// regenerate rebuilds the schema for every file marked dirty.
func regenerate(fset *token.FileSet, states []*fileState) ([]Fallback, error) {
	var declined []Fallback
	for _, st := range states {
		if !st.dirty {
			continue
		}
		schema, dropped, err := Generate(fset, st.file.AST, st.file.Src, st.mutants)
		if err != nil {
			// The generator produced something unparseable — a bug here, not
			// in the user's code. Decline the file rather than handing the
			// compiler nonsense.
			declined = append(declined, fallbacksFor(st.mutants, "schema generation failed: "+err.Error())...)
			st.mutants = nil
			st.schema = FileSchema{Source: st.file.Src}
			st.dirty = false
			continue
		}
		// Drop what Generate declined from the file's list, so a later
		// round neither re-reports it nor mistakes it for an active mutant.
		if len(dropped) > 0 {
			gone := make(map[int]bool, len(dropped))
			for _, f := range dropped {
				gone[f.mutant.ID] = true
				declined = append(declined, Fallback{MutantID: f.mutant.ID, Reason: f.reason})
			}
			kept := st.mutants[:0:0]
			for _, mu := range st.mutants {
				if !gone[mu.ID] {
					kept = append(kept, mu)
				}
			}
			st.mutants = kept
		}
		st.schema = schema
		st.dirty = false
	}
	return declined, nil
}

// writeOverlay materializes each generated file and the per-package guard
// helper, then writes the -overlay index cmd/go reads.
//
// The helper is injected at a path that does not exist on disk; cmd/go's
// overlay filesystem synthesizes the directory entry, so a package can gain
// a file this way.
func writeOverlay(opts Options, overlayPath string, states []*fileState) error {
	replace := map[string]string{}
	helpers := map[string]string{} // package dir -> package name

	for i, st := range states {
		if len(st.mutants) == 0 {
			continue // Nothing schematized here; compile the original.
		}
		if st.genPath == "" {
			st.genPath = filepath.Join(opts.TmpDir, fmt.Sprintf("schema-%d-%s", i, filepath.Base(st.file.Path)))
		}
		if err := os.WriteFile(st.genPath, st.schema.Source, 0o644); err != nil {
			return fmt.Errorf("schemata: writing generated source: %w", err)
		}
		replace[st.file.Path] = st.genPath
		helpers[st.pkg.Dir] = st.file.AST.Name.Name
	}

	for dir, pkgName := range helpers {
		path := filepath.Join(opts.TmpDir, "helper-"+sanitize(dir)+".go")
		if err := os.WriteFile(path, helperSource(pkgName), 0o644); err != nil {
			return fmt.Errorf("schemata: writing guard helper: %w", err)
		}
		replace[filepath.Join(dir, HelperFile)] = path
	}

	body, err := json.Marshal(struct {
		Replace map[string]string `json:"Replace"`
	}{Replace: replace})
	if err != nil {
		return fmt.Errorf("schemata: encoding overlay: %w", err)
	}
	return os.WriteFile(overlayPath, body, 0o644)
}

// compile type-checks the schematized packages without producing
// artifacts. -e lifts the compiler's ten-error cap so one round can see
// every mutant that needs demoting; it is scoped per package so the
// dependency tree is not rebuilt under different flags.
func compile(ctx context.Context, opts Options, states []*fileState) (string, error) {
	args := []string{"build", "-o", os.DevNull}
	for _, pkg := range schematizedPkgs(states) {
		args = append(args, "-gcflags="+pkg+"=-e")
	}
	args = append(args, "-overlay="+filepath.Join(opts.TmpDir, "schemata-overlay.json"))
	if opts.Tags != "" {
		args = append(args, "-tags="+opts.Tags)
	}
	args = append(args, schematizedPkgs(states)...)

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = opts.ProjectDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func schematizedPkgs(states []*fileState) []string {
	var out []string
	for _, st := range states {
		if len(st.mutants) > 0 {
			out = append(out, st.pkg.ImportPath)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// diagnostic matches a compiler error line. The column is optional: the
// type checker reports "file.go:12:5: ...", but some backend diagnostics —
// constant overflow among them, which INTEGER_DECREMENT reliably provokes —
// report only a line.
var diagnostic = regexp.MustCompile(`^\s*(\S+\.go):(\d+)(?::(\d+))?:\s*(.*)$`)

// attribute maps each compiler diagnostic back to the mutant whose guard
// caused it, and removes those mutants from the fast path.
//
// Containment is tried first: a guard's own generated range is the tightest
// answer, and nested guards win over the ones enclosing them. When nothing
// contains the position — a guarded block that was a function's terminating
// statement reports "missing return" at the closing brace, well outside the
// guard — every guard in the same declaration is demoted instead.
func attribute(projectDir, out string, states []*fileState) ([]Fallback, string) {
	// Index both names. cmd/go hands the compiler the overlay's backing
	// file, so diagnostics name the generated source rather than the path
	// it stands in for.
	byPath := map[string]*fileState{}
	for _, st := range states {
		if st.genPath == "" || len(st.mutants) == 0 {
			continue
		}
		byPath[filepath.Clean(st.file.Path)] = st
		byPath[filepath.Clean(st.genPath)] = st
	}

	var declined []Fallback
	var unresolved []string
	drop := map[*fileState]map[int]bool{}

	for _, line := range strings.Split(out, "\n") {
		m := diagnostic.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		st := byPath[resolvePath(projectDir, m[1])]
		if st == nil {
			unresolved = append(unresolved, strings.TrimSpace(line))
			continue
		}
		row, _ := strconv.Atoi(m[2])
		lo, hi := spanOf(st.schema.Source, row, m[3])
		ids := guardsAt(st.schema, lo, hi)
		if len(ids) == 0 {
			unresolved = append(unresolved, strings.TrimSpace(line))
			continue
		}
		if drop[st] == nil {
			drop[st] = map[int]bool{}
		}
		for _, id := range ids {
			drop[st][id] = true
		}
	}

	for st, ids := range drop {
		kept := st.mutants[:0:0]
		for _, mu := range st.mutants {
			if ids[mu.ID] {
				declined = append(declined, Fallback{MutantID: mu.ID, Reason: "rejected by the schema build"})
				continue
			}
			kept = append(kept, mu)
		}
		st.mutants = kept
		st.dirty = true
	}
	slices.SortFunc(declined, func(a, b Fallback) int { return a.MutantID - b.MutantID })
	return declined, strings.Join(unresolved, "; ")
}

// resolvePath turns a compiler-reported path into an absolute one. Paths
// are reported relative to the go command's working directory, which is the
// project root.
func resolvePath(projectDir, reported string) string {
	if filepath.IsAbs(reported) {
		return filepath.Clean(reported)
	}
	return filepath.Clean(filepath.Join(projectDir, reported))
}

// guardsAt returns the mutants to blame for a diagnostic covering
// [lo, hi). The tightest overlapping guard wins, so a guard nested inside
// another is blamed ahead of the one enclosing it.
func guardsAt(schema FileSchema, lo, hi int) []int {
	best, bestSize := -1, -1
	for i, g := range schema.Guards {
		if g.End <= lo || hi <= g.Start {
			continue
		}
		if size := g.End - g.Start; bestSize < 0 || size < bestSize {
			best, bestSize = i, size
		}
	}
	if best >= 0 {
		return []int{schema.Guards[best].MutantID}
	}
	// Nothing overlaps: blame every guard in the enclosing declaration. A
	// guarded block that was a function's terminating statement reports
	// "missing return" at the closing brace, well outside any guard.
	var ids []int
	for _, g := range schema.Guards {
		if g.DeclStart < hi && lo < g.DeclEnd {
			ids = append(ids, g.MutantID)
		}
	}
	return ids
}

// spanOf converts a 1-based line and an optional column into a byte range.
// Without a column the whole line is the range, since the diagnostic could
// belong to any guard on it.
func spanOf(src []byte, line int, col string) (int, int) {
	start, cur := 0, 1
	for cur < line && start < len(src) {
		if src[start] == '\n' {
			cur++
		}
		start++
	}
	end := start
	for end < len(src) && src[end] != '\n' {
		end++
	}
	if col == "" {
		return start, max(end, start+1)
	}
	n, err := strconv.Atoi(col)
	if err != nil || n < 1 {
		return start, max(end, start+1)
	}
	at := min(start+n-1, len(src))
	return at, at + 1
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
}

// firstLines trims compiler output down to something a warning can carry.
func firstLines(out string, n int) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "; ")
}
