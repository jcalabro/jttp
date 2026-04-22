package jttp

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// respWithHeaders builds a response with canonically-keyed headers, matching
// what the stdlib transport would produce from a real wire response.
func respWithHeaders(kv map[string]string) *http.Response {
	h := make(http.Header, len(kv))
	for k, v := range kv {
		h.Set(k, v)
	}
	return &http.Response{Header: h}
}

func TestRateLimitResetDeltaSeconds(t *testing.T) {
	rt := &retryTransport{
		waitMin:       100 * time.Millisecond,
		waitMax:       1 * time.Second,
		maxRetryAfter: 1 * time.Minute,
	}
	resp := respWithHeaders(map[string]string{"RateLimit-Reset": "5"})
	requireEqual(t, rt.backoff(0, resp), 5*time.Second)
}

func TestRetryAfterTakesPrecedenceOverRateLimitReset(t *testing.T) {
	rt := &retryTransport{
		waitMin:       100 * time.Millisecond,
		waitMax:       1 * time.Second,
		maxRetryAfter: 1 * time.Minute,
	}
	resp := respWithHeaders(map[string]string{
		"Retry-After":     "3",
		"RateLimit-Reset": "10",
	})
	requireEqual(t, rt.backoff(0, resp), 3*time.Second)
}

func TestXRateLimitResetDeltaSeconds(t *testing.T) {
	rt := &retryTransport{
		waitMin:       100 * time.Millisecond,
		waitMax:       1 * time.Second,
		maxRetryAfter: 1 * time.Minute,
	}
	resp := respWithHeaders(map[string]string{"X-RateLimit-Reset": "7"})
	requireEqual(t, rt.backoff(0, resp), 7*time.Second)
}

func TestXRateLimitResetUnixEpoch(t *testing.T) {
	rt := &retryTransport{
		waitMin:       100 * time.Millisecond,
		waitMax:       1 * time.Second,
		maxRetryAfter: 1 * time.Minute,
	}
	// GitHub-style: value is a unix epoch time in the future.
	future := time.Now().Add(4 * time.Second).Unix()
	resp := respWithHeaders(map[string]string{
		"X-RateLimit-Reset": fmt.Sprintf("%d", future),
	})
	d := rt.backoff(0, resp)
	requireTrue(t, d >= 3*time.Second && d <= 5*time.Second)
}

func TestXRateLimitResetPastUnixEpochFallsBackToBackoff(t *testing.T) {
	rt := &retryTransport{
		waitMin: 100 * time.Millisecond,
		waitMax: 500 * time.Millisecond,
	}
	// Unix epoch in the past: produces no wait, falls back to exp. backoff.
	past := time.Now().Add(-1 * time.Hour).Unix()
	resp := respWithHeaders(map[string]string{
		"X-RateLimit-Reset": fmt.Sprintf("%d", past),
	})
	d := rt.backoff(0, resp)
	requireTrue(t, d >= rt.waitMin && d <= rt.waitMax)
}

func TestRateLimitResetTakesPrecedenceOverXRateLimitReset(t *testing.T) {
	rt := &retryTransport{
		waitMin:       100 * time.Millisecond,
		waitMax:       1 * time.Second,
		maxRetryAfter: 1 * time.Minute,
	}
	resp := respWithHeaders(map[string]string{
		"RateLimit-Reset":   "2",
		"X-RateLimit-Reset": "10",
	})
	requireEqual(t, rt.backoff(0, resp), 2*time.Second)
}

func TestRateLimitResetCappedByMaxRetryAfter(t *testing.T) {
	rt := &retryTransport{
		waitMin:       100 * time.Millisecond,
		waitMax:       500 * time.Millisecond,
		maxRetryAfter: 2 * time.Second,
	}
	resp := respWithHeaders(map[string]string{"RateLimit-Reset": "60"})
	requireEqual(t, rt.backoff(0, resp), 2*time.Second)
}

func TestRateLimitResetFlooredAtWaitMin(t *testing.T) {
	rt := &retryTransport{
		waitMin:       5 * time.Second,
		waitMax:       30 * time.Second,
		maxRetryAfter: 1 * time.Minute,
	}
	resp := respWithHeaders(map[string]string{"RateLimit-Reset": "1"})
	requireEqual(t, rt.backoff(0, resp), 5*time.Second)
}

func TestInvalidRateLimitResetFallsBackToBackoff(t *testing.T) {
	rt := &retryTransport{
		waitMin: 50 * time.Millisecond,
		waitMax: 500 * time.Millisecond,
	}
	resp := respWithHeaders(map[string]string{"RateLimit-Reset": "not-a-number"})
	d := rt.backoff(0, resp)
	requireTrue(t, d >= rt.waitMin && d <= rt.waitMax)
}

// Value just above the 1e9 threshold should be treated as a unix epoch.
// Value just below should be treated as delta-seconds. This test pins the
// threshold behavior.
func TestXRateLimitResetHeuristicBoundary(t *testing.T) {
	rt := &retryTransport{
		waitMin:       100 * time.Millisecond,
		waitMax:       1 * time.Second,
		maxRetryAfter: 5 * time.Minute,
	}

	// Below threshold → delta-seconds. 100 seconds, well within maxRetryAfter.
	resp := respWithHeaders(map[string]string{"X-RateLimit-Reset": "100"})
	requireEqual(t, rt.backoff(0, resp), 100*time.Second)

	// Above threshold (1e9+1 seconds since epoch ≈ 2001-09-09) → unix epoch,
	// which is in the past → falls through to exponential backoff.
	resp2 := respWithHeaders(map[string]string{"X-RateLimit-Reset": "1000000001"})
	d := rt.backoff(0, resp2)
	requireTrue(t, d >= rt.waitMin && d <= rt.waitMax)
}
