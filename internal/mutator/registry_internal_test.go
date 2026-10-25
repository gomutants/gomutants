package mutator

import (
	"strings"
	"testing"
)

func TestNewRegistryRejectsInvalidRegistrations(t *testing.T) {
	tests := []struct {
		name          string
		registrations []registration
		wantPanic     string
	}{
		{
			name:          "empty type",
			registrations: []registration{register(&literalStep{}, "description", "example")},
			wantPanic:     "empty mutation type",
		},
		{
			name: "duplicate type",
			registrations: []registration{
				register(&arithmeticBase{}, "description", "example"),
				register(&arithmeticBase{}, "other description", "other example"),
			},
			wantPanic: "duplicate type ARITHMETIC_BASE",
		},
		{
			name:          "invalid description",
			registrations: []registration{register(&arithmeticBase{}, "", "example")},
			wantPanic:     "invalid catalog description",
		},
		{
			name:          "invalid example",
			registrations: []registration{register(&arithmeticBase{}, "description", "bad\nexample")},
			wantPanic:     "invalid catalog example",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				got := recover()
				if got == nil || !strings.Contains(got.(string), tc.wantPanic) {
					t.Fatalf("panic = %v, want text %q", got, tc.wantPanic)
				}
			}()
			newRegistry(tc.registrations)
		})
	}
}

func TestValidCatalogText(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"description", true},
		{"", false},
		{" padded", false},
		{"bad\tvalue", false},
		{"bad\rvalue", false},
		{"bad\nvalue", false},
	}

	for _, tc := range tests {
		if got := validCatalogText(tc.value); got != tc.want {
			t.Errorf("validCatalogText(%q) = %t, want %t", tc.value, got, tc.want)
		}
	}
}
