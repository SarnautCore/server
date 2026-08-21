package session

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	privatev1 "github.com/SarnautCore/server/gen/sarnaut/private/v1"
	"github.com/SarnautCore/server/internal/gateway"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// ServePrivate accepts authenticated gateway attachments. Public hello,
// tickets, and play-lock traffic terminate before this interface.
func (server Server) ServePrivate(
	ctx context.Context,
	listener transport.Listener,
	trust gateway.TrustConfig,
) error {
	if server.Characters == nil {
		return errors.New("private shard server requires character storage")
	}
	if server.sessions == nil {
		server.sessions = newSessionRegistry()
	}
	if server.privateAttachments == nil {
		server.privateAttachments = newPrivateAttachmentRegistry()
	}
	for {
		connection, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept private attachment: %w", err)
		}
		go func() {
			if err := server.handlePrivate(ctx, connection, trust); err != nil {
				server.logger().Info("private attachment ended", "error", err)
			}
			_ = connection.Close()
		}()
	}
}

func (server Server) handlePrivate(
	parent context.Context,
	connection transport.Connection,
	trust gateway.TrustConfig,
) error {
	ctx, span := tracer.Start(parent, "session.private_attachment")
	defer span.End()
	span.SetAttributes(attribute.String("network.peer.address", connection.RemoteAddr().String()))
	if err := gateway.AuthenticateShard(ctx, connection, trust); err != nil {
		return err
	}
	control := new(privatev1.Control)
	if err := transport.ReadMessage(connection, control); err != nil {
		return fmt.Errorf("read private control: %w", err)
	}
	request := control.GetAttachRequest()
	if request == nil {
		return server.refusePrivate(connection, request, privatev1.RefusalCode_REFUSAL_CODE_INVALID_REQUEST,
			"first private control message must be AttachRequest")
	}
	identity, err := validateAttachRequest(request)
	if err != nil {
		return server.refusePrivate(connection, request, privatev1.RefusalCode_REFUSAL_CODE_INVALID_REQUEST, err.Error())
	}
	binding, ok := server.Zones[request.GetZoneId()]
	if !ok || binding.World == nil {
		return server.refusePrivate(connection, request, privatev1.RefusalCode_REFUSAL_CODE_ZONE_UNAVAILABLE,
			"requested zone is not hosted")
	}
	release, err := server.privateAttachments.claim(
		request.GetSessionId(), request.GetAttachmentEpoch(), identity.CharacterID,
	)
	if err != nil {
		code := privatev1.RefusalCode_REFUSAL_CODE_STALE_EPOCH
		if errors.Is(err, errDuplicateCharacterAttachment) {
			code = privatev1.RefusalCode_REFUSAL_CODE_DUPLICATE_CHARACTER
		}
		return server.refusePrivate(connection, request, code, err.Error())
	}
	defer release()

	if err := transport.WriteMessage(connection, &privatev1.Control{Payload: &privatev1.Control_Attached{
		Attached: &privatev1.Attached{
			SessionId: request.GetSessionId(), AttachmentEpoch: request.GetAttachmentEpoch(), ZoneId: request.GetZoneId(),
		},
	}}); err != nil {
		return fmt.Errorf("write private attach acknowledgement: %w", err)
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-finished:
		case <-sessionCtx.Done():
			_ = connection.Close()
		}
	}()
	return server.runAttachment(
		ctx, sessionCtx, cancel, span, newPrivateRelayConnection(connection), binding, identity,
		request.GetMinimumSaveSeq(),
	)
}

// privateRelayConnection presents the public framed stream to the shard session
// while the private wire keeps control and relayed gameplay as distinct typed
// Control cases.
type privateRelayConnection struct {
	transport.Connection
	readMu      sync.Mutex
	readBuffer  []byte
	writeMu     sync.Mutex
	writeBuffer []byte
}

func newPrivateRelayConnection(connection transport.Connection) *privateRelayConnection {
	return &privateRelayConnection{Connection: connection}
}

func (connection *privateRelayConnection) Read(destination []byte) (int, error) {
	connection.readMu.Lock()
	defer connection.readMu.Unlock()
	for len(connection.readBuffer) == 0 {
		control := new(privatev1.Control)
		if err := transport.ReadMessage(connection.Connection, control); err != nil {
			return 0, err
		}
		frame := control.GetClientReliable()
		if frame == nil {
			return 0, errors.New("private relay expected client_reliable control")
		}
		payload := frame.GetPayload()
		if len(payload) > int(transport.MaxFrameSize) {
			return 0, transport.ErrFrameTooLarge
		}
		connection.readBuffer = make([]byte, 4+len(payload))
		binary.BigEndian.PutUint32(connection.readBuffer[:4], uint32(len(payload)))
		copy(connection.readBuffer[4:], payload)
	}
	written := copy(destination, connection.readBuffer)
	connection.readBuffer = connection.readBuffer[written:]
	return written, nil
}

func (connection *privateRelayConnection) Write(source []byte) (int, error) {
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	connection.writeBuffer = append(connection.writeBuffer, source...)
	for len(connection.writeBuffer) >= 4 {
		length := binary.BigEndian.Uint32(connection.writeBuffer[:4])
		if length > transport.MaxFrameSize {
			return 0, transport.ErrFrameTooLarge
		}
		frameLength := 4 + int(length)
		if len(connection.writeBuffer) < frameLength {
			break
		}
		payload := append([]byte(nil), connection.writeBuffer[4:frameLength]...)
		connection.writeBuffer = connection.writeBuffer[frameLength:]
		if err := transport.WriteMessage(connection.Connection, &privatev1.Control{
			Payload: &privatev1.Control_ServerReliable{ServerReliable: &privatev1.RelayFrame{Payload: payload}},
		}); err != nil {
			return 0, err
		}
	}
	return len(source), nil
}

func validateAttachRequest(request *privatev1.AttachRequest) (Admission, error) {
	if request.GetAttachmentEpoch() == 0 {
		return Admission{}, errors.New("attachment epoch must be positive")
	}
	if request.GetMinimumSaveSeq() < 0 {
		return Admission{}, errors.New("minimum save sequence must not be negative")
	}
	if _, err := uuid.Parse(request.GetSessionId()); err != nil {
		return Admission{}, errors.New("session id is not a UUID")
	}
	assertion := request.GetIdentity()
	if assertion == nil || request.GetZoneId() == "" {
		return Admission{}, errors.New("zone and asserted identity are required")
	}
	accountID, err := uuid.Parse(assertion.GetAccountId())
	if err != nil {
		return Admission{}, errors.New("asserted account id is not a UUID")
	}
	characterID, err := uuid.Parse(assertion.GetCharacterId())
	if err != nil {
		return Admission{}, errors.New("asserted character id is not a UUID")
	}
	if assertion.GetCharacterName() == "" || assertion.GetChargenOptionId() == "" {
		return Admission{}, errors.New("asserted character metadata is incomplete")
	}
	return Admission{
		AccountID: accountID, CharacterID: characterID,
		CharacterName: assertion.GetCharacterName(), ChargenOptionID: assertion.GetChargenOptionId(),
	}, nil
}

func (server Server) refusePrivate(
	connection transport.Connection,
	request *privatev1.AttachRequest,
	code privatev1.RefusalCode,
	detail string,
) error {
	refusal := &privatev1.AttachRefused{Code: code, Detail: detail}
	if request != nil {
		refusal.SessionId = request.GetSessionId()
		refusal.AttachmentEpoch = request.GetAttachmentEpoch()
	}
	writeErr := transport.WriteMessage(connection, &privatev1.Control{Payload: &privatev1.Control_AttachRefused{
		AttachRefused: refusal,
	}})
	err := fmt.Errorf("private attach refused %s: %s", code, detail)
	if writeErr != nil {
		return errors.Join(err, writeErr)
	}
	return err
}

type privateAttachmentRegistry struct {
	mu               sync.Mutex
	lastEpoch        map[string]uint64
	activeCharacters map[uuid.UUID]privateAttachmentKey
}

type privateAttachmentKey struct {
	sessionID string
	epoch     uint64
}

var errDuplicateCharacterAttachment = errors.New("character already has an active shard attachment")

func newPrivateAttachmentRegistry() *privateAttachmentRegistry {
	return &privateAttachmentRegistry{
		lastEpoch: make(map[string]uint64), activeCharacters: make(map[uuid.UUID]privateAttachmentKey),
	}
}

func (registry *privateAttachmentRegistry) claim(sessionID string, epoch uint64, characterID uuid.UUID) (func(), error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if epoch <= registry.lastEpoch[sessionID] {
		return nil, fmt.Errorf("attachment epoch %d is not newer than %d", epoch, registry.lastEpoch[sessionID])
	}
	key := privateAttachmentKey{sessionID: sessionID, epoch: epoch}
	if active, exists := registry.activeCharacters[characterID]; exists && active != key {
		return nil, errDuplicateCharacterAttachment
	}
	registry.lastEpoch[sessionID] = epoch
	registry.activeCharacters[characterID] = key
	return func() {
		registry.mu.Lock()
		if registry.activeCharacters[characterID] == key {
			delete(registry.activeCharacters, characterID)
		}
		registry.mu.Unlock()
	}, nil
}
