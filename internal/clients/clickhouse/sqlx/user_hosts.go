// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"errors"
	"fmt"
	"math/bits"
	"net/netip"
	"strconv"
	"strings"
)

// ipv4FullRange is the IPv4 full range; the server stores a list with it as
// HOST ANY (AnyHostIP) and drops any host names.
const ipv4FullRange = "0.0.0.0/0"

// NormalizeHostIP returns a HOST IP entry in the form the server stores it:
//
//   - an address, a CIDR prefix, or an address with a mask (dotted IPv4 such
//     as 255.0.0.0, or an IPv6 mask address) that is converted to a prefix
//     length;
//   - masked: 10.1.2.3/8 becomes 10.0.0.0/8;
//   - a /32 or /128 prefix as a bare address: 10.1.2.3/32 becomes 10.1.2.3;
//   - IPv6 compressed: fd00:0::1 becomes fd00::1. IPv4-mapped addresses keep
//     their form (::ffff:192.168.0.1).
//
// Zones and non-contiguous masks are errors.
func NormalizeHostIP(s string) (string, error) {
	addrPart, maskPart, hasMask := strings.Cut(s, "/")
	addr, err := netip.ParseAddr(addrPart)
	if err != nil {
		return "", fmt.Errorf("host IP %q is not an IP address, CIDR prefix or address with a mask", s)
	}
	if addr.Zone() != "" {
		return "", fmt.Errorf("host IP %q has a zone, which is not supported", s)
	}
	n := addr.BitLen()
	if hasMask {
		if n, err = maskBits(addr, maskPart); err != nil {
			return "", fmt.Errorf("host IP %q: %w", s, err)
		}
	}
	p, err := addr.Prefix(n)
	if err != nil {
		return "", fmt.Errorf("host IP %q: %w", s, err)
	}
	if p.Bits() == addr.BitLen() {
		return p.Addr().String(), nil
	}
	return p.String(), nil
}

// maskBits returns the prefix length of a mask written as a decimal prefix
// length or as an address of the same family as addr.
func maskBits(addr netip.Addr, mask string) (int, error) {
	if mask != "" && strings.Trim(mask, "0123456789") == "" {
		n, err := strconv.Atoi(mask)
		if err != nil || n > addr.BitLen() {
			return 0, fmt.Errorf("prefix length %s is out of range", mask)
		}
		return n, nil
	}
	m, err := netip.ParseAddr(mask)
	if err != nil || m.Zone() != "" {
		return 0, fmt.Errorf("mask %q is neither a prefix length nor an address", mask)
	}
	if m.BitLen() != addr.BitLen() {
		return 0, errors.New("the mask and the address are of different families")
	}
	ones, contiguous := 0, true
	for _, b := range m.AsSlice() {
		switch {
		case !contiguous && b != 0:
			return 0, fmt.Errorf("mask %s is not contiguous", mask)
		case b == 0xff:
			ones += 8
		default:
			l := bits.LeadingZeros8(^b)
			if b<<l != 0 {
				return 0, fmt.Errorf("mask %s is not contiguous", mask)
			}
			ones += l
			contiguous = false
		}
	}
	return ones, nil
}

// NormalizeHosts validates and normalizes the desired hosts of a User
// (hostIP and hostNames): every IP entry is normalized with NormalizeHostIP,
// both lists are sorted and deduplicated, and host names are kept exactly
// (the server compares them case-sensitively). No entries, or a full range
// (0.0.0.0/0 or ::/0) among the IP entries, mean ANY, returned as
// AnyHostIP. A full range together with host names is an error: the server
// would silently drop the names.
func NormalizeHosts(ips, names []string) (Hosts, error) {
	full := false
	var out Hosts
	for _, s := range ips {
		n, err := NormalizeHostIP(s)
		if err != nil {
			return Hosts{}, err
		}
		if n == ipv4FullRange || n == AnyHostIP {
			full = true
			continue
		}
		out.IPs = append(out.IPs, n)
	}
	for _, n := range names {
		if n == "" {
			return Hosts{}, errors.New("a host name is empty")
		}
	}
	out.Names = sortedSet(names)
	if full && len(out.Names) > 0 {
		return Hosts{}, errors.New("hostIP 0.0.0.0/0 or ::/0 allows every address and cannot be combined with hostNames: the server would drop the names")
	}
	if full || (len(out.IPs) == 0 && len(out.Names) == 0) {
		return Hosts{IPs: []string{AnyHostIP}}, nil
	}
	out.IPs = sortedSet(out.IPs)
	return out, nil
}

// ObservedHosts normalizes host_ip and host_names as read from system.users
// for comparison with NormalizeHosts. An IP entry that does not parse is
// kept as is, so that it never compares equal to a desired entry.
func ObservedHosts(ips, names []string) Hosts {
	out := Hosts{Names: sortedSet(names)}
	for _, s := range ips {
		n, err := NormalizeHostIP(s)
		if err != nil {
			n = s
		}
		if n == ipv4FullRange {
			n = AnyHostIP
		}
		out.IPs = append(out.IPs, n)
	}
	out.IPs = sortedSet(out.IPs)
	return out
}
