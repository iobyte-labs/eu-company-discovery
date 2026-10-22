package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/fetch"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// timeline records when each request arrived, per host, and how many were in flight at
// once. Those two numbers are the whole point of concurrent enrichment: throughput should
// rise because different sites are visited at the same time, while any single site still
// sees requests spaced by its own rate limit.
type timeline struct {
	mu          sync.Mutex
	byHost      map[string][]time.Time
	inFlight    int
	maxInFlight int
}

func (t *timeline) enter(host string) {
	t.mu.Lock()
	t.byHost[host] = append(t.byHost[host], time.Now())
	t.inFlight++
	if t.inFlight > t.maxInFlight {
		t.maxInFlight = t.inFlight
	}
	t.mu.Unlock()
}

func (t *timeline) leave() {
	t.mu.Lock()
	t.inFlight--
	t.mu.Unlock()
}

// gapsWithin returns the intervals between consecutive requests to any host under one
// registrable domain, in arrival order.
func (t *timeline) gapsWithin(domainSuffix string) []time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	var stamps []time.Time
	for host, times := range t.byHost {
		if len(host) >= len(domainSuffix) && host[len(host)-len(domainSuffix):] == domainSuffix {
			stamps = append(stamps, times...)
		}
	}
	for i := 1; i < len(stamps); i++ { // insertion sort: a handful of entries
		for j := i; j > 0 && stamps[j].Before(stamps[j-1]); j-- {
			stamps[j], stamps[j-1] = stamps[j-1], stamps[j]
		}
	}
	gaps := make([]time.Duration, 0, len(stamps))
	for i := 1; i < len(stamps); i++ {
		gaps = append(gaps, stamps[i].Sub(stamps[i-1]))
	}
	return gaps
}

// manySites serves every host that resolves to it, recording the traffic.
func manySites(t *testing.T, requestsPerMin float64) (*fetch.Fetcher, *timeline, int) {
	t.Helper()
	record := &timeline{byHost: map[string][]time.Time{}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		record.enter(host)
		defer record.leave()
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><a href="mailto:jobs@%s">jobs</a></body></html>`, host)
	}))
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
	guard.LookupIP = func(_ context.Context, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}

	fetcher, err := fetch.New(fetch.Options{
		Guard:  guard,
		Limits: model.CrawlLimits{RequestsPerMin: requestsPerMin},
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	return fetcher, record, port
}

func TestConcurrentEnrichmentVisitsSitesInParallel(t *testing.T) {
	// 600 requests a minute is one every 100 ms per domain — slow enough to measure, fast
	// enough for a test.
	fetcher, record, port := manySites(t, 600)

	// Three registrable domains, two companies on each. Note that every host under
	// example.org — acme1.example.org, acme2.example.org — shares ONE registrable domain,
	// so a fixture built that way would be a single group and would serialise correctly
	// while looking like a concurrency bug.
	var companies []model.Company
	for _, domain := range []string{"example.org", "example.net", "example.com"} {
		for _, prefix := range []string{"one", "two"} {
			host := prefix + "." + domain
			companies = append(companies, model.Company{
				ID: host, Name: host, Country: "de",
				Website: fmt.Sprintf("http://%s:%d", host, port),
			})
		}
	}

	start := time.Now()
	visited := 0
	contacts := 0
	for result := range visitAll(context.Background(), fetcher, companies, 2, 3,
		newDeadline(time.Minute)) {
		visited++
		contacts += len(result.visit.contacts)
	}
	elapsed := time.Since(start)

	if visited != len(companies) {
		t.Errorf("visited %d companies, want %d", visited, len(companies))
	}
	if contacts != len(companies) {
		t.Errorf("found %d contacts, want one per company", contacts)
	}
	// Twelve requests at one per 100 ms on a single queue is ~1.1 s. Three domains visited
	// at once should take roughly a third of that.
	if elapsed > 700*time.Millisecond {
		t.Errorf("six companies across three domains took %s with concurrency 3; "+
			"that is not parallel", elapsed)
	}
	if record.maxInFlight < 2 {
		t.Errorf("at most %d request(s) were ever in flight; nothing ran concurrently",
			record.maxInFlight)
	}
}

func TestConcurrencyDoesNotOutpaceTheRateLimitOnOneDomain(t *testing.T) {
	// The property that must survive concurrency: two companies on different hosts of the
	// SAME registrable domain are one site as far as politeness is concerned, however many
	// workers are running.
	fetcher, record, port := manySites(t, 600) // one request per 100 ms per domain

	// one.example.org and two.example.org are different hosts but one registrable domain,
	// which is the unit the rate limit is keyed on. The third is a separate domain, so it
	// is free to run alongside them.
	companies := []model.Company{
		{ID: "one", Name: "One", Country: "de", Website: fmt.Sprintf("http://one.example.org:%d", port)},
		{ID: "two", Name: "Two", Country: "de", Website: fmt.Sprintf("http://two.example.org:%d", port)},
		{ID: "three", Name: "Three", Country: "de", Website: fmt.Sprintf("http://elsewhere.example.net:%d", port)},
	}

	for result := range visitAll(context.Background(), fetcher, companies, 2, 4,
		newDeadline(time.Minute)) {
		if result.visit.abandoned {
			t.Fatalf("%s was abandoned with a minute of budget left", result.company.Name)
		}
	}

	gaps := record.gapsWithin("example.org")
	if len(gaps) < 3 {
		t.Fatalf("only %d gap(s) recorded for the shared domain; the test did not exercise it",
			len(gaps))
	}
	for i, gap := range gaps {
		if gap < 80*time.Millisecond {
			t.Errorf("requests %d and %d to the shared domain were %s apart, want at least "+
				"the rate limit's 100ms: concurrency outpaced politeness", i, i+1, gap)
		}
	}
}

func TestADeadlinePassedMidVisitAbandonsRatherThanBlames(t *testing.T) {
	// A company still being visited when the clock runs out must be left exactly as it
	// was. Recording a crawl error for it would blame the site for our deadline, and every
	// truncated run would poison the tail of the queue with failures that never happened.
	fetcher, _, port := manySites(t, 60) // one request per second: the deadline will bite

	var companies []model.Company
	for i := 1; i <= 8; i++ {
		host := fmt.Sprintf("slow%d.example.org", i)
		companies = append(companies, model.Company{
			ID: fmt.Sprintf("slow%d", i), Name: host, Country: "de",
			Website: fmt.Sprintf("http://%s:%d", host, port),
		})
	}

	clock := &deadline{limit: 150 * time.Millisecond, start: time.Now(), now: time.Now}
	finished, abandoned := 0, 0
	for result := range visitAll(context.Background(), fetcher, companies, 3, 4, clock) {
		if result.visit.abandoned {
			abandoned++
			if result.visit.status != "" {
				t.Errorf("%s was abandoned but carries status %q", result.company.Name,
					result.visit.status)
			}
			continue
		}
		finished++
		if result.visit.status == model.CrawlError {
			t.Errorf("%s was recorded as an error; the deadline is not the site's fault",
				result.company.Name)
		}
	}
	if finished+abandoned == 0 {
		t.Fatal("no outcomes at all")
	}
	if finished == len(companies) {
		t.Fatal("every company finished inside a 150ms deadline; the test proves nothing")
	}
}
