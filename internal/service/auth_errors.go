package service

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
