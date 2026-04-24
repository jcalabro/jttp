package jttp

import (
	"net/http"
	"strings"
	"testing"
)

func requireEqual[T comparable](t testing.TB, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func requireNoErr(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func requireIsErr(t testing.TB, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func requireErrContains(t testing.TB, err error, substr string) {
	t.Helper()
	requireIsErr(t, err)
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q does not contain %q", err.Error(), substr)
	}
}

func requireTrue(t testing.TB, v bool) {
	t.Helper()
	if !v {
		t.Fatal("expected true, got false")
	}
}

func requireFalse(t testing.TB, v bool) {
	t.Helper()
	if v {
		t.Fatal("expected false, got true")
	}
}

// innerHTTPTransport unwraps the jttp transport chain
// (retryTransport -> safetyTransport -> *http.Transport) to the underlying
// *http.Transport. Fatals the test if any layer has the wrong type.
func innerHTTPTransport(t testing.TB, rt *retryTransport) *http.Transport {
	t.Helper()
	st, ok := rt.next.(*safetyTransport)
	if !ok {
		t.Fatalf("retryTransport.next = %T, want *safetyTransport", rt.next)
	}
	tr, ok := st.next.(*http.Transport)
	if !ok {
		t.Fatalf("safetyTransport.next = %T, want *http.Transport", st.next)
	}
	return tr
}
