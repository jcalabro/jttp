package jttp

import (
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

// Default HTTP/2 health-check values. ReadIdleTimeout triggers a PING when
// no frame has been received on the connection for that long; PingTimeout
// governs how long we wait for the PING response before tearing the
// connection down. Without these, a dead half-open HTTP/2 connection (e.g.
// killed silently by a load balancer) sits idle in the pool and returns
// errors only on the next use — the classic "black-hole" failure mode.
const (
	DefaultHTTP2ReadIdleTimeout = 30 * time.Second
	DefaultHTTP2PingTimeout     = 15 * time.Second
)

// configureHTTP2 upgrades a standard *http.Transport to speak HTTP/2 and
// returns the resulting *http2.Transport so its health-check fields can be
// tuned. It is a thin wrapper over http2.ConfigureTransports with the two
// timeouts set.
func configureHTTP2(tr *http.Transport, readIdle, pingTimeout time.Duration) (*http2.Transport, error) {
	h2, err := http2.ConfigureTransports(tr)
	if err != nil {
		return nil, err
	}
	h2.ReadIdleTimeout = readIdle
	h2.PingTimeout = pingTimeout
	return h2, nil
}
