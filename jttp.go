// Package jttp provides a robust HTTP client with reasonable defaults and tunable behavior.
//
// The returned *http.Client is fully standard — callers use client.Do, client.Get, etc.
// Optional retry logic is implemented at the RoundTripper layer with exponential backoff
// and jitter.
//
// Basic usage:
//
//	client := jttp.New() // be sure to reuse this single object across multiple requests!
//	resp, err := client.Get("https://example.com")
//
// With options:
//
//	client := jttp.New(
//	    jttp.WithTimeout(10 * time.Second),
//	    jttp.WithRetries(5),
//	    jttp.WithAdditionalRetryableStatusCodes(500),
//	)
package jttp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Default configuration values. All can be overridden via Option values.
const (
	DefaultTimeout               = 30 * time.Second
	DefaultMaxRedirects          = 5
	DefaultMaxIdleConns          = 20
	DefaultMaxIdleConnsPerHost   = 20
	DefaultMaxConnsPerHost       = 100
	DefaultIdleConnTimeout       = 90 * time.Second
	DefaultTLSHandshakeTimeout   = 5 * time.Second
	DefaultResponseHeaderTimeout = 10 * time.Second
	DefaultDialTimeout           = 5 * time.Second
	DefaultDialKeepAlive         = 30 * time.Second
	DefaultMaxRetries            = 3
	DefaultRetryWaitMin          = 250 * time.Millisecond
	DefaultRetryWaitMax          = 2 * time.Second
	DefaultExpectContinueTimeout = 2 * time.Second
	DefaultMaxRetryBodyBytes     = 4 << 20 // 4 MiB
	DefaultMaxRetryAfter         = 1 * time.Minute
)

type config struct {
	// Client-level
	timeout      time.Duration
	maxRedirects int
	userAgent    string

	// Transport-level
	maxIdleConns           int
	maxIdleConnsPerHost    int
	maxConnsPerHost        int
	idleConnTimeout        time.Duration
	tlsHandshakeTimeout    time.Duration
	responseHeaderTimeout  time.Duration
	maxResponseHeaderBytes int64
	dialTimeout            time.Duration
	dialKeepAlive          time.Duration
	expectContinueTimeout  time.Duration
	disableKeepAlives      bool
	disableCompression     bool
	forceHTTP2             bool
	dialContext            func(ctx context.Context, network, address string) (net.Conn, error)
	resolver               *net.Resolver
	proxy                  func(*http.Request) (*url.URL, error)

	// Retries
	maxRetries           int
	retryWaitMin         time.Duration
	retryWaitMax         time.Duration
	maxRetryAfter        time.Duration
	maxRetryBodyBytes    int64
	retryableStatusCodes map[int]struct{}
	retryableMethods     map[string]struct{}
	checkRetry           func(req *http.Request, resp *http.Response, err error) bool
	retryObserver        func(attempt int, req *http.Request, resp *http.Response, err error)

	// TLS
	tlsConfig *tls.Config

	// Transport escape hatch
	transport http.RoundTripper
}

func defaults() *config {
	return &config{
		timeout:               DefaultTimeout,
		maxRedirects:          DefaultMaxRedirects,
		maxIdleConns:          DefaultMaxIdleConns,
		maxIdleConnsPerHost:   DefaultMaxIdleConnsPerHost,
		maxConnsPerHost:       DefaultMaxConnsPerHost,
		idleConnTimeout:       DefaultIdleConnTimeout,
		tlsHandshakeTimeout:   DefaultTLSHandshakeTimeout,
		responseHeaderTimeout: DefaultResponseHeaderTimeout,
		dialTimeout:           DefaultDialTimeout,
		dialKeepAlive:         DefaultDialKeepAlive,
		expectContinueTimeout: DefaultExpectContinueTimeout,
		forceHTTP2:            true,
		maxRetries:            DefaultMaxRetries,
		retryWaitMin:          DefaultRetryWaitMin,
		retryWaitMax:          DefaultRetryWaitMax,
		maxRetryAfter:         DefaultMaxRetryAfter,
		maxRetryBodyBytes:     DefaultMaxRetryBodyBytes,
		retryableStatusCodes: map[int]struct{}{
			http.StatusTooManyRequests:    {},
			http.StatusBadGateway:         {},
			http.StatusServiceUnavailable: {},
			http.StatusGatewayTimeout:     {},
		},
		retryableMethods: map[string]struct{}{
			http.MethodGet:     {},
			http.MethodHead:    {},
			http.MethodOptions: {},
		},
	}
}

// Option configures the HTTP client.
type Option func(*config)

// New creates a new *http.Client with good defaults.
// All defaults can be overridden via Option values.
// As with all http.Clients, be sure to use the returned
// client across the lifetime of multiple requests.
func New(opts ...Option) *http.Client {
	cfg := defaults()
	for _, opt := range opts {
		opt(cfg)
	}

	cfg.maxRetries = max(cfg.maxRetries, 0)
	cfg.maxRedirects = max(cfg.maxRedirects, 0)
	if cfg.retryWaitMin <= 0 {
		cfg.retryWaitMin = DefaultRetryWaitMin
	}
	if cfg.retryWaitMax <= 0 {
		cfg.retryWaitMax = DefaultRetryWaitMax
	}
	if cfg.retryWaitMin > cfg.retryWaitMax {
		cfg.retryWaitMin, cfg.retryWaitMax = cfg.retryWaitMax, cfg.retryWaitMin
	}
	if cfg.maxRetryAfter <= 0 {
		cfg.maxRetryAfter = DefaultMaxRetryAfter
	}

	// Clamp negative durations for transport-level settings.
	// Zero is valid and means "no limit" for most of these.
	cfg.timeout = max(cfg.timeout, 0)
	cfg.dialTimeout = max(cfg.dialTimeout, 0)
	cfg.dialKeepAlive = max(cfg.dialKeepAlive, 0)
	cfg.idleConnTimeout = max(cfg.idleConnTimeout, 0)
	cfg.tlsHandshakeTimeout = max(cfg.tlsHandshakeTimeout, 0)
	cfg.responseHeaderTimeout = max(cfg.responseHeaderTimeout, 0)
	cfg.expectContinueTimeout = max(cfg.expectContinueTimeout, 0)

	var base http.RoundTripper
	if cfg.transport != nil {
		base = cfg.transport
	} else {
		tlsCfg := cfg.tlsConfig
		if tlsCfg == nil {
			tlsCfg = &tls.Config{}
		} else {
			tlsCfg = tlsCfg.Clone()
		}
		if tlsCfg.MinVersion < tls.VersionTLS12 {
			tlsCfg.MinVersion = tls.VersionTLS12
		}

		// Build the dial function. WithDialContext takes full precedence;
		// otherwise the default dialer is used with an optional custom
		// resolver (WithResolver).
		dialCtx := cfg.dialContext
		if dialCtx == nil {
			dialer := &net.Dialer{
				Timeout:   cfg.dialTimeout,
				KeepAlive: cfg.dialKeepAlive,
				Resolver:  cfg.resolver,
			}
			dialCtx = dialer.DialContext
		}

		proxyFn := http.ProxyFromEnvironment
		if cfg.proxy != nil {
			proxyFn = cfg.proxy
		}

		base = &http.Transport{
			Proxy:                  proxyFn,
			DialContext:            dialCtx,
			MaxIdleConns:           cfg.maxIdleConns,
			MaxIdleConnsPerHost:    cfg.maxIdleConnsPerHost,
			MaxConnsPerHost:        cfg.maxConnsPerHost,
			IdleConnTimeout:        cfg.idleConnTimeout,
			TLSHandshakeTimeout:    cfg.tlsHandshakeTimeout,
			ResponseHeaderTimeout:  cfg.responseHeaderTimeout,
			MaxResponseHeaderBytes: cfg.maxResponseHeaderBytes,
			ExpectContinueTimeout:  cfg.expectContinueTimeout,
			ForceAttemptHTTP2:      cfg.forceHTTP2,
			DisableKeepAlives:      cfg.disableKeepAlives,
			DisableCompression:     cfg.disableCompression,
			TLSClientConfig:        tlsCfg,
		}
	}

	rt := &retryTransport{
		next:              base,
		maxRetries:        cfg.maxRetries,
		waitMin:           cfg.retryWaitMin,
		waitMax:           cfg.retryWaitMax,
		maxRetryAfter:     cfg.maxRetryAfter,
		maxRetryBodyBytes: cfg.maxRetryBodyBytes,
		retryableCodes:    cfg.retryableStatusCodes,
		retryableMethods:  cfg.retryableMethods,
		checkRetry:        cfg.checkRetry,
		retryObserver:     cfg.retryObserver,
		userAgent:         cfg.userAgent,
	}

	client := &http.Client{
		Transport: rt,
		Timeout:   cfg.timeout,
	}

	if cfg.maxRedirects == 0 {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	} else {
		maxRedirects := cfg.maxRedirects
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		}
	}

	return client
}

// WithTimeout sets the overall client timeout (dial + TLS + headers + body).
// Default: 30s.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// WithRedirectPolicy sets the maximum number of redirects to follow.
// Set to 0 to disable redirects. Default: 5.
func WithRedirectPolicy(n int) Option {
	return func(c *config) { c.maxRedirects = n }
}

// WithNoRedirects disables following redirects.
func WithNoRedirects() Option {
	return WithRedirectPolicy(0)
}

// WithUserAgent sets the User-Agent header on requests that don't already have one.
// By default, no User-Agent override is applied (the stdlib default is used).
func WithUserAgent(ua string) Option {
	return func(c *config) { c.userAgent = ua }
}

// WithMaxIdleConns sets the maximum number of idle connections across all hosts.
// Default: 20.
func WithMaxIdleConns(n int) Option {
	return func(c *config) { c.maxIdleConns = n }
}

// WithMaxIdleConnsPerHost sets the maximum number of idle connections per host.
// Default: 20 (stdlib default is 2).
func WithMaxIdleConnsPerHost(n int) Option {
	return func(c *config) { c.maxIdleConnsPerHost = n }
}

// WithMaxConnsPerHost sets the maximum total connections per host.
// 0 means unlimited. Default: 100.
func WithMaxConnsPerHost(n int) Option {
	return func(c *config) { c.maxConnsPerHost = n }
}

// WithIdleConnTimeout sets how long idle connections remain in the pool.
// Default: 90s.
func WithIdleConnTimeout(d time.Duration) Option {
	return func(c *config) { c.idleConnTimeout = d }
}

// WithTLSHandshakeTimeout sets the maximum time for TLS handshakes.
// Default: 5s.
func WithTLSHandshakeTimeout(d time.Duration) Option {
	return func(c *config) { c.tlsHandshakeTimeout = d }
}

// WithResponseHeaderTimeout sets the maximum time to wait for response headers
// after the request is fully written. 0 means no limit. Default: 10s.
func WithResponseHeaderTimeout(d time.Duration) Option {
	return func(c *config) { c.responseHeaderTimeout = d }
}

// WithDialTimeout sets the maximum time to establish a TCP connection.
// Default: 5s (stdlib default is 30s).
func WithDialTimeout(d time.Duration) Option {
	return func(c *config) { c.dialTimeout = d }
}

// WithRetries sets the maximum number of retries. 0 disables retries.
// Default: 3.
func WithRetries(n int) Option {
	return func(c *config) { c.maxRetries = n }
}

// WithNoRetries disables retry logic entirely.
func WithNoRetries() Option {
	return WithRetries(0)
}

// WithRetryWait sets the minimum and maximum wait times between retries.
// Backoff is exponential with full jitter within these bounds.
// Default: 250ms min, 2s max.
func WithRetryWait(minWait, maxWait time.Duration) Option {
	return func(c *config) {
		c.retryWaitMin = minWait
		c.retryWaitMax = maxWait
	}
}

// WithRetryableStatusCodes replaces the default retryable status codes.
// Default: 429, 502, 503, 504.
func WithRetryableStatusCodes(codes ...int) Option {
	return func(c *config) {
		c.retryableStatusCodes = make(map[int]struct{}, len(codes))
		for _, code := range codes {
			c.retryableStatusCodes[code] = struct{}{}
		}
	}
}

// WithAdditionalRetryableStatusCodes adds status codes to the default retryable set
// without replacing it. For example, to also retry on 500:
//
//	jttp.New(jttp.WithAdditionalRetryableStatusCodes(500))
func WithAdditionalRetryableStatusCodes(codes ...int) Option {
	return func(c *config) {
		for _, code := range codes {
			c.retryableStatusCodes[code] = struct{}{}
		}
	}
}

// WithRetryableMethods replaces the default retryable HTTP methods.
// Default: GET, HEAD, OPTIONS.
func WithRetryableMethods(methods ...string) Option {
	return func(c *config) {
		c.retryableMethods = make(map[string]struct{}, len(methods))
		for _, m := range methods {
			c.retryableMethods[m] = struct{}{}
		}
	}
}

// WithAdditionalRetryableMethods adds HTTP methods to the default retryable set
// without replacing it. For example, to also retry POST and PUT:
//
//	jttp.New(jttp.WithAdditionalRetryableMethods("POST", "PUT"))
func WithAdditionalRetryableMethods(methods ...string) Option {
	return func(c *config) {
		for _, m := range methods {
			c.retryableMethods[m] = struct{}{}
		}
	}
}

// WithMaxRetryAfter sets the maximum duration that a server's Retry-After
// header will be respected. If the server requests a longer delay, it will
// be capped at this value. Retry-After values are also floored at the
// minimum retry wait time (see WithRetryWait). Default: 1 minute.
func WithMaxRetryAfter(d time.Duration) Option {
	return func(c *config) { c.maxRetryAfter = d }
}

// WithMaxRetryBodyBytes sets the maximum request body size (in bytes) that will
// be buffered into memory for retry support. Bodies larger than this limit cause
// an error when retries are enabled and the body is not already seekable.
// Set to 0 for no limit. Default: 4 MiB.
func WithMaxRetryBodyBytes(n int64) Option {
	return func(c *config) { c.maxRetryBodyBytes = n }
}

// WithCheckRetry provides a custom function to determine if a request should be retried.
// When set, this overrides the default status-code and error classification logic,
// but the method check still applies first — only methods in the retryable set
// are candidates for retry. Return true to retry, false to stop.
func WithCheckRetry(fn func(req *http.Request, resp *http.Response, err error) bool) Option {
	return func(c *config) { c.checkRetry = fn }
}

// WithRetryObserver registers a callback that is invoked before each retry
// attempt. The attempt number is 0-indexed (0 = first failed attempt that
// will be retried). This is not called on the final exhausted attempt —
// only when a retry will actually follow. This is useful for logging or
// metrics.
func WithRetryObserver(fn func(attempt int, req *http.Request, resp *http.Response, err error)) Option {
	return func(c *config) { c.retryObserver = fn }
}

// WithTransport provides a custom base RoundTripper, bypassing the default
// transport construction. Retry logic is still applied on top.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *config) { c.transport = rt }
}

// WithTLSConfig sets a custom TLS configuration on the default transport.
// A minimum TLS version of 1.2 is enforced regardless of the provided config.
// This option is ignored when WithTransport is used.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(c *config) { c.tlsConfig = cfg }
}

// WithDisableKeepAlives disables HTTP keep-alives, making each request use a
// new connection. Useful for short-lived CLI tools.
func WithDisableKeepAlives() Option {
	return func(c *config) { c.disableKeepAlives = true }
}

// WithDisableCompression disables transparent gzip decompression.
// The client will not add Accept-Encoding: gzip and will not decompress
// responses automatically. This can be useful when Content-Length must match
// the actual body size.
func WithDisableCompression() Option {
	return func(c *config) { c.disableCompression = true }
}

// WithForceHTTP2 controls whether HTTP/2 is attempted when a custom TLS
// config is set. Default: true.
func WithForceHTTP2(force bool) Option {
	return func(c *config) { c.forceHTTP2 = force }
}

// WithExpectContinueTimeout sets the maximum time to wait for a server's
// first response headers after fully writing the request headers if the
// request has an "Expect: 100-continue" header. Default: 2s.
func WithExpectContinueTimeout(d time.Duration) Option {
	return func(c *config) { c.expectContinueTimeout = d }
}

// WithDialKeepAlive sets the TCP keep-alive interval for connections.
// Default: 30s.
func WithDialKeepAlive(d time.Duration) Option {
	return func(c *config) { c.dialKeepAlive = d }
}

// WithDialContext provides a custom function for establishing TCP connections.
// When set, WithDialTimeout, WithDialKeepAlive, and WithResolver are ignored
// since they configure the default dialer that this replaces.
// This option is ignored when WithTransport is used.
func WithDialContext(fn func(ctx context.Context, network, address string) (net.Conn, error)) Option {
	return func(c *config) { c.dialContext = fn }
}

// WithResolver sets a custom DNS resolver on the default dialer.
// This is useful for directing DNS queries to a specific server (e.g., 1.1.1.1)
// without replacing the entire dial function. Example:
//
//	jttp.New(jttp.WithResolver(&net.Resolver{
//	    PreferGo: true,
//	    Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
//	        return (&net.Dialer{}).DialContext(ctx, "udp", "1.1.1.1:53")
//	    },
//	}))
//
// This option is ignored when WithDialContext or WithTransport is used.
func WithResolver(r *net.Resolver) Option {
	return func(c *config) { c.resolver = r }
}

// WithProxy sets a custom proxy function for the transport.
// The default is http.ProxyFromEnvironment. Use WithNoProxy to disable
// proxy support entirely.
// This option is ignored when WithTransport is used.
func WithProxy(fn func(*http.Request) (*url.URL, error)) Option {
	return func(c *config) { c.proxy = fn }
}

// WithNoProxy disables proxy support, making all connections direct.
// This option is ignored when WithTransport is used.
func WithNoProxy() Option {
	return WithProxy(func(*http.Request) (*url.URL, error) { return nil, nil })
}

// WithMaxResponseHeaderBytes sets the maximum number of response bytes that
// the transport will read looking for the header. 0 means no limit.
// This option is ignored when WithTransport is used.
func WithMaxResponseHeaderBytes(n int64) Option {
	return func(c *config) { c.maxResponseHeaderBytes = n }
}
