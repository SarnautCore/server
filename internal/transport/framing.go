// Package transport defines the byte-stream boundary used by the game protocol.
package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

// MaxFrameSize limits allocations made from untrusted length prefixes.
const MaxFrameSize uint32 = 1 << 20

// MaxUnreliableMessageSize keeps datagrams below the common QUIC path limit.
const MaxUnreliableMessageSize = 1100

// ErrFrameTooLarge reports a frame that exceeds MaxFrameSize.
var ErrFrameTooLarge = errors.New("protobuf frame exceeds maximum size")

// WriteMessage writes a protobuf message with a four-byte, big-endian length prefix.
func WriteMessage(writer io.Writer, message proto.Message) error {
	payload, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal protobuf message: %w", err)
	}
	if len(payload) > int(MaxFrameSize) {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(payload))
	}

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeFull(writer, header[:]); err != nil {
		return fmt.Errorf("write frame length: %w", err)
	}
	if err := writeFull(writer, payload); err != nil {
		return fmt.Errorf("write frame payload: %w", err)
	}

	return nil
}

func writeFull(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	return nil
}

// ReadMessage reads a length-prefixed protobuf message from a byte stream.
func ReadMessage(reader io.Reader, message proto.Message) error {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return fmt.Errorf("read frame length: %w", err)
	}

	length := binary.BigEndian.Uint32(header[:])
	if length > MaxFrameSize {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, length)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return fmt.Errorf("read frame payload: %w", err)
	}
	if err := proto.Unmarshal(payload, message); err != nil {
		return fmt.Errorf("unmarshal protobuf message: %w", err)
	}

	return nil
}

// MarshalUnreliable serializes a protobuf message for an unreliable packet.
func MarshalUnreliable(message proto.Message) ([]byte, error) {
	payload, err := proto.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("marshal unreliable protobuf message: %w", err)
	}
	if len(payload) > MaxUnreliableMessageSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(payload))
	}
	return payload, nil
}

// UnmarshalUnreliable decodes one protobuf message from an unreliable packet.
func UnmarshalUnreliable(payload []byte, message proto.Message) error {
	if len(payload) > MaxUnreliableMessageSize {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(payload))
	}
	if err := proto.Unmarshal(payload, message); err != nil {
		return fmt.Errorf("unmarshal unreliable protobuf message: %w", err)
	}
	return nil
}
