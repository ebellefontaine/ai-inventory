package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// captureLogs returns a logger writing JSON into the buffer, so a test can
// assert both what operators keep and what must never be written down.
func captureLogs() (*bytes.Buffer, *slog.Logger) {
	buf := &bytes.Buffer{}
	return buf, slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestPanicIsRecoveredAndServerKeepsServing: a panic in one handler must cost
// one request, not the process.
func TestPanicIsRecoveredAndServerKeepsServing(t *testing.T) {
	logs, logger := captureLogs()
	handler := NewServer(logger).wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("descriptor decode exploded")
		}
		w.WriteHeader(http.StatusOK)
	}))

	got := do(t, handler, http.MethodGet, "/boom", nil)
	require.Equal(t, http.StatusInternalServerError, got.Status)
	require.Equal(t, codeInternal, decodeEnvelope(t, got.Body).Error.Code)
	require.NotContains(t, string(got.Body), "descriptor decode exploded", "panic text is a server detail")
	require.Contains(t, logs.String(), "descriptor decode exploded", "operators keep the panic")

	next := do(t, handler, http.MethodGet, "/ok", nil)
	require.Equal(t, http.StatusOK, next.Status, "the next request still succeeds")
}

// TestOversizedDeclaredBodyIsRejected: a declared Content-Length over the cap
// is refused before the body is read at all.
func TestOversizedDeclaredBodyIsRejected(t *testing.T) {
	oversized := strings.Repeat("a", MaxBodyBytes+1)

	got := do(t, testHandler(), http.MethodPost, "/api/v1/agents:register", strings.NewReader(oversized))

	require.Equal(t, http.StatusRequestEntityTooLarge, got.Status)
	require.Equal(t, codePayloadTooLarge, decodeEnvelope(t, got.Body).Error.Code)
}

// TestOversizedUndeclaredBodyIsRejected: a client that declares no length, or
// lies about it, is caught by MaxBytesReader when the handler reads.
func TestOversizedUndeclaredBodyIsRejected(t *testing.T) {
	logger := discardLogger()
	handler := NewServer(logger).wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			respondError(w, r, logger, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents:register",
		strings.NewReader(strings.Repeat("a", MaxBodyBytes+1)))
	req.ContentLength = -1 // chunked: the size is not known up front
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusRequestEntityTooLarge, res.StatusCode)
	require.Equal(t, codePayloadTooLarge, decodeEnvelope(t, body).Error.Code)
}

// TestBodyAtTheCapIsAccepted guards against a limit that rejects everything.
func TestBodyAtTheCapIsAccepted(t *testing.T) {
	got := do(t, testHandler(), http.MethodPost, "/api/v1/agents:register",
		strings.NewReader(strings.Repeat("a", MaxBodyBytes)))

	require.NotEqual(t, http.StatusRequestEntityTooLarge, got.Status)
}

// TestRequestLogRecordsTheRequestButNoCredential: the log is the operator's
// view of the traffic, and it is also a place a token would live forever.
func TestRequestLogRecordsTheRequestButNoCredential(t *testing.T) {
	logs, logger := captureLogs()
	handler := NewServer(logger).Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents:register", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer aiinv_ac_supersecretvalue")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	line := logs.String()
	require.Contains(t, line, `"method":"POST"`)
	require.Contains(t, line, `"path":"/api/v1/agents:register"`)
	require.Contains(t, line, `"status":501`)
	require.Contains(t, line, `"duration_ms"`)

	require.NotContains(t, line, "aiinv_ac_supersecretvalue", "a token in a log is a token that outlives its rotation")
	require.NotContains(t, line, "Authorization")
	require.NotContains(t, line, "Bearer")
}
