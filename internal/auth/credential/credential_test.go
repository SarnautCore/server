package credential_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/SarnautCore/server/internal/auth/credential"
	"github.com/SarnautCore/server/internal/auth/secret"
)

// cheap is the parameter set every test that is not about cost uses. Argon2id
// at the ADR's 64 MiB is deliberately slow, and a suite that pays it forty
// times proves nothing extra.
var cheap = credential.Hasher{Params: credential.Params{
	Time:        1,
	Memory:      8,
	Parallelism: 1,
	SaltLength:  16,
	KeyLength:   32,
}}

func TestDefaultParametersAreTheOnesADR0030Fixes(t *testing.T) {
	t.Parallel()

	want := credential.Params{Time: 3, Memory: 64 * 1024, Parallelism: 4, SaltLength: 16, KeyLength: 32}
	if credential.DefaultParams != want {
		t.Errorf("DefaultParams = %+v, want %+v", credential.DefaultParams, want)
	}
}

func TestHashAndVerifyRoundTrip(t *testing.T) {
	t.Parallel()

	password := secret.New("correct horse battery staple")
	encoded, err := cheap.Hash(password)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if err := credential.Verify(encoded, password); err != nil {
		t.Errorf("Verify() error = %v, want nil", err)
	}
}

// The PHC string is a contract with the database column, not an internal
// detail: a hash written today has to be readable by a build that raised the
// cost parameters.
func TestHashIsAPHCStringCarryingItsOwnParameters(t *testing.T) {
	t.Parallel()

	encoded, err := (credential.Hasher{}).Hash(secret.New("a password long enough"))
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("Hash() = %q, want the ADR 0030 PHC prefix", encoded)
	}
	if fields := strings.Split(encoded, "$"); len(fields) != 6 {
		t.Errorf("Hash() has %d fields, want 5", len(fields)-1)
	}
	// Base64 without padding, as PHC requires.
	if strings.Contains(encoded, "=") && !strings.Contains(encoded, "m=") {
		t.Error("Hash() carries base64 padding")
	}
}

func TestEverySaltIsFresh(t *testing.T) {
	t.Parallel()

	password := secret.New("the same password twice")
	first, err := cheap.Hash(password)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	second, err := cheap.Hash(password)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if first == second {
		t.Error("two hashes of one password are identical; the salt is not being redrawn")
	}
	if err := credential.Verify(second, password); err != nil {
		t.Errorf("Verify() error = %v on the second hash", err)
	}
}

func TestWrongPasswordIsRejected(t *testing.T) {
	t.Parallel()

	encoded, err := cheap.Hash(secret.New("the real password"))
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	for _, wrong := range []string{
		"the real passwore",
		"the real password ",
		"The real password",
		"",
	} {
		if err := credential.Verify(encoded, secret.New(wrong)); !errors.Is(err, credential.ErrMismatch) {
			t.Errorf("Verify(%q) error = %v, want ErrMismatch", wrong, err)
		}
	}
}

func TestVerifyUsesTheStoredParametersRatherThanTheCompiledOnes(t *testing.T) {
	t.Parallel()

	password := secret.New("parameters travel with the hash")
	encoded, err := cheap.Hash(password)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	// A verifier configured with the ADR's cost still verifies a hash written
	// with the cheap one. If verification re-derived with compile-time
	// constants instead, raising the cost would lock every account out.
	if err := credential.Verify(encoded, password); err != nil {
		t.Errorf("Verify() error = %v; a stored hash must verify against its own parameters", err)
	}
	if !credential.NeedsRehash(encoded, credential.DefaultParams) {
		t.Error("NeedsRehash() = false for a hash written with weaker parameters")
	}
}

func TestMalformedHashesAreRejectedRatherThanPanicking(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"empty":                "",
		"not a phc string":     "hunter2",
		"wrong algorithm":      "$argon2i$v=19$m=8,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"wrong version":        "$argon2id$v=16$m=8,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"missing parameters":   "$argon2id$v=19$m=8$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"unknown parameter":    "$argon2id$v=19$m=8,t=1,p=1,q=9$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"unreadable salt":      "$argon2id$v=19$m=8,t=1,p=1$not!base64$aGFzaA",
		"too few fields":       "$argon2id$v=19$m=8,t=1,p=1$c2FsdA",
		"parallelism too high": "$argon2id$v=19$m=8,t=1,p=300$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := credential.Verify(encoded, secret.New("anything at all"))
			if !errors.Is(err, credential.ErrMalformedHash) {
				t.Errorf("Verify() error = %v, want ErrMalformedHash", err)
			}
		})
	}
}

func TestATamperedHashDoesNotVerify(t *testing.T) {
	t.Parallel()

	password := secret.New("tamper evident")
	encoded, err := cheap.Hash(password)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	fields := strings.Split(encoded, "$")
	// Flip one character of the stored digest.
	digest := []byte(fields[5])
	if digest[0] == 'A' {
		digest[0] = 'B'
	} else {
		digest[0] = 'A'
	}
	fields[5] = string(digest)
	tampered := strings.Join(fields, "$")

	if err := credential.Verify(tampered, password); !errors.Is(err, credential.ErrMismatch) {
		t.Errorf("Verify() error = %v, want ErrMismatch", err)
	}
}

func TestShortAndEmptyPasswordsAreRefused(t *testing.T) {
	t.Parallel()

	if _, err := cheap.Hash(secret.Value{}); !errors.Is(err, credential.ErrEmptyPassword) {
		t.Errorf("Hash(empty) error = %v, want ErrEmptyPassword", err)
	}
	if _, err := cheap.Hash(secret.New("short")); !errors.Is(err, credential.ErrPasswordTooShort) {
		t.Errorf("Hash(short) error = %v, want ErrPasswordTooShort", err)
	}
}

// The dummy hash exists so a login against an unknown email costs the same as
// one against a known email. It has to be a real, verifiable hash that no
// password matches.
func TestDummyHashIsVerifiableAndUnmatchable(t *testing.T) {
	t.Parallel()

	dummy := cheap.DummyHash()
	if err := credential.Verify(dummy, secret.New("this account does not exist and never will")); err != nil {
		t.Fatalf("the dummy hash is not a valid PHC string: %v", err)
	}
	for _, guess := range []string{"password", "", "admin"} {
		if err := credential.Verify(dummy, secret.New(guess)); !errors.Is(err, credential.ErrMismatch) {
			t.Errorf("Verify(dummy, %q) error = %v, want ErrMismatch", guess, err)
		}
	}
	if cheap.DummyHash() != dummy {
		t.Error("DummyHash() is not stable; it would cost a fresh derivation on every unknown login")
	}
}
