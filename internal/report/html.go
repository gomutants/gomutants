package report

import (
	"bytes"
	_ "embed"

	"github.com/gomutants/gomutants/internal/mutator"
)

// htmlBundleJS is the vendored mutation-testing-elements UMD bundle. Embedded
// at build time so the produced HTML loads with no network access — see
// assets/README.md for the pinned version and refresh procedure.
//
//go:embed assets/mutation-test-elements.js
var htmlBundleJS string

// The HTML chrome is spliced in as three constant fragments around the JS
// bundle and JSON payload. A template engine isn't needed: there are only two
// substitutions and both land inside <script> blocks where HTML entity
// escaping does not apply. json.Marshal already escapes `<`, `>`, `&` to
// `\uXXXX`, which keeps any `</script>` substrings inside report data inert.
const (
	htmlChromePrefix = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Mutation Testing Report</title>
<style>html,body{margin:0;padding:0;height:100%;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;}</style>
<script>
`

	htmlChromeMid = `
</script>
</head>
<body>
<mutation-test-report-app title-postfix="Gomutants">
  Loading mutation testing report...
</mutation-test-report-app>
<script id="mutation-report-data" type="application/json">
`

	htmlChromeSuffix = `
</script>
<script>
(function(){
  var el = document.querySelector('mutation-test-report-app');
  var data = document.getElementById('mutation-report-data').textContent;
  el.report = JSON.parse(data);
})();
</script>
</body>
</html>
`
)

// WriteHTML writes a self-contained interactive HTML mutation report at path.
// The output bundles the Stryker mutation-testing-elements web component and
// the Stryker v2 report JSON into a single file with no external assets — it
// renders in any browser, including offline (air-gapped CI artifacts).
//
// frameworkVersion is recorded in the embedded report; pass the running
// gomutants version.
func WriteHTML(path string, mutants []mutator.Mutant, projectDir, frameworkVersion string) error {
	rep, err := buildStrykerReport(mutants, projectDir, frameworkVersion)
	if err != nil {
		return err
	}
	data, err := marshalJSON(rep)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	buf.WriteString(htmlChromePrefix)
	buf.WriteString(htmlBundleJS)
	buf.WriteString(htmlChromeMid)
	buf.Write(data)
	buf.WriteString(htmlChromeSuffix)

	return writeFile(path, buf.Bytes())
}
