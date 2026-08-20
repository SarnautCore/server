package store_test

import (
	"testing"

	"github.com/SarnautCore/server/internal/store"
)

func TestMemoryRepositoryConformance(t *testing.T) {
	t.Parallel()

	runRepositoryConformance(t, func(t *testing.T) store.Repository {
		t.Helper()
		return store.NewMemory()
	})
}

func TestNormalizeCharacterName(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"Anne":     "anne",
		"O'brien":  "obrien",
		"Ob-rien":  "obrien",
		"Obrien":   "obrien",
		"Jean-luc": "jeanluc",
	}
	for name, want := range cases {
		if got := store.NormalizeCharacterName(name); got != want {
			t.Errorf("NormalizeCharacterName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	t.Parallel()

	if got := store.NormalizeEmail("  Player@Example.INVALID "); got != "player@example.invalid" {
		t.Errorf("NormalizeEmail = %q, want player@example.invalid", got)
	}
}
