# gttp

[![ci](https://github.com/bluesky-social/gttp/actions/workflows/ci.yaml/badge.svg)](https://github.com/bluesky-social/gttp/actions/workflows/ci.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/bluesky-social/gttp.svg)](https://pkg.go.dev/github.com/bluesky-social/gttp)

A robust HTTP client for Go with good defaults and tunable behavior. Called `gttp` because it's a "go http" client library.

`gttp.New()` returns a standard `*http.Client` with sensible timeouts,
connection pooling, retry logic, and safety guards built in.

## Features

- Retry with exponential backoff + full jitter, honoring `Retry-After` / `RateLimit-Reset`
- HTTP/2 health-check pings, TLS 1.2+ enforced, session cache
- Request/response body idle-timeout (stops slow-loris)
- Minimum transfer rate watchdog (opt-in; curl `--speed-limit` equivalent)
- Decompression-bomb guard (1000:1 ratio default)
- Response-body size cap (opt-in)
- Redirect loop detection, scheme-downgrade refusal
- SSRF filter on redirects (loopback / private / CGNAT / NAT64 / link-local / IMDS)
- Sensitive-header scrubbing on cross-origin redirects
- Typed error sentinels; every failure mode has an `errors.Is` target

## Usage

```go
// Use the defaults:
client := gttp.New()
resp, err := client.Get("https://example.com")

// Tune for your environment:
client := gttp.New(
    gttp.WithTimeout(10 * time.Second),
    gttp.WithRetries(5),
    gttp.WithIdleTimeout(10 * time.Second),
    gttp.WithMaxResponseBodyBytes(100 << 20),
    gttp.WithStrictSSRFProtection(),
)
```

See [godoc](https://pkg.go.dev/github.com/bluesky-social/gttp) for the full option list.
