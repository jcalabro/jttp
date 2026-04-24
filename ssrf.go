package jttp

import "net"

// isBlockedIP reports whether ip falls into one of the default-blocked ranges:
// loopback, link-local unicast, private (including IPv6 ULA), multicast,
// unspecified ("this network"), or broadcast.
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
	}

	return false
}
