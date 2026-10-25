package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gomutants/gomutants/internal/mutator"
)

// reportScriptOpenTag is the opening tag of the <script> block that carries
// the embedded report JSON, defined in html.go's htmlChromeMid fragment. If
// that fragment renames or restyles the tag, update this constant; the
// round-trip tests rely on it.
const reportScriptOpenTag = `<script id="mutation-report-data" type="application/json">`

// extractEmbeddedReport pulls the JSON payload out of the report data
// <script> block in the HTML output.
func extractEmbeddedReport(t *testing.T, html string) strykerReport {
	t.Helper()
	_, rest, ok := strings.Cut(html, reportScriptOpenTag)
	if !ok {
		t.Fatalf("HTML missing %q script block", reportScriptOpenTag)
	}
	body, _, ok := strings.Cut(rest, `</script>`)
	if !ok {
		t.Fatalf("HTML script block not terminated")
	}
	var rep strykerReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &rep); err != nil {
		t.Fatalf("embedded JSON did not parse: %v", err)
	}
	return rep
}

func TestWriteHTML_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.go")
	src := "package x\n\nfunc add(a, b int) int { return a + b }\n"
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	plusOff := strings.Index(src, "+")
	plusLine, plusCol := newFileIndex([]byte(src)).lineCol(plusOff)

	mutants := []mutator.Mutant{
		{
			ID: 1, Type: mutator.ArithmeticBase, File: srcPath, RelFile: "src.go",
			Line: plusLine, Col: plusCol, Original: "+", Replacement: "-",
			StartOffset: plusOff, EndOffset: plusOff + 1, Status: mutator.StatusLived,
		},
		{
			ID: 2, Type: mutator.ArithmeticBase, File: srcPath, RelFile: "src.go",
			Line: plusLine, Col: plusCol, Original: "+", Replacement: "*",
			StartOffset: plusOff, EndOffset: plusOff + 1, Status: mutator.StatusKilled,
		},
	}

	out := filepath.Join(dir, "report.html")
	if err := WriteHTML(out, mutants, "/proj", "0.1.0"); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	html := string(raw)

	// htmlChromePrefix is the only fragment that carries the doctype and
	// page <head>; asserting it lands at the very start kills a mutant that
	// drops the prefix write.
	if !strings.HasPrefix(html, "<!DOCTYPE html>") {
		t.Errorf("HTML missing <!DOCTYPE html> prefix; htmlChromePrefix not emitted")
	}
	if !strings.Contains(html, "<mutation-test-report-app") {
		t.Errorf("HTML missing <mutation-test-report-app> element")
	}
	// Pin that the JS bundle is embedded — the IIFE assigns a known global.
	if !strings.Contains(html, "MutationTestElements") {
		t.Errorf("HTML missing vendored JS bundle (no MutationTestElements global)")
	}

	rep := extractEmbeddedReport(t, html)
	if rep.SchemaVersion != "2" {
		t.Errorf("SchemaVersion=%q, want 2", rep.SchemaVersion)
	}
	if rep.Framework == nil || rep.Framework.Name != "gomutants" || rep.Framework.Version != "0.1.0" {
		t.Errorf("Framework=%+v", rep.Framework)
	}
	if rep.ProjectRoot != "/proj" {
		t.Errorf("ProjectRoot=%q", rep.ProjectRoot)
	}
	file, ok := rep.Files["src.go"]
	if !ok {
		t.Fatalf("missing src.go in files: %+v", rep.Files)
	}
	if file.Source != src {
		t.Errorf("Source not preserved verbatim")
	}
	if len(file.Mutants) != 2 {
		t.Fatalf("Mutants=%d, want 2", len(file.Mutants))
	}
	if file.Mutants[0].Status != "Survived" || file.Mutants[1].Status != "Killed" {
		t.Errorf("status mapping broken: %+v", file.Mutants)
	}
}

// TestWriteHTML_SelfContained pins the headline property: the produced HTML
// must not pull any external resources at view time. If the template grows a
// CDN <script src=...> or <link rel="stylesheet" href=...>, this test catches
// it before the change ships.
//
// The check inspects only the static template chrome (everything outside
// <script> / <style> blocks) — the vendored JS bundle's source contains URL
// string literals used at runtime to build DOM, but those don't trigger
// network loads when the page opens.
func TestWriteHTML_SelfContained(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.go")
	if err := os.WriteFile(srcPath, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, File: srcPath, RelFile: "src.go", Line: 1, Col: 1, Status: mutator.StatusKilled},
	}
	out := filepath.Join(dir, "r.html")
	if err := WriteHTML(out, mutants, "/p", "0.1.0"); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	raw, _ := os.ReadFile(out)

	chrome := scriptStyleBlock.ReplaceAllString(string(raw), "")
	for _, needle := range []string{`<script src=`, `<link `, `<iframe `} {
		if strings.Contains(chrome, needle) {
			t.Errorf("HTML report contains %q in static chrome; report must be self-contained", needle)
		}
	}
}

// RE2 has no backreferences, so the two tag pairs are spelled out separately.
var scriptStyleBlock = regexp.MustCompile(`(?s)<script\b[^>]*>.*?</script>|<style\b[^>]*>.*?</style>`)

// TestWriteHTML_PropagatesReadError mirrors the equivalent stryker test:
// missing source files surface as an error rather than producing a half-built
// report.
func TestWriteHTML_PropagatesReadError(t *testing.T) {
	dir := t.TempDir()
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase,
			File: filepath.Join(dir, "missing.go"), RelFile: "missing.go",
			Line: 1, Col: 1, Status: mutator.StatusKilled},
	}
	err := WriteHTML(filepath.Join(dir, "r.html"), mutants, "/p", "0.1.0")
	if err == nil {
		t.Fatal("expected error for unreadable source file")
	}
	if !strings.Contains(err.Error(), "missing.go") {
		t.Errorf("error %q should mention missing file", err)
	}
}

// TestWriteHTML_PropagatesMarshalError covers the marshalJSON error branch in
// WriteHTML. marshalJSON is a package-level swappable var (json.go) for
// exactly this purpose; without this test, the `return err` after marshalJSON
// is dead code as far as the test suite can tell.
func TestWriteHTML_PropagatesMarshalError(t *testing.T) {
	orig := marshalJSON
	marshalJSON = func(v any) ([]byte, error) {
		return nil, fmt.Errorf("injected marshal error")
	}
	defer func() { marshalJSON = orig }()

	out := filepath.Join(t.TempDir(), "r.html")
	err := WriteHTML(out, nil, "/p", "0.1.0")
	if err == nil {
		t.Fatal("expected error from marshalJSON")
	}
	if !strings.Contains(err.Error(), "injected marshal error") {
		t.Errorf("error %q should propagate the marshalJSON failure", err)
	}
}

// TestWriteHTML_EmbeddedJSONHandlesScriptInSource pins the JSON-escaping
// behavior that keeps embedded JSON safe inside <script type="application/json">.
// If a Go file under test contains a literal "</script>" string, the
// HTML-escaping done by encoding/json must convert the `<` to `<`; without
// it, the browser would terminate the script block early and corrupt the
// payload.
func TestWriteHTML_EmbeddedJSONHandlesScriptInSource(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.go")
	src := "package x\n\nvar s = \"</script>\"\n"
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mutants := []mutator.Mutant{
		{ID: 1, Type: mutator.ArithmeticBase, File: srcPath, RelFile: "src.go",
			Line: 3, Col: 1, Status: mutator.StatusKilled},
	}
	out := filepath.Join(dir, "r.html")
	if err := WriteHTML(out, mutants, "/p", "0.1.0"); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	raw, _ := os.ReadFile(out)
	html := string(raw)

	// The JSON body must NOT contain a literal `</script` (the HTML parser
	// would treat that as the end of the data block); json.Marshal escapes
	// `<` to `<` so the round-trip survives.
	_, after, ok := strings.Cut(html, reportScriptOpenTag)
	if !ok {
		t.Fatal("missing data block")
	}
	body, _, ok := strings.Cut(after, "</script>")
	if !ok {
		t.Fatal("data block not terminated")
	}
	if strings.Contains(body, "</script") {
		t.Errorf("embedded JSON contains literal </script — would break the page")
	}

	// Round-trip parse should still succeed and preserve the source verbatim.
	rep := extractEmbeddedReport(t, html)
	if rep.Files["src.go"].Source != src {
		t.Errorf("source not preserved verbatim through HTML embedding")
	}
}
