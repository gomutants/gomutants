package mutator_test

import (
	"strings"
	"testing"

	"github.com/gomutants/gomutants/internal/mutator"
)

func TestRegistryCatalogCoversRegisteredMutators(t *testing.T) {
	reg := mutator.NewRegistry()
	catalog := reg.Catalog()
	registered := reg.Mutators()

	if got, want := len(catalog), len(registered); got != want {
		t.Fatalf("Catalog() has %d entries, registry has %d mutators", got, want)
	}

	wantTypes := make(map[mutator.MutationType]bool, len(registered))
	for _, m := range registered {
		if wantTypes[m.Type()] {
			t.Fatalf("registry contains duplicate mutator type %q", m.Type())
		}
		wantTypes[m.Type()] = true
	}

	seen := make(map[mutator.MutationType]bool, len(catalog))
	for _, entry := range catalog {
		if !wantTypes[entry.Type] {
			t.Errorf("catalog contains unregistered type %q", entry.Type)
		}
		if seen[entry.Type] {
			t.Errorf("catalog contains duplicate type %q", entry.Type)
		}
		seen[entry.Type] = true
	}
}

func TestRegistryCatalogHasValidMetadata(t *testing.T) {
	for _, entry := range mutator.NewRegistry().Catalog() {
		assertValidCatalogText(t, entry.Type, "description", entry.Description)
		assertValidCatalogText(t, entry.Type, "example", entry.Example)
	}
}

func assertValidCatalogText(t *testing.T, mutationType mutator.MutationType, field, value string) {
	t.Helper()

	if value == "" || strings.TrimSpace(value) != value {
		t.Errorf("%s has invalid %s %q", mutationType, field, value)
	}
	if strings.ContainsAny(value, "\t\r\n") {
		t.Errorf("%s %s contains a catalog delimiter: %q", mutationType, field, value)
	}
}

func TestRegistryCatalogIsSortedAndIndependent(t *testing.T) {
	reg := mutator.NewRegistry()
	catalog := reg.Catalog()

	for i := 1; i < len(catalog); i++ {
		if catalog[i-1].Type >= catalog[i].Type {
			t.Fatalf("catalog is not strictly sorted at %q, %q", catalog[i-1].Type, catalog[i].Type)
		}
	}

	original := catalog[0]
	catalog[0] = mutator.CatalogEntry{}
	got := reg.Catalog()[0]
	if got != original {
		t.Fatalf("Catalog returned mutable registry state: got %+v, want %+v", got, original)
	}
}
