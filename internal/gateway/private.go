package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	privatev1 "github.com/SarnautCore/server/gen/sarnaut/private/v1"
	"github.com/SarnautCore/server/internal/transport"
)

const PrivateProtocolVersion uint32 = 1

var privateMagic = []byte("SARNAUT-GATEWAY\x00")

const nonceSize = 32

type TrustConfig struct {
	InstanceID string
	ShardID    string
	KeyID      string
	Secret     []byte
	Random     io.Reader
}

func (config TrustConfig) validate() error {
	if config.InstanceID == "" || config.ShardID == "" || config.KeyID == "" {
		return errors.New("private trust ids and key id must not be empty")
	}
	if len(config.Secret) != 32 {
		return fmt.Errorf("private trust secret is %d bytes, want 32", len(config.Secret))
	}
	return nil
}

func (config TrustConfig) random() io.Reader {
	if config.Random != nil {
		return config.Random
	}
	return rand.Reader
}

// AuthenticateGateway proves the gateway side of a private connection and
// verifies the shard response before the caller may assert an identity.
func AuthenticateGateway(ctx context.Context, connection transport.Connection, config TrustConfig) error {
	if err := config.validate(); err != nil {
		return err
	}
	challenge := new(privatev1.Challenge)
	if err := transport.ReadMessage(connection, challenge); err != nil {
		return fmt.Errorf("read private challenge: %w", err)
	}
	if subtle.ConstantTimeCompare(challenge.GetMagic(), privateMagic) != 1 ||
		challenge.GetProtocolVersion() != PrivateProtocolVersion ||
		challenge.GetShardId() != config.ShardID || challenge.GetKeyId() != config.KeyID ||
		len(challenge.GetShardNonce()) != nonceSize {
		return errors.New("private challenge was refused")
	}
	gatewayNonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(config.random(), gatewayNonce); err != nil {
		return fmt.Errorf("generate gateway nonce: %w", err)
	}
	proof := transcriptMAC(config.Secret, "gateway", config.InstanceID, config.ShardID,
		challenge.GetShardNonce(), gatewayNonce, PrivateProtocolVersion)
	if err := transport.WriteMessage(connection, &privatev1.GatewayProof{
		GatewayInstanceId: config.InstanceID, GatewayNonce: gatewayNonce, Proof: proof,
	}); err != nil {
		return fmt.Errorf("write gateway proof: %w", err)
	}
	response := new(privatev1.ShardProof)
	if err := transport.ReadMessage(connection, response); err != nil {
		return fmt.Errorf("read shard proof: %w", err)
	}
	want := transcriptMAC(config.Secret, "shard", config.InstanceID, config.ShardID,
		challenge.GetShardNonce(), gatewayNonce, PrivateProtocolVersion)
	if !hmac.Equal(response.GetProof(), want) {
		return errors.New("private shard proof was refused")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// AuthenticateShard proves the shard side of a private connection. A caller
// must not read an AttachRequest until this function succeeds.
func AuthenticateShard(ctx context.Context, connection transport.Connection, config TrustConfig) error {
	if err := config.validate(); err != nil {
		return err
	}
	shardNonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(config.random(), shardNonce); err != nil {
		return fmt.Errorf("generate shard nonce: %w", err)
	}
	if err := transport.WriteMessage(connection, &privatev1.Challenge{
		Magic: privateMagic, ProtocolVersion: PrivateProtocolVersion,
		ShardId: config.ShardID, KeyId: config.KeyID, ShardNonce: shardNonce,
	}); err != nil {
		return fmt.Errorf("write private challenge: %w", err)
	}
	request := new(privatev1.GatewayProof)
	if err := transport.ReadMessage(connection, request); err != nil {
		return fmt.Errorf("read gateway proof: %w", err)
	}
	if request.GetGatewayInstanceId() != config.InstanceID || len(request.GetGatewayNonce()) != nonceSize {
		return errors.New("private gateway identity was refused")
	}
	want := transcriptMAC(config.Secret, "gateway", config.InstanceID, config.ShardID,
		shardNonce, request.GetGatewayNonce(), PrivateProtocolVersion)
	if !hmac.Equal(request.GetProof(), want) {
		return errors.New("private gateway proof was refused")
	}
	response := transcriptMAC(config.Secret, "shard", config.InstanceID, config.ShardID,
		shardNonce, request.GetGatewayNonce(), PrivateProtocolVersion)
	if err := transport.WriteMessage(connection, &privatev1.ShardProof{Proof: response}); err != nil {
		return fmt.Errorf("write shard proof: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func transcriptMAC(secret []byte, direction, gatewayID, shardID string, shardNonce, gatewayNonce []byte, version uint32) []byte {
	mac := hmac.New(sha256.New, secret)
	writeMACPart(mac, []byte("sarnaut-private-session-v1"))
	writeMACPart(mac, []byte(direction))
	writeMACPart(mac, []byte(gatewayID))
	writeMACPart(mac, []byte(shardID))
	writeMACPart(mac, shardNonce)
	writeMACPart(mac, gatewayNonce)
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], version)
	writeMACPart(mac, encoded[:])
	return mac.Sum(nil)
}

func writeMACPart(writer io.Writer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}
