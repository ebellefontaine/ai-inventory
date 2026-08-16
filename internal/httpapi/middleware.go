package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// MaxBodyBytes caps a request body at 64 KiB.
//
// A descriptor is a name, a purpose, four small objects, and a handful of
// telemetry links: a realistic one is one to two kilobytes, and the largest
// plausible one — a long purpose and a few dozen links — is still well under
// ten. 64 KiB leaves that roughly an order of magnitude of headroom while
// keeping the worst case bounded: a thousand concurrent registrations can hold
// at most 64 MB of body between them, which a small server survives. A payload
// past this cap is not a big agent, it is a bug or an attempt to make the
// server buffer for someone.
const MaxBodyBytes = 64 << 10

// statusWriter remembers what was sent so the middleware around a handler can
// report it and so panic recovery can tell whether a response is already
// committed.
//
// It intentionally does not forward Flusher, Hijacker or Pusher. This API
// answers with small JSON documents and streams nothing; a handler that needs
// one of those would be doing something this package has not agreed to.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (sw *statusWriter) WriteHeader(status int) {
	if sw.status != 0 {
		return // the first status is the one that went out
	}
	sw.status = status
	sw.ResponseWriter.WriteHeader(status)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK // net/http's implicit 200 on first write
	}
	n, err := sw.ResponseWriter.Write(b)
	sw.bytes += n
	return n, err
}

func (sw *statusWriter) committed() bool { return sw.status != 0 }

// logRequests records one line per request: what was asked, what was answered,
// and how long it took.
//
// It logs the method and the path and nothing else from the request. The
// Authorization header is the specific omission: a token in a log file outlives
// every rotation of it, is copied wherever the logs are shipped, and is visible
// to everyone who can read them — which is a wider audience than the one that
// was ever meant to hold the credential.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}

		next.ServeHTTP(sw, r)

		status := sw.status
		if status == 0 {
			status = http.StatusOK // handler returned without writing anything
		}
		logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int("bytes", sw.bytes),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}

// recoverPanic turns a panicking handler into one failed request.
//
// A panic that reached the top of a goroutine would take the process with it,
// which would let one malformed payload stop the whole fleet from registering.
// The panic value and its stack go to the log, where they are a bug report; the
// caller gets the same opaque 500 as any other unrecognised failure.
func recoverPanic(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw, ok := w.(*statusWriter)
		if !ok {
			sw = &statusWriter{ResponseWriter: w}
			w = sw
		}

		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// http.ErrAbortHandler is the server's own way of dropping a
			// connection deliberately. Swallowing it would turn an intended
			// abort into a spurious 500.
			if v == http.ErrAbortHandler {
				panic(v)
			}

			logger.LogAttrs(r.Context(), slog.LevelError, "handler panicked",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())),
			)

			// Anything already written is on its way to the client and cannot
			// be taken back; writing a second header would only corrupt it.
			if !sw.committed() {
				writeAPIError(w, r, nil, internalFailure, nil)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// limitBody refuses a body larger than max.
//
// Two mechanisms, because there are two ways to be too large. A declared
// Content-Length over the cap is refused immediately, before a single byte is
// read. A body that declares nothing, or lies, is caught by MaxBytesReader as
// the handler reads it — which is what makes the cap a real bound on memory
// rather than a statement of trust in the client's own header.
func limitBody(logger *slog.Logger, max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > max {
			writeAPIError(w, r, logger, payloadTooLarge,
				fmt.Errorf("declared content length %d exceeds %d", r.ContentLength, max))
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken pulls the credential out of the Authorization header.
//
// Every failure returns the same errUnauthenticated, which classify collapses
// into the same 401 as a revoked credential. The scheme is matched without
// regard to case because RFC 9110 defines it that way, and because a client
// that sends "bearer" is a client with a working credential.
//
// The token is returned to be handed to a service, never inspected here: this
// layer does not know what a valid credential looks like, and the services
// authenticate inside their own transactions so that a refused call writes
// nothing. Checking here as well would mean two round trips and two places that
// could come to disagree about what "valid" means.
func bearerToken(r *http.Request) (string, error) {
	scheme, rest, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", errUnauthenticated
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", errUnauthenticated
	}
	return token, nil
}
