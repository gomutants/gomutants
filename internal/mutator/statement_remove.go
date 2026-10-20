package mutator

import (
	"fmt"
	"go/ast"
	"go/token"
)

type statementRemove struct{}

func (s *statementRemove) Type() MutationType { return StatementRemove }

func (s *statementRemove) Discover(fset *token.FileSet, file *ast.File, src []byte) []MutantCandidate {
	var candidates []MutantCandidate
	ast.Inspect(file, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			// Only plain assignment (=), not short declaration (:=).
			if stmt.Tok != token.ASSIGN {
				return true
			}
			// Parser can — under recovery from earlier syntax errors — yield
			// an AssignStmt with no Rhs. Skip rather than deref Rhs[0].
			if len(stmt.Rhs) == 0 {
				return true
			}
			// Skip "_ = expr": replacement would be byte-identical to the
			// original, producing a phantom LIVED mutant that no test can
			// kill (since the source isn't actually changed).
			if isBlankLhs(stmt.Lhs) {
				return true
			}
			// Replace "x = expr" with "_ = expr".
			// The replacement keeps the RHS, so its calls still run and
			// nothing it uses goes unused (patch.ApplyMutant would put
			// back an import or local the mutant orphaned anyway).
			pos := fset.Position(stmt.Pos())
			end := fset.Position(stmt.End())
			startOffset := pos.Offset
			endOffset := end.Offset

			// Build "_ = <rhs>" from the source bytes of the RHS.
			rhsStart := fset.Position(stmt.Rhs[0].Pos()).Offset
			rhsEnd := fset.Position(stmt.Rhs[len(stmt.Rhs)-1].End()).Offset
			rhs := string(src[rhsStart:rhsEnd])
			replacement := fmt.Sprintf("_ = %s", rhs)

			original := string(src[startOffset:endOffset])
			candidates = append(candidates, MutantCandidate{
				Type:        StatementRemove,
				Pos:         Position{Filename: pos.Filename, Line: pos.Line, Column: pos.Column, Offset: startOffset},
				Original:    original,
				Replacement: replacement,
				StartOffset: startOffset,
				EndOffset:   endOffset,
			})

		case *ast.ExprStmt:
			pos := fset.Position(stmt.Pos())
			end := fset.Position(stmt.End())
			startOffset := pos.Offset
			endOffset := end.Offset
			original := string(src[startOffset:endOffset])
			candidates = append(candidates, MutantCandidate{
				Type:        StatementRemove,
				Pos:         Position{Filename: pos.Filename, Line: pos.Line, Column: pos.Column, Offset: startOffset},
				Original:    original,
				Replacement: "_ = 0",
				StartOffset: startOffset,
				EndOffset:   endOffset,
			})

		case *ast.IncDecStmt:
			pos := fset.Position(stmt.Pos())
			end := fset.Position(stmt.End())
			startOffset := pos.Offset
			endOffset := end.Offset
			original := string(src[startOffset:endOffset])

			// Replace "x++" / "x--" with "_ = x".
			xStart := fset.Position(stmt.X.Pos()).Offset
			xEnd := fset.Position(stmt.X.End()).Offset
			xText := string(src[xStart:xEnd])
			replacement := fmt.Sprintf("_ = %s", xText)

			candidates = append(candidates, MutantCandidate{
				Type:        StatementRemove,
				Pos:         Position{Filename: pos.Filename, Line: pos.Line, Column: pos.Column, Offset: startOffset},
				Original:    original,
				Replacement: replacement,
				StartOffset: startOffset,
				EndOffset:   endOffset,
			})
		}
		return true
	})
	return candidates
}

func isBlankLhs(lhs []ast.Expr) bool {
	if len(lhs) != 1 {
		return false
	}
	id, ok := lhs[0].(*ast.Ident)
	return ok && id.Name == "_"
}
