package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// Allowlist restricts which hosts the daemon may talk to. Entries are either
// domain suffixes ("example.com" matches example.com and any subdomain) or
// CIDRs / single IPs ("192.0.2.0/24"). The zero value allows nothing.
type Allowlist struct {
	suffixes []string
	nets     []netip.Prefix
}

// ParseAllowlist parses a comma-separated list of suffixes and CIDRs.
func ParseAllowlist(s string) (Allowlist, error) {
	var a Allowlist
	for _, raw := range strings.Split(s, ",") {
		entry := strings.ToLower(strings.TrimSpace(raw))
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			p, err := netip.ParsePrefix(entry)
			if err != nil {
				return Allowlist{}, fmt.Errorf("invalid CIDR %q: %w", entry, err)
			}
			a.nets = append(a.nets, p.Masked())
			continue
		}
		if ip, err := netip.ParseAddr(entry); err == nil {
			a.nets = append(a.nets, netip.PrefixFrom(ip, ip.BitLen()))
			continue
		}
		suffix := strings.TrimPrefix(entry, ".")
		if suffix == "" || strings.ContainsAny(suffix, " :@") {
			return Allowlist{}, fmt.Errorf("invalid host suffix %q", entry)
		}
		a.suffixes = append(a.suffixes, suffix)
	}
	return a, nil
}

// Empty reports whether the allowlist has no entries.
func (a Allowlist) Empty() bool {
	return len(a.suffixes) == 0 && len(a.nets) == 0
}

// Allows reports whether host (a hostname or an IP literal, without port) is
// permitted. Hostnames are matched against suffixes only; resolved addresses
// are checked against the CIDRs by the HTTP dialer.
func (a Allowlist) Allows(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		for _, n := range a.nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	for _, s := range a.suffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}
