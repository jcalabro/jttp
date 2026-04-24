package jttp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIdleWatchdogFiresAfterTimeout(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	sentinel := errors.New("idle")
	w := newIdleWatchdog(50*time.Millisecond, cancel, sentinel)
	defer w.Stop()

	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), sentinel) {
			t.Errorf("cause = %v, want %v", context.Cause(ctx), sentinel)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("watchdog did not fire within 500ms")
	}
}

func TestIdleWatchdogResetExtends(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	sentinel := errors.New("idle")
	w := newIdleWatchdog(80*time.Millisecond, cancel, sentinel)
	defer w.Stop()

	// Hit Reset at 40ms and 80ms so the real elapsed idle time never exceeds 80ms.
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()

	deadline := time.After(150 * time.Millisecond)
	for {
		select {
		case <-tick.C:
			w.Reset()
		case <-ctx.Done():
			t.Fatalf("watchdog fired early: cause=%v", context.Cause(ctx))
		case <-deadline:
			return // success — watchdog did NOT fire
		}
	}
}

func TestIdleWatchdogStopPrevents(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	sentinel := errors.New("idle")
	w := newIdleWatchdog(50*time.Millisecond, cancel, sentinel)
	w.Stop()

	select {
	case <-ctx.Done():
		t.Errorf("fired after Stop: cause=%v", context.Cause(ctx))
	case <-time.After(200 * time.Millisecond):
		// expected: did NOT fire
	}
}

func TestIdleWatchdogZeroTimeoutIsNoop(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	sentinel := errors.New("idle")
	w := newIdleWatchdog(0, cancel, sentinel)
	defer w.Stop()

	select {
	case <-ctx.Done():
		t.Errorf("fired with zero timeout: cause=%v", context.Cause(ctx))
	case <-time.After(100 * time.Millisecond):
	}
}

// Race test: hammer Reset() and Stop() while the timer may be firing.
func TestIdleWatchdogConcurrentResetStop(t *testing.T) {
	for range 50 {
		ctx, cancel := context.WithCancelCause(context.Background())
		_ = ctx // may or may not fire; we're testing for races
		sentinel := errors.New("idle")
		w := newIdleWatchdog(5*time.Millisecond, cancel, sentinel)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 100 {
				w.Reset()
			}
		}()
		go func() {
			defer wg.Done()
			time.Sleep(2 * time.Millisecond)
			w.Stop()
		}()
		wg.Wait()
		cancel(nil)
	}
}

func TestGuardedBodySizeCapUnderLimit(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	src := io.NopCloser(strings.NewReader("hello"))
	gb, err := newGuardedBody(src, guardedBodyConfig{
		ctx:      ctx,
		cancel:   cancel,
		maxBytes: 100,
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	data, err := io.ReadAll(gb)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("got %q, want %q", data, "hello")
	}
}

func TestGuardedBodySizeCapExceeded(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	src := io.NopCloser(strings.NewReader("hello world"))
	gb, err := newGuardedBody(src, guardedBodyConfig{
		ctx:      ctx,
		cancel:   cancel,
		maxBytes: 5, // "hello" fits, anything more does not
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	_, err = io.ReadAll(gb)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("err = %v, want ErrResponseTooLarge", err)
	}
}

func TestGuardedBodySizeCapZeroMeansUnlimited(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	big := strings.Repeat("x", 10_000_000)
	src := io.NopCloser(strings.NewReader(big))
	gb, err := newGuardedBody(src, guardedBodyConfig{
		ctx:      ctx,
		cancel:   cancel,
		maxBytes: 0,
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	data, err := io.ReadAll(gb)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != len(big) {
		t.Errorf("len = %d, want %d", len(data), len(big))
	}
}

func TestGuardedBodyExactCapBoundary(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// Exactly maxBytes must succeed.
	src := io.NopCloser(strings.NewReader("hello"))
	gb, err := newGuardedBody(src, guardedBodyConfig{
		ctx:      ctx,
		cancel:   cancel,
		maxBytes: 5,
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	data, err := io.ReadAll(gb)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(data) != "hello" {
		t.Errorf("got %q", data)
	}
}

// deadReader blocks on Read until the supplied context is cancelled, then
// returns the context's error. This emulates how a real net.Conn-backed body
// behaves when the request context is cancelled by our idle watchdog —
// the network read returns, it does not stay blocked forever.
type deadReader struct {
	ctx context.Context
}

func newDeadReader(ctx context.Context) *deadReader { return &deadReader{ctx: ctx} }
func (d *deadReader) Read(p []byte) (int, error) {
	<-d.ctx.Done()
	return 0, d.ctx.Err()
}
func (d *deadReader) Close() error { return nil }

func TestGuardedBodyIdleTimeoutFires(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	dr := newDeadReader(ctx)
	gb, err := newGuardedBody(dr, guardedBodyConfig{
		ctx:         ctx,
		cancel:      cancel,
		idleTimeout: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	start := time.Now()
	_, err = io.ReadAll(gb)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrBodyIdleTimeout) {
		t.Errorf("err = %v, want ErrBodyIdleTimeout", err)
	}
	if elapsed < 60*time.Millisecond {
		t.Errorf("fired too early: %v", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("hung past bound: %v", elapsed)
	}
}

func TestGuardedBodyIdleTimeoutResetsOnProgress(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// 20ms per byte; idle timeout 100ms. Every read resets — should complete.
	src := &cappedSlowReader{interval: 20 * time.Millisecond, data: []byte("hello")}
	gb, err := newGuardedBody(src, guardedBodyConfig{
		ctx:         ctx,
		cancel:      cancel,
		idleTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	data, err := io.ReadAll(gb)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("got %q", data)
	}
}

// cappedSlowReader delivers `data` one byte at a time with `interval` between reads, then EOF.
type cappedSlowReader struct {
	interval time.Duration
	data     []byte
	pos      int
}

func (c *cappedSlowReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	time.Sleep(c.interval)
	p[0] = c.data[c.pos]
	c.pos++
	return 1, nil
}
func (c *cappedSlowReader) Close() error { return nil }

// slowReader emits one byte per interval, ignoring p length. Respects the
// given context so a cancel unblocks the current sleep.
type slowReader struct {
	interval time.Duration
	ctx      context.Context
}

func (s *slowReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	t := time.NewTimer(s.interval)
	defer t.Stop()
	select {
	case <-s.ctx.Done():
		return 0, s.ctx.Err()
	case <-t.C:
	}
	p[0] = 'x'
	return 1, nil
}
func (s *slowReader) Close() error { return nil }

// chunkyReader emits `chunk` bytes per interval from data, then EOF.
// Respects context.
type chunkyReader struct {
	chunk    int
	interval time.Duration
	data     []byte
	pos      int
	ctx      context.Context
}

func (c *chunkyReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	t := time.NewTimer(c.interval)
	defer t.Stop()
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	case <-t.C:
	}
	n := c.chunk
	if n > len(p) {
		n = len(p)
	}
	if c.pos+n > len(c.data) {
		n = len(c.data) - c.pos
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}
func (c *chunkyReader) Close() error { return nil }

func TestGuardedBodyMinRateFires(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// Server-side analog: 1 byte per 50ms → 20 B/s. Floor is 1000 B/s over 200ms.
	src := &slowReader{interval: 50 * time.Millisecond, ctx: ctx}
	gb, err := newGuardedBody(src, guardedBodyConfig{
		ctx:           ctx,
		cancel:        cancel,
		minRate:       1000,
		minRateWindow: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	start := time.Now()
	_, err = io.ReadAll(gb)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrBodyTransferTooSlow) {
		t.Errorf("err = %v, want ErrBodyTransferTooSlow", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("fired too early: %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("hung past bound: %v", elapsed)
	}
}

func TestGuardedBodyMinRateHonestReaderPasses(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// 100 B per 10ms = 10,000 B/s; floor 1000 B/s. Should pass.
	data := make([]byte, 500)
	src := &chunkyReader{chunk: 100, interval: 10 * time.Millisecond, data: data, ctx: ctx}
	gb, err := newGuardedBody(src, guardedBodyConfig{
		ctx:           ctx,
		cancel:        cancel,
		minRate:       1000,
		minRateWindow: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	out, err := io.ReadAll(gb)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(out) != len(data) {
		t.Errorf("got %d bytes, want %d", len(out), len(data))
	}
}

func gzipZeros(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	zeros := make([]byte, 4096)
	remaining := n
	for remaining > 0 {
		chunk := len(zeros)
		if chunk > remaining {
			chunk = remaining
		}
		if _, err := w.Write(zeros[:chunk]); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		remaining -= chunk
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestGuardedBodyGzipTransparentDecode(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	payload := []byte("hello world, this is a test")
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	src := io.NopCloser(bytes.NewReader(buf.Bytes()))
	gb, err := newGuardedBody(src, guardedBodyConfig{
		ctx:            ctx,
		cancel:         cancel,
		decompressGzip: true,
		maxRatio:       1000,
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	got, err := io.ReadAll(gb)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestGuardedBodyBombTripsRatioGuard(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// 256 MiB of zeros → ratio well over 1000:1, and will definitely exceed
	// 64 KiB compressed. Use maxRatio 100 so it fires fast.
	bomb := gzipZeros(t, 256<<20)
	t.Logf("bomb: %d compressed bytes, %d decompressed target", len(bomb), 256<<20)

	src := io.NopCloser(bytes.NewReader(bomb))
	gb, err := newGuardedBody(src, guardedBodyConfig{
		ctx:            ctx,
		cancel:         cancel,
		decompressGzip: true,
		maxRatio:       100,
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	_, err = io.Copy(io.Discard, gb)
	if !errors.Is(err, ErrDecompressionBomb) {
		t.Errorf("err = %v, want ErrDecompressionBomb", err)
	}
}

func TestGuardedBodyRatioGuardDisabled(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	bomb := gzipZeros(t, 1<<20) // 1 MiB zeros
	src := io.NopCloser(bytes.NewReader(bomb))
	gb, err := newGuardedBody(src, guardedBodyConfig{
		ctx:            ctx,
		cancel:         cancel,
		decompressGzip: true,
		maxRatio:       0, // disabled
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	out, err := io.ReadAll(gb)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(out) != 1<<20 {
		t.Errorf("got %d bytes, want %d", len(out), 1<<20)
	}
}

func TestGuardedBodyRatioUndersampleNoFalsePositive(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// Small gzip payload: ratio will look high locally but we haven't seen
	// enough compressed data to evaluate it. Guard must NOT fire.
	small := gzipZeros(t, 4096)
	src := io.NopCloser(bytes.NewReader(small))
	gb, err := newGuardedBody(src, guardedBodyConfig{
		ctx:            ctx,
		cancel:         cancel,
		decompressGzip: true,
		maxRatio:       10,
	})
	if err != nil {
		t.Fatalf("newGuardedBody: %v", err)
	}
	defer gb.Close()

	out, err := io.ReadAll(gb)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(out) != 4096 {
		t.Errorf("got %d bytes, want 4096", len(out))
	}
}

func TestGuardedWriterIdleTimeoutFiresWhenConsumerStalls(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// A body the transport will try to read from.
	body := io.NopCloser(bytes.NewReader([]byte("hello")))
	gw := newGuardedWriter(body, cancel, 80*time.Millisecond)
	defer gw.Close()

	// Simulate a "consumer that never calls Read" by simply not calling Read.
	// The watchdog should fire after ~80ms and cancel the context.
	start := time.Now()
	<-ctx.Done()
	elapsed := time.Since(start)

	if !errors.Is(context.Cause(ctx), ErrBodyIdleTimeout) {
		t.Errorf("cause = %v, want ErrBodyIdleTimeout", context.Cause(ctx))
	}
	if elapsed < 60*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("elapsed %v outside [60ms, 500ms]", elapsed)
	}
}

func TestGuardedWriterReadResetsWatchdog(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	body := io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), 10)))
	gw := newGuardedWriter(body, cancel, 100*time.Millisecond)
	defer gw.Close()

	// Read one byte every 30ms: well within the 100ms idle window.
	deadline := time.After(500 * time.Millisecond)
	buf := make([]byte, 1)
	for i := 0; i < 10; i++ {
		select {
		case <-ctx.Done():
			t.Fatalf("fired early at read %d: %v", i, context.Cause(ctx))
		case <-deadline:
			t.Fatal("test timed out")
		default:
		}
		time.Sleep(30 * time.Millisecond)
		if _, err := gw.Read(buf); err != nil && err != io.EOF {
			t.Fatalf("read %d: %v", i, err)
		}
	}
}

func TestGuardedWriterZeroTimeoutIsNoop(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	body := io.NopCloser(bytes.NewReader([]byte("hi")))
	gw := newGuardedWriter(body, cancel, 0)
	defer gw.Close()

	select {
	case <-ctx.Done():
		t.Errorf("canceled with zero timeout: %v", context.Cause(ctx))
	case <-time.After(120 * time.Millisecond):
		// expected
	}
}
