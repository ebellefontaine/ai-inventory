package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// ErrNoBearerToken means the request carried no usable bearer credential: no
// Authorization header, the wrong scheme, or an empty token. It is exported
// so every surface that accepts a bearer credential — HTTP request headers
// today, MCP tool calls tomorrow, both carried as the same Authorization
// header — classifies the same failure the same way, rather than each
// surface inventing its own "you sent nothing" sentinel that the other does
// not recognise.
var ErrNoBearerToken = errors.New("no usable bearer credential in request")

// BearerToken extracts the credential from an Authorization header of the
// form "Bearer <token>". The scheme is matched without regard to case, per
// RFC 9110, and because a client that sends "bearer" is a client with a
// working credential.
//
// The token is returned to be handed to a service, never inspected here:
// this function does not know what a valid credential looks like, and the
// services authenticate inside their own transactions so that a refused call
// writes nothing.
func BearerToken(header http.Header) (string, error) {
	scheme, rest, found := strings.Cut(header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", ErrNoBearerToken
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", ErrNoBearerToken
	}
	return token, nil
}

// AgentID parses raw as a UUID, refusing an unparseable one as
// ErrNoBearerToken rather than a distinct error. This is the enumeration
// oracle by another route: a caller who could tell "malformed id" apart from
// "well-formed id, wrong credential" would learn that the id's shape is
// checked before the credential is, and the difference becomes a probe. Both
// internal/httpapi and internal/mcpapi route a malformed agent id through
// this so their two 401s cannot diverge either.
func AgentID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: agent id is not a uuid: %v", ErrNoBearerToken, err)
	}
	return id, nil
}
