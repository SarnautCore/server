package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	authv1 "github.com/SarnautCore/server/gen/sarnaut/auth/v1"
)

// Admission is the identity a redeemed ticket names. It is everything the shard
// knows about an account: it holds no credentials for the `auth` schema and
// learns these four fields from a redemption reply and from nowhere else
// (ADR 0030 §4).
type Admission struct {
	AccountID       uuid.UUID
	CharacterID     uuid.UUID
	CharacterName   string
	ChargenOptionID string
}

// Refusal is an admission the auth service declined.
//
// It carries a machine-readable Reason for the shard's log. The client is told
// only `UNAUTHENTICATED`: a peer that could tell an expired ticket from an
// unknown one, or from somebody else's character, would be learning about
// another account.
type Refusal struct {
	Reason string
}

func (refusal *Refusal) Error() string {
	return fmt.Sprintf("admission refused: %s", refusal.Reason)
}

// The reasons an admission is refused. Each is a distinct log line, which is
// the only place they are distinguishable.
const (
	ReasonNoTicket        = "no_ticket"
	ReasonMalformed       = "malformed_ticket"
	ReasonUnknownTicket   = "unknown_or_expired_ticket"
	ReasonCharacterGone   = "character_not_found"
	ReasonNotOwned        = "character_not_owned_by_account"
	ReasonPlayLockHeld    = "play_lock_held_elsewhere"
	ReasonAuthUnavailable = "auth_service_unavailable"
	ReasonAuthInternal    = "auth_service_internal_error"
)

// Authority is the auth service as the shard sees it (ADR 0033 §3): ticket
// redemption terminating at the shard for M2, and the play lock that makes one
// live session per character true.
//
// It is an interface so the deviation is cheap to undo. When the gateway takes
// redemption back, this seam is what moves; nothing in `world`, `combat` or the
// other simulation modules knows it exists.
type Authority interface {
	// RedeemTicket burns a ticket and takes the play lock. It returns a
	// [*Refusal] for a ticket the service declined, and an ordinary error only
	// when the service could not be reached.
	RedeemTicket(ctx context.Context, ticket string) (Admission, error)

	// RenewPlayLock extends this shard's claim on a character.
	RenewPlayLock(ctx context.Context, characterID uuid.UUID) (bool, error)

	// ReleasePlayLock drops it on disconnect. A shard that dies without
	// releasing frees the character when the lock expires.
	ReleasePlayLock(ctx context.Context, characterID uuid.UUID) error
}

// natsAuthority talks to the auth service over the subjects of ADR 0030 §3.
//
// `internal/session` is the one simulation-adjacent package ADR 0033 lets
// import NATS, and only NATS: the exemption is narrow so it cannot grow into a
// second database caller, and it is deleted when redemption moves to the
// gateway.
type natsAuthority struct {
	conn *nats.Conn
	// holder identifies this shard instance in the play lock.
	holder  string
	timeout time.Duration
}

// NewNATSAuthority binds the connection `internal/infra` opened. The timeout is
// 2 seconds with no retry: a retried redemption cannot succeed after the
// service's GETDEL, and would only turn one failed login into two.
func NewNATSAuthority(conn *nats.Conn, holder string, timeout time.Duration) Authority {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &natsAuthority{conn: conn, holder: holder, timeout: timeout}
}

func (authority *natsAuthority) request(
	ctx context.Context,
	subject string,
	request proto.Message,
	reply proto.Message,
) error {
	payload, err := proto.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", subject, err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, authority.timeout)
	defer cancel()

	message, err := authority.conn.RequestWithContext(requestCtx, subject, payload)
	if err != nil {
		return fmt.Errorf("request %s: %w", subject, err)
	}
	if err := proto.Unmarshal(message.Data, reply); err != nil {
		return fmt.Errorf("decode %s reply: %w", subject, err)
	}
	return nil
}

func (authority *natsAuthority) RedeemTicket(ctx context.Context, ticket string) (Admission, error) {
	reply := new(authv1.RedeemTicketResponse)
	err := authority.request(
		ctx,
		SubjectTicketRedeem,
		&authv1.RedeemTicketRequest{Ticket: ticket, Holder: authority.holder},
		reply,
	)
	if err != nil {
		return Admission{}, errors.Join(&Refusal{Reason: ReasonAuthUnavailable}, err)
	}

	switch reply.GetError() {
	case authv1.TicketError_TICKET_ERROR_UNSPECIFIED:
	case authv1.TicketError_TICKET_ERROR_MALFORMED:
		return Admission{}, &Refusal{Reason: ReasonMalformed}
	case authv1.TicketError_TICKET_ERROR_UNKNOWN:
		return Admission{}, &Refusal{Reason: ReasonUnknownTicket}
	case authv1.TicketError_TICKET_ERROR_CHARACTER_NOT_FOUND:
		return Admission{}, &Refusal{Reason: ReasonCharacterGone}
	case authv1.TicketError_TICKET_ERROR_NOT_OWNED:
		return Admission{}, &Refusal{Reason: ReasonNotOwned}
	case authv1.TicketError_TICKET_ERROR_PLAY_LOCK_HELD:
		return Admission{}, &Refusal{Reason: ReasonPlayLockHeld}
	default:
		return Admission{}, &Refusal{Reason: ReasonAuthInternal}
	}

	accountID, err := uuid.Parse(reply.GetAccountId())
	if err != nil {
		return Admission{}, errors.Join(&Refusal{Reason: ReasonAuthInternal}, err)
	}
	characterID, err := uuid.Parse(reply.GetCharacterId())
	if err != nil {
		return Admission{}, errors.Join(&Refusal{Reason: ReasonAuthInternal}, err)
	}
	return Admission{
		AccountID:       accountID,
		CharacterID:     characterID,
		CharacterName:   reply.GetCharacterName(),
		ChargenOptionID: reply.GetChargenOptionId(),
	}, nil
}

func (authority *natsAuthority) RenewPlayLock(ctx context.Context, characterID uuid.UUID) (bool, error) {
	reply := new(authv1.RenewPlayLockResponse)
	err := authority.request(
		ctx,
		SubjectPlayLockRenew,
		&authv1.RenewPlayLockRequest{CharacterId: characterID.String(), Holder: authority.holder},
		reply,
	)
	if err != nil {
		return false, err
	}
	return reply.GetGranted(), nil
}

func (authority *natsAuthority) ReleasePlayLock(ctx context.Context, characterID uuid.UUID) error {
	reply := new(authv1.ReleasePlayLockResponse)
	return authority.request(
		ctx,
		SubjectPlayLockRelease,
		&authv1.ReleasePlayLockRequest{CharacterId: characterID.String(), Holder: authority.holder},
		reply,
	)
}

// Subjects the shard uses. They are duplicated from `internal/auth` rather
// than imported: the shard must not depend on the auth service's package, which
// owns a database the shard may not touch.
const (
	SubjectTicketRedeem    = "sarnaut.auth.v1.ticket.redeem"
	SubjectPlayLockRenew   = "sarnaut.auth.v1.playlock.renew"
	SubjectPlayLockRelease = "sarnaut.auth.v1.playlock.release"
)

// refusalReason reports the reason carried by err, or the empty string.
func refusalReason(err error) string {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}
