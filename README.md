# jttp

[![ci](https://github.com/jcalabro/jttp/actions/workflows/ci.yaml/badge.svg)](https://github.com/jcalabro/jttp/actions/workflows/ci.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/jcalabro/jttp.svg)](https://pkg.go.dev/github.com/jcalabro/jttp)

A robust HTTP client for Go with good defaults and tunable behavior.

`jttp.New()` returns a standard `*http.Client` with sensible timeouts,
connection pooling, retry logic, and safety guards built in.

## Features

- Retry with exponential backoff + full jitter, honoring `Retry-After` / `RateLimit-Reset`
- HTTP/2 health-check pings, TLS 1.2+ enforced, session cache
- Request/response body idle-timeout (stops slow-loris)
- Minimum transfer rate watchdog (opt-in; curl `--speed-limit` equivalent)
- Decompression-bomb guard (1000:1 ratio default)
- Response-body size cap (opt-in)
- Redirect loop detection, scheme-downgrade refusal
- SSRF filter on redirects (loopback / private / link-local / IMDS)
- Sensitive-header scrubbing on cross-origin redirects
- Typed error sentinels; every failure mode has an `errors.Is` target

## Usage

```go
// Use the defaults:
client := jttp.New()
resp, err := client.Get("https://example.com")

// Tune for your environment:
client := jttp.New(
    jttp.WithTimeout(10 * time.Second),
    jttp.WithRetries(5),
    jttp.WithIdleTimeout(10 * time.Second),
    jttp.WithMaxResponseBodyBytes(100 << 20),
    jttp.WithStrictSSRFProtection(),
)
```

For attacker-controlled URLs, combine strict SSRF protection with direct
dialing so validation is bound to the connection and DNS is resolved only
once per new connection:

```go
client := jttp.New(
    jttp.WithStrictSSRFProtection(),
    jttp.WithNoProxy(),
)
```

With a proxy or custom transport, jttp cannot control the target dial and
retains request-time DNS preflight checks instead. The same is true when
private redirects are explicitly allowed.

See [godoc](https://pkg.go.dev/github.com/jcalabro/jttp) for the full option list.
