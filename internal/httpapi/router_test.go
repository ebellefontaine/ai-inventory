package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testHandler() http.Handler { return NewServer(discardLogger()).Handler() }

func do(t *testing.T, h http.Handler, method, target string, body io.Reader) capturedResponse {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, body))

	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	return capturedResponse{Status: res.StatusCode, Header: res.Header, Body: raw}
}

// TestHealthzNeedsNoAuthentication: a probe that needs a credential fails for
// reasons that have nothing to do with health.
func TestHealthzNeedsNoAuthentication(t *testing.T) {
	got := do(t, testHandler(), http.MethodGet, "/healthz", nil)

	require.Equal(t, http.StatusOK, got.Status)
	require.NotEmpty(t, got.Body)
	require.NotContains(t, string(got.Body), `"error"`)
}

func TestUnknownPathReturnsNotFoundEnvelope(t *testing.T) {
	for _, target := range []string{"/", "/nope", "/api/v1/agents", "/api/v2/agents/x"} {
		t.Run(target, func(t *testing.T) {
			got := do(t, testHandler(), http.MethodGet, target, nil)

			require.Equal(t, http.StatusNotFound, got.Status)
			require.Equal(t, codeNotFound, decodeEnvelope(t, got.Body).Error.Code)
		})
	}
}

func TestWrongMethodReturnsMethodNotAllowedEnvelope(t *testing.T) {
	cases := []struct {
		method, target, allow string
	}{
		{http.MethodPost, "/healthz", http.MethodGet},
		{http.MethodGet, "/api/v1/agents:register", http.MethodPost},
		{http.MethodDelete, "/api/v1/agents/7f3a/heartbeat", http.MethodPost},
		{http.MethodGet, "/api/v1/agents/7f3a", http.MethodPatch},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			got := do(t, testHandler(), tc.method, tc.target, nil)

			require.Equal(t, http.StatusMethodNotAllowed, got.Status)
			require.Equal(t, codeMethodNotAllowed, decodeEnvelope(t, got.Body).Error.Code)
			require.Equal(t, tc.allow, got.Header.Get("Allow"))
		})
	}
}

// TestAgentRoutesAreRegistered proves routing, not behaviour: Tasks 21-23
// replace the 501s. A 404 here would mean the pattern never matched.
func TestAgentRoutesAreRegistered(t *testing.T) {
	cases := []struct{ method, target string }{
		{http.MethodPost, "/api/v1/agents:register"},
		{http.MethodPost, "/api/v1/agents/7f3a/heartbeat"},
		{http.MethodPatch, "/api/v1/agents/7f3a"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			got := do(t, testHandler(), tc.method, tc.target, strings.NewReader(`{"name":"billing-triage"}`))

			require.Equal(t, http.StatusNotImplemented, got.Status)
			require.Equal(t, codeNotImplemented, decodeEnvelope(t, got.Body).Error.Code)
		})
	}
}

// TestEveryErrorResponseIsTheOneEnvelope: 404 and 405 come from the router
// rather than from a service, and they must not be shaped differently from the
// failures that do.
func TestEveryErrorResponseIsTheOneEnvelope(t *testing.T) {
	cases := []struct{ method, target string }{
		{http.MethodGet, "/nope"},
		{http.MethodPost, "/healthz"},
		{http.MethodPost, "/api/v1/agents:register"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			got := do(t, testHandler(), tc.method, tc.target, nil)

			require.Equal(t, "application/json; charset=utf-8", got.Header.Get("Content-Type"))
			env := decodeEnvelope(t, got.Body)
			require.NotEmpty(t, env.Error.Code)
			require.NotEmpty(t, env.Error.Message)
		})
	}
}
