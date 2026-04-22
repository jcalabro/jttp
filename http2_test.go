package jttp

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConfigureHTTP2SetsReadIdleAndPingTimeout(t *testing.T) {
	tr := &http.Transport{}
	h2, err := configureHTTP2(tr, 30*time.Second, 15*time.Second)
	requireNoErr(t, err)
	requireTrue(t, h2 != nil)
	requireEqual(t, h2.ReadIdleTimeout, 30*time.Second)
	requireEqual(t, h2.PingTimeout, 15*time.Second)
}

func TestConfigureHTTP2ZeroReadIdleDisablesHealthCheck(t *testing.T) {
	tr := &http.Transport{}
	h2, err := configureHTTP2(tr, 0, 15*time.Second)
	requireNoErr(t, err)
	requireEqual(t, h2.ReadIdleTimeout, time.Duration(0))
}

func TestDefaultHTTP2HealthChecksActive(t *testing.T) {
	client := New(WithNoRetries())
	rt := client.Transport.(*retryTransport)
	requireTrue(t, rt.http2Transport != nil)
	requireTrue(t, rt.http2Transport.ReadIdleTimeout > 0)
	requireTrue(t, rt.http2Transport.PingTimeout > 0)
}

func TestWithHTTP2ReadIdleTimeout(t *testing.T) {
	client := New(WithHTTP2ReadIdleTimeout(10*time.Second), WithNoRetries())
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.http2Transport.ReadIdleTimeout, 10*time.Second)
}

func TestWithHTTP2PingTimeout(t *testing.T) {
	client := New(WithHTTP2PingTimeout(7*time.Second), WithNoRetries())
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.http2Transport.PingTimeout, 7*time.Second)
}

func TestHTTP2ReadIdleTimeoutZeroDisables(t *testing.T) {
	client := New(WithHTTP2ReadIdleTimeout(0), WithNoRetries())
	rt := client.Transport.(*retryTransport)
	requireEqual(t, rt.http2Transport.ReadIdleTimeout, time.Duration(0))
}

func TestForceHTTP2FalseSkipsH2Configuration(t *testing.T) {
	client := New(WithForceHTTP2(false), WithNoRetries())
	rt := client.Transport.(*retryTransport)
	tr := rt.next.(*http.Transport)
	requireFalse(t, tr.ForceAttemptHTTP2)
	requireTrue(t, rt.http2Transport == nil)
}

func TestCustomTransportSkipsH2Configuration(t *testing.T) {
	// When WithTransport is used, the caller owns the transport — we don't
	// touch HTTP/2 settings on it.
	client := New(WithTransport(http.DefaultTransport), WithNoRetries())
	rt := client.Transport.(*retryTransport)
	requireTrue(t, rt.http2Transport == nil)
}

// End-to-end: a real HTTPS server negotiating HTTP/2 must work with our
// ConfigureTransports-based wiring. This guards against regressions where
// H/2 configuration silently breaks the TLS/ALPN path.
func TestHTTPSEndToEndNegotiatesHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, r.Proto); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	certs := x509.NewCertPool()
	certs.AddCert(srv.Certificate())

	client := New(
		WithTLSConfig(&tls.Config{RootCAs: certs}),
		WithNoRetries(),
	)
	resp, err := client.Get(srv.URL)
	requireNoErr(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	requireNoErr(t, err)
	requireEqual(t, string(body), "HTTP/2.0")
}
