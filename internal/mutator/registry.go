package mutator

import (
	"go/ast"
	"go/token"
	"slices"
	"strings"
)

// Mutator discovers mutation candidates in a parsed Go source file.
type Mutator interface {
	Type() MutationType
	Discover(fset *token.FileSet, file *ast.File, src []byte) []MutantCandidate
}

// CatalogEntry is the user-facing description of one registered mutator.
// Example is representative rather than an exhaustive list of every rewrite
// the mutator can emit.
type CatalogEntry struct {
	Type        MutationType
	Description string
	Example     string
}

// registration keeps a mutator implementation and its user-facing metadata
// together. The operational Mutator interface stays focused on discovery,
// while NewRegistry remains the single place where built-ins become runnable
// and discoverable through the CLI catalog.
type registration struct {
	mutator     Mutator
	description string
	example     string
}

func register(m Mutator, description, example string) registration {
	return registration{mutator: m, description: description, example: example}
}

// Registry holds all registered mutators. typeSet mirrors mutators by
// MutationType so name-validation lookups (IsKnown / UnknownNames) and
// the directive parser share a single source of truth populated in
// NewRegistry. registrations is the source for the user-facing catalog;
// mutators is its operational projection retained for discovery/filtering.
type Registry struct {
	registrations []registration
	mutators      []Mutator
	typeSet       map[string]struct{}
}

// NewRegistry creates a registry with all built-in mutators.
func NewRegistry() *Registry {
	registrations := []registration{
		register(&arithmeticBase{}, "Swap arithmetic operators", "+ <-> -, * <-> /, % -> *"),
		register(&conditionalsBoundary{}, "Relax/tighten boundaries", "< <-> <=, > <-> >="),
		register(&conditionalsNegation{}, "Negate comparisons", "== <-> !=, < <-> >=, > <-> <="),
		register(&incrementDecrement{}, "Swap increment/decrement", "++ <-> --"),
		register(&invertNegatives{}, "Invert negation", "-x -> +x, a - b -> a + b"),
		register(&invertAssignments{}, "Swap arithmetic compound assignments", "+= <-> -=, *= <-> /=, %= -> *="),
		register(&invertBitwise{}, "Swap bitwise binary operators", "& <-> |, ^ -> &, &^ -> &, << <-> >>"),
		register(&invertBitwiseAssignments{}, "Swap bitwise compound assignments", "&= <-> |=, ^= -> &=, &^= -> &=, <<= <-> >>="),
		register(&invertLogical{}, "Swap logical operators", "&& <-> ||"),
		register(&invertLoopCtrl{}, "Swap loop control", "break <-> continue"),
		register(&removeSelfAssignments{}, "Drop op from compound assignment", "x += y -> x = y"),
		register(&removeLogicalNot{}, "Drop ! from a logical negation", "if !ok -> if ok"),
		register(&errorfWrap{}, "Downgrade the error-wrapping verb", `fmt.Errorf("load: %w", err) -> fmt.Errorf("load: %v", err)`),
		register(&branchIf{}, "Empty if/else-if body", "if x { f() } -> if x { _ = 0 }"),
		register(&branchElse{}, "Empty else body", "else { f() } -> else { _ = 0 }"),
		register(&branchCase{}, "Empty case body", "case 1: f() -> case 1: _ = 0"),
		register(&expressionRemove{}, "Remove boolean operand", "a && b -> true && b"),
		register(&statementRemove{}, "Remove statement effect", "x = expr -> _ = expr, f() -> _ = 0"),
		register(&literalStep{typ: IntegerIncrement, kind: token.INT, delta: 1}, "Increment integer literal", "42 -> 43, 0xFF -> 256"),
		register(&literalStep{typ: IntegerDecrement, kind: token.INT, delta: -1}, "Decrement integer literal", "42 -> 41, 0 -> -1"),
		register(&literalStep{typ: FloatIncrement, kind: token.FLOAT, delta: 1}, "Increment float literal", "1.5 -> 2.5, 0.0 -> 1.0"),
		register(&literalStep{typ: FloatDecrement, kind: token.FLOAT, delta: -1}, "Decrement float literal", "1.5 -> 0.5, 1e2 -> 99.0"),
		register(&loopCondition{}, "Force for-loop condition to false", "for i := 0; i < n; i++ {} -> for i := 0; false; i++ {}"),
		register(&rangeBreak{}, "Insert early break in for-range body", "for _, v := range xs { f(v) } -> for _, v := range xs { break; f(v) }"),
		register(&returnValue{typ: ReturnErrorNil, owns: ownsError, replacement: fixed("nil")}, "Swallow a propagated error", "return nil, err -> return nil, nil"),
		register(&returnValue{typ: ReturnZero, owns: ownsOther, replacement: zeroValueExpr}, "Return the zero value instead", "return count -> return 0"),
		register(&returnValue{typ: ReturnTrue, owns: ownsBool, replacement: fixed("true")}, "Force a boolean return true", "return x > 0 -> return true"),
		register(&returnValue{typ: ReturnFalse, owns: ownsBool, replacement: fixed("false")}, "Force a boolean return false", "return x > 0 -> return false"),
	}
	return newRegistry(registrations)
}

func newRegistry(registrations []registration) *Registry {
	mutators := make([]Mutator, 0, len(registrations))
	typeSet := make(map[string]struct{}, len(registrations))
	for _, entry := range registrations {
		typ := string(entry.mutator.Type())
		if typ == "" {
			panic("mutator registry contains an empty mutation type")
		}
		if _, duplicate := typeSet[typ]; duplicate {
			panic("mutator registry contains duplicate type " + typ)
		}
		if !validCatalogText(entry.description) {
			panic("mutator " + typ + " has an invalid catalog description")
		}
		if !validCatalogText(entry.example) {
			panic("mutator " + typ + " has an invalid catalog example")
		}
		mutators = append(mutators, entry.mutator)
		typeSet[typ] = struct{}{}
	}
	return &Registry{registrations: registrations, mutators: mutators, typeSet: typeSet}
}

// validCatalogText keeps the one-entry-per-line, tab-delimited rendering
// unambiguous and rejects accidental whitespace-only metadata.
func validCatalogText(s string) bool {
	return strings.TrimSpace(s) == s && s != "" && !strings.ContainsAny(s, "\t\r\n")
}

// Mutators returns all registered mutators.
func (r *Registry) Mutators() []Mutator {
	return r.mutators
}

// Catalog returns a stable, alphabetical snapshot of every registered
// mutator's user-facing metadata.
func (r *Registry) Catalog() []CatalogEntry {
	entries := make([]CatalogEntry, 0, len(r.registrations))
	for _, entry := range r.registrations {
		entries = append(entries, CatalogEntry{
			Type:        entry.mutator.Type(),
			Description: entry.description,
			Example:     entry.example,
		})
	}
	slices.SortFunc(entries, func(a, b CatalogEntry) int {
		return strings.Compare(string(a.Type), string(b.Type))
	})
	return entries
}

// IsKnown reports whether name matches a registered mutator type.
func (r *Registry) IsKnown(name string) bool {
	_, ok := r.typeSet[name]
	return ok
}

// UnknownNames returns the subset of names that don't match any
// registered mutator type. Used by callers that accept user-supplied
// mutator lists (--only / --disable, config file) to surface typos
// before silently filtering them out.
func (r *Registry) UnknownNames(names []string) []string {
	var unknown []string
	for _, n := range names {
		if !r.IsKnown(n) {
			unknown = append(unknown, n)
		}
	}
	return unknown
}

// EnabledMutators returns mutators filtered by the given only/disable lists.
// If only is non-empty, only those types are included.
// Otherwise, disabled types are excluded.
func (r *Registry) EnabledMutators(only, disable []string) []Mutator {
	if len(only) > 0 {
		set := make(map[string]bool, len(only))
		for _, t := range only {
			set[t] = true
		}
		var out []Mutator
		for _, m := range r.mutators {
			if set[string(m.Type())] {
				out = append(out, m)
			}
		}
		return out
	}

	set := make(map[string]bool, len(disable))
	for _, t := range disable {
		set[t] = true
	}
	var out []Mutator
	for _, m := range r.mutators {
		if !set[string(m.Type())] {
			out = append(out, m)
		}
	}
	return out
}
