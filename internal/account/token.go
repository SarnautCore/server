package account

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SarnautCore/server/internal/account/secret"
)

// Token prefixes and lifetimes (ADR 0030 section 2). Both tokens are 32 bytes
// from crypto/rand rendered with base64.RawURLEncoding — 43 characters — and
// both are prefixed so a leaked string is identifiable at a glance.
const (
	// SessionTokenPrefix marks the bearer credential for the auth HTTP API.
	SessionTokenPrefix = "sarnaut_as_"
	// TicketPrefix marks the single-use shard ticket.
	TicketPrefix = "sarnaut_tk_"

	// SessionLifetime is absolute: there is no sliding renewal in M2.
	SessionLifetime = 12 * time.Hour
	// TicketLifetime only has to survive the round trip from "press Enter
	// World" to the QUIC handshake.
	TicketLifetime = 60 * time.Second
	// PlayLockLifetime is what frees a character whose shard died without
	// releasing. The shard renews every [PlayLockRenewInterval].
	PlayLockLifetime      = 60 * time.Second
	PlayLockRenewInterval = 20 * time.Second

	// tokenBytes is the entropy behind both tokens.
	tokenBytes = 32
)

// Valkey key spaces. Only a token's SHA-256 digest is ever a key: the plaintext
// is never written anywhere, so a Valkey dump yields no usable credential.
const (
	sessionKeyPrefix  = "auth:session:"
	ticketKeyPrefix   = "auth:ticket:"
	playLockKeyPrefix = "auth:playing:"
)

// ErrMalformedToken reports a string that is not one of this service's tokens.
// It is returned before any store lookup, so a peer probing with garbage never
// reaches Valkey.
var ErrMalformedToken = errors.New("account: malformed token")

// mintToken draws a fresh token of the given kind.
func mintToken(prefix string) (secret.Value, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return secret.Value{}, fmt.Errorf("account: draw token: %w", err)
	}
	return secret.New(prefix + base64.RawURLEncoding.EncodeToString(raw)), nil
}

// tokenKey is the Valkey key for a token: its key space plus the SHA-256 of the
// whole token string, hex encoded.
//
// It validates the shape first. A token is opaque and unsigned, so shape is all
// that can be checked locally — the answer to "is this live" belongs to the
// store.
func tokenKey(keyPrefix, tokenPrefix string, token secret.Value) (string, error) {
	if !validTokenShape(tokenPrefix, token) {
		return "", ErrMalformedToken
	}
	return keyPrefix + token.Digest(), nil
}

func validTokenShape(prefix string, token secret.Value) bool {
	raw := token.Reveal()
	if !strings.HasPrefix(raw, prefix) {
		return false
	}
	encoded := raw[len(prefix):]
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil && len(decoded) == tokenBytes
}

func playLockKey(characterID string) string { return playLockKeyPrefix + characterID }
