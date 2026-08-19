package health_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SarnautCore/server/internal/health"
)

func TestHandlerReportsLivenessAndReadiness(t *testing.T) {
	t.Parallel()

	status := new(health.Status)
	handler := health.Handler(status)

	assertStatus(t, handler, "/healthz", http.StatusOK)
	assertStatus(t, handler, "/readyz", http.StatusServiceUnavailable)

	status.SetReady(true)
	assertStatus(t, handler, "/readyz", http.StatusOK)
}

func assertStatus(t *testing.T, handler http.Handler, path string, want int) {
	t.Helper()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("GET %s status = %d, want %d", path, response.Code, want)
	}
}
