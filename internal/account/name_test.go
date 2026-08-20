package account

import "testing"

// The accept and reject tables are ADR 0032 §3's, case for case. The Cyrillic
// homoglyph is the one that matters most: it must be rejected by the ASCII
// character class rather than accidentally accepted, because two visually
// identical names is the failure the whole name policy exists to prevent.
func TestValidNameAcceptsAndRejectsTheADRTable(t *testing.T) {
	t.Parallel()

	accepted := []string{"Abc", "Anne", "O'brien", "Jean-luc", "Averyverylongnam"}
	for _, name := range accepted {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false, want true", name)
		}
	}

	rejected := map[string]string{
		"Ab":                    "too short: it passes the pattern and fails the length check",
		"A'":                    "trailing punctuation",
		"Ann'":                  "trailing punctuation",
		"Ann--e":                "adjacent punctuation",
		"-anne":                 "leading punctuation",
		"ANNE":                  "interior uppercase",
		"Ann3":                  "digit",
		"Ann e":                 "space",
		"Averyveryverylongname": "too long",
		"Аnne":                  "Cyrillic homoglyph",
		"":                      "empty",
		"anne":                  "lowercase first letter",
	}
	for name, reason := range rejected {
		if ValidName(name) {
			t.Errorf("ValidName(%q) = true, want false (%s)", name, reason)
		}
	}
}

// Normalization is what makes impersonation by punctuation impossible: the
// three spellings collide on one unique index entry.
func TestNormalizeNameCollapsesPunctuationAndCase(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"O'brien", "Obrien", "Ob-rien", "OBRIEN"} {
		if got := NormalizeName(name); got != "obrien" {
			t.Errorf("NormalizeName(%q) = %q, want %q", name, got, "obrien")
		}
	}
	if NormalizeName("Jean-luc") == NormalizeName("Jeanluk") {
		t.Error("normalization collapsed two names that are genuinely different")
	}
}

func TestBlocklistMatchesNormalizedSubstrings(t *testing.T) {
	t.Parallel()

	blocked := []string{"Gm", "Gmoverlord", "Admin", "Ad-min", "Sarnaut", "Xsarnautx"}
	for _, name := range blocked {
		if !blockedName(DefaultNameBlocklist, NormalizeName(name)) {
			t.Errorf("blockedName(%q) = false, want true", name)
		}
	}
	allowed := []string{"Anne", "Jean-luc", "O'brien", "Gnome"}
	for _, name := range allowed {
		if blockedName(DefaultNameBlocklist, NormalizeName(name)) {
			t.Errorf("blockedName(%q) = true, want false", name)
		}
	}
	// An empty entry must not match every name, which is what a trailing comma
	// in a configured list would otherwise produce.
	if blockedName([]string{""}, "anne") {
		t.Error("an empty blocklist entry matched a name")
	}
}
