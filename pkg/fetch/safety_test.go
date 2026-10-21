package fetch

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestCheckURLAcceptsPublicSites(t *testing.T) {
	guard := NewGuard()
	cases := map[string]string{
		"https://www.acme.fr/careers": "https://www.acme.fr/careers",
		"http://acme.fr":              "http://acme.fr",
		"acme.fr/jobs":                "https://acme.fr/jobs",
		"https://acme.fr:8443/x":      "https://acme.fr:8443/x",
		"https://ACME.FR/a/?utm_x=1":  "https://acme.fr/a",
	}
	for input, want := range cases {
		got, err := guard.CheckURL(input)
		if err != nil {
			t.Errorf("CheckURL(%q) refused a public url: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("CheckURL(%q) = %q, want %q", input, got, want)
		}
	}
}

// The table that matters: each of these is a way to ask the engine to fetch something on
// the machine it runs on, or on the network around it.
func TestCheckURLRefusesUnsafeTargets(t *testing.T) {
	guard := NewGuard()
	cases := map[string]string{
		"loopback by name":    "http://localhost/admin",
		"loopback by address": "http://127.0.0.1:80/",
		"loopback shorthand":  "http://127.1/",
		"ipv6 loopback":       "http://[::1]/",
		"v4-mapped loopback":  "http://[::ffff:127.0.0.1]/",
		"private 10/8":        "http://10.0.0.5/",
		"private 172.16/12":   "http://172.16.0.1/",
		"private 192.168/16":  "http://192.168.1.1/",
		"aws/gcp metadata":    "http://169.254.169.254/latest/meta-data/",
		"metadata by name":    "http://metadata.google.internal/",
		"alibaba metadata":    "http://100.100.100.200/",
		"aws ipv6 metadata":   "http://[fd00:ec2::254]/",
		"unique local ipv6":   "http://[fc00::1]/",
		"link-local ipv6":     "http://[fe80::1]/",
		"unspecified":         "http://0.0.0.0/",
		"broadcast":           "http://255.255.255.255/",
		"multicast":           "http://224.0.0.1/",
		"cgnat":               "http://100.64.0.1/",
		"nat64":               "http://[64:ff9b::7f00:1]/",
		"docker host":         "http://host.docker.internal/",
		"kubernetes service":  "http://kubernetes.default.svc/",
		"mdns suffix":         "http://printer.local/",
		"internal suffix":     "http://db.internal/",
		"onion":               "http://x.onion/",
		"ssh port":            "https://acme.fr:22/",
		"postgres port":       "https://acme.fr:5432/",
		"redis port":          "https://acme.fr:6379/",
		"file scheme":         "file:///etc/passwd",
		"gopher scheme":       "gopher://acme.fr/",
		"mail scheme":         "mailto:info@acme.fr",
		"javascript scheme":   "javascript:fetch('/x')",
		"data scheme":         "data:text/html,<b>x</b>",
		"bare name":           "http://intranet/",
		"empty":               "",
	}
	for name, raw := range cases {
		got, err := guard.CheckURL(raw)
		if err == nil {
			t.Errorf("%s: CheckURL(%q) was allowed, returning %q", name, raw, got)
			continue
		}
		if !errors.Is(err, ErrUnsafeURL) {
			t.Errorf("%s: error does not wrap ErrUnsafeURL: %v", name, err)
		}
	}
}

func TestCheckAddr(t *testing.T) {
	guard := NewGuard()
	for _, raw := range []string{"1.1.1.1", "93.184.216.34", "2606:2800:220:1::1"} {
		if err := guard.CheckAddr(netip.MustParseAddr(raw)); err != nil {
			t.Errorf("CheckAddr(%s) refused a public address: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"127.0.0.1", "10.1.2.3", "192.168.0.1", "172.20.0.1", "169.254.169.254",
		"0.0.0.0", "100.64.1.1", "198.18.0.1", "203.0.113.1", "240.0.0.1",
		"::1", "fe80::1", "fc00::1", "2001:db8::1", "fd00:ec2::254", "ff02::1",
		"::ffff:10.0.0.1", "::ffff:127.0.0.1",
	} {
		if err := guard.CheckAddr(netip.MustParseAddr(raw)); err == nil {
			t.Errorf("CheckAddr(%s) allowed a non-public address", raw)
		}
	}
}

// Writing a private address as an IPv4-mapped IPv6 address must not get past the v4 rules.
func TestV4MappedAddressesAreUnmapped(t *testing.T) {
	guard := NewGuard()
	if err := guard.CheckAddr(netip.MustParseAddr("::ffff:192.168.1.1")); err == nil {
		t.Fatal("a v4-mapped private address was allowed")
	}
}

func TestAllowLoopbackIsOptIn(t *testing.T) {
	strict, permissive := NewGuard(), NewGuard()
	permissive.AllowLoopback = true
	addr := netip.MustParseAddr("127.0.0.1")
	if err := strict.CheckAddr(addr); err == nil {
		t.Error("loopback allowed without opting in")
	}
	if err := permissive.CheckAddr(addr); err != nil {
		t.Errorf("AllowLoopback did not permit loopback: %v", err)
	}
	// Opting into loopback must not open the private ranges as well.
	if err := permissive.CheckAddr(netip.MustParseAddr("10.0.0.1")); err == nil {
		t.Error("AllowLoopback also allowed a private address")
	}
}

// A hostname that resolves to a private address must be refused at dial time, which is the
// case a hostname-only check cannot catch.
func TestDialContextRefusesPrivateResolution(t *testing.T) {
	guard := NewGuard()
	guard.LookupIP = lookupReturning("10.0.0.7")

	_, err := guard.DialContext(context.Background(), "tcp", "sneaky.example.fr:443")
	if err == nil {
		t.Fatal("dialled a host that resolves to a private address")
	}
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("error does not wrap ErrUnsafeURL: %v", err)
	}
	if !strings.Contains(err.Error(), "private") {
		t.Errorf("error should name the reason, got: %v", err)
	}
}

// A host that answers with a public address and a private one must not be dialled on the
// private one. This is the multi-answer case a single-address check would miss.
func TestDialContextRefusesPrivateAmongPublic(t *testing.T) {
	guard := NewGuard()
	guard.LookupIP = lookupReturning("10.0.0.7", "127.0.0.1")

	if _, err := guard.DialContext(context.Background(), "tcp", "mixed.example.fr:443"); err == nil {
		t.Fatal("dialled a host whose every address was private")
	}
}

func TestDialContextRefusesDisallowedPort(t *testing.T) {
	guard := NewGuard()
	if _, err := guard.DialContext(context.Background(), "tcp", "acme.fr:22"); err == nil {
		t.Fatal("dialled a non-web port")
	}
}

// lookupReturning builds a resolver stub that answers every query with fixed addresses, so
// the dial-time address check can be tested without a DNS server.
func lookupReturning(ips ...string) func(context.Context, string) ([]netip.Addr, error) {
	addrs := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, netip.MustParseAddr(ip))
	}
	return func(context.Context, string) ([]netip.Addr, error) { return addrs, nil }
}

// A literal address in the host position skips DNS entirely and must still be checked.
func TestDialContextChecksLiteralAddress(t *testing.T) {
	guard := NewGuard()
	for _, address := range []string{
		"10.0.0.1:443", "127.0.0.1:443", "169.254.169.254:80", "[::1]:443",
	} {
		if _, err := guard.DialContext(context.Background(), "tcp", address); err == nil {
			t.Errorf("DialContext(%q) was allowed", address)
		} else if !errors.Is(err, ErrUnsafeURL) {
			t.Errorf("DialContext(%q): error does not wrap ErrUnsafeURL: %v", address, err)
		}
	}
}

func TestTruncateStripsControlCharacters(t *testing.T) {
	got := truncate("http://acme.fr/\r\nX-Injected: 1")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("control characters survived truncate: %q", got)
	}
}

// Shorthand and numeric spellings of 127.0.0.1 must all be refused. These do not parse as
// addresses, but a C resolver happily turns them into loopback.
func TestCheckURLRefusesNumericHostShorthand(t *testing.T) {
	guard := NewGuard()
	for _, raw := range []string{
		"http://127.1/",
		"http://2130706433/",
		"http://0177.0.0.1/",
		"http://10.1/",
		"http://192.168.1/",
	} {
		if got, err := guard.CheckURL(raw); err == nil {
			t.Errorf("CheckURL(%q) was allowed, returning %q", raw, got)
		}
	}
}
