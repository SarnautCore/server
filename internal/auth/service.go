// Package auth is the auth service: registration, login, the account's
// character roster, and the two credentials a player carries — an account
// session token for the HTTP API and a single-use shard ticket for the game
// connection (ADR 0030).
//
// Three rules shape everything here.
//
//   - Secrets travel as [secret.Value], never as a string, so an accidental
//     log, span attribute or JSON encode is already redacted (ADR 0030 §5).
//   - Only a token's SHA-256 digest is ever stored. The plaintext is written
//     nowhere, so a Valkey dump yields no usable credential.
//   - Every character-creation fact — race, class, spawn, starting gear —
//     comes from the compiled pack's chargen table and from nowhere else
//     (ADR 0032). There are no options in Go source to find.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/auth/credential"
	"github.com/SarnautCore/server/internal/auth/secret"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/pack"
)

// Refusals a caller distinguishes. What reaches a peer is coarser than what
// reaches a log: an answer that tells a stranger whether an account exists is
// an answer about somebody else's account.
var (
	// ErrInvalidCredentials covers both an unknown email and a wrong password,
	// deliberately as one value.
	ErrInvalidCredentials = errors.New("account: invalid credentials")
	// ErrAccountDisabled reports a login against a disabled account.
	ErrAccountDisabled = errors.New("account: account is disabled")
	// ErrUnauthenticated reports a missing, malformed or expired session token.
	ErrUnauthenticated = errors.New("account: not authenticated")
	// ErrEmailInvalid reports a registration whose email is not one.
	ErrEmailInvalid = errors.New("account: email address is not usable")
	// ErrNameInvalid reports a character name that fails the shape rules.
	ErrNameInvalid = errors.New("account: character name is not valid")
	// ErrNameBlocked reports a name whose normalized form contains a blocked
	// substring.
	ErrNameBlocked = errors.New("account: character name is blocked")
	// ErrCharacterNotFound reports a character that does not exist, is deleted,
	// or belongs to another account.
	ErrCharacterNotFound = errors.New("account: character not found")
	// ErrPlayLockHeld reports a character already being played elsewhere.
	ErrPlayLockHeld = errors.New("account: character is already in a session")
	// ErrTicketUnknown reports a ticket that has expired or was already burned.
	// The two are indistinguishable by construction: both are a key that is no
	// longer in Valkey.
	ErrTicketUnknown = errors.New("account: ticket is unknown or expired")
	// ErrNotOwned reports a ticket whose character is not owned by the account
	// it was minted for. Minting refuses this; redemption re-checks.
	ErrNotOwned = errors.New("account: character is not owned by this account")
)

// Session is what a successful login hands back.
type Session struct {
	Token     secret.Value
	AccountID uuid.UUID
	ExpiresAt time.Time
}

// Ticket is a minted shard credential, bound to one character.
type Ticket struct {
	Token       secret.Value
	CharacterID uuid.UUID
	ExpiresAt   time.Time
}

// Admission is the identity a redeemed ticket names. It is everything the shard
// learns about an account, and the shard learns it here and nowhere else: it
// holds no credentials for the `auth` schema (ADR 0030 §4).
type Admission struct {
	AccountID       uuid.UUID
	CharacterID     uuid.UUID
	CharacterName   string
	ChargenOptionID string
}

// ticketRecord is what a ticket key holds in Valkey. The ticket itself is not
// in it: only its digest is the key.
type ticketRecord struct {
	AccountID       uuid.UUID `json:"account_id"`
	CharacterID     uuid.UUID `json:"character_id"`
	CharacterName   string    `json:"character_name"`
	ChargenOptionID string    `json:"chargen_option_id"`
}

// Options configures a [Service].
type Options struct {
	// Repository owns `auth.*`. Required.
	Repository charstore.Repository
	// Keys is the short-lived store for sessions, tickets and play locks.
	// Required: ADR 0030 makes Valkey non-optional for auth, and the in-memory
	// implementation is what a test uses instead of skipping.
	Keys KeyValue
	// Catalogue is the pack's chargen table. Required: a service with no
	// options can create no characters.
	Catalogue *Catalogue
	// Hasher writes new password hashes. The zero value is the ADR's cost.
	Hasher credential.Hasher
	// NameBlocklist is matched against normalized names. Nil means
	// [DefaultNameBlocklist]; an explicitly empty slice means no blocklist.
	NameBlocklist []string
	Logger        *slog.Logger
	// Clock is injectable so an expiry test advances time instead of sleeping.
	Clock func() time.Time
}

// Service is the auth service's behaviour, independent of the transport it is
// exposed over. The HTTP API and the NATS responder are both thin adapters
// around it, which is what keeps "the shard and the launcher get the same
// answers" true by construction rather than by review.
type Service struct {
	repository charstore.Repository
	keys       KeyValue
	catalogue  *Catalogue
	hasher     credential.Hasher
	blocklist  []string
	logger     *slog.Logger
	now        func() time.Time

	// dummyHash is derived once at construction. A login against an unknown
	// email verifies against it, so an attacker cannot tell a missing account
	// from a wrong password by how long the answer took (ADR 0030 §1).
	dummyHash string
	counters  Counters
}

// New builds a service. It refuses a configuration it cannot serve rather than
// starting and failing at the first request.
func New(options Options) (*Service, error) {
	if options.Repository == nil {
		return nil, errors.New("account: a repository is required")
	}
	if options.Keys == nil {
		return nil, errors.New("account: a key-value store is required (ADR 0030 makes Valkey non-optional)")
	}
	if options.Catalogue == nil {
		return nil, ErrNoChargenOptions
	}
	blocklist := options.NameBlocklist
	if blocklist == nil {
		blocklist = DefaultNameBlocklist
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	service := &Service{
		repository: options.Repository,
		keys:       options.Keys,
		catalogue:  options.Catalogue,
		hasher:     options.Hasher,
		blocklist:  blocklist,
		logger:     logger,
		now:        clock,
	}
	service.dummyHash = options.Hasher.DummyHash()
	return service, nil
}

// Catalogue exposes the pack's options so a handler can render the creation
// form without reaching past the service.
func (service *Service) Catalogue() *Catalogue { return service.catalogue }

// Register creates an account. The email is stored as given and compared
// case-insensitively by the citext column; the password is never stored.
func (service *Service) Register(
	ctx context.Context,
	email secret.Value,
	password secret.Value,
) (uuid.UUID, error) {
	if !usableEmail(email) {
		return uuid.Nil, ErrEmailInvalid
	}
	hash, err := service.hasher.Hash(password)
	if err != nil {
		return uuid.Nil, err
	}
	accountID, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("account: mint account id: %w", err)
	}
	account := charstore.Account{
		AccountID:    accountID,
		Email:        email.Reveal(),
		PasswordHash: hash,
		CreatedAt:    service.now().UTC(),
	}
	if err := service.repository.CreateAccount(ctx, account); err != nil {
		return uuid.Nil, err
	}
	service.logger.InfoContext(ctx, "account registered",
		"account_id", accountID.String(),
		"email_domain", email.EmailDomain(),
	)
	return accountID, nil
}

// Login verifies a password and mints an account session token.
//
// An unknown email still performs one full Argon2id derivation against the
// dummy hash before returning, so the two failures cost the same.
func (service *Service) Login(
	ctx context.Context,
	email secret.Value,
	password secret.Value,
) (Session, error) {
	account, err := service.repository.AccountByEmail(ctx, email.Reveal())
	switch {
	case errors.Is(err, charstore.ErrNotFound):
		service.counters.dummyHashVerifications.Add(1)
		// The result is discarded on purpose: the work, not the answer, is
		// what closes the timing channel.
		_ = credential.Verify(service.dummyHash, password)
		service.logger.InfoContext(ctx, "login refused",
			"reason", "unknown_account",
			"email_domain", email.EmailDomain(),
		)
		return Session{}, ErrInvalidCredentials
	case err != nil:
		return Session{}, err
	}

	if account.DisabledAt != nil {
		service.logger.InfoContext(ctx, "login refused",
			"reason", "account_disabled",
			"account_id", account.AccountID.String(),
		)
		return Session{}, ErrAccountDisabled
	}
	if err := credential.Verify(account.PasswordHash, password); err != nil {
		service.counters.failedLogins.Add(1)
		service.logger.InfoContext(ctx, "login refused",
			"reason", "wrong_password",
			"account_id", account.AccountID.String(),
		)
		return Session{}, ErrInvalidCredentials
	}

	token, err := mintToken(SessionTokenPrefix)
	if err != nil {
		return Session{}, err
	}
	key, err := tokenKey(sessionKeyPrefix, SessionTokenPrefix, token)
	if err != nil {
		return Session{}, err
	}
	if err := service.keys.Set(ctx, key, []byte(account.AccountID.String()), SessionLifetime); err != nil {
		return Session{}, err
	}
	service.counters.sessionsIssued.Add(1)
	service.logger.InfoContext(ctx, "session issued",
		"account_id", account.AccountID.String(),
		"token_id", token.ID(),
	)
	return Session{
		Token:     token,
		AccountID: account.AccountID,
		ExpiresAt: service.now().UTC().Add(SessionLifetime),
	}, nil
}

// Authenticate resolves a bearer token to an account. A malformed token is
// refused before any store lookup.
func (service *Service) Authenticate(ctx context.Context, token secret.Value) (uuid.UUID, error) {
	key, err := tokenKey(sessionKeyPrefix, SessionTokenPrefix, token)
	if errors.Is(err, ErrMalformedToken) {
		return uuid.Nil, ErrUnauthenticated
	}
	if err != nil {
		return uuid.Nil, err
	}
	value, err := service.keys.Get(ctx, key)
	if errors.Is(err, ErrNoEntry) {
		return uuid.Nil, ErrUnauthenticated
	}
	if err != nil {
		return uuid.Nil, err
	}
	accountID, err := uuid.Parse(string(value))
	if err != nil {
		return uuid.Nil, ErrUnauthenticated
	}
	return accountID, nil
}

// Logout drops a session token. It is idempotent: a token that has already
// expired is already gone.
func (service *Service) Logout(ctx context.Context, token secret.Value) error {
	key, err := tokenKey(sessionKeyPrefix, SessionTokenPrefix, token)
	if errors.Is(err, ErrMalformedToken) {
		return ErrUnauthenticated
	}
	if err != nil {
		return err
	}
	return service.keys.Delete(ctx, key)
}

// ListCharacters returns an account's living roster, oldest first.
func (service *Service) ListCharacters(ctx context.Context, accountID uuid.UUID) ([]charstore.Character, error) {
	return service.repository.CharactersByAccount(ctx, accountID)
}

// CreateCharacter validates in the order ADR 0032 §5.2 fixes — name shape,
// blocklist, the option exists and is enabled — and then inserts.
//
// There is no "check whether the name is free, then insert" step, because that
// is a race by construction and the losing player gets a 500 instead of a clear
// answer. Uniqueness is the unique index's answer and nothing else's.
func (service *Service) CreateCharacter(
	ctx context.Context,
	accountID uuid.UUID,
	name string,
	chargenOptionID string,
) (charstore.Character, error) {
	if !ValidName(name) {
		return charstore.Character{}, fmt.Errorf("%w: %q", ErrNameInvalid, name)
	}
	normalized := NormalizeName(name)
	if blockedName(service.blocklist, normalized) {
		return charstore.Character{}, fmt.Errorf("%w: %q", ErrNameBlocked, name)
	}
	option, err := service.catalogue.Select(chargenOptionID)
	if err != nil {
		return charstore.Character{}, err
	}

	characterID, err := uuid.NewV7()
	if err != nil {
		return charstore.Character{}, fmt.Errorf("account: mint character id: %w", err)
	}
	character := charstore.Character{
		CharacterID:     characterID,
		AccountID:       accountID,
		Name:            name,
		NameNormalized:  normalized,
		ChargenOptionID: option.ID,
		CreatedAt:       service.now().UTC(),
	}
	if err := service.repository.CreateCharacter(ctx, character); err != nil {
		return charstore.Character{}, err
	}
	// The reservation was a courtesy for the form and has served its purpose.
	// Its failure is not the player's problem: expired rows are swept lazily.
	if err := service.repository.ReleaseNameReservation(ctx, normalized, accountID); err != nil {
		service.logger.WarnContext(ctx, "release name reservation", "error", err)
	}
	service.logger.InfoContext(ctx, "character created",
		"account_id", accountID.String(),
		"character_id", characterID.String(),
		"chargen_option_id", option.ID,
	)
	return character, nil
}

// DeleteCharacter soft-deletes one of the account's characters. The name stays
// taken indefinitely in M2: releasing names needs a grace period, a rename
// story and an impersonation policy, and none of those are written.
func (service *Service) DeleteCharacter(ctx context.Context, accountID, characterID uuid.UUID) error {
	err := service.repository.DeleteCharacter(ctx, accountID, characterID)
	if errors.Is(err, charstore.ErrNotFound) {
		return ErrCharacterNotFound
	}
	if err != nil {
		return err
	}
	service.logger.InfoContext(ctx, "character deleted",
		"account_id", accountID.String(),
		"character_id", characterID.String(),
	)
	return nil
}

// NameCheck is the answer to the creation form's courtesy check.
type NameCheck struct {
	Name          string
	Available     bool
	Reason        string
	ReservedUntil time.Time
}

// NameReservationWindow is how long a name-check holds a name for the account
// that asked, so a player typing into the form is not beaten to it mid-keystroke.
const NameReservationWindow = 5 * time.Minute

// CheckName validates a name and, when it is free, reserves it briefly. The
// reservation is a courtesy, not an authority: if it and the unique index ever
// disagree, the index wins.
func (service *Service) CheckName(ctx context.Context, accountID uuid.UUID, name string) (NameCheck, error) {
	if !ValidName(name) {
		return NameCheck{Name: name, Reason: "NAME_INVALID"}, nil
	}
	normalized := NormalizeName(name)
	if blockedName(service.blocklist, normalized) {
		return NameCheck{Name: name, Reason: "NAME_BLOCKED"}, nil
	}

	_, err := service.repository.CharacterByNormalizedName(ctx, normalized)
	switch {
	case err == nil:
		return NameCheck{Name: name, Reason: "NAME_TAKEN"}, nil
	case !errors.Is(err, charstore.ErrNotFound):
		return NameCheck{}, err
	}

	until := service.now().UTC().Add(NameReservationWindow)
	err = service.repository.ReserveName(ctx, charstore.NameReservation{
		NameNormalized: normalized,
		AccountID:      accountID,
		ReservedUntil:  until,
	})
	if errors.Is(err, charstore.ErrNameTaken) {
		return NameCheck{Name: name, Reason: "NAME_RESERVED"}, nil
	}
	if err != nil {
		return NameCheck{}, err
	}
	return NameCheck{Name: name, Available: true, ReservedUntil: until}, nil
}

// MintTicket issues a single-use shard ticket for one of the account's own
// characters. Ownership is checked here, by the only process that can check it,
// which is why the shard has no "not your character" path to get wrong
// (protocol/session.md rule 5.3.3).
func (service *Service) MintTicket(
	ctx context.Context,
	accountID uuid.UUID,
	characterID uuid.UUID,
) (Ticket, error) {
	character, err := service.repository.CharacterByID(ctx, characterID)
	if errors.Is(err, charstore.ErrNotFound) {
		return Ticket{}, ErrCharacterNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	if character.AccountID != accountID || character.DeletedAt != nil {
		// Deliberately the same answer as "no such character": a stranger must
		// not be able to probe for character ids that exist.
		service.logger.InfoContext(ctx, "ticket refused",
			"reason", "not_owned",
			"account_id", accountID.String(),
			"character_id", characterID.String(),
		)
		return Ticket{}, ErrCharacterNotFound
	}

	token, err := mintToken(TicketPrefix)
	if err != nil {
		return Ticket{}, err
	}
	key, err := tokenKey(ticketKeyPrefix, TicketPrefix, token)
	if err != nil {
		return Ticket{}, err
	}
	record, err := json.Marshal(ticketRecord{
		AccountID:       accountID,
		CharacterID:     characterID,
		CharacterName:   character.Name,
		ChargenOptionID: character.ChargenOptionID,
	})
	if err != nil {
		return Ticket{}, fmt.Errorf("account: encode ticket record: %w", err)
	}
	if err := service.keys.Set(ctx, key, record, TicketLifetime); err != nil {
		return Ticket{}, err
	}
	service.counters.ticketsMinted.Add(1)
	service.logger.InfoContext(ctx, "ticket minted",
		"account_id", accountID.String(),
		"character_id", characterID.String(),
		"token_id", token.ID(),
	)
	return Ticket{
		Token:       token,
		CharacterID: characterID,
		ExpiresAt:   service.now().UTC().Add(TicketLifetime),
	}, nil
}

// RedeemTicket burns a ticket and takes the play lock for holder.
//
// The read is a single Take — Valkey's GETDEL — so a replayed ticket finds
// nothing. There is no retry anywhere above this call for the same reason: a
// retried redemption cannot succeed and would only turn one failed login into
// two.
func (service *Service) RedeemTicket(
	ctx context.Context,
	ticket secret.Value,
	holder string,
) (Admission, error) {
	key, err := tokenKey(ticketKeyPrefix, TicketPrefix, ticket)
	if errors.Is(err, ErrMalformedToken) {
		service.logger.InfoContext(ctx, "ticket redemption refused", "reason", "malformed")
		return Admission{}, ErrMalformedToken
	}
	if err != nil {
		return Admission{}, err
	}

	payload, err := service.keys.Take(ctx, key)
	if errors.Is(err, ErrNoEntry) {
		service.logger.InfoContext(ctx, "ticket redemption refused",
			"reason", "unknown_or_expired",
			"token_id", ticket.ID(),
		)
		return Admission{}, ErrTicketUnknown
	}
	if err != nil {
		return Admission{}, err
	}

	var record ticketRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return Admission{}, fmt.Errorf("account: decode ticket record: %w", err)
	}

	// Ownership is re-checked at redemption. Minting already refuses a
	// character the account does not own, so reaching this branch means the
	// roster changed under a live ticket — or that something forged a record.
	character, err := service.repository.CharacterByID(ctx, record.CharacterID)
	switch {
	case errors.Is(err, charstore.ErrNotFound):
		service.logger.InfoContext(ctx, "ticket redemption refused",
			"reason", "character_not_found",
			"character_id", record.CharacterID.String(),
		)
		return Admission{}, ErrCharacterNotFound
	case err != nil:
		return Admission{}, err
	case character.DeletedAt != nil:
		service.logger.InfoContext(ctx, "ticket redemption refused",
			"reason", "character_deleted",
			"character_id", record.CharacterID.String(),
		)
		return Admission{}, ErrCharacterNotFound
	case character.AccountID != record.AccountID:
		service.logger.WarnContext(ctx, "ticket redemption refused",
			"reason", "not_owned",
			"account_id", record.AccountID.String(),
			"character_id", record.CharacterID.String(),
		)
		return Admission{}, ErrNotOwned
	}

	if holder != "" {
		taken, err := service.keys.SetNX(ctx, playLockKey(record.CharacterID.String()), []byte(holder), PlayLockLifetime)
		if err != nil {
			return Admission{}, err
		}
		if !taken {
			// Not this holder's lock. One live session per character is what
			// makes the load and save checkpoints safe to write without
			// cross-session merge logic.
			refreshed, err := service.keys.RefreshIf(
				ctx,
				playLockKey(record.CharacterID.String()),
				[]byte(holder),
				PlayLockLifetime,
			)
			if err != nil {
				return Admission{}, err
			}
			if !refreshed {
				service.logger.InfoContext(ctx, "ticket redemption refused",
					"reason", "play_lock_held",
					"character_id", record.CharacterID.String(),
				)
				return Admission{}, ErrPlayLockHeld
			}
		}
	}

	service.counters.ticketsRedeemed.Add(1)
	service.logger.InfoContext(ctx, "ticket redeemed",
		"account_id", record.AccountID.String(),
		"character_id", record.CharacterID.String(),
		"holder", holder,
	)
	return Admission{
		AccountID:       record.AccountID,
		CharacterID:     record.CharacterID,
		CharacterName:   character.Name,
		ChargenOptionID: character.ChargenOptionID,
	}, nil
}

// RenewPlayLock extends the lock while holder still owns it. A shard that lost
// the lock cannot renew it back.
func (service *Service) RenewPlayLock(ctx context.Context, characterID uuid.UUID, holder string) (bool, error) {
	return service.keys.RefreshIf(ctx, playLockKey(characterID.String()), []byte(holder), PlayLockLifetime)
}

// ReleasePlayLock drops the lock if holder owns it. A release from a stale
// holder is a no-op rather than an error: the TTL is the real guarantee.
func (service *Service) ReleasePlayLock(ctx context.Context, characterID uuid.UUID, holder string) error {
	_, err := service.keys.DeleteIf(ctx, playLockKey(characterID.String()), []byte(holder))
	return err
}

// PlayableOptions lists the chargen options a client may offer.
func (service *Service) PlayableOptions() []pack.ChargenOption {
	return service.catalogue.Playable()
}

// usableEmail is a shape check, not validation. M2 has no email verification
// (ADR 0030), so the only thing worth refusing is a value that cannot be an
// address at all — one that would make `email_domain` telemetry meaningless.
func usableEmail(email secret.Value) bool {
	if email.Len() < 3 || email.Len() > 254 {
		return false
	}
	return email.EmailDomain() != "" && email.Reveal()[0] != '@'
}
