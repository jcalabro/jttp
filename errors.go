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

	// ErrBodyIdleTimeout is returned when a response body read or a request
	// body write stalls for longer than the configured idle timeout
	// (see WithIdleTimeout).
	ErrBodyIdleTimeout = errors.New("jttp: body idle timeout")

	// ErrBodyTransferTooSlow is returned when the rolling average transfer
	// rate of the response body falls below the configured floor
	// (see WithMinTransferRate).
	ErrBodyTransferTooSlow = errors.New("jttp: body transfer rate below minimum")

	// ErrResponseTooLarge is returned when the decompressed response body
	// exceeds the configured maximum size (see WithMaxResponseBodyBytes).
	ErrResponseTooLarge = errors.New("jttp: response body exceeds max size")

	// ErrDecompressionBomb is returned when the ratio of decompressed to
	// compressed bytes exceeds the configured maximum
	// (see WithMaxCompressionRatio).
	ErrDecompressionBomb = errors.New("jttp: decompression ratio exceeded")

	// ErrRedirectLoop is returned when a redirect would revisit a URL
	// already seen in the current chain.
	ErrRedirectLoop = errors.New("jttp: redirect loop detected")

	// ErrSchemeDowngrade is returned when a redirect would move from https
	// to http without WithAllowSchemeDowngrade.
	ErrSchemeDowngrade = errors.New("jttp: redirect downgrades scheme https to http")

	// ErrBlockedByIPPolicy is returned when a redirect target's resolved IP
	// falls within one of the default-blocked ranges (private, loopback,
	// link-local, multicast, unique-local v6, or IMDS addresses).
	ErrBlockedByIPPolicy = errors.New("jttp: target resolves to blocked IP range")
)
