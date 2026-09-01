package gttp

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

func TestIPPolicyDialResolvesOnceAndDialsValidatedLiteral(t *testing.T) {
	var lookups atomic.Int32
	resolver := resolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		lookups.Add(1)
		if host != "public.example" {
			t.Fatalf("host = %q", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
	})

	var gotAddress string
	dial := newIPPolicyDialContext(resolver, func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" {
			t.Fatalf("network = %q", network)
		}
		gotAddress = address
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}, true)

	conn, err := dial(t.Context(), "tcp", "public.example:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if gotAddress != "203.0.113.10:443" {
		t.Fatalf("dial address = %q", gotAddress)
	}
	if lookups.Load() != 1 {
		t.Fatalf("resolver calls = %d, want 1", lookups.Load())
	}
}

func TestIPPolicyDialFailsClosedOnMixedAnswer(t *testing.T) {
	resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("203.0.113.10")},
			{IP: net.ParseIP("10.0.0.1")},
		}, nil
	})
	var dialed atomic.Bool
	dial := newIPPolicyDialContext(resolver, func(context.Context, string, string) (net.Conn, error) {
		dialed.Store(true)
		return nil, errors.New("unexpected dial")
	}, true)

	_, err := dial(t.Context(), "tcp", "mixed.example:443")
	if !errors.Is(err, ErrBlockedByIPPolicy) {
		t.Fatalf("err = %v, want ErrBlockedByIPPolicy", err)
	}
	if dialed.Load() {
		t.Fatal("dial called for mixed public/private answer")
	}
}

func TestIPPolicyDialDoesNotReResolveRebindingHost(t *testing.T) {
	var lookups atomic.Int32
	resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		if lookups.Add(1) == 1 {
			return []net.IPAddr{{IP: net.ParseIP("203.0.113.20")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	})
	var gotAddress string
	dial := newIPPolicyDialContext(resolver, func(_ context.Context, _ string, address string) (net.Conn, error) {
		gotAddress = address
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}, true)

	conn, err := dial(t.Context(), "tcp", "rebind.example:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if gotAddress != "203.0.113.20:443" {
		t.Fatalf("dial address = %q", gotAddress)
	}
	if lookups.Load() != 1 {
		t.Fatalf("resolver calls = %d, want 1", lookups.Load())
	}
}

func TestIPPolicyDialFallsBackAfterImmediateFailure(t *testing.T) {
	resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("203.0.113.30")},
			{IP: net.ParseIP("203.0.113.31")},
		}, nil
	})
	var mu sync.Mutex
	var attempts []string
	dial := newIPPolicyDialContext(resolver, func(_ context.Context, _ string, address string) (net.Conn, error) {
		mu.Lock()
		attempts = append(attempts, address)
		mu.Unlock()
		if address == "203.0.113.30:443" {
			return nil, errors.New("first failed")
		}
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}, true)

	started := time.Now()
	conn, err := dial(t.Context(), "tcp", "fallback.example:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if time.Since(started) >= ipPolicyFallbackDelay {
		t.Fatal("immediate failure waited for fallback delay")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 {
		t.Fatalf("attempts = %v", attempts)
	}
}

func TestIPPolicyDialCustomDialerFallsBackSequentially(t *testing.T) {
	resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("203.0.113.35")},
			{IP: net.ParseIP("203.0.113.36")},
		}, nil
	})
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	dial := newIPPolicyDialContext(resolver, func(_ context.Context, _ string, address string) (net.Conn, error) {
		if address == "203.0.113.35:443" {
			close(firstStarted)
			<-releaseFirst
			return nil, errors.New("first failed")
		}
		close(secondStarted)
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}, false)

	done := make(chan error, 1)
	go func() {
		conn, err := dial(t.Context(), "tcp", "custom.example:443")
		if conn != nil {
			_ = conn.Close()
		}
		done <- err
	}()
	<-firstStarted
	select {
	case <-secondStarted:
		close(releaseFirst)
		t.Fatal("custom dialer attempts ran concurrently")
	case <-time.After(ipPolicyFallbackDelay + 50*time.Millisecond):
	}
	close(releaseFirst)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestIPPolicyDialClosesLateSuccessfulConnection(t *testing.T) {
	resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("203.0.113.40")},
			{IP: net.ParseIP("203.0.113.41")},
		}, nil
	})
	lateRelease := make(chan struct{})
	lateClosed := make(chan struct{})
	dial := newIPPolicyDialContext(resolver, func(_ context.Context, _ string, address string) (net.Conn, error) {
		client, server := net.Pipe()
		if address == "203.0.113.40:443" {
			<-lateRelease
			go func() {
				defer close(lateClosed)
				buf := make([]byte, 1)
				_, _ = server.Read(buf)
				_ = server.Close()
			}()
			return client, nil
		}
		_ = server.Close()
		return client, nil
	}, true)

	conn, err := dial(t.Context(), "tcp", "fallback.example:443")
	if err != nil {
		t.Fatal(err)
	}
	close(lateRelease)
	_ = conn.Close()
	select {
	case <-lateClosed:
	case <-time.After(time.Second):
		t.Fatal("late successful connection was not closed")
	}
}

func TestIPPolicyDialLiteralPrivateAddressBlocked(t *testing.T) {
	var dialed atomic.Bool
	dial := newIPPolicyDialContext(nil, func(context.Context, string, string) (net.Conn, error) {
		dialed.Store(true)
		return nil, errors.New("unexpected dial")
	}, true)
	_, err := dial(t.Context(), "tcp", "127.0.0.1:80")
	if !errors.Is(err, ErrBlockedByIPPolicy) {
		t.Fatalf("err = %v, want ErrBlockedByIPPolicy", err)
	}
	if dialed.Load() {
		t.Fatal("dial called for private literal")
	}
}

func TestIPPolicyDialLiteralCGNATAndNAT64Blocked(t *testing.T) {
	addresses := []string{
		"100.64.0.1",
		"100.127.255.255",
		"64:ff9b::1.2.3.4",
		"64:ff9b:1::1.2.3.4",
	}
	for _, address := range addresses {
		t.Run(address, func(t *testing.T) {
			var dialed atomic.Bool
			dial := newIPPolicyDialContext(nil, func(context.Context, string, string) (net.Conn, error) {
				dialed.Store(true)
				return nil, errors.New("unexpected dial")
			}, true)
			_, err := dial(t.Context(), "tcp", net.JoinHostPort(address, "443"))
			if !errors.Is(err, ErrBlockedByIPPolicy) {
				t.Fatalf("err = %v, want ErrBlockedByIPPolicy", err)
			}
			if dialed.Load() {
				t.Fatal("dial called for blocked address literal")
			}
		})
	}
}
