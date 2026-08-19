package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/quic-go/quic-go"
)

type quicListener struct {
	listener *quic.Listener
}

type quicConnection struct {
	connection *quic.Conn
	stream     *quic.Stream
}

// ListenQUIC listens for QUIC connections and exposes their first bidirectional stream.
func ListenQUIC(address string, tlsConfig *tls.Config) (Listener, error) {
	listener, err := quic.ListenAddr(address, tlsConfig, nil)
	if err != nil {
		return nil, fmt.Errorf("listen for QUIC connections: %w", err)
	}
	return &quicListener{listener: listener}, nil
}

// DialQUIC opens a QUIC connection and its first bidirectional stream.
func DialQUIC(ctx context.Context, address string, tlsConfig *tls.Config) (Connection, error) {
	connection, err := quic.DialAddr(ctx, address, tlsConfig, nil)
	if err != nil {
		return nil, fmt.Errorf("dial QUIC endpoint: %w", err)
	}

	stream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		_ = connection.CloseWithError(0, "open stream failed")
		return nil, fmt.Errorf("open QUIC stream: %w", err)
	}

	return &quicConnection{connection: connection, stream: stream}, nil
}

func (listener *quicListener) Accept(ctx context.Context) (Connection, error) {
	connection, err := listener.listener.Accept(ctx)
	if err != nil {
		return nil, fmt.Errorf("accept QUIC connection: %w", err)
	}

	stream, err := connection.AcceptStream(ctx)
	if err != nil {
		_ = connection.CloseWithError(0, "accept stream failed")
		return nil, fmt.Errorf("accept QUIC stream: %w", err)
	}

	return &quicConnection{connection: connection, stream: stream}, nil
}

func (listener *quicListener) Addr() net.Addr {
	return listener.listener.Addr()
}

func (listener *quicListener) Close() error {
	return listener.listener.Close()
}

func (connection *quicConnection) Read(payload []byte) (int, error) {
	return connection.stream.Read(payload)
}

func (connection *quicConnection) Write(payload []byte) (int, error) {
	return connection.stream.Write(payload)
}

func (connection *quicConnection) Close() error {
	return errors.Join(
		connection.stream.Close(),
		connection.connection.CloseWithError(0, "session closed"),
	)
}

func (connection *quicConnection) CloseWrite() error {
	return connection.stream.Close()
}

func (connection *quicConnection) LocalAddr() net.Addr {
	return connection.connection.LocalAddr()
}

func (connection *quicConnection) RemoteAddr() net.Addr {
	return connection.connection.RemoteAddr()
}
