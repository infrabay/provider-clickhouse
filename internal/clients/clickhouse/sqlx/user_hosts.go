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

// LocalHostName is how the server stores HOST LOCAL: host_names =
// ['localhost']. It stores every loopback address or prefix (inside
// 127.0.0.0/8, ::1, or IPv4-mapped inside ::ffff:127.0.0.0/104) and every
// host name equal to localhost in any case as HOST LOCAL [verified on 25.3.3,
// 26.8 and Antalya 26.6].
const LocalHostName = "localhost"

// pocoMappedZero is how the server prints an IPv4-mapped address whose IPv4
// part is 0.0.0.0, for example the prefix of ::ffff:0:0/96: without the IPv4
// part ('::ffff:/96'), which net/netip cannot parse.
const pocoMappedZero = "::ffff:"

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
// Zones and non-contiguous masks are errors. Loopback entries are returned in
// this form too; the server stores them as HOST LOCAL (see NormalizeHosts).
func NormalizeHostIP(s string) (string, error) {
	p, err := parseHostIP(s)
	if err != nil {
		return "", err
	}
	return renderPrefix(p), nil
}

// parseHostIP parses a HOST IP entry into its masked prefix.
func parseHostIP(s string) (netip.Prefix, error) {
	addrPart, maskPart, hasMask := strings.Cut(s, "/")
	addr, err := netip.ParseAddr(addrPart)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("host IP %q is not an IP address, CIDR prefix or address with a mask", s)
	}
	if addr.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("host IP %q has a zone, which is not supported", s)
	}
	n := addr.BitLen()
	if hasMask {
		if n, err = maskBits(addr, maskPart); err != nil {
			return netip.Prefix{}, fmt.Errorf("host IP %q: %w", s, err)
		}
	}
	p, err := addr.Prefix(n)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("host IP %q: %w", s, err)
	}
	return p, nil
}

// renderPrefix renders a masked prefix as the server stores it: a /32 or
// /128 prefix as a bare address.
func renderPrefix(p netip.Prefix) string {
	if p.Bits() == p.Addr().BitLen() {
		return p.Addr().String()
	}
	return p.String()
}

// isLoopback reports whether the server stores the masked prefix p as HOST
// LOCAL: IPv4 inside 127.0.0.0/8 (a prefix of at least 8 bits), ::1, or
// IPv4-mapped inside ::ffff:127.0.0.0/104 (at least 104 bits). Wider
// prefixes such as 127.0.0.0/7 or ::1/127 are stored as they are.
func isLoopback(p netip.Prefix) bool {
	a := p.Addr()
	switch {
	case a.Is4():
		return p.Bits() >= 8 && a.As4()[0] == 127
	case a.Is4In6():
		return p.Bits() >= 104 && a.Unmap().As4()[0] == 127
	}
	return p.Bits() == 128 && a == netip.IPv6Loopback()
}

// lossyDisplay reports whether the server prints the masked prefix p without
// the last 24 bits of its address: an IPv4-mapped address, or an
// IPv4-compatible one (the first 96 bits zero, other than ::1), whose first
// IPv4 byte is 0 is printed as '::ffff:' or '::' followed by nothing
// [verified on 25.3.3 and 26.8]. Such an entry could never be compared.
func lossyDisplay(p netip.Prefix) bool {
	a := p.Addr()
	if !a.Is6() {
		return false
	}
	b := a.As16()
	for _, x := range b[:10] {
		if x != 0 {
			return false
		}
	}
	mapped := b[10] == 0xff && b[11] == 0xff
	compatible := b[10] == 0 && b[11] == 0
	return (mapped || compatible) && b[12] == 0 && (b[13] != 0 || b[14] != 0 || b[15] != 0) && a != netip.IPv6Loopback()
}

// hostIPEntry returns a desired HOST IP entry as the server stores it: local
// is set for a loopback entry, which the server stores as HOST LOCAL;
// otherwise ip is the entry in NormalizeHostIP form. An address the server
// cannot print completely is an error.
func hostIPEntry(s string) (ip string, local bool, err error) {
	p, err := parseHostIP(s)
	if err != nil {
		return "", false, err
	}
	if isLoopback(p) {
		return "", true, nil
	}
	if lossyDisplay(p) {
		return "", false, fmt.Errorf("host IP %q: ClickHouse prints IPv4-mapped and IPv4-compatible addresses whose first IPv4 byte is 0 without their last 24 bits, so this entry could never be compared", s)
	}
	return renderPrefix(p), false, nil
}

// isLocalName reports whether the server stores the host name n as HOST
// LOCAL: localhost in any case. The server folds only ASCII letters, so
// 'localhoſt' (U+017F, which Unicode folds to s) is an ordinary host name
// [verified on every supported version]; strings.EqualFold would match it.
func isLocalName(n string) bool { return equalFoldASCII(n, LocalHostName) }

// equalFoldASCII reports whether a and b are equal when ASCII letters are
// compared case-insensitively; every other byte must be equal.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
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
// (hostIP and hostNames) to what the server stores for them: every IP entry
// is normalized with NormalizeHostIP, both lists are sorted and
// deduplicated, and host names are kept exactly (the server compares them
// case-sensitively). Loopback entries and the host name localhost in any
// case become the host name localhost (LocalHostName), as the server stores
// HOST LOCAL. No entries, or a full range (0.0.0.0/0 or ::/0) among the IP
// entries, mean ANY, returned as AnyHostIP. A full range together with host
// names is an error: the server would silently drop the names.
func NormalizeHosts(ips, names []string) (Hosts, error) {
	full, local := false, false
	var out Hosts
	for _, s := range ips {
		n, isLocal, err := hostIPEntry(s)
		if err != nil {
			return Hosts{}, err
		}
		switch {
		case n == ipv4FullRange || n == AnyHostIP:
			full = true
		case isLocal:
			local = true
		default:
			out.IPs = append(out.IPs, n)
		}
	}
	for _, n := range names {
		if n == "" {
			return Hosts{}, errors.New("a host name is empty")
		}
	}
	if full && len(names) > 0 {
		return Hosts{}, errors.New("hostIP 0.0.0.0/0 or ::/0 allows every address and cannot be combined with hostNames: the server would drop the names")
	}
	if full || (len(out.IPs) == 0 && len(names) == 0 && !local) {
		return Hosts{IPs: []string{AnyHostIP}}, nil
	}
	out.IPs = sortedSet(out.IPs)
	out.Names = hostNames(names, local)
	return out, nil
}

// hostNames returns the host names sorted and deduplicated, with localhost
// in any case as LocalHostName, plus LocalHostName when local is set.
func hostNames(names []string, local bool) []string {
	var out []string
	for _, n := range names {
		if isLocalName(n) {
			local = true
			continue
		}
		out = append(out, n)
	}
	if local {
		out = append(out, LocalHostName)
	}
	return sortedSet(out)
}

// ObservedHosts normalizes host_ip and host_names as read from system.users
// for comparison with NormalizeHosts. An IP entry that does not parse is
// kept as is, so that it never compares equal to a desired entry; the
// server's form of an IPv4-mapped address whose IPv4 part is zero
// ('::ffff:', '::ffff:/96') is read as ::ffff:0.0.0.0.
func ObservedHosts(ips, names []string) Hosts {
	local := false
	out := Hosts{}
	for _, s := range ips {
		addr, mask, hasMask := strings.Cut(s, "/")
		if equalFoldASCII(addr, pocoMappedZero) {
			if s = pocoMappedZero + "0.0.0.0"; hasMask {
				s += "/" + mask
			}
		}
		n, isLocal, err := hostIPEntry(s)
		switch {
		case err != nil:
			n = s
		case isLocal:
			local = true
			continue
		case n == ipv4FullRange:
			n = AnyHostIP
		}
		out.IPs = append(out.IPs, n)
	}
	out.IPs = sortedSet(out.IPs)
	out.Names = hostNames(names, local)
	return out
}
