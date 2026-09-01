package gttp

import (
	"net"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		name    string
		ip      string
		blocked bool
	}{
		// loopback
		{"v4 loopback", "127.0.0.1", true},
		{"v4 loopback high", "127.255.255.255", true},
		{"v6 loopback", "::1", true},

		// link-local (includes IMDS v4)
		{"v4 link-local low", "169.254.0.1", true},
		{"v4 IMDS", "169.254.169.254", true},
		{"v4 link-local high", "169.254.255.255", true},
		{"v6 link-local", "fe80::1", true},

		// private v4
		{"v4 private 10", "10.0.0.1", true},
		{"v4 private 10 high", "10.255.255.255", true},
		{"v4 private 172.16 low", "172.16.0.0", true},
		{"v4 private 172 high", "172.31.255.255", true},
		{"v4 private 192.168", "192.168.1.1", true},

		// private v4 boundaries
		{"v4 just below 172.16", "172.15.255.255", false},
		{"v4 just above 172.31", "172.32.0.0", false},

		// RFC 6598 CGNAT (used by overlay networks such as Tailscale)
		{"v4 just below CGNAT", "100.63.255.255", false},
		{"v4 CGNAT low", "100.64.0.0", true},
		{"v4 CGNAT", "100.100.100.100", true},
		{"v4 CGNAT high", "100.127.255.255", true},
		{"v4 just above CGNAT", "100.128.0.0", false},

		// ULA v6 (includes IMDS v6)
		{"v6 ULA fc00", "fc00::1", true},
		{"v6 ULA fd00", "fd00::1", true},
		{"v6 IMDS ec2", "fd00:ec2::254", true},

		// multicast
		{"v4 multicast low", "224.0.0.1", true},
		{"v4 multicast high", "239.255.255.255", true},
		{"v6 multicast", "ff02::1", true},

		// "this network" / unspecified
		{"v4 this-net", "0.0.0.0", true},
		{"v4 this-net host", "0.1.2.3", true},
		{"v6 unspecified", "::", true},

		// broadcast
		{"v4 broadcast", "255.255.255.255", true},

		// deprecated site-local fec0::/10 — NOT blocked (deprecated, treat as public)
		{"v6 site-local (deprecated)", "fec0::1", false},

		// RFC 6052 well-known NAT64
		{"v6 NAT64 just below prefix", "64:ff9a:ffff::", false},
		{"v6 NAT64 prefix", "64:ff9b::", true},
		{"v6 NAT64 embedded IPv4", "64:ff9b::1.2.3.4", true},
		{"v6 NAT64 prefix high", "64:ff9b::ffff:ffff", true},
		{"v6 NAT64 just above prefix", "64:ff9b:0:0:0:1::", false},

		// RFC 8215 local-use NAT64
		{"v6 local NAT64 just below prefix", "64:ff9b:0:ffff::", false},
		{"v6 local NAT64 prefix", "64:ff9b:1::", true},
		{"v6 local NAT64 embedded IPv4", "64:ff9b:1::1.2.3.4", true},
		{"v6 local NAT64 high", "64:ff9b:1:ffff:ffff:ffff:ffff:ffff", true},
		{"v6 local NAT64 just above prefix", "64:ff9b:2::", false},

		// public
		{"v4 public cloudflare", "1.1.1.1", false},
		{"v4 public google", "8.8.8.8", false},
		{"v6 public cloudflare", "2606:4700:4700::1111", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("bad test setup: invalid IP %q", tt.ip)
			}
			got := isBlockedIP(ip)
			if got != tt.blocked {
				t.Errorf("isBlockedIP(%s) = %v, want %v", tt.ip, got, tt.blocked)
			}
		})
	}
}

func TestIsBlockedIPNil(t *testing.T) {
	// Nil IP (failed resolution) must be treated as blocked — fail closed.
	if !isBlockedIP(nil) {
		t.Error("nil IP must be treated as blocked")
	}
}

func FuzzIsBlockedIP(f *testing.F) {
	f.Add([]byte{127, 0, 0, 1})
	f.Add([]byte{8, 8, 8, 8})
	f.Add(make([]byte, 16))

	f.Fuzz(func(t *testing.T, b []byte) {
		// Only valid lengths are 4 and 16.
		if len(b) != 4 && len(b) != 16 {
			t.Skip()
		}
		ip := net.IP(b)
		// Must not panic for any input.
		_ = isBlockedIP(ip)
	})
}
