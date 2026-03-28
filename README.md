# jttp

[![CI](https://github.com/jcalabro/jttp/actions/workflows/ci.yml/badge.svg)](https://github.com/jcalabro/jttp/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/jcalabro/jttp.svg)](https://pkg.go.dev/github.com/jcalabro/jttp)

A robust HTTP client for Go with good defaults and tunable behavior.

`jttp.New()` returns a standard `*http.Client` with sensible timeouts, connection pooling, and retry logic built in.

## Usage

```go
// Use the defaults:
client := jttp.New()
resp, err := client.Get("https://example.com")

// Or tune as needed:
client := jttp.New(
    jttp.WithTimeout(10 * time.Second),
    jttp.WithRetries(5),
    jttp.WithAdditionalRetryableStatusCodes(500),
)
```
