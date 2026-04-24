package jttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestErrBodyTooLargeSentinel(t *testing.T) {
	r := &plainReader{data: []byte("this body is too large")}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", http.NoBody)
	req.Body = io.NopCloser(r)
	req.GetBody = nil

	_, err := prepareBody(req, 5)
	requireIsErr(t, err)
	requireTrue(t, errors.Is(err, ErrBodyTooLarge))
}

func TestErrBodyReadSentinel(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", http.NoBody)
	req.Body = io.NopCloser(&failReader{err: errors.New("read boom")})
	req.GetBody = nil

	_, err := prepareBody(req, 0)
	requireIsErr(t, err)
	requireTrue(t, errors.Is(err, ErrBodyRead))
}

func TestErrBodyCloseSentinel(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", http.NoBody)
	req.Body = &failCloser{Reader: strings.NewReader("data"), err: errors.New("close boom")}
	req.GetBody = nil

	_, err := prepareBody(req, 0)
	requireIsErr(t, err)
	requireTrue(t, errors.Is(err, ErrBodyClose))
}

func TestErrBodyRewindSentinel(t *testing.T) {
	// Force a retry where GetBody returns an error on rewind.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := New(
		WithRetries(2),
		WithRetryWait(1*time.Millisecond, 5*time.Millisecond),
		WithRetryableMethods("POST"),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("data"))
	// Replace GetBody with one that fails.
	req.GetBody = func() (io.ReadCloser, error) {
		return nil, errors.New("rewind fail")
	}

	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	requireIsErr(t, err)
	requireTrue(t, errors.Is(err, ErrBodyRewind))
}

func TestErrTooManyRedirectsSentinel(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		http.Redirect(w, r, fmt.Sprintf("/next-%d", n), http.StatusFound)
	}))
	defer srv.Close()

	client := New(WithRetries(0), WithRedirectPolicy(3), WithAllowPrivateRedirects())
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	requireIsErr(t, err)
	requireTrue(t, errors.Is(err, ErrTooManyRedirects))
}

func TestTier1SentinelsDefined(t *testing.T) {
	for _, err := range []error{
		ErrBodyIdleTimeout,
		ErrBodyTransferTooSlow,
		ErrResponseTooLarge,
		ErrDecompressionBomb,
		ErrRedirectLoop,
		ErrSchemeDowngrade,
		ErrBlockedByIPPolicy,
	} {
		if err == nil {
			t.Errorf("sentinel is nil")
		}
		if err.Error() == "" {
			t.Errorf("sentinel has empty message: %v", err)
		}
	}
}
