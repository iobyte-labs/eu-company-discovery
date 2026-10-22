package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/fetch"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// testSite starts a local server and makes it answer for a real-looking hostname.
//
// The hostname matters: whether a link is on "the company's own site" is decided by its
// registrable domain, and a loopback address has none — so a test that used 127.0.0.1
// would quietly exercise a different path than a real crawl. The guard's own resolver hook
// maps the name to loopback, and AllowLoopback plus an explicit port are what let the guard
// reach a test server at all. All three are set only in tests.
func testSite(t *testing.T, hostname string, handler http.Handler) (*fetch.Fetcher, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("server url: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("server port: %v", err)
	}

	guard := fetch.NewGuard()
	guard.AllowLoopback = true
	guard.AllowedPorts = map[int]bool{port: true}
	guard.LookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host != hostname {
			return nil, fmt.Errorf("the test resolver was asked for %q", host)
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}

	fetcher, err := fetch.New(fetch.Options{
		Guard: guard,
		// Politeness is tested in pkg/fetch; here it would only make the suite slow.
		Limits: model.CrawlLimits{RequestsPerMin: 100_000},
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	return fetcher, fmt.Sprintf("http://%s:%d", hostname, port)
}

func TestVisitCompanyRefusesAnUnusableWebsite(t *testing.T) {
	fetcher, _ := testSite(t, "acme.example.org", http.NotFoundHandler())

	for _, website := range []string{"mailto:info@acme.example.org", "not a url", ""} {
		company := model.Company{Name: "Acme", Country: "fr", Website: website}
		result := visitCompany(context.Background(), fetcher, company, 3)
		if result.status != model.CrawlRefused {
			t.Errorf("website %q: status = %q, want refused", website, result.status)
		}
		if result.statusDetail == "" {
			t.Errorf("website %q: no reason recorded", website)
		}
	}
}

func TestVisitCompanyRefusesAWebsiteThatNamesThePrivateNetwork(t *testing.T) {
	// A website read off somebody's directory page is untrusted input like any other.
	fetcher, _ := testSite(t, "acme.example.org", http.NotFoundHandler())

	for _, website := range []string{
		"http://169.254.169.254/",
		"http://localhost/",
		"http://10.0.0.5/",
	} {
		company := model.Company{Name: "Acme", Country: "fr", Website: website}
		result := visitCompany(context.Background(), fetcher, company, 3)
		if result.status != model.CrawlRefused {
			t.Errorf("website %q: status = %q, want refused", website, result.status)
		}
		if len(result.refused) == 0 && result.statusDetail == "" {
			t.Errorf("website %q: the refusal was not reported", website)
		}
	}
}

func TestReserveHomepageKeepsTheHomepageWithinThePageBudget(t *testing.T) {
	candidates := []string{"/careers", "/jobs", "/carrieres", "/contact", "/"}

	cases := map[int][]string{
		1: {"/"},
		2: {"/careers", "/"},
		3: {"/careers", "/jobs", "/"},
		9: {"/careers", "/jobs", "/carrieres", "/contact", "/"},
	}
	for budget, want := range cases {
		got := reserveHomepage(candidates, budget)
		if len(got) != len(want) {
			t.Errorf("budget %d: got %v, want %v", budget, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("budget %d: got %v, want %v", budget, got, want)
				break
			}
		}
	}
	if got := reserveHomepage(nil, 5); got != nil {
		t.Errorf("reserveHomepage(nil) = %v, want nil", got)
	}
}

func TestDeadlineStopsOnlyAfterItsLimit(t *testing.T) {
	now := time.Now()
	clock := &deadline{limit: time.Minute, start: now, now: func() time.Time { return now }}
	if clock.passed() {
		t.Error("the deadline passed immediately")
	}
	if clock.remaining() != time.Minute {
		t.Errorf("remaining = %s, want 1m", clock.remaining())
	}

	clock.now = func() time.Time { return now.Add(90 * time.Second) }
	if !clock.passed() {
		t.Error("the deadline did not pass after its limit")
	}
	if clock.remaining() != 0 {
		t.Errorf("remaining = %s, want 0", clock.remaining())
	}

	// No limit means never stopping early, which is what a local run wants.
	var unlimited *deadline
	if unlimited.passed() {
		t.Error("a nil deadline passed")
	}
	if (&deadline{now: time.Now}).passed() {
		t.Error("a zero limit passed")
	}
}
