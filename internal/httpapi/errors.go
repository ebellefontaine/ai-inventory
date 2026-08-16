package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

// The codes a client may ever see. They are a closed set on purpose: a client
// can branch on them, and an operator reading a support ticket can map one back
// to a class of failure without needing the message to be stable.
const (
	codeUnauthorized     = "unauthorized"
	codeBadRequest       = "bad_request"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codePayloadTooLarge  = "payload_too_large"
	codeInternal         = "internal"
)

// errorEnvelope is the one shape every failure is reported in, whatever layer
// produced it. Nesting under "error" leaves room for a success payload to be
// added at the top level later without a client having to guess which of two
// shapes it received.
type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// apiError is a complete response: the status, the code, and the exact message
// that goes on the wire. Nothing about the underlying error survives into one
// of these — the mapping is many-to-one by design, and the discarded detail is
// written to the log instead.
type apiError struct {
	status  int
	code    string
	message string
}

// The responses. Each is a package-level value rather than something built per
// call, because that is what makes indistinguishability structural instead of a
// property somebody has to keep re-establishing: every authentication failure
// returns the same unauthorized value, through the same writer, so there is no
// code path on which the eight could come out different.
var (
	// unauthorized is the only 401 in this package. Its message says nothing
	// about which check failed, or even that a check was reached.
	unauthorized = apiError{http.StatusUnauthorized, codeUnauthorized, "invalid credentials"}

	badRequestJSON = apiError{http.StatusBadRequest, codeBadRequest, "malformed JSON body"}
	badRequestName = apiError{http.StatusBadRequest, codeBadRequest, "descriptor name is required"}

	notFound         = apiError{http.StatusNotFound, codeNotFound, "resource not found"}
	methodNotAllowed = apiError{http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed"}
	payloadTooLarge  = apiError{http.StatusRequestEntityTooLarge, codePayloadTooLarge, "request body too large"}

	// internalFailure is deliberately uninformative. Service errors are wrapped
	// with SQL context on their way up and would otherwise hand a caller the
	// schema.
	internalFailure = apiError{http.StatusInternalServerError, codeInternal, "internal server error"}
)

// errUnauthenticated is what the request never got as far as a credential —
// no Authorization header, the wrong scheme, an empty token. It is an alias
// for auth.ErrNoBearerToken, not a second definition of the same idea: MCP
// tool calls hit the identical check through the identical sentinel.
var errUnauthenticated = auth.ErrNoBearerToken

// authFailures is every error that must be indistinguishable on the wire:
// errUnauthenticated (this layer's own "sent nothing" case) plus the closed
// set every service-layer credential check can return. Sourcing the second
// half from service.AuthSentinels rather than listing it here again is what
// keeps a future addition to that set — Task 2 of the MCP surface plan adds
// ErrAgentNotVisible — from silently not reaching this classifier.
//
// service.AuthSentinels carries all eight of the service's token and
// credential sentinels, including ErrInvalidEnvironment. That one is not an
// authentication failure at all — it is an operator mistyping an environment
// on Mint, on an admin surface this router does not yet expose — and
// classifying it here anyway is the fail-closed choice: an over-broad 401
// tells a caller nothing, whereas leaving a token sentinel out of this list
// is exactly the omission that reopens the oracle. When the admin API
// arrives, ErrInvalidEnvironment should be reclassified as a 400 there, with
// a test that keeps it out of any response an unauthenticated caller can
// reach.
var authFailures = append([]error{errUnauthenticated}, service.AuthSentinels...)

// classify turns any error into the response a caller is allowed to see.
//
// The default is 500 with a fixed message, so an error nobody thought to map
// degrades into telling the caller nothing rather than into telling them
// everything. Adding a case is how a new failure becomes visible; forgetting to
// is not how one leaks.
func classify(err error) apiError {
	for _, sentinel := range authFailures {
		if errors.Is(err, sentinel) {
			return unauthorized
		}
	}

	// Checked before the JSON cases: a body truncated at the cap fails to parse
	// as JSON too, and "too large" is the answer that tells the client what to
	// change.
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return payloadTooLarge
	}

	if errors.Is(err, domain.ErrNameRequired) {
		return badRequestName
	}

	var syntax *json.SyntaxError
	var unmarshalType *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax), errors.As(err, &unmarshalType),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return badRequestJSON
	}

	return internalFailure
}

// respondError maps err and sends it. It is the only way a service error
// reaches a client.
func respondError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	writeAPIError(w, r, logger, classify(err), err)
}

// writeAPIError sends apiErr and logs cause.
//
// The whole error goes to the log and none of it to the body. That split is the
// point of the package: the services distinguish revoked from expired from
// absent so an operator chasing a quiet fleet can tell them apart, and this is
// where that distinction stops being something a caller can observe.
//
// cause may be nil for a failure with nothing more to say than the response
// itself.
func writeAPIError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, apiErr apiError, cause error) {
	// Marshalling cannot fail: apiError holds two strings and the envelope has
	// no custom marshaller. The error is discarded rather than handled because
	// there is no second response to send.
	body, _ := json.Marshal(errorEnvelope{Error: errorDetail{Code: apiErr.code, Message: apiErr.message}})

	// Content-Length is set explicitly so the response is fully determined here
	// rather than partly by the server's buffering: two 401s must not differ in
	// their framing either.
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(apiErr.status)
	_, _ = w.Write(body)

	if logger == nil {
		return
	}
	attrs := []any{
		slog.Int("status", apiErr.status),
		slog.String("code", apiErr.code),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	}
	if cause != nil {
		// The service errors are the only thing here that could carry a secret,
		// and they do not: they wrap SQL context and sentinel text, never the
		// token, which the services hash before it reaches a query.
		attrs = append(attrs, slog.String("error", cause.Error()))
	}
	logger.Log(r.Context(), apiErr.logLevel(), "request failed", attrs...)
}

// logLevel sorts failures by who has to act on them. A 5xx is the server's
// fault and belongs in an alert; a 401 is worth a warning because a run of them
// is what an enumeration attempt looks like from the inside; the rest are
// ordinary client mistakes.
func (e apiError) logLevel() slog.Level {
	switch {
	case e.status >= http.StatusInternalServerError:
		return slog.LevelError
	case e.status == http.StatusUnauthorized:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
