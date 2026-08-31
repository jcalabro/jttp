package jttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

const ipPolicyFallbackDelay = 250 * time.Millisecond

var (
	wellKnownNAT64Prefix = [12]byte{0x00, 0x64, 0xff, 0x9b, 0, 0, 0, 0, 0, 0, 0, 0}
	localNAT64Prefix     = [6]byte{0x00, 0x64, 0xff, 0x9b, 0, 0x01}
)

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// newIPPolicyDialContext returns a dial function that resolves a hostname,
// validates the complete answer set, and passes only literal approved
// addresses to dial. Validation and dialing therefore share one DNS answer;
// the underlying dial function cannot re-resolve the attacker-controlled
// hostname.
func newIPPolicyDialContext(resolver ipLookuper, dial dialContextFunc, staggeredFallback bool) dialContextFunc {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("jttp: parse dial address %q: %w", address, err)
		}

		if ip := net.ParseIP(host); ip != nil {
			if isBlockedIP(ip) {
				return nil, fmt.Errorf("%w: %s", ErrBlockedByIPPolicy, ip)
			}
			return dial(ctx, network, net.JoinHostPort(ip.String(), port))
		}

		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve %s: %w", ErrBlockedByIPPolicy, host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("%w: resolve %s: no addresses", ErrBlockedByIPPolicy, host)
		}

		addresses := make([]string, 0, len(ips))
		for _, ipa := range ips {
			if isBlockedIP(ipa.IP) {
				return nil, fmt.Errorf("%w: %s resolves to %s", ErrBlockedByIPPolicy, host, ipa.IP)
			}
			if !ipMatchesNetwork(ipa.IP, network) {
				continue
			}
			addresses = append(addresses, net.JoinHostPort(ipa.String(), port))
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("jttp: resolve %s: no addresses for network %s", host, network)
		}

		var conn net.Conn
		if staggeredFallback {
			conn, err = dialApprovedAddresses(ctx, network, addresses, dial)
		} else {
			conn, err = dialApprovedAddressesSequentially(ctx, network, addresses, dial)
		}
		if err != nil {
			return nil, fmt.Errorf("jttp: dial %s: %w", host, err)
		}
		return conn, nil
	}
}

// dialApprovedAddressesSequentially avoids imposing concurrency on a
// caller-supplied dial function, whose cancellation behavior jttp cannot
// enforce. The built-in net.Dialer path uses staggered fallback below.
func dialApprovedAddressesSequentially(ctx context.Context, network string, addresses []string, dial dialContextFunc) (net.Conn, error) {
	var errs []error
	for _, address := range addresses {
		conn, err := dial(ctx, network, address)
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			return nil, errors.Join(ctx.Err(), errors.Join(errs...))
		}
	}
	return nil, errors.Join(errs...)
}

func ipMatchesNetwork(ip net.IP, network string) bool {
	switch network {
	case "tcp4":
		return ip.To4() != nil
	case "tcp6":
		return ip.To4() == nil
	default:
		return true
	}
}

type dialResult struct {
	conn net.Conn
	err  error
}

// dialApprovedAddresses preserves multi-address fallback without opening a
// connection to every DNS answer at once. Attempts are staggered; an immediate
// failure starts the next address immediately, while a slow attempt gets a
// bounded head start before the next begins.
func dialApprovedAddresses(ctx context.Context, network string, addresses []string, dial dialContextFunc) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan dialResult, len(addresses))
	start := func(address string) {
		go func() {
			conn, err := dial(ctx, network, address)
			results <- dialResult{conn: conn, err: err}
		}()
	}
	drainOutstanding := func(n int) {
		if n <= 0 {
			return
		}
		go func() {
			for range n {
				result := <-results
				if result.conn != nil {
					_ = result.conn.Close() //nolint:errcheck // best-effort cleanup of a connection that lost the dial race
				}
			}
		}()
	}

	started, completed := 1, 0
	start(addresses[0])
	timer := time.NewTimer(ipPolicyFallbackDelay)
	defer timer.Stop()

	var errs []error
	for completed < len(addresses) {
		select {
		case <-ctx.Done():
			drainOutstanding(started - completed)
			return nil, ctx.Err()
		case result := <-results:
			completed++
			if result.err == nil {
				cancel()
				drainOutstanding(started - completed)
				return result.conn, nil
			}
			errs = append(errs, result.err)
			if started < len(addresses) && completed == started {
				start(addresses[started])
				started++
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(ipPolicyFallbackDelay)
			}
		case <-timer.C:
			if started < len(addresses) {
				start(addresses[started])
				started++
			}
			if started < len(addresses) {
				timer.Reset(ipPolicyFallbackDelay)
			}
		}
	}
	return nil, errors.Join(errs...)
}

// isBlockedIP reports whether ip falls into one of the default-blocked ranges:
// loopback, link-local unicast, private (including IPv6 ULA), multicast,
// unspecified ("this network"), broadcast, RFC 6598 CGNAT, RFC 6052
// well-known NAT64, or RFC 8215 local-use NAT64.
//
// Nil / invalid input is treated as blocked — we fail closed.
//
// IPv4 link-local covers 169.254.0.0/16 including the cloud-metadata IMDS
// address 169.254.169.254. IPv6 ULA covers fc00::/7 including EC2's v6 IMDS
// address fd00:ec2::254.
//
// The classifier does NOT block the deprecated IPv6 site-local range
// (fec0::/10) — it has been reclaimed as public by IANA and treating it
// as private would incorrectly block legitimate public traffic.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	if ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		ip.IsPrivate() {
		return true
	}

	if ip4 := ip.To4(); ip4 != nil {
		// 0.0.0.0/8 "this network" — IsUnspecified covers 0.0.0.0 but not
		// the rest of the /8. RFC 1122 §3.2.1.3 forbids its use as a
		// destination.
		if ip4[0] == 0 {
			return true
		}
		// Limited broadcast.
		if ip4[0] == 255 && ip4[1] == 255 && ip4[2] == 255 && ip4[3] == 255 {
			return true
		}
		// RFC 6598 carrier-grade NAT. Overlay networks commonly use this
		// range, including Tailscale.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
	} else if ip16 := ip.To16(); ip16 != nil {
		// RFC 6052 well-known NAT64.
		if bytes.Equal(ip16[:12], wellKnownNAT64Prefix[:]) {
			return true
		}
		// RFC 8215 local-use NAT64.
		if bytes.Equal(ip16[:6], localNAT64Prefix[:]) {
			return true
		}
	}

	return false
}
