package jttp

import "errors"

// Sentinel errors returned by jttp. Callers can use errors.Is to distinguish
// them. All are wrapped with %w when returned, so the underlying cause (if
// any) remains reachable via errors.Unwrap / errors.As / errors.AsType.
var (
	// ErrBodyTooLarge is returned when a request body exceeds the retry
	// buffer limit (see WithMaxRetryBodyBytes) and the body does not
	// already provide a GetBody function for rewinding.
	ErrBodyTooLarge = errors.New("jttp: request body exceeds retry buffer limit")

	// ErrBodyRead is returned when reading the request body into the retry
	// buffer fails.
	ErrBodyRead = errors.New("jttp: reading request body for retry")

	// ErrBodyClose is returned when closing the original request body (after
	// buffering it for retry) fails.
	ErrBodyClose = errors.New("jttp: closing request body")

	// ErrBodyRewind is returned when rewinding the request body between
	// retry attempts fails (req.GetBody returned an error).
	ErrBodyRewind = errors.New("jttp: rewinding request body")

	// ErrTooManyRedirects is returned when the redirect chain exceeds the
	// configured maximum (see WithRedirectPolicy).
	ErrTooManyRedirects = errors.New("jttp: too many redirects")
)
