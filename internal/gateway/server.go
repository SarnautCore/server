package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	privatev1 "github.com/SarnautCore/server/gen/sarnaut/private/v1"
	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/google/uuid"
)

type SessionState uint8

const (
	StateConnected SessionState = iota
	StateVersioned
	StateAuthenticated
	StateAttaching
	StateInZone
	StateTransferring
	StateDraining
	StateClosed
)

type DialShard func(context.Context, Route) (transport.Connection, error)

type Server struct {
	ProtocolVersion     sarnautv1.ProtocolVersion
	BuildID             string
	PackID              string
	AllowUnverifiedPack bool
	Authority           Authority
	Routes              Routes
	DialShard           DialShard
	Trust               TrustConfig
	AdmissionTimeout    time.Duration
	AttachTimeout       time.Duration
	RenewInterval       time.Duration
	ReleaseTimeout      time.Duration
	Logger              *slog.Logger
	// StateChanged is an optional diagnostic hook. Production leaves it nil;
	// tests use it to assert the typed transition order without reaching into
	// the session implementation.
	StateChanged func(SessionState)

	mu       sync.Mutex
	sessions map[uuid.UUID]registeredSession
}

type registeredSession struct {
	id     uuid.UUID
	cancel context.CancelFunc
	done   chan struct{}
}

const (
	defaultAdmissionTimeout = 10 * time.Second
	defaultAttachTimeout    = 5 * time.Second
	defaultRenewInterval    = 20 * time.Second
	defaultReleaseTimeout   = 2 * time.Second
	publicQueueDepth        = 128
)

func (server *Server) Serve(ctx context.Context, listener transport.Listener) error {
	if err := server.validate(); err != nil {
		return err
	}
	server.mu.Lock()
	if server.sessions == nil {
		server.sessions = make(map[uuid.UUID]registeredSession)
	}
	server.mu.Unlock()
	for {
		connection, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept public session: %w", err)
		}
		go func() {
			if err := server.handle(ctx, connection); err != nil {
				server.logger().Info("gateway session ended", "error", err)
			}
			_ = connection.Close()
		}()
	}
}

func (server *Server) validate() error {
	if server.Authority == nil {
		return errors.New("gateway authority is required")
	}
	if server.DialShard == nil {
		return errors.New("gateway shard dialer is required")
	}
	if len(server.Routes.byZone) == 0 {
		return errors.New("gateway routes are required")
	}
	return server.Trust.validate()
}

func (server *Server) handle(parent context.Context, public transport.Connection) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	server.transition(StateConnected)

	timer := time.AfterFunc(server.admissionTimeout(), func() { _ = public.Close() })
	defer timer.Stop()
	if err := server.exchangeHello(public); err != nil {
		return err
	}
	server.transition(StateVersioned)
	request := new(sarnautv1.EnterZoneRequest)
	if err := transport.ReadMessage(public, request); err != nil {
		return fmt.Errorf("read enter zone request: %w", err)
	}
	route, err := server.Routes.Resolve(request.GetZoneId())
	if err != nil {
		return server.refuse(public, sarnautv1.ErrorCode_ERROR_CODE_NOT_IN_ZONE, err.Error())
	}
	admission, err := server.admit(ctx, public, request.GetTicket())
	if err != nil {
		return err
	}
	server.transition(StateAuthenticated)
	timer.Stop()

	sessionID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate gateway session id: %w", err)
	}
	releaseClaim, err := server.claim(ctx, admission.CharacterID, cancel)
	if err != nil {
		return err
	}
	defer releaseClaim()
	defer server.releaseLock(admission.CharacterID)

	commands := make(chan []byte, publicQueueDepth)
	broker := newAttachmentBroker()
	defer broker.close()
	readerErrors := make(chan error, 2)
	go readReliableFrames(ctx, public, commands, readerErrors)
	go relayCommands(ctx, broker, commands, readerErrors)
	go server.renewLock(ctx, admission.CharacterID, readerErrors)

	epoch := uint64(0)
	recoveryDeadline := time.Now().Add(server.attachTimeout())
	for {
		epoch++
		server.transition(StateAttaching)
		attachment, err := server.attach(ctx, route, sessionID, epoch, admission)
		if err != nil {
			select {
			case publicErr := <-readerErrors:
				server.transition(StateDraining)
				server.transition(StateClosed)
				return publicErr
			default:
			}
			if time.Now().After(recoveryDeadline) {
				_ = server.refuse(public, sarnautv1.ErrorCode_ERROR_CODE_INTERNAL, "reconnect required")
				return err
			}
			if retryErr := server.waitAttachRetry(ctx, err); retryErr != nil {
				_ = server.refuse(public, sarnautv1.ErrorCode_ERROR_CODE_INTERNAL, "reconnect required")
				return errors.Join(err, retryErr)
			}
			continue
		}
		broker.set(epoch, attachment)
		server.transition(StateInZone)
		lost := make(chan error, 1)
		go relayEvents(ctx, broker, sessionID.String(), epoch, attachment, public, lost)
		if public.SupportsUnreliable() && attachment.SupportsUnreliable() {
			go relayNewest(ctx, public, attachment, lost)
			go relayNewest(ctx, attachment, public, lost)
		}

		select {
		case <-ctx.Done():
			server.transition(StateDraining)
			_ = attachment.Close()
			server.transition(StateClosed)
			return ctx.Err()
		case err := <-readerErrors:
			server.transition(StateDraining)
			_ = attachment.Close()
			server.transition(StateClosed)
			return err
		case err := <-lost:
			broker.clear(epoch, attachment)
			_ = attachment.Close()
			server.logger().Warn("private attachment lost; reattaching",
				"session_id", sessionID.String(), "attachment_epoch", epoch, "error", err)
			// Resolve again. A routing-table replacement affects this epoch, while
			// the failed connection remains pinned to the route it used.
			route, err = server.Routes.Resolve(request.GetZoneId())
			recoveryDeadline = time.Now().Add(server.attachTimeout())
			if err != nil {
				_ = server.refuse(public, sarnautv1.ErrorCode_ERROR_CODE_INTERNAL, "reconnect required")
				return err
			}
		}
	}
}

func (server *Server) attach(
	ctx context.Context,
	route Route,
	sessionID uuid.UUID,
	epoch uint64,
	admission Admission,
) (transport.Connection, error) {
	attachCtx, cancel := context.WithTimeout(ctx, server.attachTimeout())
	defer cancel()
	connection, err := server.DialShard(attachCtx, route)
	if err != nil {
		return nil, fmt.Errorf("dial shard %q: %w", route.ShardID, err)
	}
	trust := server.Trust
	trust.ShardID = route.ShardID
	if err := AuthenticateGateway(attachCtx, connection, trust); err != nil {
		_ = connection.Close()
		return nil, err
	}
	request := &privatev1.Control{Payload: &privatev1.Control_AttachRequest{
		AttachRequest: &privatev1.AttachRequest{
			SessionId: sessionID.String(), AttachmentEpoch: epoch, ZoneId: route.ZoneID,
			Identity: &privatev1.AssertedIdentity{
				AccountId: admission.AccountID.String(), CharacterId: admission.CharacterID.String(),
				CharacterName: admission.CharacterName, ChargenOptionId: admission.ChargenOptionID,
			},
		},
	}}
	if err := transport.WriteMessage(connection, request); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("write attach request: %w", err)
	}
	response := new(privatev1.Control)
	if err := transport.ReadMessage(connection, response); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("read attach response: %w", err)
	}
	if refusal := response.GetAttachRefused(); refusal != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("attach refused: %s", refusal.GetCode())
	}
	attached := response.GetAttached()
	if attached == nil || attached.GetSessionId() != sessionID.String() ||
		attached.GetAttachmentEpoch() != epoch || attached.GetZoneId() != route.ZoneID {
		_ = connection.Close()
		return nil, errors.New("invalid attach acknowledgement")
	}
	return connection, nil
}

func (server *Server) exchangeHello(connection transport.Connection) error {
	hello := new(sarnautv1.ClientHello)
	if err := transport.ReadMessage(connection, hello); err != nil {
		return fmt.Errorf("read client hello: %w", err)
	}
	if hello.GetProtocolVersion() != server.ProtocolVersion {
		return fmt.Errorf("client protocol version %s does not match gateway version %s",
			hello.GetProtocolVersion(), server.ProtocolVersion)
	}
	if err := transport.WriteMessage(connection, &sarnautv1.ServerHello{
		ProtocolVersion: server.ProtocolVersion, BuildId: server.BuildID, PackId: server.PackID,
	}); err != nil {
		return fmt.Errorf("write server hello: %w", err)
	}
	if server.PackID != "" && hello.GetPackId() != server.PackID &&
		!(hello.GetPackId() == "" && server.AllowUnverifiedPack) {
		return server.refuse(connection, sarnautv1.ErrorCode_ERROR_CODE_PACK_MISMATCH,
			fmt.Sprintf("client content pack %q does not match gateway pack %q", hello.GetPackId(), server.PackID))
	}
	return nil
}

func (server *Server) admit(ctx context.Context, connection transport.Connection, ticket string) (Admission, error) {
	if ticket == "" {
		return Admission{}, server.refuse(connection, sarnautv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED,
			"a gateway ticket is required")
	}
	admission, err := server.Authority.RedeemTicket(ctx, ticket)
	if err != nil {
		server.logger().Info("gateway admission refused", "reason", refusalReason(err))
		return Admission{}, server.refuse(connection, sarnautv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED,
			"the gateway ticket was refused")
	}
	return admission, nil
}

func (server *Server) refuse(connection transport.Connection, code sarnautv1.ErrorCode, detail string) error {
	err := transport.WriteMessage(connection, &sarnautv1.ServerMessage{Payload: &sarnautv1.ServerMessage_Error{
		Error: &sarnautv1.Error{Code: code, Detail: detail},
	}})
	violation := fmt.Errorf("gateway refusal %s: %s", code, detail)
	if err != nil {
		return errors.Join(violation, err)
	}
	return violation
}

func (server *Server) claim(
	ctx context.Context,
	characterID uuid.UUID,
	cancel context.CancelFunc,
) (func(), error) {
	claimID := uuid.New()
	done := make(chan struct{})
	server.mu.Lock()
	previous := server.sessions[characterID]
	server.sessions[characterID] = registeredSession{id: claimID, cancel: cancel, done: done}
	server.mu.Unlock()
	release := func() {
		server.mu.Lock()
		if current := server.sessions[characterID]; current.id == claimID {
			delete(server.sessions, characterID)
		}
		server.mu.Unlock()
		close(done)
	}
	if previous.cancel != nil {
		previous.cancel()
		select {
		case <-previous.done:
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		case <-time.After(server.attachTimeout()):
			release()
			return nil, errors.New("previous gateway session did not drain")
		}
	}
	return release, nil
}

func (server *Server) renewLock(ctx context.Context, characterID uuid.UUID, failures chan<- error) {
	ticker := time.NewTicker(server.renewInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			granted, err := server.Authority.RenewPlayLock(ctx, characterID)
			if err != nil {
				server.logger().Warn("play lock renewal failed", "character_id", characterID, "error", err)
				continue
			}
			if !granted {
				select {
				case failures <- errors.New("gateway lost play lock"):
				case <-ctx.Done():
				}
				return
			}
		}
	}
}

func (server *Server) releaseLock(characterID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), server.releaseTimeout())
	defer cancel()
	if err := server.Authority.ReleasePlayLock(ctx, characterID); err != nil {
		server.logger().Warn("play lock release failed", "character_id", characterID, "error", err)
	}
}

func (server *Server) waitAttachRetry(ctx context.Context, attachErr error) error {
	server.logger().Warn("private attach failed; retrying", "error", attachErr)
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (server *Server) logger() *slog.Logger {
	if server.Logger != nil {
		return server.Logger
	}
	return slog.Default()
}

func (server *Server) transition(state SessionState) {
	if server.StateChanged != nil {
		server.StateChanged(state)
	}
}

func (server *Server) admissionTimeout() time.Duration {
	if server.AdmissionTimeout > 0 {
		return server.AdmissionTimeout
	}
	return defaultAdmissionTimeout
}
func (server *Server) attachTimeout() time.Duration {
	if server.AttachTimeout > 0 {
		return server.AttachTimeout
	}
	return defaultAttachTimeout
}
func (server *Server) renewInterval() time.Duration {
	if server.RenewInterval > 0 {
		return server.RenewInterval
	}
	return defaultRenewInterval
}
func (server *Server) releaseTimeout() time.Duration {
	if server.ReleaseTimeout > 0 {
		return server.ReleaseTimeout
	}
	return defaultReleaseTimeout
}

type attachmentBroker struct {
	mu         sync.Mutex
	writeMu    sync.Mutex
	changed    chan struct{}
	connection transport.Connection
	epoch      uint64
	closed     bool
}

func newAttachmentBroker() *attachmentBroker { return &attachmentBroker{changed: make(chan struct{})} }

func (broker *attachmentBroker) notify() { close(broker.changed); broker.changed = make(chan struct{}) }

func (broker *attachmentBroker) set(epoch uint64, connection transport.Connection) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.epoch, broker.connection = epoch, connection
	broker.notify()
}
func (broker *attachmentBroker) clear(epoch uint64, connection transport.Connection) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.epoch == epoch && broker.connection == connection {
		broker.connection = nil
		broker.notify()
	}
}
func (broker *attachmentBroker) close() {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.closed = true
	if broker.connection != nil {
		_ = broker.connection.Close()
	}
	broker.connection = nil
	broker.notify()
}
func (broker *attachmentBroker) wait(ctx context.Context) (uint64, transport.Connection, error) {
	for {
		broker.mu.Lock()
		if broker.closed {
			broker.mu.Unlock()
			return 0, nil, errors.New("attachment broker closed")
		}
		if broker.connection != nil {
			epoch, connection := broker.epoch, broker.connection
			broker.mu.Unlock()
			return epoch, connection, nil
		}
		changed := broker.changed
		broker.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-changed:
		}
	}
}

func (broker *attachmentBroker) writeControl(
	ctx context.Context,
	epoch uint64,
	connection transport.Connection,
	control *privatev1.Control,
) error {
	broker.writeMu.Lock()
	defer broker.writeMu.Unlock()
	broker.mu.Lock()
	current := !broker.closed && broker.epoch == epoch && broker.connection == connection
	broker.mu.Unlock()
	if !current {
		return errors.New("private attachment is no longer current")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return transport.WriteMessage(connection, control)
	}
}

func readReliableFrames(ctx context.Context, connection transport.Connection, output chan<- []byte, failures chan<- error) {
	for {
		payload, err := transport.ReadFrame(connection)
		if err != nil {
			select {
			case failures <- err:
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- payload:
		case <-ctx.Done():
			return
		}
	}
}

func relayCommands(ctx context.Context, broker *attachmentBroker, input <-chan []byte, failures chan<- error) {
	for {
		var payload []byte
		select {
		case <-ctx.Done():
			return
		case payload = <-input:
		}
		for {
			epoch, connection, err := broker.wait(ctx)
			if err != nil {
				return
			}
			control := &privatev1.Control{Payload: &privatev1.Control_ClientReliable{
				ClientReliable: &privatev1.RelayFrame{Payload: payload},
			}}
			if err := broker.writeControl(ctx, epoch, connection, control); err != nil {
				broker.clear(epoch, connection)
				_ = connection.Close()
				continue
			}
			break
		}
	}
}

func relayEvents(
	ctx context.Context,
	broker *attachmentBroker,
	sessionID string,
	epoch uint64,
	source, destination transport.Connection,
	lost chan<- error,
) {
	for {
		control := new(privatev1.Control)
		err := transport.ReadMessage(source, control)
		if err != nil {
			select {
			case lost <- err:
			case <-ctx.Done():
			}
			return
		}
		if frame := control.GetServerReliable(); frame != nil {
			if err := transport.WriteFrame(destination, frame.GetPayload()); err != nil {
				select {
				case lost <- err:
				case <-ctx.Done():
				}
				return
			}
		} else if transfer := control.GetTransferRequested(); transfer != nil {
			if transfer.GetSessionId() != sessionID || transfer.GetCurrentEpoch() != epoch {
				select {
				case lost <- errors.New("stale private transfer request"):
				case <-ctx.Done():
				}
				return
			}
			refusal := &privatev1.Control{Payload: &privatev1.Control_TransferRefused{
				TransferRefused: &privatev1.TransferRefused{
					SessionId: sessionID, CurrentEpoch: epoch,
					Code: privatev1.RefusalCode_REFUSAL_CODE_ZONE_UNAVAILABLE,
				},
			}}
			if err := broker.writeControl(ctx, epoch, source, refusal); err != nil {
				select {
				case lost <- err:
				case <-ctx.Done():
				}
				return
			}
			continue
		} else {
			select {
			case lost <- errors.New("unexpected private control after attach"):
			case <-ctx.Done():
			}
			return
		}
		broker.mu.Lock()
		current := broker.epoch == epoch && broker.connection == source
		broker.mu.Unlock()
		if !current {
			return
		}
	}
}

func relayNewest(ctx context.Context, source, destination transport.Connection, lost chan<- error) {
	for {
		payload, err := source.ReceiveUnreliable(ctx)
		if err != nil {
			return
		}
		if err := destination.SendUnreliable(payload); err != nil {
			select {
			case lost <- err:
			default:
			}
			return
		}
	}
}
