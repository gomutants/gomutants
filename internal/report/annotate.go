package report

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/gomutants/gomutants/internal/mutator"
)

// WriteGitHubAnnotations writes one ::warning:: workflow command per LIVED
// mutant in r to w. The format is consumed by GitHub Actions and surfaces
// as inline annotations on the PR diff. See:
// https://docs.github.com/en/actions/using-workflows/workflow-commands-for-github-actions
func WriteGitHubAnnotations(w io.Writer, r *Report) error {
	livedStatus := mutator.StatusLived.String()
	bw := bufio.NewWriter(w)
	for _, f := range r.Files {
		for _, m := range f.Mutations {
			if m.Status != livedStatus {
				continue
			}
			fmt.Fprintf(
				bw,
				"::warning file=%s,line=%d,col=%d::%s\n",
				escapeProperty(f.FileName),
				m.Line,
				m.Column,
				escapeMessage(annotationMessage(m)),
			)
		}
	}
	return bw.Flush()
}

func annotationMessage(m MutationReport) string {
	if m.Original != "" || m.Replacement != "" {
		return fmt.Sprintf("Mutant LIVED — %s (%s → %s)", m.Type, m.Original, m.Replacement)
	}
	return fmt.Sprintf("Mutant LIVED — %s", m.Type)
}

// escapeMessage encodes characters that would otherwise terminate or break
// a workflow command's message segment.
var messageEscaper = strings.NewReplacer(
	"%", "%25",
	"\r", "%0D",
	"\n", "%0A",
)

func escapeMessage(s string) string { return messageEscaper.Replace(s) }

// escapeProperty encodes characters in a workflow-command property value.
// Properties additionally need to escape `:` and `,` — without this, a path
// containing a colon (Windows) or a comma would be parsed as a separator.
var propertyEscaper = strings.NewReplacer(
	"%", "%25",
	"\r", "%0D",
	"\n", "%0A",
	":", "%3A",
	",", "%2C",
)

func escapeProperty(s string) string { return propertyEscaper.Replace(s) }
