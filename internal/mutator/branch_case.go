package mutator

import (
	"go/ast"
	"go/token"
)

type branchCase struct{}

func (b *branchCase) Type() MutationType { return BranchCase }

func (b *branchCase) Discover(fset *token.FileSet, file *ast.File, src []byte) []MutantCandidate {
	var candidates []MutantCandidate
	ast.Inspect(file, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		body := trimTrailingEmpty(cc.Body)
		if len(body) == 0 {
			return true
		}
		// Replace the body statements with a noop.
		// Body starts after the colon, ends at the last statement's end.
		firstStmt := body[0]
		lastStmt := body[len(body)-1]
		startPos := fset.Position(firstStmt.Pos())
		endPos := fset.Position(lastStmt.End())
		startOffset := startPos.Offset
		endOffset := endPos.Offset
		original := string(src[startOffset:endOffset])
		candidates = append(candidates, MutantCandidate{
			Type:        BranchCase,
			Pos:         Position{Filename: startPos.Filename, Line: startPos.Line, Column: startPos.Column, Offset: startOffset},
			Original:    original,
			Replacement: "_ = 0",
			StartOffset: startOffset,
			EndOffset:   endOffset,
		})
		return true
	})
	return candidates
}

// trimTrailingEmpty drops the explicit empty statements that end stmts. In
// unformatted source such a `;` can be the only separator before the next
// clause — `case 0:;default:` — so a body span reaching it would swallow
// the separator and leave `_ = 0default:`. A body of nothing else has no
// effect to remove and gets no mutant. Written as recursion rather than a
// loop for the reason unwrapParens gives.
func trimTrailingEmpty(stmts []ast.Stmt) []ast.Stmt {
	if n := len(stmts); n > 0 {
		if _, ok := stmts[n-1].(*ast.EmptyStmt); ok {
			return trimTrailingEmpty(stmts[:n-1])
		}
	}
	return stmts
}
