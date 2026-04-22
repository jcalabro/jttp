package jttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A POST with a body larger than the retry buffer should not fail when POST
// isn't in the retryable method set — we should never have tried to buffer it.
func TestPOSTWithLargeBodyNotBufferedWhenNonRetryable(t *testing.T) {
	var got atomic.Value // string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.Store(string(body))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New(
		WithRetries(3),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
		WithMaxRetryBodyBytes(5), // tiny buffer
		// default retryable methods: GET, HEAD, OPTIONS — POST is NOT retryable
	)

	big := strings.Repeat("x", 1000)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, http.NoBody)
	// Use a body without GetBody to force the buffering code path if it runs.
	req.Body = io.NopCloser(strings.NewReader(big))
	req.GetBody = nil
	req.ContentLength = -1

	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	requireNoErr(t, err)
	requireEqual(t, resp.StatusCode, http.StatusOK)
	requireEqual(t, got.Load().(string), big)
}

// Verify the body is buffered when the method IS retryable and checkRetry is nil.
func TestPOSTBodyBufferedWhenRetryableMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := New(
		WithRetries(2),
		WithRetryWait(1*time.Millisecond, 5*time.Millisecond),
		WithRetryableMethods("POST"),
		WithMaxRetryBodyBytes(5),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, http.NoBody)
	req.Body = io.NopCloser(strings.NewReader("way too big"))
	req.GetBody = nil

	_, err := client.Do(req)
	requireIsErr(t, err)
	requireTrue(t, errors.Is(err, ErrBodyTooLarge))
}

// Even if the method isn't in the default retryable set, a custom checkRetry
// function could widen the set — but the method filter always runs first
// (see retry.go shouldRetry), so a non-retryable method never retries
// regardless of checkRetry. Therefore we still don't need to buffer.
func TestPOSTWithCheckRetryAndNonRetryableMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New(
		WithRetries(3),
		WithRetryWait(1*time.Millisecond, 5*time.Millisecond),
		WithMaxRetryBodyBytes(5),
		WithCheckRetry(func(_ *http.Request, _ *http.Response, _ error) bool { return true }),
		// retryableMethods still defaults to GET/HEAD/OPTIONS
	)

	big := strings.Repeat("y", 1000)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, http.NoBody)
	req.Body = io.NopCloser(strings.NewReader(big))
	req.GetBody = nil

	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	requireNoErr(t, err)
}
