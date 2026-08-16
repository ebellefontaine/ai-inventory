package service

import (
	"errors"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
)

// AuthSentinels is the closed set of errors that mean "this credential did
// not work," across every surface that authenticates one against this
// service layer.
//
// It exists so that HTTP and MCP cannot independently decide what counts as
// an authentication failure and drift apart. A caller who could tell one
// surface's rejection apart from the other's — or tell two of these apart
// from each other, on either surface — would learn something about a
// credential they were guessing at that this system exists to not reveal.
// Every surface's error classifier ranges over this slice rather than
// listing the sentinels itself.
var AuthSentinels = []error{
	ErrTokenNotFound,
	ErrTokenRevoked,
	ErrTokenExpired,
	ErrInvalidEnvironment,
	ErrCredentialNotFound,
	ErrCredentialRevoked,
	ErrCredentialExpired,
	ErrCredentialAgentMismatch,
}

// IsAuthFailure reports whether err is part of the closed set of credential
// failures every surface must answer identically: an enrollment token, an
// agent credential, or the complete absence of one. It exists so a new
// surface (MCP today; whatever comes after it) has one function to call
// rather than a second hand-copied sentinel list that can drift from this
// one -- which is exactly the drift internal/mcpapi shipped with until this
// fix, since it was built without ever referencing AuthSentinels at all.
func IsAuthFailure(err error) bool {
	if errors.Is(err, auth.ErrNoBearerToken) {
		return true
	}
	for _, sentinel := range AuthSentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}
