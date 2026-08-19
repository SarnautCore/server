package transport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
)

func TestWriteMessageWritesLengthPrefixedProtobuf(t *testing.T) {
	t.Parallel()

	var got bytes.Buffer
	message := &sarnautv1.ClientHello{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildId:         "dev",
	}

	if err := WriteMessage(&got, message); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}

	want := []byte{0, 0, 0, 7, 0x08, 0x01, 0x12, 0x03, 'd', 'e', 'v'}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("WriteMessage() = %x, want %x", got.Bytes(), want)
	}
}

func TestReadMessageRejectsOversizedFrame(t *testing.T) {
	t.Parallel()

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxFrameSize+1)

	err := ReadMessage(bytes.NewReader(header[:]), new(sarnautv1.ClientHello))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("ReadMessage() error = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadMessageReadsLengthPrefixedProtobuf(t *testing.T) {
	t.Parallel()

	frame := []byte{0, 0, 0, 7, 0x08, 0x01, 0x12, 0x03, 'd', 'e', 'v'}
	got := new(sarnautv1.ClientHello)

	if err := ReadMessage(bytes.NewReader(frame), got); err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	if got.GetProtocolVersion() != sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1 {
		t.Errorf("protocol_version = %v, want PROTOCOL_VERSION_1", got.GetProtocolVersion())
	}
	if got.GetBuildId() != "dev" {
		t.Errorf("build_id = %q, want %q", got.GetBuildId(), "dev")
	}
}

func TestWriteMessageHandlesShortWrites(t *testing.T) {
	t.Parallel()

	writer := new(oneByteWriter)
	message := &sarnautv1.ClientHello{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildId:         "dev",
	}

	if err := WriteMessage(writer, message); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}

	want := []byte{0, 0, 0, 7, 0x08, 0x01, 0x12, 0x03, 'd', 'e', 'v'}
	if !bytes.Equal(writer.Bytes(), want) {
		t.Fatalf("WriteMessage() = %x, want %x", writer.Bytes(), want)
	}
}

func TestWriteMessageRejectsOversizedFrame(t *testing.T) {
	t.Parallel()

	message := &sarnautv1.ClientHello{BuildId: strings.Repeat("x", int(MaxFrameSize)+1)}
	err := WriteMessage(new(bytes.Buffer), message)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("WriteMessage() error = %v, want ErrFrameTooLarge", err)
	}
}

type oneByteWriter struct {
	bytes.Buffer
}

func (writer *oneByteWriter) Write(payload []byte) (int, error) {
	if len(payload) > 1 {
		payload = payload[:1]
	}
	return writer.Buffer.Write(payload)
}
