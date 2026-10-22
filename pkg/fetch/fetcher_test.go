package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	eucderrors "github.com/iobyte-labs/eu-company-discovery/pkg/errors"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// Every test in this file runs against a local httptest server. Nothing here reaches the
// internet, which is what makes the suite usable in CI on a public repository.
//
// AllowLoopback is the one switch that disables the protection pkg/fetch exists to provide,
// so it is set here and nowhere else — and the port has to be allowed explicitly too,
// because an httptest server listens on a high port that no public website would use.
func testFetcher(t *testing.T, server *httptest.Server, opts Options) *Fetcher {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("test server url %q: %v", server.URL, err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("test server port %q: %v", parsed.Port(), err)
	}
	guard := NewGuard()
	guard.AllowLoopback = true
	guard.AllowedPorts = map[int]bool{port: true}

	if opts.Guard == nil {
		opts.Guard = guard
	}
	if opts.Limits.RequestsPerMin == 0 {
		// Politeness is not what these tests are measuring; the one test that is says so.
		opts.Limits.RequestsPerMin = 100_000
	}
	fetcher, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return fetcher
}

// serve starts a server whose handler is a map of path to handler, with a robots.txt that
// allows everything unless the map overrides it.
func serve(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	if _, ok := routes["/robots.txt"]; !ok {
		mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		})
	}
	for path, handler := range routes {
		mux.HandleFunc(path, handler)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func html(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, body)
	}
}

func TestGetReadsAPageAndIdentifiesItself(t *testing.T) {
	var agent atomic.Value
	server := serve(t, map[string]http.HandlerFunc{
		"/members": func(w http.ResponseWriter, r *http.Request) {
			agent.Store(r.Header.Get("User-Agent"))
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, "<html><body>ok</body></html>")
		},
	})
	fetcher := testFetcher(t, server, Options{UserAgent: UserAgent("1.2.3")})

	page, err := fetcher.Get(context.Background(), server.URL+"/members")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if page.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", page.Status)
	}
	if !strings.Contains(string(page.Body), "ok") {
		t.Errorf("body = %q, want the served document", page.Body)
	}
	if page.FromCache {
		t.Error("a first fetch reports FromCache")
	}
	if page.FetchedAt.IsZero() {
		t.Error("FetchedAt is zero, so no contact read from this page could be dated")
	}

	// An operator who wants to complain, rate-limit or block this engine has to be able to
	// find out what it is.
	sent, _ := agent.Load().(string)
	if !strings.Contains(sent, AgentToken) || !strings.Contains(sent, ContactURL) {
		t.Errorf("User-Agent = %q, want the project token and a contact URL", sent)
	}
}

func TestCacheServesTheSecondFetchWithoutARequest(t *testing.T) {
	var requests atomic.Int64
	server := serve(t, map[string]http.HandlerFunc{
		"/members": func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, "<html><body>members</body></html>")
		},
	})
	fetcher := testFetcher(t, server, Options{CacheDir: t.TempDir()})

	first, err := fetcher.Get(context.Background(), server.URL+"/members")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	// A second fetcher, as a second run would be: the cache is on disk, not in memory.
	second := testFetcher(t, server, Options{CacheDir: cacheDirOf(t, fetcher)})
	page, err := second.Get(context.Background(), server.URL+"/members")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if !page.FromCache {
		t.Error("the second fetch did not come from the cache")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("the page was requested %d times, want 1: a re-run must not refetch", got)
	}
	if string(page.Body) != string(first.Body) {
		t.Errorf("cached body = %q, want %q", page.Body, first.Body)
	}
}

func cacheDirOf(t *testing.T, f *Fetcher) string {
	t.Helper()
	if f.cache == nil {
		t.Fatal("fetcher has no cache")
	}
	return f.cache.dir
}

func TestRobotsDisallowIsBlockedNotAnError(t *testing.T) {
	server := serve(t, map[string]http.HandlerFunc{
		"/robots.txt": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, "User-agent: *\nDisallow: /members\nAllow: /members/public\n")
		},
		"/members":        html("<html>should never be read</html>"),
		"/members/public": html("<html><body>public</body></html>"),
	})
	fetcher := testFetcher(t, server, Options{})

	_, err := fetcher.Get(context.Background(), server.URL+"/members")
	if err == nil {
		t.Fatal("a disallowed path was fetched anyway")
	}
	reason, blocked := eucderrors.BlockedReason(err)
	if !blocked {
		t.Fatalf("err = %v, want a blocked result a caller can report", err)
	}
	// The reason has to name the rule, or a blocked source is a mystery rather than
	// something a human can act on.
	if !strings.Contains(reason, "robots.txt") || !strings.Contains(reason, "/members") {
		t.Errorf("reason = %q, want it to name robots.txt and the disallowed path", reason)
	}

	// Allow beats Disallow on a longer match: a site that disallows /members and allows
	// /members/public means the second one to be readable.
	if _, err := fetcher.Get(context.Background(), server.URL+"/members/public"); err != nil {
		t.Errorf("Allow: /members/public was not honoured: %v", err)
	}
}

func TestRobotsCrawlDelayIsHonoured(t *testing.T) {
	server := serve(t, map[string]http.HandlerFunc{
		"/robots.txt": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, "User-agent: *\nCrawl-delay: 0.4\n")
		},
		"/a": html("<html>a</html>"),
		"/b": html("<html>b</html>"),
	})
	fetcher := testFetcher(t, server, Options{})

	start := time.Now()
	for _, path := range []string{"/a", "/b"} {
		if _, err := fetcher.Get(context.Background(), server.URL+path); err != nil {
			t.Fatalf("Get %s: %v", path, err)
		}
	}
	// Two pages after the robots fetch means at least two gaps of 400ms.
	if elapsed := time.Since(start); elapsed < 700*time.Millisecond {
		t.Errorf("two requests took %s, want at least 700ms: Crawl-delay: 0.4 was not honoured",
			elapsed)
	}
}

func TestRateLimitIsPerRegistrableDomain(t *testing.T) {
	// The bucket key is what the limit is per. A site with many subdomains must not be hit
	// at the full rate once per subdomain.
	cases := map[string]string{
		"https://www.acme.fr/careers":  "acme.fr",
		"https://shop.acme.fr/":        "acme.fr",
		"https://acme.fr/a":            "acme.fr",
		"https://members.assoc.co.uk/": "assoc.co.uk",
		"https://acme.wixsite.com/x":   "acme.wixsite.com",
	}
	for input, want := range cases {
		if got := bucketKey(input); got != want {
			t.Errorf("bucketKey(%q) = %q, want %q", input, got, want)
		}
	}
	// A loopback test server has no registrable domain; it must still get its own bucket
	// rather than sharing one with everything else.
	if got := bucketKey("http://127.0.0.1:8080/a"); got != "127.0.0.1:8080" {
		t.Errorf("bucketKey for a test server = %q, want the host and port", got)
	}
}

func TestRateLimitSlowsRepeatedRequests(t *testing.T) {
	server := serve(t, map[string]http.HandlerFunc{
		"/a": html("<html>a</html>"),
		"/b": html("<html>b</html>"),
	})
	// 120 requests a minute is one every 500ms. robots.txt takes the first token, so two
	// pages after it wait twice.
	fetcher := testFetcher(t, server, Options{
		Limits: model.CrawlLimits{RequestsPerMin: 120},
	})

	start := time.Now()
	for _, path := range []string{"/a", "/b"} {
		if _, err := fetcher.Get(context.Background(), server.URL+path); err != nil {
			t.Fatalf("Get %s: %v", path, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 800*time.Millisecond {
		t.Errorf("two requests took %s, want at least 800ms at 120/min", elapsed)
	}
}

func TestRedirectToAPrivateAddressIsRefused(t *testing.T) {
	// The standard SSRF escalation: a public URL that 302s to the cloud metadata service.
	// Checking only the first URL would walk straight into it.
	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:9/",
		"http://10.0.0.5/admin",
		"http://metadata.google.internal/",
	} {
		server := serve(t, map[string]http.HandlerFunc{
			"/go": func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target, http.StatusFound)
			},
		})
		fetcher := testFetcher(t, server, Options{})

		_, err := fetcher.Get(context.Background(), server.URL+"/go")
		if err == nil {
			t.Fatalf("a redirect to %s was followed", target)
		}
		if !errors.Is(err, ErrUnsafeURL) {
			t.Errorf("redirect to %s: err = %v, want it to wrap ErrUnsafeURL", target, err)
		}
	}
}

func TestRedirectChainIsCapped(t *testing.T) {
	var requests atomic.Int64
	server := serve(t, map[string]http.HandlerFunc{
		"/hop": func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			http.Redirect(w, r, "/hop?n="+strconv.FormatInt(requests.Load(), 10), http.StatusFound)
		},
	})
	fetcher := testFetcher(t, server, Options{MaxRedirects: 3})

	if _, err := fetcher.Get(context.Background(), server.URL+"/hop"); err == nil {
		t.Fatal("an endless redirect chain was followed to the end")
	}
	if got := requests.Load(); got > 4 {
		t.Errorf("the chain made %d requests, want at most 4 with MaxRedirects 3", got)
	}
}

func TestResponseSizeCapRefusesAFloodRatherThanTruncating(t *testing.T) {
	// A page half-read and parsed as if it were whole produces a confident wrong answer, so
	// the cap is a refusal, not a truncation.
	server := serve(t, map[string]http.HandlerFunc{
		"/huge": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			chunk := strings.Repeat("x", 4096)
			for i := 0; i < 64; i++ {
				_, _ = fmt.Fprint(w, chunk)
			}
		},
		"/small": html("<html>small</html>"),
	})
	fetcher := testFetcher(t, server, Options{
		Limits: model.CrawlLimits{MaxResponseSize: 8192},
	})

	_, err := fetcher.Get(context.Background(), server.URL+"/huge")
	if err == nil {
		t.Fatal("a response over the cap was accepted")
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
	if _, err := fetcher.Get(context.Background(), server.URL+"/small"); err != nil {
		t.Errorf("a small page was refused by the cap: %v", err)
	}
}

func TestHardRefusalsAreBlockedAndMissingPagesAreNot(t *testing.T) {
	server := serve(t, map[string]http.HandlerFunc{
		"/forbidden": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		},
		"/throttled": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		},
		"/gone": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	fetcher := testFetcher(t, server, Options{})

	for _, path := range []string{"/forbidden", "/throttled"} {
		_, err := fetcher.Get(context.Background(), server.URL+path)
		if !eucderrors.IsBlocked(err) {
			t.Errorf("%s: err = %v, want a blocked result", path, err)
		}
	}

	// A missing page is ordinary — most companies have no /impressum — and must not be
	// reported as a site that refuses us.
	_, err := fetcher.Get(context.Background(), server.URL+"/gone")
	var status *StatusError
	if !errors.As(err, &status) {
		t.Fatalf("err = %v, want a StatusError", err)
	}
	if status.Status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status.Status)
	}
	if eucderrors.IsBlocked(err) {
		t.Error("a 404 was reported as a site refusing access")
	}
}

func TestARedirectToASignInPageIsBlocked(t *testing.T) {
	server := serve(t, map[string]http.HandlerFunc{
		"/members": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/login?next=/members", http.StatusFound)
		},
		"/login": html("<html><form>sign in</form></html>"),
	})
	fetcher := testFetcher(t, server, Options{})

	_, err := fetcher.Get(context.Background(), server.URL+"/members")
	if !eucderrors.IsBlocked(err) {
		t.Fatalf("err = %v, want a login wall to be reported as blocked", err)
	}
}

func TestUnreadableRobotsTreatsTheSiteAsDisallowed(t *testing.T) {
	// We cannot say what the site permits, and a crawler that cannot tell does not crawl.
	server := serve(t, map[string]http.HandlerFunc{
		"/robots.txt": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"/members": html("<html>members</html>"),
	})
	fetcher := testFetcher(t, server, Options{})

	_, err := fetcher.Get(context.Background(), server.URL+"/members")
	if !eucderrors.IsBlocked(err) {
		t.Fatalf("err = %v, want the site to be treated as disallowed", err)
	}
}

func TestRobotsIsFetchedOncePerRun(t *testing.T) {
	var robotsRequests atomic.Int64
	server := serve(t, map[string]http.HandlerFunc{
		"/robots.txt": func(w http.ResponseWriter, _ *http.Request) {
			robotsRequests.Add(1)
			_, _ = fmt.Fprint(w, "User-agent: *\nDisallow: /private\n")
		},
		"/a": html("<html>a</html>"),
		"/b": html("<html>b</html>"),
	})
	fetcher := testFetcher(t, server, Options{})

	for _, path := range []string{"/a", "/b", "/a"} {
		if _, err := fetcher.Get(context.Background(), server.URL+path); err != nil {
			t.Fatalf("Get %s: %v", path, err)
		}
	}
	if got := robotsRequests.Load(); got != 1 {
		t.Errorf("robots.txt was fetched %d times in one run, want 1", got)
	}
}

func TestAnUnsafeURLIsRefusedBeforeAnyRequest(t *testing.T) {
	server := serve(t, map[string]http.HandlerFunc{})
	fetcher := testFetcher(t, server, Options{})

	for _, raw := range []string{
		"http://localhost/admin",
		"file:///etc/passwd",
		"http://169.254.169.254/",
		"ftp://acme.fr/",
		"not a url",
	} {
		if _, err := fetcher.Get(context.Background(), raw); !errors.Is(err, ErrUnsafeURL) {
			t.Errorf("Get(%q) err = %v, want ErrUnsafeURL", raw, err)
		}
	}
}

func TestGetHonoursACancelledContext(t *testing.T) {
	server := serve(t, map[string]http.HandlerFunc{
		"/slow": func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		},
	})
	fetcher := testFetcher(t, server, Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := fetcher.Get(ctx, server.URL+"/slow"); err == nil {
		t.Fatal("a cancelled fetch returned no error")
	}
}

func TestRobotsIsConsultedBeforeTheCache(t *testing.T) {
	// A cached body is still the site's content. If robots has since disallowed the path,
	// serving the cached copy would honour the letter of "we made no request" while
	// ignoring what the file asks.
	var disallow atomic.Bool
	server := serve(t, map[string]http.HandlerFunc{
		"/robots.txt": func(w http.ResponseWriter, _ *http.Request) {
			if disallow.Load() {
				_, _ = fmt.Fprint(w, "User-agent: *\nDisallow: /members\n")
				return
			}
			http.NotFound(w, nil)
		},
		"/members": html("<html><body>members</body></html>"),
	})
	cacheDir := t.TempDir()

	first := testFetcher(t, server, Options{CacheDir: cacheDir})
	if _, err := first.Get(context.Background(), server.URL+"/members"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	// The page is now cached. The site changes its mind.
	disallow.Store(true)
	second := testFetcher(t, server, Options{CacheDir: cacheDir})
	_, err := second.Get(context.Background(), server.URL+"/members")
	if !eucderrors.IsBlocked(err) {
		t.Fatalf("err = %v, want the now-disallowed path to be refused from the cache too", err)
	}
}
