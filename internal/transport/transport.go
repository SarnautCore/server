package transport

import (
	"context"
	"io"
	"net"
)

// Connection is the ordered byte stream used by the session protocol.
// Implementations may use QUIC, raw UDP with reliability, or another transport.
type Connection interface {
	io.Reader
	io.Writer
	io.Closer
	CloseWrite() error
	SupportsUnreliable() bool
	SendUnreliable([]byte) error
	ReceiveUnreliable(context.Context) ([]byte, error)
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

// Listener accepts transport-independent session connections.
type Listener interface {
	Accept(context.Context) (Connection, error)
	Addr() net.Addr
	Close() error
}
