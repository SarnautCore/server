package gateway

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

// Admission is the credential-free identity produced by one successful ticket
// redemption. The gateway is the only process that sees both this value and
// the ticket that produced it.
type Admission struct {
	AccountID       uuid.UUID
	CharacterID     uuid.UUID
	CharacterName   string
	ChargenOptionID string
}

type Refusal struct{ Reason string }

func (refusal *Refusal) Error() string { return fmt.Sprintf("admission refused: %s", refusal.Reason) }

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

// Authority owns ticket redemption and the play lock for a public gateway
// session. The shard never receives this interface or an implementation of it.
type Authority interface {
	RedeemTicket(context.Context, string) (Admission, error)
	RenewPlayLock(context.Context, uuid.UUID) (bool, error)
	ReleasePlayLock(context.Context, uuid.UUID) error
}

type natsAuthority struct {
	conn    *nats.Conn
	holder  string
	timeout time.Duration
}

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
	if authority.conn == nil {
		return errors.New("NATS connection is nil")
	}
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
	err := authority.request(ctx, SubjectTicketRedeem,
		&authv1.RedeemTicketRequest{Ticket: ticket, Holder: authority.holder}, reply)
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
		AccountID: accountID, CharacterID: characterID,
		CharacterName: reply.GetCharacterName(), ChargenOptionID: reply.GetChargenOptionId(),
	}, nil
}

func (authority *natsAuthority) RenewPlayLock(ctx context.Context, characterID uuid.UUID) (bool, error) {
	reply := new(authv1.RenewPlayLockResponse)
	err := authority.request(ctx, SubjectPlayLockRenew,
		&authv1.RenewPlayLockRequest{CharacterId: characterID.String(), Holder: authority.holder}, reply)
	if err != nil {
		return false, err
	}
	return reply.GetGranted(), nil
}

func (authority *natsAuthority) ReleasePlayLock(ctx context.Context, characterID uuid.UUID) error {
	return authority.request(ctx, SubjectPlayLockRelease,
		&authv1.ReleasePlayLockRequest{CharacterId: characterID.String(), Holder: authority.holder},
		new(authv1.ReleasePlayLockResponse))
}

const (
	SubjectTicketRedeem    = "sarnaut.auth.v1.ticket.redeem"
	SubjectPlayLockRenew   = "sarnaut.auth.v1.playlock.renew"
	SubjectPlayLockRelease = "sarnaut.auth.v1.playlock.release"
)

func refusalReason(err error) string {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}
