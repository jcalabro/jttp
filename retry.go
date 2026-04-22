package jttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/net/http2"
)

// retryTransport wraps an http.RoundTripper with retry logic, exponential
// backoff with jitter, and Retry-After header support.
type retryTransport struct {
	next              http.RoundTripper
	maxRetries        int
	waitMin           time.Duration
	waitMax           time.Duration
	maxRetryAfter     time.Duration
	maxRetryBodyBytes int64
	retryableCodes    map[int]struct{}
	retryableMethods  map[string]struct{}
	checkRetry        func(req *http.Request, resp *http.Response, err error) bool
	retryObserver     func(attempt int, req *http.Request, resp *http.Response, err error)
	userAgent         string
	// http2Transport is non-nil when we built the underlying *http.Transport
	// ourselves and successfully configured HTTP/2. Exposed for test
	// introspection and for callers who want to adjust H/2 settings at
	// runtime.
	http2Transport *http2.Transport
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request to avoid mutating the caller's original.
	// RoundTripper contract: "RoundTrip should not modify the request,
	// except for consuming and closing the Body."
	req = req.Clone(req.Context())

	if t.userAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", t.userAgent)
	}

	// Only buffer the body if this request could actually be retried.
	// The method filter in shouldRetry always applies (even with a custom
	// checkRetry), so a non-retryable method means no retries will ever
	// fire — buffering is wasted work and can spuriously reject large bodies.
	var getBody func() (io.ReadCloser, error)
	if t.maxRetries > 0 {
		if _, retryableMethod := t.retryableMethods[req.Method]; retryableMethod {
			var err error
			getBody, err = prepareBody(req, t.maxRetryBodyBytes)
			if err != nil {
				return nil, err
			}
		}
	}

	ctx := req.Context()
	var (
		resp *http.Response
		err  error
	)

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// Drain the previous response so the connection can return to
			// the pool while we back off. Retry-After (a header) is still
			// readable after draining the body.
			drainAndClose(resp)
			if werr := t.waitForRetry(ctx, attempt-1, resp, getBody, req); werr != nil {
				return resp, werr
			}
		}

		resp, err = t.next.RoundTrip(req)

		// Stop if we shouldn't retry, or if we've used our last attempt.
		// On the final attempt we return the response undrained so the
		// caller can read the error body.
		if !t.shouldRetry(req, resp, err) || attempt == t.maxRetries {
			return resp, err
		}

		if t.retryObserver != nil {
			t.retryObserver(attempt, req, resp, err)
		}
	}
}

// CloseIdleConnections propagates to the underlying transport so that
// client.CloseIdleConnections() works correctly.
func (t *retryTransport) CloseIdleConnections() {
	type closer interface {
		CloseIdleConnections()
	}
	if c, ok := t.next.(closer); ok {
		c.CloseIdleConnections()
	}
}

func (t *retryTransport) shouldRetry(req *http.Request, resp *http.Response, err error) bool {
	// Method check always applies, even with a custom checkRetry.
	if _, ok := t.retryableMethods[req.Method]; !ok {
		return false
	}

	if t.checkRetry != nil {
		return t.checkRetry(req, resp, err)
	}

	if err != nil {
		return isRetryableError(err)
	}

	if resp != nil {
		_, retryable := t.retryableCodes[resp.StatusCode]
		return retryable
	}

	return false
}

// maxBackoffShift caps the exponent to prevent integer overflow in backoff
// calculation. With waitMin of 1ns and shift of 62, the result fits in int64.
const maxBackoffShift = 62

func (t *retryTransport) backoff(attempt int, resp *http.Response) time.Duration {
	// Respect server-directed wait hints in priority order:
	// Retry-After (RFC 7231), RateLimit-Reset (RFC 9745 draft), then
	// X-RateLimit-Reset (widespread vendor convention, e.g. GitHub/Stripe).
	if resp != nil {
		if d, ok := t.parseServerWaitHint(resp); ok {
			return d
		}
	}

	// Exponential backoff: waitMin * 2^attempt, capped at waitMax.
	// Guard against overflow by capping the shift.
	shift := uint(min(attempt, maxBackoffShift))
	wait := t.waitMin * (1 << shift)
	if wait <= 0 || wait > t.waitMax {
		wait = t.waitMax
	}

	// Full jitter: uniform random in [waitMin, wait].
	if wait > t.waitMin {
		jitter := rand.Int64N(int64(wait-t.waitMin) + 1)
		return t.waitMin + time.Duration(jitter)
	}
	return wait
}

// parseServerWaitHint extracts a wait duration from response headers,
// trying Retry-After first, then RateLimit-Reset, then X-RateLimit-Reset.
// The returned duration is floored at waitMin and capped at maxRetryAfter.
func (t *retryTransport) parseServerWaitHint(resp *http.Response) (time.Duration, bool) {
	sources := []struct {
		name  string
		parse func(string) time.Duration
	}{
		{"Retry-After", parseRetryAfter},
		// RateLimit-Reset (RFC 9745 draft): always delta-seconds.
		{"RateLimit-Reset", parseDeltaSeconds},
		// X-RateLimit-Reset: usually delta-seconds, sometimes unix-epoch
		// (GitHub). Heuristic: large values are treated as epoch.
		{"X-RateLimit-Reset", parseDeltaSecondsOrEpoch},
	}
	for _, s := range sources {
		raw := resp.Header.Get(s.name)
		if raw == "" {
			continue
		}
		d := s.parse(raw)
		if d <= 0 {
			continue
		}
		return t.clampServerWait(d), true
	}
	return 0, false
}

// clampServerWait floors at waitMin and caps at maxRetryAfter.
func (t *retryTransport) clampServerWait(d time.Duration) time.Duration {
	if t.waitMin > 0 {
		d = max(d, t.waitMin)
	}
	if t.maxRetryAfter > 0 {
		d = min(d, t.maxRetryAfter)
	}
	return d
}

// epochHeuristicThreshold separates delta-seconds from unix-epoch for
// X-RateLimit-Reset. Values above this are assumed to be seconds since
// 1970, which places the crossover in September 2001 — any plausible
// future unix time is above it, and any plausible delta-seconds wait is
// below it.
const epochHeuristicThreshold = 1_000_000_000

// parseDeltaSeconds parses a positive integer count of seconds.
func parseDeltaSeconds(val string) time.Duration {
	if seconds, err := strconv.Atoi(val); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}

// parseDeltaSecondsOrEpoch parses either a delta-seconds count or a unix
// epoch timestamp (seconds since 1970). Values above
// epochHeuristicThreshold are treated as epoch.
func parseDeltaSecondsOrEpoch(val string) time.Duration {
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	if n >= epochHeuristicThreshold {
		// Epoch seconds.
		if d := time.Until(time.Unix(n, 0)); d > 0 {
			return d
		}
		return 0
	}
	return time.Duration(n) * time.Second
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// Never retry context cancellation or deadline exceeded.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Connection closed unexpectedly.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// Syscall-level connection errors.
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}

	// Net timeout errors (dial timeout, TLS timeout, etc.)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// DNS failures that aren't NXDOMAIN.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && !dnsErr.IsNotFound {
		return true
	}

	return false
}

// parseRetryAfter parses a Retry-After header value as either delta-seconds
// or an HTTP-date, returning the duration to wait.
func parseRetryAfter(val string) time.Duration {
	// Try as integer seconds first (most common).
	if seconds, err := strconv.Atoi(val); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}

	// Try as HTTP-date.
	if t, err := http.ParseTime(val); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}

	return 0
}

// prepareBody ensures the request body can be rewound for retries.
// maxBytes limits how much data will be buffered when the body must be read
// into memory (no GetBody, not seekable). Pass 0 for no limit.
func prepareBody(req *http.Request, maxBytes int64) (func() (io.ReadCloser, error), error) {
	if req.Body == nil || req.Body == http.NoBody {
		return func() (io.ReadCloser, error) { return http.NoBody, nil }, nil
	}

	// If GetBody is already set (e.g., by http.NewRequest for
	// strings.NewReader, bytes.NewReader), use it.
	if req.GetBody != nil {
		return req.GetBody, nil
	}

	// Last resort: read the body into memory with an optional size limit.
	// This handles io.ReadSeeker bodies safely — relying on Seek after the
	// transport closes the body (e.g., an *os.File) would fail on retry.
	var reader io.Reader = req.Body
	if maxBytes > 0 {
		reader = io.LimitReader(reader, maxBytes+1)
	}

	body, readErr := io.ReadAll(reader)
	closeErr := req.Body.Close()

	if readErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrBodyRead, readErr)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrBodyClose, closeErr)
	}

	if maxBytes > 0 && int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%w (%d bytes)", ErrBodyTooLarge, maxBytes)
	}

	getBody := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	req.ContentLength = int64(len(body))
	req.GetBody = getBody
	var bodyErr error
	req.Body, bodyErr = getBody()
	if bodyErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrBodyRewind, bodyErr)
	}

	return getBody, nil
}

// waitForRetry handles the backoff sleep and body rewind between retry attempts.
func (t *retryTransport) waitForRetry(
	ctx context.Context,
	prevAttempt int,
	lastResp *http.Response,
	getBody func() (io.ReadCloser, error),
	req *http.Request,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	wait := t.backoff(prevAttempt, lastResp)
	if err := sleep(ctx, wait); err != nil {
		return err
	}

	if getBody != nil {
		body, err := getBody()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrBodyRewind, err)
		}
		req.Body = body
	}
	return nil
}

// maxDrainBytes limits how many bytes are read from a response body when
// draining it for connection reuse during retries.
const maxDrainBytes = 1 << 16 // 64 KiB

// drainAndClose drains the response body up to maxDrainBytes (to allow
// connection reuse) and closes it. Errors are intentionally ignored.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}

	//nolint // best-effort drain for connection reuse
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))

	//nolint // best-effort close
	resp.Body.Close()
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
