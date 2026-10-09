package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/szhekpisov/gomutants/internal/mutator"
)

// Package holds resolved package info from go list. Imports/TestImports/
// XTestImports carry the production, in-package-test, and external-test
// import paths respectively; the integration-mode reverse-dependency closure
// reads them (test imports are how an importing package's test reaches a
// target).
type Package struct {
	Dir          string   // Absolute directory path.
	ImportPath   string   // e.g. "github.com/szhekpisov/gomutants/internal/discover"
	GoFiles      []string // .go source files (base names).
	TestGoFiles  []string // _test.go files (base names).
	Imports      []string // production imports.
	TestImports  []string // imports of in-package (_test.go) test files.
	XTestImports []string // imports of external (foo_test) test files.
	// EmbedFiles lists the files the package's production //go:embed
	// directives pull in, as Dir-relative slash paths (go list resolves the
	// patterns for us, so `all:`, globs and directory trees need no
	// reimplementation here). The incremental cache hashes them alongside
	// the package's .go files: embedded data decides test outcomes exactly
	// as source does.
	EmbedFiles []string
}

// goListJSON mirrors the fields we need from `go list -json`.
type goListJSON struct {
	Dir          string   `json:"Dir"`
	ImportPath   string   `json:"ImportPath"`
	GoFiles      []string `json:"GoFiles"`
	TestGoFiles  []string `json:"TestGoFiles"`
	Imports      []string `json:"Imports"`
	TestImports  []string `json:"TestImports"`
	XTestImports []string `json:"XTestImports"`
	EmbedFiles   []string `json:"EmbedFiles"`
	Error        *struct {
		Err string `json:"Err"`
	} `json:"Error"`
}

// ResolvePackages runs `go list -json` to resolve package patterns. tags,
// when non-empty, is forwarded as `-tags=<value>` so packages whose only
// files are behind matching build constraints are still resolved.
func ResolvePackages(ctx context.Context, dir string, patterns []string, tags string) ([]Package, error) {
	args := []string{"list", "-json"}
	if tags != "" {
		args = append(args, "-tags="+tags)
	}
	args = append(args, patterns...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list: %w\n%s", err, stderr.String())
	}

	return decodeGoListJSON(&stdout)
}

// decodeGoListJSON parses JSON output from `go list -json`.
func decodeGoListJSON(r io.Reader) ([]Package, error) {
	var pkgs []Package
	dec := json.NewDecoder(r)
	for dec.More() {
		var p goListJSON
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("parsing go list output: %w", err)
		}
		if p.Error != nil {
			return nil, fmt.Errorf("go list error for %s: %s", p.ImportPath, p.Error.Err)
		}
		pkgs = append(pkgs, Package{
			Dir:          p.Dir,
			ImportPath:   p.ImportPath,
			GoFiles:      p.GoFiles,
			TestGoFiles:  p.TestGoFiles,
			Imports:      p.Imports,
			TestImports:  p.TestImports,
			XTestImports: p.XTestImports,
			EmbedFiles:   p.EmbedFiles,
		})
	}
	return pkgs, nil
}

// ParsedFile is a cached source file: bytes plus the AST parsed with
// parser.ParseComments. FilterByDirectivesWithCache reuses both so it
// doesn't re-read or re-parse during the directives pass.
type ParsedFile struct {
	Src  []byte
	File *ast.File
}

// Result is the discovery output: the candidate mutants plus the per-file
// parse cache keyed by absolute path. Pass Files into
// FilterByDirectivesWithCache to skip the duplicate read+parse.
type Result struct {
	Mutants []mutator.Mutant
	Files   map[string]*ParsedFile
}

// Discover walks the given packages, parses each source file, invokes all
// mutators, and returns a sorted list of Mutants with sequential IDs
// alongside the parse cache so downstream filters can reuse the AST.
// moduleRoot is the absolute path to the project root (for computing absolute paths).
// goModule is the Go module name (for computing gremlins-compatible relative paths).
func Discover(fset *token.FileSet, pkgs []Package, mutators []mutator.Mutator, moduleRoot, goModule string) *Result {
	return DiscoverTo(fset, pkgs, mutators, moduleRoot, goModule, os.Stderr)
}

// DiscoverTo is Discover with its warnings written to warn instead of
// os.Stderr, for a pre-check that parses files a later Discover parses
// again: it holds them back and prints them only when that later call
// will not run.
func DiscoverTo(fset *token.FileSet, pkgs []Package, mutators []mutator.Mutator, moduleRoot, goModule string, warn io.Writer) *Result {
	var allCandidates []mutator.MutantCandidate
	files := make(map[string]*ParsedFile)
	// Function spans per file, for resolving each candidate's stable-ID
	// anchor. Kept local rather than on ParsedFile so the parse cache's
	// contract stays unchanged.
	spansByFile := make(map[string][]funcSpan)

	for _, pkg := range pkgs {
		for _, filename := range pkg.GoFiles {
			absPath := filepath.Join(pkg.Dir, filename)
			src, file, err := parseFile(fset, absPath)
			if err != nil {
				// Soft failure: skip unparseable files. Log so silent
				// skips don't hide classifier-blindspots in reports.
				fmt.Fprintf(warn, "gomutants: skipping unparseable %s: %v\n", absPath, err)
				continue
			}
			files[absPath] = &ParsedFile{Src: src, File: file}
			spansByFile[absPath] = funcSpans(fset, file)
			for _, m := range mutators {
				candidates := m.Discover(fset, file, src)
				allCandidates = append(allCandidates, candidates...)
			}
		}
	}

	// Sort into candidateLess order: deterministic output, and the total
	// order the stable-ID ordinals below are counted in.
	sort.Slice(allCandidates, func(i, j int) bool {
		return candidateLess(allCandidates[i], allCandidates[j])
	})

	// Build file→package lookup.
	filePkg := make(map[string]string)
	for _, p := range pkgs {
		for _, f := range p.GoFiles {
			filePkg[filepath.Join(p.Dir, f)] = p.ImportPath
		}
	}

	// Find common import path prefix for gremlins-compatible relative paths.
	// e.g. if all packages are under "github.com/foo/bar/pkg/diffyml",
	// then RelFile for "github.com/foo/bar/pkg/diffyml/cli/cli.go" is "cli/cli.go".
	commonPrefix := longestCommonPrefix(pkgs)

	// Convert candidates to mutants. The loop runs in candidateLess order,
	// so the per-group ordinals feeding stable IDs are deterministic.
	mutants := make([]mutator.Mutant, len(allCandidates))
	ordinals := make(map[ordinalKey]int)
	for i, c := range allCandidates {
		absPath := c.Pos.Filename
		pkg := filePkg[absPath]

		// Compute gremlins-compatible relative path: strip common prefix from ImportPath.
		relPath := computeRelFile(commonPrefix, pkg, absPath, moduleRoot)

		// Coverage profile path: ImportPath/filename.
		coverageFile := pkg + "/" + filepath.Base(absPath)

		key := ordinalKey{
			file:   stableIDFile(absPath, moduleRoot),
			anchor: anchorFor(spansByFile[absPath], c.Pos.Offset),
			typ:    c.Type,
		}
		ordinals[key]++

		mutants[i] = mutator.Mutant{
			ID:           i + 1,
			StableID:     stableID(key, ordinals[key]),
			Type:         c.Type,
			File:         absPath,
			RelFile:      relPath,
			Line:         c.Pos.Line,
			Col:          c.Pos.Column,
			Original:     c.Original,
			Replacement:  c.Replacement,
			StartOffset:  c.StartOffset,
			EndOffset:    c.EndOffset,
			CoverageFile: coverageFile,
			Status:       mutator.StatusPending,
			Pkg:          pkg,
		}
	}

	return &Result{Mutants: mutants, Files: files}
}

// parseFile reads and parses one file with parser.ParseComments so the
// returned *ast.File can be reused by FilterByDirectivesWithCache without
// re-parsing. The bytes are read once and handed to the parser.
//
// The read error is wrapped with a "read <path>:" prefix so callers
// know which file failed. The prefix is also load-bearing: without it,
// dropping the early return would let parser.ParseFile fall through
// with src==nil, re-read `path` from disk, and either silently succeed
// or surface a parser-shaped error — both observably equivalent to the
// original from a caller that only checks `err != nil`.
func parseFile(fset *token.FileSet, path string) ([]byte, *ast.File, error) {
	src, err := readFileBytes(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}
	return src, file, nil
}

// candidateLess orders candidates by (file, line, column, type, end offset,
// replacement) so Discover produces deterministic output regardless of the
// AST-walk order.
//
// The last two fields make the order total, which the stable-ID ordinals
// depend on. Position and type alone do not separate every candidate: in a
// nested boolean expression like `a && b && c`, EXPRESSION_REMOVE emits one
// candidate for the outer left operand (`a && b`) and one for the inner
// (`a`), both starting at `a`. Left tied, sort.Slice — which is not stable —
// could order them either way, and the two would swap the `#1` and `#2`
// suffixes of an otherwise identical ID, so the same string would name a
// different mutation from run to run. End offset separates them by span,
// innermost first; replacement is a final backstop for same-type candidates
// covering the identical span.
//
// Each field is compared with a < / > pair (no `!=` guard). The pattern
// keeps every `<` mutation observable: a guarded `if a != b { return a < b }`
// is equivalent under `<` ↔ `<=` mutation because the guard rules out the
// equal case where the boundary would matter.
func candidateLess(a, b mutator.MutantCandidate) bool {
	if a.Pos.Filename < b.Pos.Filename {
		return true
	}
	if a.Pos.Filename > b.Pos.Filename {
		return false
	}
	if a.Pos.Line < b.Pos.Line {
		return true
	}
	if a.Pos.Line > b.Pos.Line {
		return false
	}
	if a.Pos.Column < b.Pos.Column {
		return true
	}
	if a.Pos.Column > b.Pos.Column {
		return false
	}
	if a.Type < b.Type {
		return true
	}
	if a.Type > b.Type {
		return false
	}
	if a.EndOffset < b.EndOffset {
		return true
	}
	if a.EndOffset > b.EndOffset {
		return false
	}
	return a.Replacement < b.Replacement
}

// computeRelFile produces a gremlins-compatible module-relative path.
// If the candidate's package shares the common prefix, we strip it and
// append the file base; otherwise we fall back to filepath.Rel against
// moduleRoot. Factored out so the empty / no-prefix / non-matching-pkg
// cases can be unit-tested independently of Discover's wiring.
func computeRelFile(commonPrefix, pkg, absPath, moduleRoot string) string {
	if commonPrefix != "" && strings.HasPrefix(pkg, commonPrefix) {
		sub := strings.TrimPrefix(pkg, commonPrefix)
		sub = strings.TrimPrefix(sub, "/")
		if sub == "" {
			return filepath.Base(absPath)
		}
		return sub + "/" + filepath.Base(absPath)
	}
	rel, _ := filepath.Rel(moduleRoot, absPath)
	return rel
}

// longestCommonPrefix finds the longest common import path prefix across all packages.
func longestCommonPrefix(pkgs []Package) string {
	if len(pkgs) == 0 {
		return ""
	}
	prefix := pkgs[0].ImportPath
	for _, p := range pkgs[1:] {
		for !strings.HasPrefix(p.ImportPath, prefix) {
			// Compare against the sentinel returned by LastIndex when the
			// substring is absent. `< 0` is equivalent to `<= 0` here:
			// LastIndex never returns 0 for "/" since the path can't start
			// with one, so the slash==0 case never fires and `<` ↔ `<=` is
			// indistinguishable. Using `== -1` lets the boundary mutator
			// no-op and exposes the negation mutant `!= -1` to tests.
			slash := strings.LastIndex(prefix, "/")
			if slash == -1 {
				return ""
			}
			prefix = prefix[:slash]
		}
	}
	return prefix
}
