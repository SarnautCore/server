// Package health exposes process liveness and readiness probes.
package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// Status tracks whether a process can serve traffic.
type Status struct {
	ready atomic.Bool
}

func (status *Status) SetReady(ready bool) {
	status.ready.Store(ready)
}

// Handler returns /healthz and /readyz endpoints.
func Handler(status *Status) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeStatus(writer, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if !status.ready.Load() {
			writeStatus(writer, http.StatusServiceUnavailable, "not ready\n")
			return
		}
		writeStatus(writer, http.StatusOK, "ready\n")
	})
	return mux
}

// Serve runs the health HTTP server until ctx is canceled.
func Serve(ctx context.Context, address string, status *Status) error {
	server := &http.Server{
		Addr:              address,
		Handler:           Handler(status),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errorsChannel := make(chan error, 1)
	go func() {
		errorsChannel <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down health server: %w", err)
		}
		return nil
	case err := <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve health endpoints: %w", err)
	}
}

func writeStatus(writer http.ResponseWriter, statusCode int, body string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(statusCode)
	_, _ = writer.Write([]byte(body))
}
