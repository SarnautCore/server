// Package credential hashes and verifies passwords with Argon2id, and encodes
// the result as a PHC string (ADR 0030 section 1).
//
// `golang.org/x/crypto/argon2` returns raw bytes and has no PHC encoder, so the
// encoding and the parsing are here. They are deliberately readable rather than
// clever: this is the code somebody reads during an incident.
//
// Verification re-derives with the parameters read from the stored string,
// never with the compile-time constants. Carrying the parameters in the record
// is what makes raising the cost later a rehash-on-next-login rather than a
// flag day.
package credential

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/SarnautCore/server/internal/auth/secret"
)

// Failures a caller can distinguish. A caller that turns any of them into a
// message for a peer must collapse them: which one it was is information about
// somebody else's account.
var (
	// ErrMismatch reports that the password does not derive the stored hash.
	ErrMismatch = errors.New("credential: password does not match")
	// ErrMalformedHash reports a stored string this package cannot parse.
	ErrMalformedHash = errors.New("credential: malformed PHC hash")
	// ErrEmptyPassword reports a registration or change with no password.
	ErrEmptyPassword = errors.New("credential: password must not be empty")
	// ErrPasswordTooShort reports a password under [MinimumPasswordLength].
	ErrPasswordTooShort = errors.New("credential: password is too short")
)

// Params are the Argon2id cost parameters. ADR 0030 fixes one set for every
// account; they are a value here so a stored hash can be verified against the
// parameters it was written with.
type Params struct {
	// Time is the number of passes (`t`).
	Time uint32
	// Memory is the memory cost in KiB (`m`).
	Memory uint32
	// Parallelism is the number of lanes (`p`).
	Parallelism uint8
	// SaltLength is the salt size in bytes, drawn fresh for every hash.
	SaltLength uint32
	// KeyLength is the derived key size in bytes.
	KeyLength uint32
}

// DefaultParams is the ADR 0030 section 1 table, and the only set new hashes are
// written with.
var DefaultParams = Params{
	Time:        3,
	Memory:      64 * 1024,
	Parallelism: 4,
	SaltLength:  16,
	KeyLength:   32,
}

// MinimumPasswordLength is a floor, not a policy. Password strength rules,
// rotation and reset are out of M2 scope (ADR 0030), but accepting a one
// character password would make the tests dishonest.
const MinimumPasswordLength = 8

// phcPrefix is the algorithm identifier and version every hash this package
// writes carries. Version 19 is 0x13, the Argon2 version `x/crypto` implements.
const (
	phcAlgorithm = "argon2id"
	phcVersion   = 19
)

// Hasher derives and verifies password hashes.
//
// The zero Hasher is usable and writes [DefaultParams]. A Hasher with cheaper
// parameters exists so a test suite that is not about cost can run in
// milliseconds; nothing in a running service constructs one.
type Hasher struct {
	// Params is what new hashes are written with. The zero value means
	// [DefaultParams].
	Params Params
}

func (hasher Hasher) params() Params {
	if hasher.Params == (Params{}) {
		return DefaultParams
	}
	return hasher.Params
}

// Hash derives a new PHC string for password, with a fresh salt.
func (hasher Hasher) Hash(password secret.Value) (string, error) {
	if password.Empty() {
		return "", ErrEmptyPassword
	}
	if password.Len() < MinimumPasswordLength {
		return "", ErrPasswordTooShort
	}
	parameters := hasher.params()
	salt := make([]byte, parameters.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("credential: read salt: %w", err)
	}
	return encode(parameters, salt, derive(password, salt, parameters)), nil
}

// Verify reports whether password derives encoded, using the parameters
// encoded records rather than this Hasher's.
func Verify(encoded string, password secret.Value) error {
	parameters, salt, want, err := decode(encoded)
	if err != nil {
		return err
	}
	got := derive(password, salt, parameters)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash reports whether a stored hash was written with parameters weaker
// than target, so a successful login can quietly upgrade it. Nothing calls this
// in M2; it is the reason the parameters travel in the record.
func NeedsRehash(encoded string, target Params) bool {
	parameters, _, _, err := decode(encoded)
	if err != nil {
		return true
	}
	return parameters.Time < target.Time ||
		parameters.Memory < target.Memory ||
		parameters.KeyLength < target.KeyLength
}

// DummyHash is a hash of a fixed passphrase nobody can log in with. A login
// against an unknown email verifies against it so that response time does not
// disclose whether an account exists (ADR 0030 section 1).
//
// It is derived once per process with the caller's parameters, because a login
// that skips the derivation would be exactly the timing signal this closes.
func (hasher Hasher) DummyHash() string {
	parameters := hasher.params()
	salt := make([]byte, parameters.SaltLength)
	copy(salt, "sarnaut-dummy-salt")
	password := secret.New("this account does not exist and never will")
	return encode(parameters, salt, derive(password, salt, parameters))
}

func derive(password secret.Value, salt []byte, parameters Params) []byte {
	return argon2.IDKey(
		[]byte(password.Reveal()),
		salt,
		parameters.Time,
		parameters.Memory,
		parameters.Parallelism,
		parameters.KeyLength,
	)
}

// encode writes `$argon2id$v=19$m=…,t=…,p=…$<salt>$<hash>` with both fields in
// base64.RawStdEncoding, which is the PHC convention: no padding.
func encode(parameters Params, salt, key []byte) string {
	return fmt.Sprintf(
		"$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		phcAlgorithm,
		phcVersion,
		parameters.Memory,
		parameters.Time,
		parameters.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

func decode(encoded string) (Params, []byte, []byte, error) {
	fields := strings.Split(encoded, "$")
	// A PHC string starts with `$`, so the split yields an empty first field
	// and then algorithm, version, parameters, salt and hash.
	if len(fields) != 6 || fields[0] != "" {
		return Params{}, nil, nil, fmt.Errorf("%w: want 5 fields, got %d", ErrMalformedHash, len(fields)-1)
	}
	if fields[1] != phcAlgorithm {
		return Params{}, nil, nil, fmt.Errorf("%w: algorithm %q is not %s", ErrMalformedHash, fields[1], phcAlgorithm)
	}
	var version int
	if _, err := fmt.Sscanf(fields[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: unreadable version %q", ErrMalformedHash, fields[2])
	}
	if version != phcVersion {
		return Params{}, nil, nil, fmt.Errorf("%w: version %d is not %d", ErrMalformedHash, version, phcVersion)
	}

	parameters := Params{}
	for _, pair := range strings.Split(fields[3], ",") {
		name, raw, found := strings.Cut(pair, "=")
		if !found {
			return Params{}, nil, nil, fmt.Errorf("%w: parameter %q has no value", ErrMalformedHash, pair)
		}
		number, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return Params{}, nil, nil, fmt.Errorf("%w: parameter %q: %w", ErrMalformedHash, pair, err)
		}
		switch name {
		case "m":
			parameters.Memory = uint32(number)
		case "t":
			parameters.Time = uint32(number)
		case "p":
			if number > 255 {
				return Params{}, nil, nil, fmt.Errorf("%w: parallelism %d exceeds 255", ErrMalformedHash, number)
			}
			parameters.Parallelism = uint8(number)
		default:
			return Params{}, nil, nil, fmt.Errorf("%w: unknown parameter %q", ErrMalformedHash, name)
		}
	}
	if parameters.Memory == 0 || parameters.Time == 0 || parameters.Parallelism == 0 {
		return Params{}, nil, nil, fmt.Errorf("%w: parameters %q are incomplete", ErrMalformedHash, fields[3])
	}

	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: unreadable salt: %w", ErrMalformedHash, err)
	}
	key, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: unreadable hash: %w", ErrMalformedHash, err)
	}
	if len(salt) == 0 || len(key) == 0 {
		return Params{}, nil, nil, fmt.Errorf("%w: empty salt or hash", ErrMalformedHash)
	}
	parameters.SaltLength = uint32(len(salt))
	parameters.KeyLength = uint32(len(key))
	return parameters, salt, key, nil
}
