package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/ebellefontaine/ai-inventory/internal/service"
)

// The three response bodies. They are declared here rather than reused from the
// service results because these are the wire contract and those are not: a
// field added to a service result must be a deliberate decision to publish it,
// not something that appears in every client's payload because a struct grew.
//
// snake_case throughout, matching what internal/domain already puts on the wire.
type registerResponse struct {
	AgentID uuid.UUID `json:"agent_id"`
	// AgentToken is the plaintext credential, sent exactly once. Only its hash
	// is stored, so a client that loses it has to register again.
	AgentToken               string   `json:"agent_token"`
	HeartbeatIntervalSeconds int      `json:"heartbeat_interval_seconds"`
	Flags                    []string `json:"flags"`
	// Created distinguishes a new record from a re-registration, which is an
	// update. A client that cannot tell them apart cannot tell a restart from a
	// deployment it did not know about.
	Created bool `json:"created"`
}

type heartbeatResponse struct {
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	LifecycleState           string `json:"lifecycle_state"`
	ReplicaCount             int    `json:"replica_count"`
}

type updateResponse struct {
	Flags   []string `json:"flags"`
	Changed bool     `json:"changed"`
}

// handleRegister enrolls an agent with a scoped enrollment token.
//
// Nothing here reads a team or an environment from the request — not from the
// body, not from a query string, not from a header. The scope comes from the
// token inside RegistrationService, and the only way this layer could break
// that guarantee is by reintroducing one of those reads, so it does not have
// one to remove.
//
// The body is read before the token is checked against the database, which is
// unavoidable: the service needs the raw bytes, and it authenticates inside the
// transaction that writes. The read is bounded by MaxBodyBytes, so an
// unauthenticated caller can cost this process 64 KiB and no more.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	token, err := bearerToken(r)
	if err != nil {
		respondError(w, r, s.logger, err)
		return
	}

	// The descriptor is passed through as bytes. Decoding it here would throw
	// away the difference between a key the agent omitted and a key it sent that
	// this system refuses to honour, and that difference is the evidence the
	// service records.
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		respondError(w, r, s.logger, err)
		return
	}

	result, err := s.registration.Register(r.Context(), service.RegisterInput{
		EnrollmentToken: token,
		RawDescriptor:   raw,
	})
	if err != nil {
		respondError(w, r, s.logger, err)
		return
	}

	// 201 for a new record, 200 for a re-registration. The agent is told which
	// because the two mean different things to whatever is watching the fleet.
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}

	s.writeJSON(w, r, status, registerResponse{
		AgentID:                  result.AgentID,
		AgentToken:               result.AgentToken,
		HeartbeatIntervalSeconds: result.HeartbeatIntervalSeconds,
		Flags:                    jsonList(result.Flags),
		Created:                  result.Created,
	})
}

// handleHeartbeat records liveness for one replica.
//
// The request body is deliberately not read. A heartbeat carries no payload:
// everything it reports is the credential presenting itself, and everything it
// returns is the server's own policy or the server's own observation. Reading
// and parsing a body would invent a way for a beat to fail that has nothing to
// do with whether the agent is alive — which is the one thing this endpoint is
// for. An empty body therefore succeeds because there is nothing to be empty.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	token, err := bearerToken(r)
	if err != nil {
		respondError(w, r, s.logger, err)
		return
	}

	id, err := agentID(r)
	if err != nil {
		respondError(w, r, s.logger, err)
		return
	}

	result, err := s.heartbeat.Heartbeat(r.Context(), service.HeartbeatInput{
		AgentID:    id,
		AgentToken: token,
	})
	if err != nil {
		respondError(w, r, s.logger, err)
		return
	}

	s.writeJSON(w, r, http.StatusOK, heartbeatResponse{
		HeartbeatIntervalSeconds: result.HeartbeatIntervalSeconds,
		LifecycleState:           string(result.LifecycleState),
		ReplicaCount:             result.ReplicaCount,
	})
}

// handleUpdate applies a partial descriptor to a record that already exists.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	token, err := bearerToken(r)
	if err != nil {
		respondError(w, r, s.logger, err)
		return
	}

	id, err := agentID(r)
	if err != nil {
		respondError(w, r, s.logger, err)
		return
	}

	// Raw for the reason a PATCH exists at all: a decoded struct cannot tell a
	// field the agent omitted from one it set to empty, and only the first of
	// those must leave the stored value alone.
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		respondError(w, r, s.logger, err)
		return
	}

	result, err := s.update.Update(r.Context(), service.UpdateInput{
		AgentID:       id,
		AgentToken:    token,
		RawDescriptor: raw,
	})
	if err != nil {
		respondError(w, r, s.logger, err)
		return
	}

	s.writeJSON(w, r, http.StatusOK, updateResponse{
		Flags:   jsonList(result.Flags),
		Changed: result.Changed,
	})
}

// agentID reads the {id} wildcard, and refuses an unparseable one as an
// authentication failure rather than a bad request.
//
// That is the enumeration oracle again, by another route. A 400 for
// "not-a-uuid" and a 401 for a well-formed uuid would tell a caller that the
// shape of an id is checked before the credential is — from which it follows
// that any uuid answering 401 got as far as a credential lookup, and the
// difference between the two answers becomes a probe. Returning the same 401
// leaves nothing to compare: this endpoint answers "no" to everyone who cannot
// prove they own a specific record, and says nothing about why.
//
// The real reason is wrapped in, so the log tells an operator that a client is
// sending malformed ids while the wire still says only "invalid credentials".
func agentID(r *http.Request) (uuid.UUID, error) {
	raw := r.PathValue("id")
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: agent id is not a uuid: %v", errUnauthenticated, err)
	}
	return id, nil
}

// jsonList turns a nil slice into an empty one.
//
// json.Marshal renders a nil slice as null, and flags is a list a client will
// range over. A field that is sometimes [] and sometimes null is a field every
// client has to special-case, and the ones that forget crash on the empty case
// — which is the common case, since a well-formed descriptor raises no flags.
func jsonList(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// writeJSON sends a success payload.
//
// It sets the same headers writeAPIError does, deliberately: a client should not
// have to parse two different content types depending on whether the call
// worked, and Content-Length is set here for the same reason it is set there —
// the response is fully determined by this function rather than partly by the
// server's buffering.
//
// A marshalling failure is reported as an internal error. It cannot happen for
// the three types above, which hold only strings, ints, bools and a uuid, but
// the alternative to handling it is a half-written body with a 200 already on
// the wire.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		respondError(w, r, s.logger, fmt.Errorf("encode response: %w", err))
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
