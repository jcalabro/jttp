package jttp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// safetyConfig is the subset of config the safety layer cares about.
type safetyConfig struct {
	// Compression control
	compressionEnabled bool

	// Response body guards
	maxBytes      int64
	idleTimeout   time.Duration
	minRate       int64
	minRateWindow time.Duration
	maxRatio      float64

	// SSRF: if true, the safety transport blocks the initial request too
	// (the redirectGuard handles subsequent ones via http.Client.CheckRedirect).
	strictSSRFInitial bool

	// redirectGuard supplies the IP-policy logic reused by checkInitial.
	redirectGuard *redirectGuard
}

// safetyTransport wraps a base http.RoundTripper with response-body guards
// (idle, min-rate, size cap, decompression-bomb), request-body write idle,
// and gzip decompression control.
type safetyTransport struct {
	next http.RoundTripper
	cfg  safetyConfig
}

func (t *safetyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Strict SSRF: reject the initial request if target resolves to blocked.
	if t.cfg.strictSSRFInitial && t.cfg.redirectGuard != nil {
		if err := t.cfg.redirectGuard.checkIPPolicy(req.Context(), req.URL.Hostname()); err != nil {
			return nil, err
		}
	}

	// Clone before mutating headers or swapping context/body. The
	// http.RoundTripper contract says RoundTrip must not modify the caller's
	// request. retryTransport (our immediate parent in the default chain)
	// already clones, but callers wiring safetyTransport directly — or any
	// future refactor that removes the parent clone — still deserve
	// correctness.
	req = req.Clone(req.Context())

	// Accept-Encoding: only auto-add if compression is enabled AND caller has
	// not set it themselves.
	if t.cfg.compressionEnabled && req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "gzip")
	}

	// When a guard feature is active, attach a cancelable child context so
	// the request-body guardedWriter and the response-body guardedBody can
	// unblock in-flight I/O by cancelling.
	ctx := req.Context()
	var cancel context.CancelCauseFunc
	if t.cfg.idleTimeout > 0 || t.cfg.minRate > 0 || t.cfg.maxBytes > 0 || t.cfg.maxRatio > 0 {
		var cctx context.Context
		cctx, cancel = context.WithCancelCause(ctx)
		req = req.WithContext(cctx)
		ctx = cctx
	}

	if t.cfg.idleTimeout > 0 && req.Body != nil && req.Body != http.NoBody && cancel != nil {
		req.Body = newGuardedWriter(req.Body, cancel, t.cfg.idleTimeout)
	}

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		if cancel != nil {
			cancel(nil)
		}
		return nil, err
	}

	// gzip decode? Only when compression is enabled AND the server actually
	// returned gzip. A user-supplied transport may have auto-decoded already
	// (stdlib behavior), in which case Content-Encoding is empty and we skip.
	decompressGzip := false
	if t.cfg.compressionEnabled && strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		decompressGzip = true
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
	}

	// Always wrap the body so future Reads are guarded even if only some
	// features are configured. If cancel is nil, we didn't create a child
	// context; pass the request's original ctx straight through.
	// guardedBody.ctxErr handles cancel == nil as a no-op.
	gb, gerr := newGuardedBody(resp.Body, guardedBodyConfig{
		ctx:            ctx,
		cancel:         cancel, // may be nil
		maxBytes:       t.cfg.maxBytes,
		idleTimeout:    t.cfg.idleTimeout,
		minRate:        t.cfg.minRate,
		minRateWindow:  t.cfg.minRateWindow,
		decompressGzip: decompressGzip,
		maxRatio:       t.cfg.maxRatio,
	})
	if gerr != nil {
		// Close the raw body and tear down the watchdogs — otherwise a
		// malformed gzip header leaks a connection + a live watchdog
		// goroutine on every bad response.
		drainAndClose(resp)
		if cancel != nil {
			cancel(nil)
		}
		return nil, fmt.Errorf("jttp: wrap response body: %w", gerr)
	}
	resp.Body = gb
	return resp, nil
}

// CloseIdleConnections propagates to the next transport.
func (t *safetyTransport) CloseIdleConnections() {
	type closer interface {
		CloseIdleConnections()
	}
	if c, ok := t.next.(closer); ok {
		c.CloseIdleConnections()
	}
}
