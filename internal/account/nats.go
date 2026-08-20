package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	authv1 "github.com/SarnautCore/server/gen/sarnaut/auth/v1"
	"github.com/SarnautCore/server/internal/account/secret"
	"github.com/SarnautCore/server/internal/store"
)

// NATS subjects (ADR 0030 section 3). Request/reply subjects are served in
// queue group [QueueGroup], so a second auth instance shares load rather than
// answering twice. `session.revoked` is a plain publish with no queue group,
// because every shard must see it.
const (
	SubjectTicketRedeem    = "sarnaut.auth.v1.ticket.redeem"
	SubjectCharacterList   = "sarnaut.auth.v1.character.list"
	SubjectCharacterCreate = "sarnaut.auth.v1.character.create"
	SubjectPlayLockRenew   = "sarnaut.auth.v1.playlock.renew"
	SubjectPlayLockRelease = "sarnaut.auth.v1.playlock.release"
	SubjectSessionRevoked  = "sarnaut.auth.v1.session.revoked"

	QueueGroup = "auth"

	// requestTimeout bounds one responder's own work. The shard's side is 2
	// seconds with no retry; answering slower than that is answering nobody.
	requestTimeout = 2 * time.Second
)

// Responder serves the auth service over NATS.
type Responder struct {
	service *Service
	conn    *nats.Conn
	logger  *slog.Logger
}

// NewResponder binds a service to the NATS connection `internal/infra` opened.
func NewResponder(service *Service, conn *nats.Conn, logger *slog.Logger) *Responder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Responder{service: service, conn: conn, logger: logger}
}

// Subscribe starts every subscription and returns a function that drains them.
// The subscriptions are queue subscriptions, so scaling out is adding a
// process.
func (responder *Responder) Subscribe(ctx context.Context) (func() error, error) {
	handlers := []struct {
		subject string
		handle  func(context.Context, *nats.Msg) (proto.Message, error)
	}{
		{SubjectTicketRedeem, responder.redeemTicket},
		{SubjectCharacterList, responder.listCharacters},
		{SubjectCharacterCreate, responder.createCharacter},
		{SubjectPlayLockRenew, responder.renewPlayLock},
		{SubjectPlayLockRelease, responder.releasePlayLock},
	}

	subscriptions := make([]*nats.Subscription, 0, len(handlers))
	drain := func() error {
		var failures []error
		for _, subscription := range subscriptions {
			if err := subscription.Drain(); err != nil {
				failures = append(failures, err)
			}
		}
		return errors.Join(failures...)
	}

	for _, handler := range handlers {
		subject, handle := handler.subject, handler.handle
		subscription, err := responder.conn.QueueSubscribe(subject, QueueGroup, func(message *nats.Msg) {
			responder.answer(ctx, subject, message, handle)
		})
		if err != nil {
			return nil, errors.Join(fmt.Errorf("subscribe to %s: %w", subject, err), drain())
		}
		subscriptions = append(subscriptions, subscription)
	}
	return drain, nil
}

// answer runs one handler under its own deadline and replies with whatever it
// produced. A handler that fails outright replies with a typed refusal rather
// than nothing: the shard's 2-second timeout is for a dead auth service, not
// for an ordinary error.
func (responder *Responder) answer(
	ctx context.Context,
	subject string,
	message *nats.Msg,
	handle func(context.Context, *nats.Msg) (proto.Message, error),
) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	reply, err := handle(requestCtx, message)
	if err != nil {
		responder.logger.ErrorContext(requestCtx, "auth request failed",
			"subject", subject,
			"error", err,
		)
	}
	if reply == nil || message.Reply == "" {
		return
	}
	payload, err := proto.Marshal(reply)
	if err != nil {
		responder.logger.ErrorContext(requestCtx, "encode auth reply", "subject", subject, "error", err)
		return
	}
	if err := message.Respond(payload); err != nil {
		responder.logger.ErrorContext(requestCtx, "write auth reply", "subject", subject, "error", err)
	}
}

func (responder *Responder) redeemTicket(ctx context.Context, message *nats.Msg) (proto.Message, error) {
	request := new(authv1.RedeemTicketRequest)
	if err := proto.Unmarshal(message.Data, request); err != nil {
		return &authv1.RedeemTicketResponse{Error: authv1.TicketError_TICKET_ERROR_MALFORMED}, nil
	}

	admission, err := responder.service.RedeemTicket(ctx, secret.New(request.GetTicket()), request.GetHolder())
	switch {
	case errors.Is(err, ErrMalformedToken):
		return &authv1.RedeemTicketResponse{Error: authv1.TicketError_TICKET_ERROR_MALFORMED}, nil
	case errors.Is(err, ErrTicketUnknown):
		return &authv1.RedeemTicketResponse{Error: authv1.TicketError_TICKET_ERROR_UNKNOWN}, nil
	case errors.Is(err, ErrCharacterNotFound):
		return &authv1.RedeemTicketResponse{Error: authv1.TicketError_TICKET_ERROR_CHARACTER_NOT_FOUND}, nil
	case errors.Is(err, ErrNotOwned):
		return &authv1.RedeemTicketResponse{Error: authv1.TicketError_TICKET_ERROR_NOT_OWNED}, nil
	case errors.Is(err, ErrPlayLockHeld):
		return &authv1.RedeemTicketResponse{Error: authv1.TicketError_TICKET_ERROR_PLAY_LOCK_HELD}, nil
	case err != nil:
		return &authv1.RedeemTicketResponse{Error: authv1.TicketError_TICKET_ERROR_INTERNAL}, err
	}

	return &authv1.RedeemTicketResponse{
		AccountId:       admission.AccountID.String(),
		CharacterId:     admission.CharacterID.String(),
		CharacterName:   admission.CharacterName,
		ChargenOptionId: admission.ChargenOptionID,
	}, nil
}

func (responder *Responder) listCharacters(ctx context.Context, message *nats.Msg) (proto.Message, error) {
	request := new(authv1.ListCharactersRequest)
	if err := proto.Unmarshal(message.Data, request); err != nil {
		return &authv1.ListCharactersResponse{Error: authv1.CharacterError_CHARACTER_ERROR_UNAUTHENTICATED}, nil
	}
	accountID, err := responder.service.Authenticate(ctx, secret.New(request.GetSessionToken()))
	if err != nil {
		return &authv1.ListCharactersResponse{Error: authv1.CharacterError_CHARACTER_ERROR_UNAUTHENTICATED}, nil
	}
	characters, err := responder.service.ListCharacters(ctx, accountID)
	if err != nil {
		return &authv1.ListCharactersResponse{Error: authv1.CharacterError_CHARACTER_ERROR_INTERNAL}, err
	}

	reply := &authv1.ListCharactersResponse{Characters: make([]*authv1.Character, 0, len(characters))}
	for _, character := range characters {
		reply.Characters = append(reply.Characters, &authv1.Character{
			CharacterId:     character.CharacterID.String(),
			Name:            character.Name,
			ChargenOptionId: character.ChargenOptionID,
		})
	}
	return reply, nil
}

func (responder *Responder) createCharacter(ctx context.Context, message *nats.Msg) (proto.Message, error) {
	request := new(authv1.CreateCharacterRequest)
	if err := proto.Unmarshal(message.Data, request); err != nil {
		return &authv1.CreateCharacterResponse{ErrorCode: authv1.CharacterError_CHARACTER_ERROR_UNAUTHENTICATED}, nil
	}
	accountID, err := responder.service.Authenticate(ctx, secret.New(request.GetSessionToken()))
	if err != nil {
		return &authv1.CreateCharacterResponse{ErrorCode: authv1.CharacterError_CHARACTER_ERROR_UNAUTHENTICATED}, nil
	}

	character, err := responder.service.CreateCharacter(ctx, accountID, request.GetName(), request.GetChargenOptionId())
	switch {
	case errors.Is(err, ErrNameInvalid):
		return &authv1.CreateCharacterResponse{ErrorCode: authv1.CharacterError_CHARACTER_ERROR_NAME_INVALID}, nil
	case errors.Is(err, ErrNameBlocked):
		return &authv1.CreateCharacterResponse{ErrorCode: authv1.CharacterError_CHARACTER_ERROR_NAME_BLOCKED}, nil
	case errors.Is(err, store.ErrNameTaken):
		return &authv1.CreateCharacterResponse{ErrorCode: authv1.CharacterError_CHARACTER_ERROR_NAME_TAKEN}, nil
	case errors.Is(err, ErrUnknownOption):
		return &authv1.CreateCharacterResponse{ErrorCode: authv1.CharacterError_CHARACTER_ERROR_UNKNOWN_OPTION}, nil
	case errors.Is(err, ErrOptionDisabled):
		return &authv1.CreateCharacterResponse{ErrorCode: authv1.CharacterError_CHARACTER_ERROR_OPTION_DISABLED}, nil
	case err != nil:
		return &authv1.CreateCharacterResponse{ErrorCode: authv1.CharacterError_CHARACTER_ERROR_INTERNAL}, err
	}
	return &authv1.CreateCharacterResponse{CharacterId: character.CharacterID.String()}, nil
}

func (responder *Responder) renewPlayLock(ctx context.Context, message *nats.Msg) (proto.Message, error) {
	request := new(authv1.RenewPlayLockRequest)
	if err := proto.Unmarshal(message.Data, request); err != nil {
		return &authv1.RenewPlayLockResponse{}, nil
	}
	characterID, err := uuid.Parse(request.GetCharacterId())
	if err != nil {
		return &authv1.RenewPlayLockResponse{}, nil
	}
	granted, err := responder.service.RenewPlayLock(ctx, characterID, request.GetHolder())
	if err != nil {
		return &authv1.RenewPlayLockResponse{}, err
	}
	return &authv1.RenewPlayLockResponse{Granted: granted}, nil
}

func (responder *Responder) releasePlayLock(ctx context.Context, message *nats.Msg) (proto.Message, error) {
	request := new(authv1.ReleasePlayLockRequest)
	if err := proto.Unmarshal(message.Data, request); err != nil {
		return &authv1.ReleasePlayLockResponse{}, nil
	}
	characterID, err := uuid.Parse(request.GetCharacterId())
	if err != nil {
		return &authv1.ReleasePlayLockResponse{}, nil
	}
	if err := responder.service.ReleasePlayLock(ctx, characterID, request.GetHolder()); err != nil {
		return &authv1.ReleasePlayLockResponse{}, err
	}
	return &authv1.ReleasePlayLockResponse{}, nil
}

// RevokeSession fans out a revocation. Every shard sees it and drops matching
// connections; nothing acknowledges it.
func (responder *Responder) RevokeSession(accountID, characterID uuid.UUID, reason string) error {
	payload, err := proto.Marshal(&authv1.SessionRevoked{
		AccountId:   accountID.String(),
		CharacterId: characterID.String(),
		Reason:      reason,
	})
	if err != nil {
		return fmt.Errorf("encode session revocation: %w", err)
	}
	return responder.conn.Publish(SubjectSessionRevoked, payload)
}
