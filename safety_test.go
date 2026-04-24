package jttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stubTransport struct {
	lastReq *http.Request
	body    string
	status  int
	header  http.Header
	// respHeader overrides the header on the returned response (as opposed
	// to `header` which would be request-header-related — here we just have
	// one output field on the response).
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.lastReq = req
	if s.status == 0 {
		s.status = 200
	}
	h := s.header
	if h == nil {
		h = make(http.Header)
	}
	return &http.Response{
		StatusCode: s.status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    req,
	}, nil
}

func TestSafetyTransportSendsAcceptEncodingWhenEnabled(t *testing.T) {
	s := &stubTransport{body: "ok"}
	st := &safetyTransport{
		next: s,
		cfg:  safetyConfig{compressionEnabled: true},
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://x/", http.NoBody)
	resp, err := st.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if ae := s.lastReq.Header.Get("Accept-Encoding"); ae != "gzip" {
		t.Errorf("Accept-Encoding = %q, want gzip", ae)
	}
}

func TestSafetyTransportSkipsAcceptEncodingWhenDisabled(t *testing.T) {
	s := &stubTransport{body: "ok"}
	st := &safetyTransport{
		next: s,
		cfg:  safetyConfig{compressionEnabled: false},
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://x/", http.NoBody)
	resp, err := st.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if ae := s.lastReq.Header.Get("Accept-Encoding"); ae != "" {
		t.Errorf("Accept-Encoding = %q, want empty", ae)
	}
}

func TestSafetyTransportPreservesCallerAcceptEncoding(t *testing.T) {
	s := &stubTransport{body: "ok"}
	st := &safetyTransport{
		next: s,
		cfg:  safetyConfig{compressionEnabled: true},
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://x/", http.NoBody)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := st.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if ae := s.lastReq.Header.Get("Accept-Encoding"); ae != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity (caller set)", ae)
	}
}

func TestSafetyTransportWrapsResponseBodyInGuardedBody(t *testing.T) {
	big := strings.Repeat("x", 2000)
	s := &stubTransport{body: big}
	st := &safetyTransport{
		next: s,
		cfg:  safetyConfig{maxBytes: 1000},
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://x/", http.NoBody)
	resp, err := st.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("err = %v, want ErrResponseTooLarge", err)
	}
}

func TestSafetyTransportStrictSSRFBlocksInitial(t *testing.T) {
	s := &stubTransport{body: "ok"}
	rg := newRedirectGuard(redirectConfig{strictInitial: true})
	st := &safetyTransport{
		next: s,
		cfg: safetyConfig{
			strictSSRFInitial: true,
			redirectGuard:     rg,
		},
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/", http.NoBody)
	_, err := st.RoundTrip(req)

	if !errors.Is(err, ErrBlockedByIPPolicy) {
		t.Errorf("err = %v, want ErrBlockedByIPPolicy", err)
	}
	if s.lastReq != nil {
		t.Error("request should not have been dispatched")
	}
}

// Verify req.Body is wrapped when idle timeout is set. Uses a counting
// read-closer so we can confirm the wrap swapped it out.
type countingReadCloser struct {
	r     io.Reader
	reads *atomic.Int32
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.r.Read(p)
}
func (c *countingReadCloser) Close() error { return nil }

func TestSafetyTransportWrapsRequestBodyWhenIdleTimeoutSet(t *testing.T) {
	var readCount atomic.Int32
	s := &stubTransport{body: "ok"}
	body := &countingReadCloser{r: strings.NewReader("hello"), reads: &readCount}
	st := &safetyTransport{
		next: s,
		cfg: safetyConfig{
			idleTimeout: 10 * time.Millisecond,
		},
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://x/", body)
	_, err := st.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	// The stub transport doesn't actually read the body, but we can check
	// that req.Body on the outgoing request has been replaced.
	if s.lastReq.Body == io.ReadCloser(body) {
		t.Error("request body was not wrapped")
	}
}

// Gzip response path: if server returns Content-Encoding: gzip and
// compression is enabled, safety transport sets up gzip decoding in
// guardedBody, and the Content-Encoding header is stripped so callers
// don't see it.
func TestSafetyTransportDecodesGzipResponseAndStripsHeader(t *testing.T) {
	// A valid gzip stream for "hello".
	// Generated inline to avoid cross-test coupling.
	gzipHello := []byte{
		0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff,
		0xca, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x04, 0x00, 0x00, 0xff, 0xff,
		0x86, 0xa6, 0x10, 0x36, 0x05, 0x00, 0x00, 0x00,
	}
	s := &stubTransport{
		body: string(gzipHello),
		header: http.Header{
			"Content-Encoding": []string{"gzip"},
			"Content-Length":   []string{"29"},
		},
	}
	st := &safetyTransport{
		next: s,
		cfg:  safetyConfig{compressionEnabled: true},
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://x/", http.NoBody)
	resp, err := st.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding not stripped: %q", resp.Header.Get("Content-Encoding"))
	}
	if resp.ContentLength != -1 {
		t.Errorf("ContentLength = %d, want -1", resp.ContentLength)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("decoded = %q, want hello", got)
	}
}
