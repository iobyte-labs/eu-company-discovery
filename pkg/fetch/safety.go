// Package fetch makes outbound HTTP requests that are safe to point at a URL the engine
// read off somebody else's web page.
//
// Every URL this engine fetches ultimately comes from untrusted input: a link in an
// association's member directory, an href on a company's own site, a value in a config
// file. A request to such a URL must not be able to reach anything but the public
// internet. This file is the only thing standing between a hostile link and the machine
// the engine runs on, so it is deliberately strict and deliberately boring.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// ErrUnsafeURL is the base for every refusal made before a request goes out. Callers
// record these against a company as [model.CrawlRefused] rather than retrying them.
var ErrUnsafeURL = errors.New("unsafe url")

// blockedPrefixes are address ranges that must never be reached through a URL that came
// from outside. [netip.Addr]'s own predicates cover loopback, private, link-local and
// multicast; these are the ranges it has no method for.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),          // "this host on this network"
	netip.MustParsePrefix("100.64.0.0/10"),      // CGNAT — also Alibaba metadata at 100.100.100.200
	netip.MustParsePrefix("192.0.0.0/24"),       // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),       // documentation
	netip.MustParsePrefix("192.88.99.0/24"),     // deprecated 6to4 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),      // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"),    // documentation
	netip.MustParsePrefix("203.0.113.0/24"),     // documentation
	netip.MustParsePrefix("240.0.0.0/4"),        // reserved
	netip.MustParsePrefix("255.255.255.255/32"), // broadcast
	netip.MustParsePrefix("64:ff9b::/96"),       // NAT64 — maps straight back to IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),     // local-use NAT64
	netip.MustParsePrefix("100::/64"),           // discard-only
	netip.MustParsePrefix("2001::/32"),          // Teredo
	netip.MustParsePrefix("2001:10::/28"),       // ORCHID
	netip.MustParsePrefix("2001:20::/28"),       // ORCHIDv2
	netip.MustParsePrefix("2001:db8::/32"),      // documentation
	netip.MustParsePrefix("2002::/16"),          // 6to4
	netip.MustParsePrefix("fc00::/7"),           // unique local
	netip.MustParsePrefix("fd00:ec2::/32"),      // AWS IMDS over IPv6
}

// blockedHostSuffixes name the local network by DNS rather than by address.
var blockedHostSuffixes = []string{
	".local", ".localhost", ".internal", ".intranet", ".lan", ".home.arpa",
	".in-addr.arpa", ".ip6.arpa", ".onion", ".i2p", ".test", ".invalid", ".example",
}

// blockedHostnames are exact hostnames that only ever mean "this machine" or a metadata
// service. The address checks catch these too; refusing them by name as well means a
// hostile link gets a clear refusal without a DNS lookup first.
var blockedHostnames = map[string]bool{
	"localhost":                true,
	"localhost.localdomain":    true,
	"ip6-localhost":            true,
	"ip6-loopback":             true,
	"metadata":                 true,
	"metadata.google.internal": true,
	"metadata.goog":            true,
	"instance-data":            true,
	"169.254.169.254":          true,
	"metadata.azure.com":       true,
	"kubernetes":               true,
	"kubernetes.default":       true,
	"kubernetes.default.svc":   true,
	"host.docker.internal":     true,
	"gateway.docker.internal":  true,
	"docker.for.mac.localhost": true,
	"docker.for.win.localhost": true,
}

// DefaultAllowedPorts is what a public company website is served on. Anything else is a
// service that was not meant to be browsed, so a link naming it is refused.
var DefaultAllowedPorts = map[int]bool{80: true, 443: true, 8080: true, 8443: true}

// Guard decides whether a URL may be requested, and dials the connection itself so that the
// address it approved is the address actually reached.
//
// The zero value is not usable; call [NewGuard].
type Guard struct {
	// AllowedPorts restricts which ports may be reached. Empty means DefaultAllowedPorts.
	AllowedPorts map[int]bool
	// AllowLoopback permits 127.0.0.0/8 and ::1. Only a test server should ever set this,
	// and only in a test: it is the one switch that disables the protection this file
	// exists to provide.
	AllowLoopback bool
	// LookupIP resolves a hostname to addresses. nil uses the system resolver.
	//
	// It is a function rather than a *net.Resolver so that the dial-time address check can
	// be tested without a DNS server, and so a caller can put a cache in front of
	// resolution without this package knowing about it.
	LookupIP func(ctx context.Context, host string) ([]netip.Addr, error)
	// DialTimeout bounds one TCP connection attempt.
	DialTimeout time.Duration
}

// NewGuard returns a Guard with the engine's defaults.
func NewGuard() *Guard {
	return &Guard{
		AllowedPorts: DefaultAllowedPorts,
		DialTimeout:  10 * time.Second,
	}
}

func (g *Guard) allowedPorts() map[int]bool {
	if len(g.AllowedPorts) == 0 {
		return DefaultAllowedPorts
	}
	return g.AllowedPorts
}

// CheckURL validates a URL and returns its normalised form.
//
// It checks the parts of a URL that can be judged without a network lookup: the scheme,
// the port and the hostname. It deliberately does *not* decide the request is safe —
// a name that passes here can still resolve to a private address, which is why the
// address check happens in [Guard.DialContext] at connection time.
func (g *Guard) CheckURL(raw string) (string, error) {
	canonical := normalize.URL(raw)
	if canonical == "" {
		return "", fmt.Errorf("%w: %q is not an http(s) url", ErrUnsafeURL, truncate(raw))
	}
	parsed, err := url.Parse(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: %q will not parse", ErrUnsafeURL, truncate(raw))
	}

	host := parsed.Hostname()
	if err := g.CheckHostname(host); err != nil {
		return "", err
	}

	port := 443
	if parsed.Scheme == "http" {
		port = 80
	}
	if explicit := parsed.Port(); explicit != "" {
		if port, err = net.LookupPort("tcp", explicit); err != nil {
			return "", fmt.Errorf("%w: %q has an unreadable port", ErrUnsafeURL, host)
		}
	}
	if !g.allowedPorts()[port] {
		return "", fmt.Errorf("%w: port %d is not a public web port", ErrUnsafeURL, port)
	}
	return canonical, nil
}

// CheckHostname refuses hostnames that name the local network, and literal IP addresses
// that fall in a blocked range.
func (g *Guard) CheckHostname(host string) error {
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "."))
	if host == "" {
		return fmt.Errorf("%w: no hostname", ErrUnsafeURL)
	}
	if blockedHostnames[host] {
		return fmt.Errorf("%w: %q names the local machine or a metadata service",
			ErrUnsafeURL, host)
	}
	for _, suffix := range blockedHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return fmt.Errorf("%w: %q is in the private namespace %q", ErrUnsafeURL, host, suffix)
		}
	}
	// A URL may carry a literal address and skip DNS entirely.
	if addr, err := netip.ParseAddr(host); err == nil {
		return g.CheckAddr(addr)
	}
	if !strings.Contains(host, ".") {
		return fmt.Errorf("%w: %q is a bare name, not a public host", ErrUnsafeURL, host)
	}
	// "127.1", "2130706433" and "0177.0.0.1" are all loopback to a C resolver, but none of
	// them parse as an address, so without this they would sail past the check above and be
	// resolved by the system afterwards. Every real top-level domain contains a letter —
	// including IDN ones, which are punycode "xn--…" — so a final label without one is a
	// number written to look like a name.
	lastLabel := host[strings.LastIndex(host, ".")+1:]
	if !strings.ContainsFunc(lastLabel, isASCIILetter) {
		return fmt.Errorf("%w: %q ends in %q, which is not a top-level domain",
			ErrUnsafeURL, host, lastLabel)
	}
	return nil
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// CheckAddr reports whether one resolved address may be connected to.
func (g *Guard) CheckAddr(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrUnsafeURL)
	}
	// An IPv4-mapped IPv6 address ("::ffff:127.0.0.1") must be judged as the IPv4 address
	// it stands for, or every v4 rule below can be sidestepped by writing it in v6.
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	addr = addr.WithZone("")

	if addr.IsLoopback() {
		if g.AllowLoopback {
			return nil
		}
		return fmt.Errorf("%w: %s is loopback", ErrUnsafeURL, addr)
	}
	switch {
	case addr.IsUnspecified():
		return fmt.Errorf("%w: %s is unspecified", ErrUnsafeURL, addr)
	case addr.IsPrivate():
		return fmt.Errorf("%w: %s is a private address", ErrUnsafeURL, addr)
	case addr.IsLinkLocalUnicast():
		// 169.254.169.254 and friends live here: the cloud metadata endpoints.
		return fmt.Errorf("%w: %s is link-local", ErrUnsafeURL, addr)
	case addr.IsLinkLocalMulticast(), addr.IsInterfaceLocalMulticast(), addr.IsMulticast():
		return fmt.Errorf("%w: %s is multicast", ErrUnsafeURL, addr)
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return fmt.Errorf("%w: %s is in the reserved range %s", ErrUnsafeURL, addr, prefix)
		}
	}
	if !addr.IsGlobalUnicast() {
		return fmt.Errorf("%w: %s is not a global unicast address", ErrUnsafeURL, addr)
	}
	return nil
}

// DialContext resolves the host, checks every address it resolves to, and connects to one
// it approved.
//
// Checking a hostname and then handing it to the transport to resolve again would be a
// time-of-check/time-of-use hole: a name can return a public address to the check and a
// private one microseconds later to the real lookup (DNS rebinding). Dialling the verified
// address ourselves closes it, because the address that was approved is the address the
// socket goes to.
//
// Install it on a transport:
//
//	transport := &http.Transport{DialContext: guard.DialContext}
func (g *Guard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not host:port", ErrUnsafeURL, truncate(address))
	}
	portNum, err := net.LookupPort(network, port)
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable port in %q", ErrUnsafeURL, truncate(address))
	}
	if !g.allowedPorts()[portNum] {
		return nil, fmt.Errorf("%w: port %d is not a public web port", ErrUnsafeURL, portNum)
	}
	if err := g.CheckHostname(host); err != nil {
		return nil, err
	}

	addrs, err := g.lookup(ctx, host)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: g.dialTimeout()}
	var refusals []string
	var lastDialErr error
	for _, addr := range addrs {
		if err := g.CheckAddr(addr); err != nil {
			refusals = append(refusals, err.Error())
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastDialErr = err
	}
	if lastDialErr != nil {
		return nil, lastDialErr
	}
	return nil, fmt.Errorf("%w: every address for %q was refused (%s)",
		ErrUnsafeURL, host, strings.Join(refusals, "; "))
}

func (g *Guard) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	lookup := g.LookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	found, err := lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("%w: %q resolved to nothing", ErrUnsafeURL, host)
	}
	return found, nil
}

func (g *Guard) dialTimeout() time.Duration {
	if g.DialTimeout > 0 {
		return g.DialTimeout
	}
	return 10 * time.Second
}

// truncate keeps a hostile URL from filling the log, and keeps control characters out of it.
func truncate(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '.'
		}
		return r
	}, s)
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}
