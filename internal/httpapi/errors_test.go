package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

// capturedResponse is everything a caller can observe about a response.
type capturedResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// respondTo drives the error mapper end to end and captures what went out.
func respondTo(t *testing.T, err error) capturedResponse {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/heartbeat", nil)
	respondError(rec, req, discardLogger(), err)

	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	body, readErr := io.ReadAll(res.Body)
	require.NoError(t, readErr)

	return capturedResponse{Status: res.StatusCode, Header: res.Header, Body: body}
}

func decodeEnvelope(t *testing.T, body []byte) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(body, &env), "every error response is the one envelope")
	return env
}

// TestAuthFailuresAreByteIdentical is the reason this package exists. The
// services distinguish revoked from expired from absent for the audit trail; if
// any of that reaches the wire, a caller can enumerate live credentials by
// diffing responses.
//
// The assertion is deliberately against the first response rather than a
// hardcoded literal: a table of eight expected bodies could drift into
// asserting eight different-but-expected answers, which is the bug this test
// exists to catch.
func TestAuthFailuresAreByteIdentical(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"token not found", service.ErrTokenNotFound},
		{"token revoked", service.ErrTokenRevoked},
		{"token expired", service.ErrTokenExpired},
		{"invalid environment", service.ErrInvalidEnvironment},
		{"credential not found", service.ErrCredentialNotFound},
		{"credential revoked", service.ErrCredentialRevoked},
		{"credential expired", service.ErrCredentialExpired},
		{"credential agent mismatch", service.ErrCredentialAgentMismatch},

		// Wrapped, because services return their sentinels through call stacks
		// that add context. A mapper that compared with == rather than
		// errors.Is would pass the bare cases and leak on the wrapped ones.
		{"wrapped token revoked", fmt.Errorf("register: %w", service.ErrTokenRevoked)},
		{"wrapped credential mismatch", fmt.Errorf("heartbeat: authenticate: %w", service.ErrCredentialAgentMismatch)},

		// A request that carried no usable bearer token at all must be
		// indistinguishable from one that carried a dead credential.
		{"no bearer credential", errUnauthenticated},
	}

	require.GreaterOrEqual(t, len(cases), 8, "all eight token and credential sentinels must be exercised")

	first := respondTo(t, cases[0].err)
	require.Equal(t, http.StatusUnauthorized, first.Status)
	require.Equal(t, codeUnauthorized, decodeEnvelope(t, first.Body).Error.Code)
	require.Equal(t, "invalid credentials", decodeEnvelope(t, first.Body).Error.Message)

	for _, tc := range cases[1:] {
		t.Run(tc.name, func(t *testing.T) {
			got := respondTo(t, tc.err)
			require.Equal(t, first.Status, got.Status, "status must not distinguish %v", tc.err)
			require.Equal(t, first.Header, got.Header, "headers must not distinguish %v", tc.err)
			require.Equal(t, first.Body, got.Body, "body bytes must not distinguish %v", tc.err)
		})
	}
}

// TestAuthFailureBodyNamesNoSentinel guards the other direction: identical
// responses that all happened to say "credential revoked" would pass the test
// above and still be a leak.
func TestAuthFailureBodyNamesNoSentinel(t *testing.T) {
	got := respondTo(t, service.ErrCredentialRevoked)

	for _, forbidden := range []string{"revoked", "expired", "not found", "mismatch", "different agent", "enrollment"} {
		require.NotContains(t, string(got.Body), forbidden)
	}
}

// TestUnusableAuthorizationHeadersProduceTheSame401 ties header parsing to the
// same response as a dead credential.
func TestUnusableAuthorizationHeadersProduceTheSame401(t *testing.T) {
	canonical := respondTo(t, service.ErrCredentialRevoked)

	for name, header := range map[string]string{
		"missing":       "",
		"scheme only":   "Bearer",
		"empty token":   "Bearer ",
		"blank token":   "Bearer    ",
		"wrong scheme":  "Basic aGk6dGhlcmU=",
		"token only":    "aiinv_ac_abc123",
		"wrong scheme2": "Token aiinv_ac_abc123",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/heartbeat", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}

			token, err := bearerToken(req)
			require.Empty(t, token)
			require.ErrorIs(t, err, errUnauthenticated)

			got := respondTo(t, err)
			require.Equal(t, canonical.Status, got.Status)
			require.Equal(t, canonical.Header, got.Header)
			require.Equal(t, canonical.Body, got.Body)
		})
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		t.Run(scheme, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/heartbeat", nil)
			req.Header.Set("Authorization", scheme+" aiinv_ac_secret")

			token, err := bearerToken(req)

			require.NoError(t, err)
			require.Equal(t, "aiinv_ac_secret", token)
		})
	}
}

func TestNameRequiredMapsToBadRequest(t *testing.T) {
	got := respondTo(t, fmt.Errorf("register: %w", domain.ErrNameRequired))

	require.Equal(t, http.StatusBadRequest, got.Status)
	require.Equal(t, codeBadRequest, decodeEnvelope(t, got.Body).Error.Code)
}

func TestMalformedJSONMapsToBadRequest(t *testing.T) {
	_, err := domain.DecodeDescriptor([]byte(`{"name":`))
	require.Error(t, err)

	got := respondTo(t, err)

	require.Equal(t, http.StatusBadRequest, got.Status)
	require.Equal(t, codeBadRequest, decodeEnvelope(t, got.Body).Error.Code)
}

// TestUnrecognisedErrorIsGeneric500 covers the leak the service errors would
// cause if echoed: they are wrapped with SQL context that names tables and
// columns.
func TestUnrecognisedErrorIsGeneric500(t *testing.T) {
	err := fmt.Errorf("heartbeat: resolve credential: %w",
		fmt.Errorf(`ERROR: relation "agent_credentials" does not exist (SQLSTATE 42P01)`))

	got := respondTo(t, err)

	require.Equal(t, http.StatusInternalServerError, got.Status)
	require.Equal(t, codeInternal, decodeEnvelope(t, got.Body).Error.Code)
	for _, leak := range []string{"agent_credentials", "SQLSTATE", "42P01", "relation", "resolve credential"} {
		require.NotContains(t, string(got.Body), leak, "server error text must not reach the wire")
	}
}

// TestErrorsAreLoggedServerSide: the diagnostics the services went to the
// trouble of producing must survive somewhere, just not on the wire.
func TestErrorsAreLoggedServerSide(t *testing.T) {
	logs, logger := captureLogs()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/heartbeat", nil)
	respondError(rec, req, logger, fmt.Errorf("heartbeat: %w", service.ErrCredentialRevoked))

	require.Contains(t, logs.String(), "agent credential revoked")
}
