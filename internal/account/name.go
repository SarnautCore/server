package account

import (
	"regexp"
	"strings"

	"github.com/SarnautCore/server/internal/store"
)

// Character name shape (ADR 0032 section 3): 3 to 16 characters, an uppercase
// ASCII first letter, then lowercase ASCII letters with at most a single
// apostrophe or hyphen between runs of them.
const (
	MinimumNameLength = 3
	MaximumNameLength = 16
)

// namePattern is the shape check. Go's regexp is RE2 and has no lookahead, so
// the punctuation rule is expressed by requiring `[a-z]+` after every `'` or
// `-` rather than by an assertion.
//
// The second quantifier is `[a-z]*`, not `[a-z]+`, on purpose: `[a-z]+` would
// require a lowercase letter between the initial capital and the first
// punctuation mark, and so would reject `O'brien`.
var namePattern = regexp.MustCompile(`^[A-Z][a-z]*(?:['-][a-z]+)*$`)

// ValidName reports whether name has the shape ADR 0032 fixes.
//
// The length is a separate check rather than a `{2,15}` quantifier so the two
// rules fail separately. Because the pattern is ASCII-only, a name that matches
// has as many characters as bytes, and a Cyrillic homoglyph such as "Аnne" is
// rejected by the character class rather than accidentally accepted.
func ValidName(name string) bool {
	return len(name) >= MinimumNameLength &&
		len(name) <= MaximumNameLength &&
		namePattern.MatchString(name)
}

// NormalizeName produces the value the unique index is taken on: apostrophes
// and hyphens stripped, then lowercased. "O'brien", "Obrien" and "Ob-rien"
// therefore collide, which is the intent — impersonation by punctuation is the
// cheapest attack on a name system.
//
// The normalization itself lives in `internal/store`, next to the column whose
// behaviour it has to match; this is the name the rest of the service uses.
func NormalizeName(name string) string { return store.NormalizeCharacterName(name) }

// DefaultNameBlocklist is the M2 blocklist of ADR 0032 section 3: impersonation
// prefixes and nothing else. A half-hearted profanity list is worse than an
// honest empty one, and real moderation policy is not an M2 problem.
//
// ADR 0032 places the blocklist in a `chargen.name_blocklist` pack document.
// Until that document type exists it is configuration rather than code — see
// `config.AuthConfig.NameBlocklist` — so changing it stays a deploy rather than
// a rebuild. The substrings are matched against the normalized name.
var DefaultNameBlocklist = []string{"gm", "admin", "sarnaut"}

// blockedName reports whether normalized contains a blocked substring. It is
// checked before uniqueness so a blocked name never gets reserved.
func blockedName(blocklist []string, normalized string) bool {
	for _, blocked := range blocklist {
		if blocked != "" && strings.Contains(normalized, blocked) {
			return true
		}
	}
	return false
}
