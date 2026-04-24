package jttp

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// idleWatchdog cancels a context with a given cause if Reset is not called
// within the configured timeout. It is safe for concurrent Reset / Stop.
//
// Implementation detail: we use time.AfterFunc plus an atomic "last reset"
// timestamp. When the timer fires we check whether enough real time has
// elapsed since the last Reset; if not, we reschedule for the remainder.
// This avoids the classic Timer.Stop/drain/Reset race.
type idleWatchdog struct {
	timeout  time.Duration
	cancel   context.CancelCauseFunc
	cause    error
	lastTick atomic.Int64 // unix nanos
	done     atomic.Bool
	timer    *time.Timer
}

func newIdleWatchdog(timeout time.Duration, cancel context.CancelCauseFunc, cause error) *idleWatchdog {
	w := &idleWatchdog{
		timeout: timeout,
		cancel:  cancel,
		cause:   cause,
	}
	if timeout <= 0 {
		return w // no-op mode
	}
	w.lastTick.Store(time.Now().UnixNano())
	w.timer = time.AfterFunc(timeout, w.fire)
	return w
}

// Reset marks the watchdog as having just been "tickled" — e.g. a successful
// read. The next fire is pushed out by timeout.
func (w *idleWatchdog) Reset() {
	if w.timeout <= 0 || w.done.Load() {
		return
	}
	w.lastTick.Store(time.Now().UnixNano())
}

// Stop permanently disables the watchdog. Safe to call multiple times.
func (w *idleWatchdog) Stop() {
	if w.timeout <= 0 {
		return
	}
	if !w.done.CompareAndSwap(false, true) {
		return
	}
	if w.timer != nil {
		w.timer.Stop()
	}
}

func (w *idleWatchdog) fire() {
	if w.done.Load() {
		return
	}
	elapsed := time.Duration(time.Now().UnixNano() - w.lastTick.Load())
	if elapsed >= w.timeout {
		// Truly idle — cancel.
		if w.done.CompareAndSwap(false, true) {
			w.cancel(w.cause)
		}
		return
	}
	// A Reset landed between our schedule and fire. Reschedule for the
	// remaining window.
	remaining := w.timeout - elapsed
	w.timer.Reset(remaining)
}

// guardedBodyConfig bundles all parameters so callers (safetyTransport)
// don't have to supply a long positional argument list.
type guardedBodyConfig struct {
	ctx            context.Context
	cancel         context.CancelCauseFunc
	maxBytes       int64
	idleTimeout    time.Duration
	minRate        int64         // bytes per second; 0 disables
	minRateWindow  time.Duration // window over which to average
	decompressGzip bool          // wrap inner in gzip.Reader
	maxRatio       float64       // 0 disables the ratio guard
}

// guardedBody wraps an http.Response.Body with robustness protections.
// It implements io.ReadCloser.
type guardedBody struct {
	inner      io.ReadCloser
	cfg        guardedBodyConfig
	readBytes  int64
	watchdog   *idleWatchdog
	rate       *rateTracker
	closeOnce  sync.Once
	closeErr   error
	gz         *gzip.Reader    // nil when not decompressing
	compressed *countingReader // nil when not decompressing
}

func newGuardedBody(inner io.ReadCloser, cfg guardedBodyConfig) (*guardedBody, error) {
	g := &guardedBody{inner: inner, cfg: cfg}
	if cfg.idleTimeout > 0 && cfg.cancel != nil {
		g.watchdog = newIdleWatchdog(cfg.idleTimeout, cfg.cancel, ErrBodyIdleTimeout)
	}
	if cfg.minRate > 0 && cfg.minRateWindow > 0 {
		g.rate = newRateTracker(cfg.minRateWindow)
	}
	if cfg.decompressGzip {
		g.compressed = &countingReader{r: inner}
		gz, err := gzip.NewReader(g.compressed)
		if err != nil {
			return nil, fmt.Errorf("jttp: gzip.NewReader: %w", err)
		}
		g.gz = gz
	}
	return g, nil
}

func (g *guardedBody) Read(p []byte) (int, error) {
	if err := g.ctxErr(); err != nil {
		return 0, err
	}

	source := io.Reader(g.inner)
	if g.gz != nil {
		source = g.gz
	}

	n, err := source.Read(p)
	if n > 0 {
		if g.watchdog != nil {
			g.watchdog.Reset()
		}
		g.readBytes += int64(n)

		if g.cfg.maxBytes > 0 && g.readBytes > g.cfg.maxBytes {
			return n, fmt.Errorf("%w (%d bytes)", ErrResponseTooLarge, g.cfg.maxBytes)
		}
		if g.rate != nil {
			if bps, full := g.rate.observe(int64(n)); full && bps < g.cfg.minRate {
				return n, fmt.Errorf("%w (%d B/s < %d B/s)", ErrBodyTransferTooSlow, bps, g.cfg.minRate)
			}
		}
		if g.cfg.maxRatio > 0 && g.compressed != nil && g.compressed.total >= bombGuardMinCompressed {
			ratio := float64(g.readBytes) / float64(g.compressed.total)
			if ratio > g.cfg.maxRatio {
				return n, fmt.Errorf("%w (%.1f:1 > %.1f:1)", ErrDecompressionBomb, ratio, g.cfg.maxRatio)
			}
		}
	}
	if err != nil {
		if ce := g.ctxErr(); ce != nil {
			return n, ce
		}
	}
	return n, err
}

func (g *guardedBody) Close() error {
	g.closeOnce.Do(func() {
		if g.watchdog != nil {
			g.watchdog.Stop()
		}
		if g.gz != nil {
			if err := g.gz.Close(); err != nil {
				// Best-effort close: gzip.Reader.Close can fail but we
				// always prefer the inner body's close error for reporting.
				_ = err
			}
		}
		g.closeErr = g.inner.Close()
	})
	return g.closeErr
}

// ctxErr returns the context's cancellation cause if the context is done,
// otherwise nil. If the cause is a jttp sentinel it is returned unwrapped so
// errors.Is works naturally at the call site.
func (g *guardedBody) ctxErr() error {
	if g.cfg.ctx == nil {
		return nil
	}
	select {
	case <-g.cfg.ctx.Done():
		if cause := context.Cause(g.cfg.ctx); cause != nil {
			return cause
		}
		return g.cfg.ctx.Err()
	default:
		return nil
	}
}

// ensure interface
var _ io.ReadCloser = (*guardedBody)(nil)

// rateTracker measures a rolling-window average transfer rate.
// Not safe for concurrent use; guardedBody calls it serially from Read.
//
// The "ramp window" — the initial period during which we refuse to evaluate
// the rate — is anchored on the FIRST OBSERVED SAMPLE, not on tracker
// construction. This matters because a guardedBody may be constructed well
// before the caller's first Read (e.g., the caller holds the response and
// drains it later). Anchoring on the first sample ensures the ramp actually
// reflects "time since bytes started flowing" rather than "time since the
// tracker object was allocated."
type rateTracker struct {
	window     time.Duration
	firstTime  time.Time // zero until first observed sample
	samples    []rateSample
}

type rateSample struct {
	t     time.Time
	bytes int64
}

func newRateTracker(window time.Duration) *rateTracker {
	return &rateTracker{window: window}
}

// observe records n bytes read just now. Returns the current rate in
// bytes/sec averaged over the last window, and whether the window has
// been fully populated yet (i.e. we have at least `window` of elapsed
// time since the first sample so the rate is meaningful).
func (r *rateTracker) observe(n int64) (bps int64, windowFull bool) {
	now := time.Now()
	if n > 0 {
		r.samples = append(r.samples, rateSample{t: now, bytes: n})
		if r.firstTime.IsZero() {
			r.firstTime = now
		}
	}
	cutoff := now.Add(-r.window)
	trim := 0
	for ; trim < len(r.samples); trim++ {
		if !r.samples[trim].t.Before(cutoff) {
			break
		}
	}
	if trim > 0 {
		r.samples = r.samples[trim:]
	}

	if r.firstTime.IsZero() || now.Sub(r.firstTime) < r.window {
		return 0, false
	}
	var total int64
	for _, s := range r.samples {
		total += s.bytes
	}
	secs := r.window.Seconds()
	if secs <= 0 {
		return 0, true
	}
	return int64(float64(total) / secs), true
}

// bombGuardMinCompressed is the minimum number of compressed bytes that must
// have been seen before the ratio guard will fire. Ratios over tiny inputs
// are statistically meaningless (gzip frame overhead dominates). 64 KiB is
// well past the frame-overhead regime.
const bombGuardMinCompressed = 64 << 10

// countingReader wraps an io.Reader and tracks total bytes read.
// It does NOT implement io.Closer — it's an adapter for gzip.NewReader.
type countingReader struct {
	r     io.Reader
	total int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.total += int64(n)
	return n, err
}

// guardedWriter wraps an io.ReadCloser that will be consumed by an
// http.Transport as a request body. The transport calls Read to get bytes to
// send. If the transport stops calling Read (because its socket Write is
// blocked on a server that isn't draining), the watchdog fires and cancels
// the request context, unblocking the transport.
//
// Every successful Read resets the watchdog.
type guardedWriter struct {
	inner     io.ReadCloser
	watchdog  *idleWatchdog
	closeOnce sync.Once
	closeErr  error
}

func newGuardedWriter(inner io.ReadCloser, cancel context.CancelCauseFunc, idleTimeout time.Duration) *guardedWriter {
	gw := &guardedWriter{inner: inner}
	if idleTimeout > 0 && cancel != nil {
		gw.watchdog = newIdleWatchdog(idleTimeout, cancel, ErrBodyIdleTimeout)
	}
	return gw
}

func (g *guardedWriter) Read(p []byte) (int, error) {
	n, err := g.inner.Read(p)
	if n > 0 && g.watchdog != nil {
		g.watchdog.Reset()
	}
	return n, err
}

func (g *guardedWriter) Close() error {
	g.closeOnce.Do(func() {
		if g.watchdog != nil {
			g.watchdog.Stop()
		}
		g.closeErr = g.inner.Close()
	})
	return g.closeErr
}

var _ io.ReadCloser = (*guardedWriter)(nil)
