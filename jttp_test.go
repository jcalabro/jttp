package jttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	client := New()
	requireEqual(t, client.Timeout, DefaultTimeout)

	rt, ok := client.Transport.(*retryTransport)
	requireTrue(t, ok)
	requireEqual(t, rt.maxRetries, DefaultMaxRetries)
	requireEqual(t, rt.waitMin, DefaultRetryWaitMin)
	requireEqual(t, rt.waitMax, DefaultRetryWaitMax)
	requireEqual(t, rt.maxRetryAfter, DefaultMaxRetryAfter)
	requireEqual(t, rt.userAgent, "")
}

func TestWithTimeout(t *testing.T) {
	client := New(WithTimeout(5 * time.Second))
	requireEqual(t, client.Timeout, 5*time.Second)
}

func TestWithRetries(t *testing.T) {
	client := New(WithRetries(0))
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.maxRetries, 0)
}

func TestWithRetriesNegative(t *testing.T) {
	client := New(WithRetries(-1))
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.maxRetries, 0)
}

func TestWithRetryWait(t *testing.T) {
	client := New(WithRetryWait(500*time.Millisecond, 5*time.Second))
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.waitMin, 500*time.Millisecond)
	requireEqual(t, rt.waitMax, 5*time.Second)
}

func TestWithUserAgent(t *testing.T) {
	client := New(WithUserAgent("myapp/1.0"))
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.userAgent, "myapp/1.0")
}

func TestWithRetryableStatusCodes(t *testing.T) {
	client := New(WithRetryableStatusCodes(500, 429))
	rt := client.Transport.(*retryTransport)
	_, has500 := rt.retryableCodes[500]
	requireTrue(t, has500)
	_, has429 := rt.retryableCodes[429]
	requireTrue(t, has429)
	_, has502 := rt.retryableCodes[502]
	requireFalse(t, has502)
}

func TestWithRetryableMethods(t *testing.T) {
	client := New(WithRetryableMethods("POST", "PUT"))
	rt := client.Transport.(*retryTransport)
	_, hasPost := rt.retryableMethods["POST"]
	requireTrue(t, hasPost)
	_, hasGet := rt.retryableMethods["GET"]
	requireFalse(t, hasGet)
}

func TestWithTransport(t *testing.T) {
	custom := http.DefaultTransport
	client := New(WithTransport(custom))
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.next, custom)
}

func TestRetryOn503(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "unavailable")
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := New(
		WithRetries(3),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, resp.StatusCode, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	requireEqual(t, string(body), "ok")
	requireEqual(t, count.Load(), int32(3))
}

func TestRetryOn429WithRetryAfter(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n <= 1 {
			// Server asks for 10s, but waitMax will cap it.
			w.Header().Set("Retry-After", "10")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "rate limited")
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := New(
		WithRetries(2),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
		// MaxRetryAfter caps the server's Retry-After (10s) to 50ms.
		WithMaxRetryAfter(50*time.Millisecond),
	)

	start := time.Now()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, resp.StatusCode, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	requireEqual(t, string(body), "ok")
	requireTrue(t, elapsed < 500*time.Millisecond)
	requireEqual(t, count.Load(), int32(2))
}

func TestNoRetryOnPost(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := New(
		WithRetries(3),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("data"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, resp.StatusCode, http.StatusServiceUnavailable)
	requireEqual(t, count.Load(), int32(1))
}

func TestExhaustedRetries(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "bad gateway")
	}))
	defer srv.Close()

	client := New(
		WithRetries(2),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, resp.StatusCode, http.StatusBadGateway)
	requireEqual(t, count.Load(), int32(3)) // 1 initial + 2 retries
}

func TestExhaustedRetriesBodyReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "error details")
	}))
	defer srv.Close()

	client := New(
		WithRetries(1),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	requireNoErr(t, err)
	requireEqual(t, string(body), "error details")
}

func TestNoRetryOnSuccess(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := New(
		WithRetries(3),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, resp.StatusCode, http.StatusOK)
	requireEqual(t, count.Load(), int32(1))
}

func TestZeroRetriesMakesOneRequest(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := New(WithRetries(0))

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, resp.StatusCode, http.StatusOK)
	requireEqual(t, count.Load(), int32(1))
}

func TestRedirectPolicy(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n <= 10 {
			http.Redirect(w, r, fmt.Sprintf("/%d", n), http.StatusFound)
			return
		}
		fmt.Fprint(w, "final")
	}))
	defer srv.Close()

	client := New(WithRetries(0))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	requireErrContains(t, err, "stopped after 5 redirects")
}

func TestRedirectPolicyNegativeClamped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer srv.Close()

	client := New(WithRedirectPolicy(-5), WithRetries(0))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, resp.StatusCode, http.StatusFound)
}

func TestNoRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer srv.Close()

	client := New(WithNoRedirects(), WithRetries(0))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, resp.StatusCode, http.StatusFound)
}

func TestUserAgentSet(t *testing.T) {
	var mu sync.Mutex
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotUA = r.Header.Get("User-Agent")
		mu.Unlock()
	}))
	defer srv.Close()

	client := New(WithUserAgent("myapp/2.0"), WithRetries(0))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	resp.Body.Close()

	mu.Lock()
	ua := gotUA
	mu.Unlock()
	requireEqual(t, ua, "myapp/2.0")
}

func TestUserAgentNotOverridden(t *testing.T) {
	var mu sync.Mutex
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotUA = r.Header.Get("User-Agent")
		mu.Unlock()
	}))
	defer srv.Close()

	client := New(WithUserAgent("myapp/2.0"), WithRetries(0))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	req.Header.Set("User-Agent", "custom/1.0")
	resp, err := client.Do(req)
	requireNoErr(t, err)
	resp.Body.Close()

	mu.Lock()
	ua := gotUA
	mu.Unlock()
	requireEqual(t, ua, "custom/1.0")
}

func TestRetryWithBody(t *testing.T) {
	var count atomic.Int32
	var mu sync.Mutex
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = string(body)
		mu.Unlock()
		n := count.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := New(
		WithRetries(3),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
		WithRetryableMethods("GET", "POST"),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("hello"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, resp.StatusCode, http.StatusOK)
	mu.Lock()
	got := lastBody
	mu.Unlock()
	requireEqual(t, got, "hello")
}

func TestWithCheckRetry(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	client := New(
		WithRetries(2),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
		WithCheckRetry(func(_ *http.Request, resp *http.Response, _ error) bool {
			return resp != nil && resp.StatusCode == http.StatusTeapot
		}),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, count.Load(), int32(3)) // 1 initial + 2 retries
}

func TestCheckRetryRespectsMethodFilter(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	client := New(
		WithRetries(2),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
		WithCheckRetry(func(_ *http.Request, resp *http.Response, _ error) bool {
			return resp != nil && resp.StatusCode == http.StatusTeapot
		}),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("data"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, count.Load(), int32(1)) // POST not retryable
}

func TestContextCanceledDuringRetry(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	client := New(
		WithRetries(10),
		WithRetryWait(5*time.Millisecond, 10*time.Millisecond),
	)

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	requireErrContains(t, err, "context canceled")

	n := count.Load()
	requireTrue(t, n >= 1 && n <= 10)
}

func TestConcurrentUsage(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := New(WithRetries(0))

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
			resp, err := client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		requireNoErr(t, err)
	}
	requireEqual(t, count.Load(), int32(20))
}

func TestCloseIdleConnections(t *testing.T) {
	client := New(WithRetries(0))
	// Should not panic — verifies CloseIdleConnections propagates.
	client.CloseIdleConnections()
}

func TestRetryOnConnectionError(t *testing.T) {
	var attempts atomic.Int32
	failTransport := &fakeRoundTripper{
		fn: func(_ *http.Request) (*http.Response, error) {
			n := attempts.Add(1)
			if n <= 2 {
				return nil, &net.OpError{
					Op:  "dial",
					Net: "tcp",
					Addr: &net.TCPAddr{
						IP:   net.IPv4(127, 0, 0, 1),
						Port: 9999,
					},
					Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
				}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("recovered")),
				Header:     make(http.Header),
			}, nil
		},
	}

	client := New(
		WithTransport(failTransport),
		WithRetries(3),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:9999", http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	requireEqual(t, string(body), "recovered")
	requireEqual(t, attempts.Load(), int32(3))
}

type fakeRoundTripper struct {
	fn func(req *http.Request) (*http.Response, error)
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f.fn(req)
}

func TestTransportConfigPropagation(t *testing.T) {
	client := New(
		WithMaxIdleConns(50),
		WithMaxIdleConnsPerHost(25),
		WithMaxConnsPerHost(100),
		WithIdleConnTimeout(120*time.Second),
		WithTLSHandshakeTimeout(10*time.Second),
		WithResponseHeaderTimeout(15*time.Second),
		WithDialTimeout(3*time.Second),
		WithDialKeepAlive(60*time.Second),
		WithExpectContinueTimeout(5*time.Second),
		WithDisableKeepAlives(),
		WithDisableCompression(),
		WithForceHTTP2(false),
		WithRetries(0),
	)

	rt := client.Transport.(*retryTransport)
	tr, ok := rt.next.(*http.Transport)
	requireTrue(t, ok)

	requireEqual(t, tr.MaxIdleConns, 50)
	requireEqual(t, tr.MaxIdleConnsPerHost, 25)
	requireEqual(t, tr.MaxConnsPerHost, 100)
	requireEqual(t, tr.IdleConnTimeout, 120*time.Second)
	requireEqual(t, tr.TLSHandshakeTimeout, 10*time.Second)
	requireEqual(t, tr.ResponseHeaderTimeout, 15*time.Second)
	requireEqual(t, tr.ExpectContinueTimeout, 5*time.Second)
	requireTrue(t, tr.DisableKeepAlives)
	requireTrue(t, tr.DisableCompression)
	requireFalse(t, tr.ForceAttemptHTTP2)
	requireTrue(t, tr.TLSClientConfig != nil && tr.TLSClientConfig.MinVersion >= tls.VersionTLS12)
}

func TestWithTLSConfig(t *testing.T) {
	custom := &tls.Config{
		ServerName: "custom.example.com",
	}
	client := New(WithTLSConfig(custom), WithRetries(0))
	rt := client.Transport.(*retryTransport)
	tr := rt.next.(*http.Transport)

	requireEqual(t, tr.TLSClientConfig.ServerName, "custom.example.com")
	requireTrue(t, tr.TLSClientConfig.MinVersion >= tls.VersionTLS12)
}

func TestWithTLSConfigEnforcesMinVersion(t *testing.T) {
	custom := &tls.Config{
		MinVersion: tls.VersionTLS10,
	}
	client := New(WithTLSConfig(custom), WithRetries(0))
	rt := client.Transport.(*retryTransport)
	tr := rt.next.(*http.Transport)

	requireEqual(t, tr.TLSClientConfig.MinVersion, uint16(tls.VersionTLS12))
}

func TestWithAdditionalRetryableStatusCodes(t *testing.T) {
	client := New(WithAdditionalRetryableStatusCodes(500))
	rt := client.Transport.(*retryTransport)

	for _, code := range []int{429, 502, 503, 504, 500} {
		_, ok := rt.retryableCodes[code]
		requireTrue(t, ok)
	}
}

func TestDefaultRetryableStatusCodesInclude408And425(t *testing.T) {
	client := New()
	rt := client.Transport.(*retryTransport)

	for _, code := range []int{408, 425, 429, 502, 503, 504} {
		_, ok := rt.retryableCodes[code]
		if !ok {
			t.Fatalf("expected %d to be in default retryable codes, was not", code)
		}
	}
}

func TestRetryOn408RequestTimeout(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := New(
		WithRetries(2),
		WithRetryWait(1*time.Millisecond, 5*time.Millisecond),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()
	requireEqual(t, resp.StatusCode, http.StatusOK)
	requireEqual(t, count.Load(), int32(2))
}

func TestWithAdditionalRetryableMethods(t *testing.T) {
	client := New(WithAdditionalRetryableMethods("POST", "PUT"))
	rt := client.Transport.(*retryTransport)

	for _, method := range []string{"GET", "HEAD", "OPTIONS", "POST", "PUT"} {
		_, ok := rt.retryableMethods[method]
		requireTrue(t, ok)
	}
}

func TestWithRetryObserver(t *testing.T) {
	var observed []int
	var mu sync.Mutex
	var count atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := New(
		WithRetries(3),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
		WithRetryObserver(func(attempt int, _ *http.Request, _ *http.Response, _ error) {
			mu.Lock()
			observed = append(observed, attempt)
			mu.Unlock()
		}),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	requireEqual(t, len(observed), 2)
	for i, attempt := range observed {
		requireEqual(t, attempt, i)
	}
}

func TestWithMaxRetryBodyBytes(t *testing.T) {
	client := New(WithMaxRetryBodyBytes(10))
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.maxRetryBodyBytes, int64(10))
}

func TestRetryWaitZeroDurationsClamped(t *testing.T) {
	client := New(WithRetryWait(0, 0))
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.waitMin, DefaultRetryWaitMin)
	requireEqual(t, rt.waitMax, DefaultRetryWaitMax)
}

func TestRetryWaitNegativeDurationsClamped(t *testing.T) {
	client := New(WithRetryWait(-1*time.Second, -2*time.Second))
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.waitMin, DefaultRetryWaitMin)
	requireEqual(t, rt.waitMax, DefaultRetryWaitMax)
}

func TestRetryBodyTooLargeForRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := New(
		WithRetries(2),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
		WithRetryableMethods("POST"),
		WithMaxRetryBodyBytes(5),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, http.NoBody)
	req.Body = io.NopCloser(strings.NewReader("this is way too large"))
	req.GetBody = nil

	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	requireErrContains(t, err, "exceeds retry buffer limit")
}

func TestTLSConfigNotMutated(t *testing.T) {
	custom := &tls.Config{
		MinVersion: tls.VersionTLS10,
		ServerName: "example.com",
	}
	New(WithTLSConfig(custom), WithRetries(0))

	// The caller's original config must not be mutated.
	requireEqual(t, custom.MinVersion, uint16(tls.VersionTLS10))
	requireEqual(t, custom.ServerName, "example.com")
}

func TestDefaultResponseHeaderTimeout(t *testing.T) {
	client := New(WithRetries(0))
	rt := client.Transport.(*retryTransport)
	tr := rt.next.(*http.Transport)
	requireEqual(t, tr.ResponseHeaderTimeout, DefaultResponseHeaderTimeout)
	requireTrue(t, tr.ResponseHeaderTimeout > 0)
}

func TestDefaultMaxConnsPerHost(t *testing.T) {
	client := New(WithRetries(0))
	rt := client.Transport.(*retryTransport)
	tr := rt.next.(*http.Transport)
	requireEqual(t, tr.MaxConnsPerHost, DefaultMaxConnsPerHost)
	requireTrue(t, tr.MaxConnsPerHost > 0)
}

func TestWithNoRetries(t *testing.T) {
	client := New(WithNoRetries())
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.maxRetries, 0)
}

func TestWithMaxRetryAfter(t *testing.T) {
	client := New(WithMaxRetryAfter(30 * time.Second))
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.maxRetryAfter, 30*time.Second)
}

func TestNegativeDurationsClamped(t *testing.T) {
	client := New(
		WithTimeout(-1*time.Second),
		WithDialTimeout(-1*time.Second),
		WithDialKeepAlive(-1*time.Second),
		WithIdleConnTimeout(-1*time.Second),
		WithTLSHandshakeTimeout(-1*time.Second),
		WithResponseHeaderTimeout(-1*time.Second),
		WithExpectContinueTimeout(-1*time.Second),
		WithRetries(0),
	)

	requireEqual(t, client.Timeout, time.Duration(0))

	rt := client.Transport.(*retryTransport)
	tr := rt.next.(*http.Transport)
	requireEqual(t, tr.TLSHandshakeTimeout, time.Duration(0))
	requireEqual(t, tr.ResponseHeaderTimeout, time.Duration(0))
	requireEqual(t, tr.ExpectContinueTimeout, time.Duration(0))
	requireEqual(t, tr.IdleConnTimeout, time.Duration(0))
}

func TestRetryableStatusCodesEmpty(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// Empty status codes disables status-code-based retries.
	client := New(
		WithRetryableStatusCodes(),
		WithRetries(3),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	resp.Body.Close()

	requireEqual(t, resp.StatusCode, http.StatusServiceUnavailable)
	requireEqual(t, count.Load(), int32(1)) // No retries
}

func TestCloseIdleConnectionsNonCloser(t *testing.T) {
	custom := &fakeRoundTripper{
		fn: func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
			}, nil
		},
	}
	client := New(WithTransport(custom), WithRetries(0))
	// Should not panic even though fakeRoundTripper doesn't implement CloseIdleConnections.
	client.CloseIdleConnections()
}

func TestRetryObserverNotCalledOnFinalExhaustedAttempt(t *testing.T) {
	var observed []int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := New(
		WithRetries(2),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
		WithRetryObserver(func(attempt int, _ *http.Request, _ *http.Response, _ error) {
			mu.Lock()
			observed = append(observed, attempt)
			mu.Unlock()
		}),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	// Observer should be called for attempts 0 and 1 only (which will be retried).
	// The final exhausted attempt (2) should NOT trigger the observer.
	requireEqual(t, len(observed), 2)
	requireEqual(t, observed[0], 0)
	requireEqual(t, observed[1], 1)
}

func TestRetryWithReadSeekerBody(t *testing.T) {
	var count atomic.Int32
	var mu sync.Mutex
	var lastBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = string(body)
		mu.Unlock()
		n := count.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := New(
		WithRetries(3),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
		WithRetryableMethods("GET", "POST"),
	)

	// Use a ReadSeeker body with no GetBody set — exercises the buffer fallback
	// path that replaced the old (broken) ReadSeeker-seek path.
	body := bytes.NewReader([]byte("seekable-data"))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, http.NoBody)
	req.Body = readSeekerCloser{body}
	req.GetBody = nil

	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	requireEqual(t, resp.StatusCode, http.StatusOK)

	mu.Lock()
	got := lastBody
	mu.Unlock()
	requireEqual(t, got, "seekable-data")
}

func TestRetryWithCustomTransport(t *testing.T) {
	var attempts atomic.Int32
	transport := &fakeRoundTripper{
		fn: func(_ *http.Request) (*http.Response, error) {
			n := attempts.Add(1)
			if n <= 2 {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Body:       io.NopCloser(strings.NewReader("unavailable")),
					Header:     make(http.Header),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
			}, nil
		},
	}

	client := New(
		WithTransport(transport),
		WithRetries(3),
		WithRetryWait(10*time.Millisecond, 50*time.Millisecond),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://fake.test", http.NoBody)
	resp, err := client.Do(req)
	requireNoErr(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	requireEqual(t, string(body), "ok")
	requireEqual(t, attempts.Load(), int32(3))
}

func TestRetryAdditionalStatusCodesOrdering(t *testing.T) {
	// WithAdditionalRetryableStatusCodes before WithRetryableStatusCodes:
	// the additional code is lost because the latter replaces the map.
	client := New(
		WithAdditionalRetryableStatusCodes(500),
		WithRetryableStatusCodes(429),
	)
	rt := client.Transport.(*retryTransport)

	_, has429 := rt.retryableCodes[429]
	requireTrue(t, has429)
	_, has500 := rt.retryableCodes[500]
	requireFalse(t, has500) // 500 was wiped by the subsequent replace

	// Reverse order: WithRetryableStatusCodes first, then additional.
	client2 := New(
		WithRetryableStatusCodes(429),
		WithAdditionalRetryableStatusCodes(500),
	)
	rt2 := client2.Transport.(*retryTransport)

	_, has429 = rt2.retryableCodes[429]
	requireTrue(t, has429)
	_, has500 = rt2.retryableCodes[500]
	requireTrue(t, has500)
}

func TestWithDialContext(t *testing.T) {
	var dialCalled atomic.Bool
	customDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialCalled.Store(true)
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	client := New(WithDialContext(customDial), WithNoRetries())
	resp, err := client.Get(srv.URL)
	requireNoErr(t, err)
	resp.Body.Close()
	requireTrue(t, dialCalled.Load())
}

func TestWithResolver(t *testing.T) {
	// Verify the resolver is set on the transport's dialer by checking that
	// the transport is constructed without error and the resolver field is
	// propagated. We can't easily inspect the dialer inside the transport,
	// so we verify it works end-to-end with a real server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	resolver := &net.Resolver{PreferGo: true}
	client := New(WithResolver(resolver), WithNoRetries())
	resp, err := client.Get(srv.URL)
	requireNoErr(t, err)
	resp.Body.Close()
	requireEqual(t, resp.StatusCode, 200)
}

func TestWithDialContextOverridesResolver(t *testing.T) {
	// When both WithDialContext and WithResolver are set, the custom
	// DialContext takes precedence.
	var customDialCalled atomic.Bool
	customDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		customDialCalled.Store(true)
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	resolver := &net.Resolver{PreferGo: true}
	client := New(WithDialContext(customDial), WithResolver(resolver), WithNoRetries())
	resp, err := client.Get(srv.URL)
	requireNoErr(t, err)
	resp.Body.Close()
	requireTrue(t, customDialCalled.Load())
}

func TestWithProxy(t *testing.T) {
	client := New(WithProxy(http.ProxyFromEnvironment), WithNoRetries())
	rt := client.Transport.(*retryTransport)
	tr := rt.next.(*http.Transport)
	// Verify the transport was built (not nil) — we can't compare function
	// pointers directly, but we can verify the transport exists.
	requireTrue(t, tr != nil)
}

func TestWithNoProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	client := New(WithNoProxy(), WithNoRetries())
	resp, err := client.Get(srv.URL)
	requireNoErr(t, err)
	resp.Body.Close()
	requireEqual(t, resp.StatusCode, 200)
}

func TestWithMaxResponseHeaderBytes(t *testing.T) {
	client := New(WithMaxResponseHeaderBytes(1<<20), WithNoRetries())
	rt := client.Transport.(*retryTransport)
	tr := rt.next.(*http.Transport)
	requireEqual(t, tr.MaxResponseHeaderBytes, int64(1<<20))
}

func TestDialContextIgnoredWithTransport(t *testing.T) {
	// When WithTransport is used, WithDialContext should be ignored — the
	// custom transport is used as-is.
	var dialCalled atomic.Bool
	customDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialCalled.Store(true)
		return nil, fmt.Errorf("should not be called")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	client := New(
		WithDialContext(customDial),
		WithTransport(http.DefaultTransport),
		WithNoRetries(),
	)
	resp, err := client.Get(srv.URL)
	requireNoErr(t, err)
	resp.Body.Close()
	requireFalse(t, dialCalled.Load())
}
