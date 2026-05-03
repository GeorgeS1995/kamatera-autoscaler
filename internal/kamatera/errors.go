package kamatera

import (
	"errors"
	"fmt"
)

// APIError is returned when the API responded with a non-2xx HTTP status.
// Distinguishing "the API answered" from "the call never made it out" matters
// for retry decisions: 4xx responses are deterministic (retry won't change
// them), 5xx might recover, transport failures need a different code path
// (recovery-by-name for non-idempotent POSTs).
type APIError struct {
	Method string
	Path   string
	Code   int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kamatera: %s %s: HTTP %d: %s", e.Method, e.Path, e.Code, truncate(e.Body, 400))
}

// Client reports whether this is a 4xx (client-side / retry will not change the answer).
func (e *APIError) Client() bool { return e.Code >= 400 && e.Code < 500 }

// Server reports whether this is a 5xx (server-side / may recover on retry).
func (e *APIError) Server() bool { return e.Code >= 500 }

// IsClientError reports whether err wraps a 4xx APIError.
func IsClientError(err error) bool {
	var se *APIError
	return errors.As(err, &se) && se.Client()
}

// IsServerError reports whether err wraps a 5xx APIError.
func IsServerError(err error) bool {
	var se *APIError
	return errors.As(err, &se) && se.Server()
}

// IsTransportError reports whether err came from the HTTP transport layer
// (timeout, connection drop, DNS failure) rather than a structured HTTP
// response from the API.
//
// The nil guard is required for correctness, not convention: errors.As(nil, ...)
// returns false, so without the guard IsTransportError(nil) would incorrectly
// answer true. Callers in our code today always check `err != nil` first, but
// this is part of the package's public API and a defensive caller might pass
// nil unconditionally.
func IsTransportError(err error) bool {
	if err == nil {
		return false
	}
	var se *APIError
	return !errors.As(err, &se)
}

// ServerNotFoundError is returned by FindServerByName when no server matches
// the requested name. Stays compatible with the old sentinel comparison via
// the Is method, while carrying the requested name for richer logs.
type ServerNotFoundError struct {
	Name string
}

func (e *ServerNotFoundError) Error() string {
	return fmt.Sprintf("kamatera: server %q not found", e.Name)
}

// Is lets `errors.Is(err, ErrServerNotFound)` keep working for callers that
// don't care about the name.
func (e *ServerNotFoundError) Is(target error) bool {
	return target == ErrServerNotFound
}

// ErrServerNotFound is the sentinel for "server not found" — see
// ServerNotFoundError for the typed variant carrying the missing name.
var ErrServerNotFound = errors.New("kamatera: server not found")

// ErrCreateRecovered is returned by CreateServer when the initial POST failed
// at the transport layer (timeout, connection drop, TCP reset) but a follow-up
// name lookup found that Kamatera had nevertheless accepted the create command
// and the server now exists. The caller MUST NOT retry the POST — it would
// provision a duplicate (Kamatera renames colliding names with -1, -2 suffixes).
// Treat the embedded Server as the result and skip WaitCommand.
type ErrCreateRecovered struct {
	Server Server
}

func (e *ErrCreateRecovered) Error() string {
	return fmt.Sprintf("kamatera: create transport failed but server %q (id=%s) exists; treated as success",
		e.Server.Name, e.Server.ID)
}
