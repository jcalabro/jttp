package jttp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Verifies that a fully-configured client handles a normal gzipped HTTPS
// response without issue.
func TestIntegrationHTTPSGzipAllGuardsEnabled(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	payload := []byte("hello from a legitimate server")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer func() { _ = gz.Close() }()
		_, _ = gz.Write(payload)
	}))
	defer srv.Close()

	base := srv.Client()
	client := New(
		WithTLSConfig(base.Transport.(*http.Transport).TLSClientConfig),
		WithNoRetries(),
	)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

// 100 goroutines sharing a single client hitting a fast server. Exercises
// concurrent guard state under the race detector.
func TestIntegrationConcurrentHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := New(
		WithNoRetries(),
		WithIdleTimeout(5*time.Second),
		WithAllowPrivateRedirects(), // httptest uses loopback
	)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(srv.URL)
			if err != nil {
				errs <- err
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("err: %v", e)
	}
}

// Retry + guard interaction: the server returns 503 twice then slow-drips on
// attempt 3. With retries enabled and idleTimeout, we retry twice, then abort
// the third attempt with ErrBodyIdleTimeout.
func TestIntegrationRetryThenGuardFires(t *testing.T) {
	var attempt atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempt.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Start writing the headers, then hang without writing body bytes.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijack")
			return
		}
		conn, bw, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = bw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n")
		_ = bw.Flush()
		// Hold the connection open, write no body bytes.
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	client := New(
		WithRetries(3),
		WithRetryWait(5*time.Millisecond, 20*time.Millisecond),
		WithIdleTimeout(150*time.Millisecond),
		WithAllowPrivateRedirects(),
	)
	start := time.Now()
	resp, err := client.Get(srv.URL)
	var readErr error
	if resp != nil {
		_, readErr = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	elapsed := time.Since(start)

	// The error could come from Get or from reading the body
	finalErr := err
	if finalErr == nil {
		finalErr = readErr
	}
	if !errors.Is(finalErr, ErrBodyIdleTimeout) {
		t.Errorf("err = %v, want ErrBodyIdleTimeout", finalErr)
	}
	if attempt.Load() < 3 {
		t.Errorf("attempts = %d, want >= 3", attempt.Load())
	}
	if elapsed > 3*time.Second {
		t.Errorf("elapsed %v exceeds bound", elapsed)
	}
}

// Adversarial: raw TCP listener that accepts then sleeps without reading.
// Exercises guardedWriter / write-side idle timeout.
func TestIntegrationSlowWriteServer(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lis.Close() }()

	// Accept one connection, then sleep forever without reading.
	go func() {
		conn, aerr := lis.Accept()
		if aerr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		time.Sleep(10 * time.Second)
	}()

	// Large body so the socket send buffer fills (at least for loopback defaults).
	body := bytes.NewReader(make([]byte, 64<<20)) // 64 MiB

	client := New(
		WithNoRetries(),
		WithIdleTimeout(150*time.Millisecond),
		WithAllowPrivateRedirects(),
	)
	url := fmt.Sprintf("http://%s/", lis.Addr().String())

	start := time.Now()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, url, body)
	req.ContentLength = int64(body.Len())
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	elapsed := time.Since(start)

	if !errors.Is(err, ErrBodyIdleTimeout) {
		t.Errorf("err = %v, want ErrBodyIdleTimeout", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("elapsed %v too long", elapsed)
	}
}

// WithDisableCompression: client must not negotiate compression. Server
// verifies Accept-Encoding is empty and returns raw bytes.
func TestIntegrationDisableCompression(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "" {
			t.Errorf("server got Accept-Encoding: %s", r.Header.Get("Accept-Encoding"))
		}
		fmt.Fprint(w, "raw")
	}))
	defer srv.Close()

	client := New(
		WithNoRetries(),
		WithDisableCompression(),
		WithAllowPrivateRedirects(),
	)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "raw" {
		t.Errorf("body = %q", body)
	}
}

// Large honest download: must not false-positive idle timeout or ratio guard,
// and must complete despite being large.
func TestIntegrationLargeHonestDownload(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	size := 2 << 20 // 2 MiB of repeatable ASCII
	payload := bytes.Repeat([]byte("honest server data\n"), size/19)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	client := New(
		WithNoRetries(),
		WithIdleTimeout(5*time.Second),
		WithAllowPrivateRedirects(),
	)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("size = %d, want %d", len(got), len(payload))
	}
}

// Integration: a POST body is consumed, the server slow-writes, the guard
// fires and the retry layer retries on a fresh connection. On attempt 2 the
// server behaves normally, body is rewound from GetBody, request succeeds.
//
// This pins the write-side-idle-timeout + body-rewind interaction. If
// retryTransport ever fails to reset req.Body from GetBody after a write-
// side cancel, this test catches it.
func TestIntegrationWriteIdleRetriesWithBodyRewind(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	var attempt atomic.Int32
	var gotBody atomic.Value // string from attempt 2

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := attempt.Add(1)
		if n == 1 {
			// Attempt 1: accept but never read the body, then hang on write.
			// That causes the client's next Read on req.Body to stall when
			// the kernel send buffer fills — our guard fires.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("no hijack")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			time.Sleep(5 * time.Second)
			return
		}
		// Attempt 2: behave normally.
		body, _ := io.ReadAll(r.Body)
		gotBody.Store(string(body))
		fmt.Fprint(w, "ok")
	})

	client := New(
		WithRetries(2),
		WithRetryWait(5*time.Millisecond, 20*time.Millisecond),
		WithIdleTimeout(150*time.Millisecond),
		// POST must be in the retryable set for the retry to run.
		WithAdditionalRetryableMethods(http.MethodPost),
		WithAllowPrivateRedirects(),
	)

	// Use a body large enough to fill the kernel send buffer on loopback.
	// We rely on Go's http client to set GetBody from strings.NewReader.
	payload := strings.Repeat("x", 8<<20) // 8 MiB
	start := time.Now()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/", strings.NewReader(payload))
	resp, err := client.Do(req)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("err = %v, want nil (retry should have succeeded)", err)
	}
	got, _ := gotBody.Load().(string)
	if got != payload {
		t.Errorf("second-attempt body mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if attempt.Load() < 2 {
		t.Errorf("attempts = %d, want >= 2", attempt.Load())
	}
	if elapsed > 3*time.Second {
		t.Errorf("elapsed %v too long", elapsed)
	}
}

