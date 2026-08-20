package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// acceptStreamTimeout bounds how long a peer may hold an accepted QUIC
// connection without opening the session stream on it.
const acceptStreamTimeout = 10 * time.Second

type quicListener struct {
	listener *quic.Listener

	// accepted carries connections whose session stream is already open. It
	// exists because accepting the stream cannot happen on the caller's
	// goroutine: a QUIC stream is invisible to the peer until the opener writes
	// on it, so a client that connects and stays silent would block AcceptStream
	// — and with it the shard's whole accept loop — for everyone else. The stream
	// accept therefore runs per connection, with a deadline of its own.
	accepted chan acceptedConnection
	start    sync.Once
	stop     sync.Once
	closed   chan struct{}
}

type acceptedConnection struct {
	connection Connection
	err        error
}

type quicConnection struct {
	connection *quic.Conn
	stream     *quic.Stream
}

// ListenQUIC listens for QUIC connections and exposes their first bidirectional stream.
func ListenQUIC(address string, tlsConfig *tls.Config) (Listener, error) {
	listener, err := quic.ListenAddr(address, tlsConfig, quicConfig())
	if err != nil {
		return nil, fmt.Errorf("listen for QUIC connections: %w", err)
	}
	return &quicListener{
		listener: listener,
		accepted: make(chan acceptedConnection),
		closed:   make(chan struct{}),
	}, nil
}

// DialQUIC opens a QUIC connection and its first bidirectional stream.
func DialQUIC(ctx context.Context, address string, tlsConfig *tls.Config) (Connection, error) {
	connection, err := quic.DialAddr(ctx, address, tlsConfig, quicConfig())
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

func quicConfig() *quic.Config {
	return &quic.Config{EnableDatagrams: true}
}

func (listener *quicListener) Accept(ctx context.Context) (Connection, error) {
	listener.start.Do(func() { go listener.pump() })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-listener.closed:
		return nil, net.ErrClosed
	case accepted := <-listener.accepted:
		return accepted.connection, accepted.err
	}
}

// pump accepts QUIC connections and opens each one's session stream on a
// goroutine of its own, so a peer that never opens one delays nobody but itself.
func (listener *quicListener) pump() {
	for {
		connection, err := listener.listener.Accept(context.Background())
		if err != nil {
			listener.offer(acceptedConnection{err: fmt.Errorf("accept QUIC connection: %w", err)})
			return
		}
		go listener.acceptStream(connection)
	}
}

func (listener *quicListener) acceptStream(connection *quic.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), acceptStreamTimeout)
	defer cancel()

	stream, err := connection.AcceptStream(ctx)
	if err != nil {
		// A connection with no session stream is not a listener failure and must
		// not be reported as one: it is one peer that never started talking.
		_ = connection.CloseWithError(0, "no session stream was opened")
		return
	}
	if !listener.offer(acceptedConnection{
		connection: &quicConnection{connection: connection, stream: stream},
	}) {
		_ = connection.CloseWithError(0, "listener closed")
	}
}

// offer hands one accept result to whoever is calling Accept, and reports
// whether anyone took it before the listener closed.
func (listener *quicListener) offer(accepted acceptedConnection) bool {
	select {
	case listener.accepted <- accepted:
		return true
	case <-listener.closed:
		return false
	}
}

func (listener *quicListener) Addr() net.Addr {
	return listener.listener.Addr()
}

func (listener *quicListener) Close() error {
	listener.stop.Do(func() { close(listener.closed) })
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

func (connection *quicConnection) SupportsUnreliable() bool {
	state := connection.connection.ConnectionState().SupportsDatagrams
	return state.Local && state.Remote
}

func (connection *quicConnection) SendUnreliable(payload []byte) error {
	return connection.connection.SendDatagram(payload)
}

func (connection *quicConnection) ReceiveUnreliable(ctx context.Context) ([]byte, error) {
	return connection.connection.ReceiveDatagram(ctx)
}

func (connection *quicConnection) LocalAddr() net.Addr {
	return connection.connection.LocalAddr()
}

func (connection *quicConnection) RemoteAddr() net.Addr {
	return connection.connection.RemoteAddr()
}
