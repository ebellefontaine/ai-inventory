// Package httpapi is the HTTP surface of the inventory: the routing table, the
// single envelope every failure is reported in, and the middleware every
// request passes through.
//
// It authenticates nothing itself. The services authenticate inside the
// transaction that does the work, which is what makes a refused call write
// nothing at all; a middleware that checked the credential first would mean two
// round trips to the database and two definitions of a valid credential that
// could drift apart. This layer extracts the bearer token, hands it to a
// service, and maps whatever comes back.
//
// The mapping is the security-critical part. The services distinguish a revoked
// credential from an expired one from one that never existed, deliberately, so
// that an operator chasing a fleet that has gone quiet can tell them apart.
// This is the layer where that distinction dies: every authentication failure
// leaves here as the same bytes, and the detail goes to the log instead. A
// caller who could tell the failures apart could confirm which guessed
// credentials are real, which is exactly what a brute-force search needs.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// Server holds what the handlers need. Tasks 21-23 add the services to it; the
// logger is here from the start because the failures this package maps are only
// diagnosable if the real error is written down somewhere.
type Server struct {
	logger *slog.Logger
}

// NewServer returns a Server logging to logger, or to the default logger if
// that is nil — a server that silently discarded its diagnostics would be worse
// than a noisy one.
func NewServer(logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{logger: logger}
}

// Handler builds the routing table and wraps it in the middleware chain.
//
// Routing is net/http's ServeMux with method-and-wildcard patterns. A
// third-party router would buy pattern features this table does not use, at the
// cost of a dependency in a project whose whole pitch is one binary and one
// database.
//
// Each agent route is registered twice: once with its method, and once bare.
// The bare registration is strictly less specific, so it only wins when the
// path matched and the method did not — which is how this package answers a
// method mismatch in its own envelope instead of letting ServeMux emit its
// plain-text default. The trailing "/" pattern catches everything else as a
// 404, for the same reason.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("/healthz", s.methodNotAllowedHandler(http.MethodGet))

	// The colon in agents:register marks an action on the collection rather
	// than a subresource of it, so that "register" can never be read as an
	// agent id.
	mux.HandleFunc("POST /api/v1/agents:register", s.handleRegister)
	mux.Handle("/api/v1/agents:register", s.methodNotAllowedHandler(http.MethodPost))

	mux.HandleFunc("POST /api/v1/agents/{id}/heartbeat", s.handleHeartbeat)
	mux.Handle("/api/v1/agents/{id}/heartbeat", s.methodNotAllowedHandler(http.MethodPost))

	mux.HandleFunc("PATCH /api/v1/agents/{id}", s.handleUpdate)
	mux.Handle("/api/v1/agents/{id}", s.methodNotAllowedHandler(http.MethodPatch))

	mux.HandleFunc("/", s.handleNotFound)

	return s.wrap(mux)
}

// wrap is the chain every request passes through, outermost first.
//
// Logging is outermost so that it sees the status of whatever the layers below
// decided, including a recovered panic. Recovery is above the body limit so
// that a panic anywhere below it — in the limit, in routing, in a handler — is
// still one failed request rather than a dead process.
func (s *Server) wrap(h http.Handler) http.Handler {
	return logRequests(s.logger, recoverPanic(s.logger, limitBody(s.logger, MaxBodyBytes, h)))
}

// handleHealthz answers the liveness probe.
//
// It requires no credential, which is not an oversight: a probe that needs a
// token fails whenever the token does, and an orchestrator would then restart a
// perfectly healthy process because a credential expired. It reports that this
// process is serving and claims nothing about the database, so it cannot be
// read as a statement that registrations will succeed.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	body := []byte(`{"status":"ok"}`)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// errNotImplemented is what the three stubs below report until Tasks 21-23
// replace them.
var errNotImplemented = errors.New("endpoint not implemented")

// handleRegister will enroll an agent with a scoped enrollment token. Task 21.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, s.logger, notImplemented, errNotImplemented)
}

// handleHeartbeat will record liveness for one replica. Task 22. The agent id
// is r.PathValue("id").
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, s.logger, notImplemented, errNotImplemented)
}

// handleUpdate will apply a partial descriptor update. Task 23. The agent id is
// r.PathValue("id").
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, s.logger, notImplemented, errNotImplemented)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, s.logger, notFound, nil)
}

// methodNotAllowedHandler answers a path that exists with a method that does
// not. It names the methods that would have worked in Allow, which is what the
// status is for: the path is not a secret, and refusing to say costs a client a
// round of guessing without denying anyone anything.
func (s *Server) methodNotAllowedHandler(allowed ...string) http.Handler {
	allow := strings.Join(allowed, ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeAPIError(w, r, s.logger, methodNotAllowed, nil)
	})
}
