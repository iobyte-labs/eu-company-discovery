package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/iobyte-labs/eu-company-discovery/pkg/directory"
	eucderrors "github.com/iobyte-labs/eu-company-discovery/pkg/errors"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// Engine defaults for one request. Every one of them exists to bound what a single hostile
// or broken endpoint can do to a run.
const (
	// DefaultTimeout bounds one request, including reading its body. A hung request must
	// not hold a worker for the length of the job.
	DefaultTimeout = 20 * time.Second
	// DefaultMaxResponseSize is the most we will read from one response. A hostile or
	// broken endpoint can otherwise stream forever.
	DefaultMaxResponseSize int64 = 2 << 20 // 2 MiB
	// DefaultRequestsPerMin is the per-registrable-domain rate. A member directory is
	// somebody's small server, and the engine has no deadline worth being rude for.
	DefaultRequestsPerMin = 10.0
	// DefaultMaxRedirects caps a redirect chain. Each hop is a fresh untrusted URL, and a
	// 302 to a metadata endpoint is the standard SSRF escalation, so the chain is both
	// re-validated per hop and kept short.
	DefaultMaxRedirects = 5
)

// ContactURL is published in the User-Agent so an operator who wants to complain, rate-limit
// or block this engine can find out what it is and who runs it.
const ContactURL = "https://github.com/iobyte-labs/eu-company-discovery"

// AgentToken is the product token robots.txt groups are matched against.
const AgentToken = "eucd"

// UserAgent builds the User-Agent string for a version of the engine.
func UserAgent(version string) string {
	if version == "" {
		version = "dev"
	}
	return fmt.Sprintf("%s/%s (+%s)", AgentToken, version, ContactURL)
}

// Options configures a [Fetcher].
type Options struct {
	// Guard refuses unsafe URLs and dials the addresses it approved. nil builds a default
	// one, which is what every non-test caller wants.
	Guard *Guard
	// UserAgent identifies the engine. Empty uses UserAgent("dev").
	UserAgent string
	// Limits are an association's configured limits. Non-zero fields override the engine
	// defaults above, which is how one slow source gets a slower rate without changing the
	// rate for everything else.
	Limits model.CrawlLimits
	// MaxRedirects overrides DefaultMaxRedirects.
	MaxRedirects int
	// CacheDir enables the on-disk page cache. Empty disables it.
	CacheDir string
	// CacheTTL overrides DefaultCacheTTL. Negative means cached pages never expire.
	CacheTTL time.Duration
	// Logger receives progress. Never a page body, never an address: see the skill
	// .claude/skills/safe-crawling.
	Logger *slog.Logger
	// Now is the clock, for tests.
	Now func() time.Time
}

// Fetcher is the engine's only way out to the network.
//
// It implements [directory.Fetcher], and nothing else in this repository constructs an
// http.Client. That is deliberate: a client built somewhere else would bypass the SSRF
// guard, the per-domain rate limit, the response cap, robots handling and the cache all at
// once, and it would pass review by looking unremarkable.
//
// One Fetcher per run. Its robots and rate-limit state is per instance, so two
// associations crawled in sequence each get their own politeness accounting, and nothing
// leaks between runs.
type Fetcher struct {
	guard *Guard
	// client applies every check. robotsClient fetches robots.txt itself, which must not
	// consult robots.txt to find out whether it may: same transport, same guard, no
	// recursion.
	client       *http.Client
	robotsClient *http.Client

	userAgent    string
	maxBytes     int64
	perMinute    float64
	maxRedirects int
	cache        *cache
	logger       *slog.Logger
	now          func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	robots  map[string]*robotsHost
}

// bucket is one registrable domain's politeness state.
//
// The rate limit is per registrable domain rather than per URL or per host, because a slow
// site with many subdomains would otherwise be hit at the full rate once per subdomain.
type bucket struct {
	limiter *rate.Limiter

	mu    sync.Mutex
	delay time.Duration
	last  time.Time
}

// robotsHost is one scheme://host's robots.txt, fetched at most once per run.
type robotsHost struct {
	once  sync.Once
	rules *robotsRules
}

var _ directory.Fetcher = (*Fetcher)(nil)

// New builds a Fetcher.
func New(opts Options) (*Fetcher, error) {
	guard := opts.Guard
	if guard == nil {
		guard = NewGuard()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	timeout := DefaultTimeout
	if opts.Limits.Timeout > 0 {
		timeout = opts.Limits.Timeout
	}
	maxBytes := DefaultMaxResponseSize
	if opts.Limits.MaxResponseSize > 0 {
		maxBytes = opts.Limits.MaxResponseSize
	}
	perMinute := DefaultRequestsPerMin
	if opts.Limits.RequestsPerMin > 0 {
		perMinute = opts.Limits.RequestsPerMin
	}
	maxRedirects := DefaultMaxRedirects
	if opts.MaxRedirects > 0 {
		maxRedirects = opts.MaxRedirects
	}
	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = UserAgent("dev")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	pageCache, err := newCache(opts.CacheDir, opts.CacheTTL, now)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		DialContext: guard.DialContext,
		// No proxy, ever. A proxy makes the socket go to the proxy's address instead of
		// the one the guard resolved and approved, which hands the whole SSRF protection
		// back to whatever HTTP_PROXY happens to be set to.
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: timeout,
	}

	f := &Fetcher{
		guard:        guard,
		userAgent:    userAgent,
		maxBytes:     maxBytes,
		perMinute:    perMinute,
		maxRedirects: maxRedirects,
		cache:        pageCache,
		logger:       logger,
		now:          now,
		buckets:      map[string]*bucket{},
		robots:       map[string]*robotsHost{},
	}
	f.client = &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: f.checkRedirect,
		// No cookie jar. Carrying cookies between requests is how a crawler ends up
		// holding a session it was never granted.
		Jar: nil,
	}
	f.robotsClient = &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: f.checkRobotsRedirect,
	}
	return f, nil
}

// StatusError is a response that was not a readable page: a 404, a 500, anything that is
// neither a success nor a refusal. Separate from a refusal because the two mean different
// things to a caller — a missing /impressum is ordinary, a 403 is a decision.
type StatusError struct {
	URL    string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d", e.URL, e.Status)
}

// ErrTooLarge is returned when a response exceeds the configured size cap.
var ErrTooLarge = errors.New("response is larger than the configured cap")

// Get fetches rawURL, honouring every control this package exists to apply.
//
// The order is deliberate:
//
//  1. The guard checks the URL before anything is resolved or dialled. A refusal here wraps
//     [ErrUnsafeURL] and a caller records it as model.CrawlRefused rather than retrying it.
//  2. The cache is consulted. A cache hit makes no request at all, so it is also not
//     subject to robots or the rate limit — there is nothing to be polite about.
//  3. robots.txt for the host is fetched (once per run) and consulted. A disallowed path
//     returns a blocked error, which [eucderrors.BlockedReason] reads, and which a caller
//     turns into a directory.Blocked entry. It is never worked around.
//  4. The rate limiter and any Crawl-delay are waited out.
//  5. The request goes out, every redirect hop is re-validated, and the body is read
//     through an io.LimitReader.
func (f *Fetcher) Get(ctx context.Context, rawURL string) (*directory.Page, error) {
	canonical, err := f.guard.CheckURL(rawURL)
	if err != nil {
		return nil, err
	}
	if page, ok := f.cache.get(canonical); ok {
		f.logger.Debug("cache hit", "url", canonical)
		return page, nil
	}
	if err := f.permit(ctx, canonical); err != nil {
		return nil, err
	}
	page, err := f.do(ctx, canonical)
	if err != nil {
		return nil, err
	}
	if err := f.cache.put(canonical, page); err != nil {
		// A cache that cannot be written costs a refetch next time, nothing more.
		f.logger.Warn("could not cache a page", "url", canonical, "error", err.Error())
	}
	return page, nil
}

// permit applies robots.txt and the rate limit to one URL. It is called for the requested
// URL and again for every redirect hop, because a hop is a request to a server that has
// said nothing about us yet.
func (f *Fetcher) permit(ctx context.Context, canonical string) error {
	parsed, err := url.Parse(canonical)
	if err != nil {
		return fmt.Errorf("%w: %q will not parse", ErrUnsafeURL, canonical)
	}
	rules, err := f.robotsFor(ctx, parsed)
	if err != nil {
		return err
	}
	target := parsed.EscapedPath()
	if target == "" {
		target = "/"
	}
	if parsed.RawQuery != "" {
		target += "?" + parsed.RawQuery
	}
	if allowed, pattern := rules.allows(target); !allowed {
		return eucderrors.NewBlocked(
			"robots.txt on %s disallows %q for user-agent %q (rule: Disallow: %s)",
			parsed.Host, target, rules.agentGroup, pattern)
	}
	return f.wait(ctx, canonical, rules.crawlDelay)
}

// bucketKey is the rate-limit bucket a URL belongs to: its registrable domain.
//
// A loopback or bare-IP host has no registrable domain, so it falls back to the host and
// port. That case only arises for a test server, but a nil key would put every such request
// in one bucket and make the limiter look broken.
func bucketKey(canonical string) string {
	if domain := normalize.CanonicalDomain(canonical); domain != "" {
		return domain
	}
	if parsed, err := url.Parse(canonical); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return canonical
}

// wait blocks until this domain's rate limit and any Crawl-delay allow another request.
func (f *Fetcher) wait(ctx context.Context, canonical string, crawlDelay time.Duration) error {
	b := f.bucketFor(canonical, crawlDelay)
	if err := b.limiter.Wait(ctx); err != nil {
		return err
	}

	// Crawl-delay is a minimum gap between requests, so it is enforced against the last
	// request to this domain rather than as a token rate. Holding the lock across the sleep
	// is what serialises two concurrent requests to the same slow site.
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.delay > 0 && !b.last.IsZero() {
		if remaining := b.delay - f.now().Sub(b.last); remaining > 0 {
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	b.last = f.now()
	return nil
}

func (f *Fetcher) bucketFor(canonical string, crawlDelay time.Duration) *bucket {
	key := bucketKey(canonical)
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[key]
	if !ok {
		// Burst 1: the first request to a domain goes immediately, and every one after it
		// waits out the interval. A larger burst would let a run open with a volley.
		b = &bucket{limiter: rate.NewLimiter(rate.Limit(f.perMinute/60), 1)}
		f.buckets[key] = b
	}
	if crawlDelay > b.delay {
		b.delay = crawlDelay
	}
	return b
}

// robotsFor returns the rules for a host, fetching robots.txt at most once per run.
func (f *Fetcher) robotsFor(ctx context.Context, target *url.URL) (*robotsRules, error) {
	key := target.Scheme + "://" + target.Host
	f.mu.Lock()
	host, ok := f.robots[key]
	if !ok {
		host = &robotsHost{}
		f.robots[key] = host
	}
	f.mu.Unlock()

	var fetchErr error
	host.once.Do(func() {
		host.rules, fetchErr = f.loadRobots(ctx, key)
	})
	if fetchErr != nil {
		return nil, fetchErr
	}
	if host.rules == nil {
		// An earlier attempt in this run failed. Whatever it was, we still cannot say what
		// this site permits, and a crawler that cannot tell does not crawl.
		return nil, eucderrors.NewBlocked(
			"robots.txt on %s could not be read earlier in this run, so the site is treated "+
				"as disallowed", target.Host)
	}
	return host.rules, nil
}

// loadRobots fetches and parses one host's robots.txt.
//
// The failure policy is the conservative half of RFC 9309. A 4xx means the site published
// no rules, so everything is allowed. Anything else — a 5xx, a timeout, a body too large to
// be a rules file — leaves us unable to say what the site permits, and a crawler that
// cannot tell does not crawl. That decision is reported as blocked, not retried.
func (f *Fetcher) loadRobots(ctx context.Context, origin string) (*robotsRules, error) {
	robotsURL := origin + "/robots.txt"
	if err := f.wait(ctx, robotsURL, 0); err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrUnsafeURL, robotsURL)
	}
	f.setHeaders(request, "text/plain,*/*;q=0.5")

	response, err := f.robotsClient.Do(request)
	if err != nil {
		// A refusal by the guard is our own decision and must surface as itself, not as a
		// site that blocks us.
		if errors.Is(err, ErrUnsafeURL) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The site did not answer at all: a name that does not resolve, a refused
		// connection, a timeout. That is an error about reaching the site, not a site
		// refusing us, and calling it "blocked" would file a dead domain alongside the
		// directories that genuinely turned us away.
		return nil, fmt.Errorf("robots.txt on %s could not be fetched: %w",
			originHost(origin), err)
	}
	defer response.Body.Close()

	switch {
	case response.StatusCode >= 400 && response.StatusCode < 500:
		// No robots.txt is not a restriction.
		f.logger.Debug("no robots.txt", "origin", origin, "status", response.StatusCode)
		return allowAllRules(), nil
	case response.StatusCode >= 200 && response.StatusCode < 300:
		body, err := readCapped(response.Body, f.maxBytes)
		if err != nil {
			return nil, eucderrors.NewBlocked(
				"robots.txt on %s could not be read (%v), so the site is treated as disallowed",
				originHost(origin), err)
		}
		rules := parseRobots(body, f.userAgent)
		f.logger.Debug("robots.txt read", "origin", origin,
			"rules", len(rules.rules), "crawl_delay", rules.crawlDelay.String())
		return rules, nil
	default:
		return nil, eucderrors.NewBlocked(
			"robots.txt on %s returned HTTP %d, so the site is treated as disallowed",
			originHost(origin), response.StatusCode)
	}
}

func originHost(origin string) string {
	if parsed, err := url.Parse(origin); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return origin
}

// do performs one request and reads a bounded body.
func (f *Fetcher) do(ctx context.Context, canonical string) (*directory.Page, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, canonical, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %q cannot be requested", ErrUnsafeURL, canonical)
	}
	f.setHeaders(request, "text/html,application/xhtml+xml,text/plain;q=0.8,*/*;q=0.5")

	response, err := f.client.Do(request)
	if err != nil {
		// Refusals and refusal-shaped results made during the redirect chain arrive here
		// wrapped in a *url.Error, so they are unwrapped rather than relabelled.
		if errors.Is(err, ErrUnsafeURL) || eucderrors.IsBlocked(err) || ctx.Err() != nil {
			if unwrapped := errors.Unwrap(err); unwrapped != nil {
				return nil, unwrapped
			}
			return nil, err
		}
		return nil, fmt.Errorf("fetch %s: %w", canonical, err)
	}
	defer response.Body.Close()

	finalURL := canonical
	if response.Request != nil && response.Request.URL != nil {
		if normalized := normalize.URL(response.Request.URL.String()); normalized != "" {
			finalURL = normalized
		}
	}

	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden,
		http.StatusProxyAuthRequired, http.StatusTooManyRequests,
		http.StatusUnavailableForLegalReasons:
		// A hard refusal. Reported and left alone: no retry storm, no second attempt
		// pretending to be someone else.
		return nil, eucderrors.NewBlocked("%s returned HTTP %d (%s)",
			finalURL, response.StatusCode, http.StatusText(response.StatusCode))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &StatusError{URL: finalURL, Status: response.StatusCode}
	}
	if reason, gated := loginWall(canonical, finalURL); gated {
		return nil, eucderrors.NewBlocked("%s", reason)
	}
	if response.ContentLength > 0 && response.ContentLength > f.maxBytes {
		return nil, fmt.Errorf("%s: %w (%d bytes declared, cap %d)",
			finalURL, ErrTooLarge, response.ContentLength, f.maxBytes)
	}

	body, err := readCapped(response.Body, f.maxBytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", finalURL, err)
	}
	return &directory.Page{
		URL:          finalURL,
		RequestedURL: canonical,
		Status:       response.StatusCode,
		ContentType:  response.Header.Get("Content-Type"),
		Body:         body,
		FetchedAt:    f.now().UTC(),
	}, nil
}

func (f *Fetcher) setHeaders(request *http.Request, accept string) {
	request.Header.Set("User-Agent", f.userAgent)
	request.Header.Set("Accept", accept)
	request.Header.Set("Accept-Language", "en,fr;q=0.8,de;q=0.8,*;q=0.5")
}

// checkRedirect re-validates every hop of a redirect chain.
//
// CheckURL alone on the first URL is not enough: a 302 to http://169.254.169.254/ is the
// standard way to turn a crawler into a reader of cloud credentials. Each hop is a fresh
// untrusted URL, so each one goes through the guard, through robots for its host, and
// through its domain's rate limit.
func (f *Fetcher) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= f.maxRedirects {
		return fmt.Errorf("%s: stopped after %d redirects", via[0].URL, f.maxRedirects)
	}
	canonical, err := f.guard.CheckURL(request.URL.String())
	if err != nil {
		return err
	}
	return f.permit(request.Context(), canonical)
}

// checkRobotsRedirect validates the hops of a robots.txt fetch. It cannot consult robots to
// decide whether it may read robots, so it applies the guard and the hop cap only.
func (f *Fetcher) checkRobotsRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= f.maxRedirects {
		return fmt.Errorf("%s: stopped after %d redirects", via[0].URL, f.maxRedirects)
	}
	_, err := f.guard.CheckURL(request.URL.String())
	return err
}

// readCapped reads at most maxBytes, and reports a response that wanted to be bigger rather
// than returning a silently truncated page. Half a document parsed as if it were whole is
// worse than a refusal: it would produce a confident wrong answer.
func readCapped(r io.Reader, maxBytes int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%w (cap %d bytes)", ErrTooLarge, maxBytes)
	}
	return body, nil
}

// loginGatePaths are the paths a site redirects to when the page we asked for is behind a
// sign-in. Reaching one is a refusal, not a page: carrying on would record a login form's
// text as a company's description.
var loginGatePaths = []string{
	"/login", "/signin", "/sign-in", "/auth/", "/account/login", "/wp-login.php",
	"/connexion", "/anmelden", "/anmeldung", "/inloggen", "/accedi", "/acceso",
}

// loginWall reports whether a redirect chain ended at a sign-in page that the requested URL
// was not itself asking for.
func loginWall(requested, final string) (string, bool) {
	if requested == final {
		return "", false
	}
	requestedPath, finalPath := pathOf(requested), pathOf(final)
	for _, gate := range loginGatePaths {
		if strings.Contains(finalPath, gate) && !strings.Contains(requestedPath, gate) {
			return fmt.Sprintf("%s redirected to the sign-in page %s; the content is behind a "+
				"login and is left alone", requested, final), true
		}
	}
	return "", false
}

func pathOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.EscapedPath())
}
