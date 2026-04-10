package jttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"connection refused", syscall.ECONNREFUSED, true},
		{"connection reset", syscall.ECONNRESET, true},
		{"connection aborted", syscall.ECONNABORTED, true},
		{"broken pipe", syscall.EPIPE, true},
		{
			"wrapped connection refused",
			&net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}},
			true,
		},
		{
			"DNS not found",
			&net.DNSError{Err: "no such host", Name: "bad.invalid", IsNotFound: true},
			false,
		},
		{
			"DNS temporary failure",
			&net.DNSError{Err: "temporary failure", Name: "example.com", IsNotFound: false},
			true,
		},
		{
			"net timeout",
			&net.OpError{Op: "read", Err: timeoutError{}},
			true,
		},
		{
			"context canceled wrapped in url.Error style",
			wrapError(context.Canceled),
			false,
		},
		{"generic error", errors.New("something broke"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireEqual(t, isRetryableError(tt.err), tt.retryable)
		})
	}
}

// timeoutError implements net.Error with Timeout() == true.
type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// wrappedError wraps an error one level deep.
type wrappedError struct {
	inner error
}

func (e wrappedError) Error() string { return e.inner.Error() }
func (e wrappedError) Unwrap() error { return e.inner }

func wrapError(err error) error { return wrappedError{inner: err} }

func TestBackoff(t *testing.T) {
	rt := &retryTransport{
		waitMin: 100 * time.Millisecond,
		waitMax: 10 * time.Second,
	}

	for attempt := range 10 {
		d := rt.backoff(attempt, nil)
		requireTrue(t, d >= rt.waitMin)

		maxForAttempt := min(rt.waitMin*(1<<uint(attempt)), rt.waitMax)
		requireTrue(t, d <= maxForAttempt)
	}
}

func TestBackoffOverflowSafe(t *testing.T) {
	rt := &retryTransport{
		waitMin: 1 * time.Second,
		waitMax: 30 * time.Second,
	}

	for _, attempt := range []int{50, 63, 64, 100, 1000} {
		d := rt.backoff(attempt, nil)
		requireTrue(t, d >= 0)
		requireTrue(t, d <= rt.waitMax)
		requireTrue(t, d >= rt.waitMin)
	}
}

func TestBackoffRetryAfterSeconds(t *testing.T) {
	rt := &retryTransport{
		waitMin: 100 * time.Millisecond,
		waitMax: 30 * time.Second,
	}

	resp := &http.Response{
		Header: http.Header{"Retry-After": []string{"5"}},
	}

	requireEqual(t, rt.backoff(0, resp), 5*time.Second)
}

func TestBackoffRetryAfterCapped(t *testing.T) {
	rt := &retryTransport{
		waitMin:       100 * time.Millisecond,
		waitMax:       2 * time.Second,
		maxRetryAfter: 5 * time.Second,
	}

	// Server asks for 10 seconds — capped at maxRetryAfter (5s), not waitMax.
	resp := &http.Response{
		Header: http.Header{"Retry-After": []string{"10"}},
	}

	requireEqual(t, rt.backoff(0, resp), 5*time.Second)
}

func TestBackoffRetryAfterHTTPDate(t *testing.T) {
	rt := &retryTransport{
		waitMin: 100 * time.Millisecond,
		waitMax: 30 * time.Second,
	}

	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	resp := &http.Response{
		Header: http.Header{"Retry-After": []string{future}},
	}

	d := rt.backoff(0, resp)
	requireTrue(t, d >= 2*time.Second && d <= 4*time.Second)
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		val  string
		want time.Duration
	}{
		{"integer seconds", "5", 5 * time.Second},
		{"zero", "0", 0},
		{"negative", "-1", 0},
		{"invalid", "abc", 0},
		{"empty", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireEqual(t, parseRetryAfter(tt.val), tt.want)
		})
	}
}

func TestPrepareBodyNil(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", http.NoBody)
	getBody, err := prepareBody(req, 0)
	requireNoErr(t, err)
	body, err := getBody()
	requireNoErr(t, err)
	requireTrue(t, body == http.NoBody)
}

func TestPrepareBodyGetBody(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", strings.NewReader("hello"))
	getBody, err := prepareBody(req, 0)
	requireNoErr(t, err)

	data, _ := io.ReadAll(req.Body)
	requireEqual(t, string(data), "hello")

	body, err := getBody()
	requireNoErr(t, err)
	data, _ = io.ReadAll(body)
	requireEqual(t, string(data), "hello")
}

func TestPrepareBodyReadSeeker(t *testing.T) {
	// ReadSeeker bodies without GetBody are buffered into memory (not seeked),
	// because the transport closes the body after each attempt, which would
	// break seek-based rewinding for types like *os.File.
	body := bytes.NewReader([]byte("seekable"))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", http.NoBody)
	req.Body = readSeekerCloser{body}
	req.GetBody = nil

	getBody, err := prepareBody(req, 0)
	requireNoErr(t, err)

	data, _ := io.ReadAll(req.Body)
	requireEqual(t, string(data), "seekable")

	b, err := getBody()
	requireNoErr(t, err)
	data, _ = io.ReadAll(b)
	requireEqual(t, string(data), "seekable")
}

type readSeekerCloser struct {
	io.ReadSeeker
}

func (readSeekerCloser) Close() error { return nil }

func TestPrepareBodyFallback(t *testing.T) {
	r := &plainReader{data: []byte("buffered")}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", http.NoBody)
	req.Body = io.NopCloser(r)
	req.GetBody = nil

	getBody, err := prepareBody(req, 0)
	requireNoErr(t, err)

	data, _ := io.ReadAll(req.Body)
	requireEqual(t, string(data), "buffered")

	b, err := getBody()
	requireNoErr(t, err)
	data, _ = io.ReadAll(b)
	requireEqual(t, string(data), "buffered")
}

type plainReader struct {
	data []byte
	pos  int
}

func (r *plainReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func TestSleepWithContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := sleep(ctx, 10*time.Second)
	elapsed := time.Since(start)

	requireTrue(t, errors.Is(err, context.Canceled))
	requireTrue(t, elapsed < 100*time.Millisecond)
}

func TestSleepWithContextCompletes(t *testing.T) {
	start := time.Now()
	err := sleep(context.Background(), 5*time.Millisecond)
	elapsed := time.Since(start)

	requireNoErr(t, err)
	requireTrue(t, elapsed >= 4*time.Millisecond)
}

func TestPrepareBodyExceedsLimit(t *testing.T) {
	r := &plainReader{data: []byte("this body is too large")}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", http.NoBody)
	req.Body = io.NopCloser(r)
	req.GetBody = nil

	_, err := prepareBody(req, 5)
	requireErrContains(t, err, "exceeds retry buffer limit")
}

func TestPrepareBodyWithinLimit(t *testing.T) {
	r := &plainReader{data: []byte("ok")}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", http.NoBody)
	req.Body = io.NopCloser(r)
	req.GetBody = nil

	getBody, err := prepareBody(req, 100)
	requireNoErr(t, err)
	body, err := getBody()
	requireNoErr(t, err)
	data, _ := io.ReadAll(body)
	requireEqual(t, string(data), "ok")
}

func TestPrepareBodyReadError(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", http.NoBody)
	req.Body = io.NopCloser(&failReader{err: errors.New("read boom")})
	req.GetBody = nil

	_, err := prepareBody(req, 0)
	requireErrContains(t, err, "reading request body")
}

type failReader struct {
	err error
}

func (r *failReader) Read(_ []byte) (int, error) { return 0, r.err }

func TestPrepareBodyCloseError(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", http.NoBody)
	req.Body = &failCloser{Reader: strings.NewReader("data"), err: errors.New("close boom")}
	req.GetBody = nil

	_, err := prepareBody(req, 0)
	requireErrContains(t, err, "closing request body")
}

type failCloser struct {
	io.Reader
	err error
}

func (c *failCloser) Close() error { return c.err }

func TestBackoffRetryAfterPastDate(t *testing.T) {
	rt := &retryTransport{
		waitMin: 100 * time.Millisecond,
		waitMax: 30 * time.Second,
	}

	past := time.Now().Add(-1 * time.Hour).UTC().Format(http.TimeFormat)
	resp := &http.Response{
		Header: http.Header{"Retry-After": []string{past}},
	}

	d := rt.backoff(0, resp)
	requireTrue(t, d >= rt.waitMin)
}

func TestBackoffMinGreaterThanMax(t *testing.T) {
	client := New(
		WithRetryWait(10*time.Second, 1*time.Second),
		WithRetries(3),
	)
	rt := client.Transport.(*retryTransport)

	requireEqual(t, rt.waitMin, 1*time.Second)
	requireEqual(t, rt.waitMax, 10*time.Second)

	d := rt.backoff(0, nil)
	requireTrue(t, d >= rt.waitMin && d <= rt.waitMax)
}

func TestRetryAfterRespected(t *testing.T) {
	rt := &retryTransport{
		waitMin:       100 * time.Millisecond,
		waitMax:       2 * time.Second,
		maxRetryAfter: 30 * time.Second,
	}

	// Server asks for 10 seconds — respected because it's within maxRetryAfter.
	// Note: this exceeds waitMax (2s), which is correct. Retry-After is a
	// server-directed delay and is capped by maxRetryAfter, not waitMax.
	resp := &http.Response{
		Header: http.Header{"Retry-After": []string{"10"}},
	}
	requireEqual(t, rt.backoff(0, resp), 10*time.Second)
}

func TestRetryAfterFlooredAtWaitMin(t *testing.T) {
	rt := &retryTransport{
		waitMin:       5 * time.Second,
		waitMax:       30 * time.Second,
		maxRetryAfter: 1 * time.Minute,
	}

	// Server asks for 1 second, but waitMin is 5 seconds.
	resp := &http.Response{
		Header: http.Header{"Retry-After": []string{"1"}},
	}
	requireEqual(t, rt.backoff(0, resp), 5*time.Second)
}

func TestContentLengthAfterBodyBuffering(t *testing.T) {
	r := &plainReader{data: []byte("hello world")}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", http.NoBody)
	req.Body = io.NopCloser(r)
	req.GetBody = nil
	req.ContentLength = -1 // unknown

	_, err := prepareBody(req, 0)
	requireNoErr(t, err)

	requireEqual(t, req.ContentLength, int64(11)) // "hello world" = 11 bytes
}

func TestMaxRetryAfterNegativeClamped(t *testing.T) {
	client := New(WithMaxRetryAfter(-1 * time.Second))
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.maxRetryAfter, DefaultMaxRetryAfter)
}
