// Package secret carries values that must never reach a log, a span attribute,
// a metric label or an error string: passwords, email addresses and tokens
// (ADR 0030 section 5).
//
// The rule is mechanical rather than a habit. [Value] implements every
// interface a value is normally rendered through — [fmt.Stringer],
// [slog.LogValuer], [encoding.TextMarshaler] and [json.Marshaler] — and each of
// them returns the redaction. An accidental `%v`, `%s`, `%q`, `slog` attribute
// or JSON encode is therefore already redacted, and reaching the real bytes
// requires calling [Value.Reveal], which is one grep away.
package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// Redaction is what every rendering of a [Value] produces.
const Redaction = "[redacted]"

// Value is a secret string.
//
// The zero Value is empty and still redacts. It is comparable, so it may be a
// map key or a struct field compared with ==, and it is safe to copy.
type Value struct {
	inner string
}

// New wraps a secret.
func New(raw string) Value { return Value{inner: raw} }

// Reveal returns the secret itself. Every call site is a deliberate decision to
// handle plaintext, which is why this is a method rather than a field.
func (value Value) Reveal() string { return value.inner }

// Empty reports whether the secret carries nothing.
func (value Value) Empty() bool { return value.inner == "" }

// Len is the length of the secret in bytes. Useful for shape validation that
// would otherwise need the plaintext.
func (value Value) Len() int { return len(value.inner) }

// String satisfies [fmt.Stringer].
func (value Value) String() string { return Redaction }

// GoString satisfies [fmt.GoStringer], so `%#v` redacts too.
func (value Value) GoString() string { return Redaction }

// Format satisfies [fmt.Formatter], which covers verbs [fmt.Stringer] does not:
// `%q`, `%x`, `%d` and anything else a caller reaches for.
func (value Value) Format(state fmt.State, verb rune) {
	switch verb {
	case 'q':
		_, _ = fmt.Fprintf(state, "%q", Redaction)
	default:
		_, _ = state.Write([]byte(Redaction))
	}
}

// LogValue satisfies [slog.LogValuer].
func (value Value) LogValue() slog.Value { return slog.StringValue(Redaction) }

// MarshalText satisfies [encoding.TextMarshaler].
func (value Value) MarshalText() ([]byte, error) { return []byte(Redaction), nil }

// MarshalJSON satisfies [json.Marshaler].
func (value Value) MarshalJSON() ([]byte, error) { return json.Marshal(Redaction) }

// UnmarshalJSON reads a secret out of a request body. Decoding into a Value is
// the point: the field is a secret from the moment it is parsed, not from the
// moment somebody remembers to wrap it.
func (value *Value) UnmarshalJSON(payload []byte) error {
	var raw string
	if err := json.Unmarshal(payload, &raw); err != nil {
		return err
	}
	value.inner = raw
	return nil
}

// Digest is the SHA-256 of the secret as lowercase hex. It is the only form
// that may be stored: the plaintext is never written to Valkey, to PostgreSQL
// or to a log, so a database dump yields no usable credential (ADR 0030
// section 2).
func (value Value) Digest() string {
	sum := sha256.Sum256([]byte(value.inner))
	return hex.EncodeToString(sum[:])
}

// ID is the first 16 hex characters of [Value.Digest]: the one derived form of
// a token that may appear in a log line, so two records can be tied together
// without either carrying the credential.
func (value Value) ID() string { return value.Digest()[:16] }

// EmailDomain is the part of an email address after the last `@`, lowercased.
// It is the only derived form of an email address permitted in telemetry.
// A value that is not an address yields the empty string rather than leaking
// what it actually was.
func (value Value) EmailDomain() string {
	at := strings.LastIndex(value.inner, "@")
	if at < 0 || at == len(value.inner)-1 {
		return ""
	}
	return strings.ToLower(value.inner[at+1:])
}
