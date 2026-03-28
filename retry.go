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
)

// retryTransport wraps an http.RoundTripper with retry logic, exponential
// backoff with jitter, and Retry-After header support.
type retryTransport struct {
	next              http.RoundTripper
	maxRetries        int
	waitMin           time.Duration
	waitMax           time.Duration
	maxRetryBodyBytes int64
	retryableCodes    map[int]struct{}
	retryableMethods  map[string]struct{}
	checkRetry        func(req *http.Request, resp *http.Response, err error) bool
	retryObserver     func(attempt int, req *http.Request, resp *http.Response, err error)
	userAgent         string
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request to avoid mutating the caller's original.
	// RoundTripper contract: "RoundTrip should not modify the request,
	// except for consuming and closing the Body."
	req = req.Clone(req.Context())

	if t.userAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", t.userAgent)
	}

	var getBody func() (io.ReadCloser, error)
	if t.maxRetries > 0 {
		var err error
		getBody, err = prepareBody(req, t.maxRetryBodyBytes)
		if err != nil {
			return nil, err
		}
	}

	ctx := req.Context()
	var lastResp *http.Response

	for attempt := range t.maxRetries + 1 {
		if attempt > 0 {
			if err := t.waitForRetry(ctx, attempt-1, lastResp, getBody, req); err != nil {
				return lastResp, err
			}
		}

		resp, err := t.next.RoundTrip(req)

		if !t.shouldRetry(req, resp, err) {
			return resp, err
		}

		if t.retryObserver != nil {
			t.retryObserver(attempt, req, resp, err)
		}

		// On the final attempt, don't drain the response — return it as-is
		// so the caller can read the error response body.
		if attempt == t.maxRetries {
			return resp, err
		}

		// Best-effort drain to return the connection to the pool.
		drainAndClose(resp)

		lastResp = resp
	}

	// Unreachable: the loop always returns via shouldRetry or final-attempt check.
	return nil, nil
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
	// Respect Retry-After header if present.
	if resp != nil {
		if d, ok := t.parseRetryAfterHeader(resp); ok {
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

// parseRetryAfterHeader extracts a wait duration from the Retry-After header.
func (t *retryTransport) parseRetryAfterHeader(resp *http.Response) (time.Duration, bool) {
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 0, false
	}
	d := parseRetryAfter(ra)
	if d <= 0 {
		return 0, false
	}
	return min(d, t.waitMax), true
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

	// If Body implements io.ReadSeeker, wrap it.
	if seeker, ok := req.Body.(io.ReadSeeker); ok {
		getBody := func() (io.ReadCloser, error) {
			if _, err := seeker.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			return req.Body, nil
		}
		req.GetBody = getBody
		return getBody, nil
	}

	// Last resort: read the body into memory with an optional size limit.
	var reader io.Reader = req.Body
	if maxBytes > 0 {
		reader = io.LimitReader(reader, maxBytes+1)
	}

	body, readErr := io.ReadAll(reader)
	closeErr := req.Body.Close()

	if readErr != nil {
		return nil, fmt.Errorf("jttp: reading request body for retry: %w", readErr)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("jttp: closing request body: %w", closeErr)
	}

	if maxBytes > 0 && int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("jttp: request body exceeds retry buffer limit (%d bytes)", maxBytes)
	}

	getBody := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	req.GetBody = getBody
	var bodyErr error
	req.Body, bodyErr = getBody()
	if bodyErr != nil {
		return nil, fmt.Errorf("jttp: resetting request body: %w", bodyErr)
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
			return fmt.Errorf("jttp: rewinding request body: %w", err)
		}
		req.Body = body
	}
	return nil
}

// drainAndClose drains a small amount of the response body (to allow
// connection reuse) and closes it. Errors are intentionally ignored.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}

	//nolint // best-effort drain for connection reuse
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

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
