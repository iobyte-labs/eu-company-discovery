package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"

	eucd "github.com/iobyte-labs/eu-company-discovery"
	eucderrors "github.com/iobyte-labs/eu-company-discovery/pkg/errors"
	"github.com/iobyte-labs/eu-company-discovery/pkg/extract"
	"github.com/iobyte-labs/eu-company-discovery/pkg/fetch"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
	"github.com/iobyte-labs/eu-company-discovery/pkg/store"
)

// deadline stops a run cleanly before a job's time limit instead of being killed by it.
//
// A crawl that is killed loses the page it was reading and, worse, says nothing about how
// far it got. Stopping on our own terms means the companies already visited are written,
// the summary says the run was cut short, and the next run resumes with the
// longest-neglected companies — which is why the enrichment query orders by last_checked_at.
type deadline struct {
	limit time.Duration
	start time.Time
	now   func() time.Time
}

func newDeadline(limit time.Duration) *deadline {
	return &deadline{limit: limit, start: time.Now(), now: time.Now}
}

func (d *deadline) passed() bool {
	return d != nil && d.limit > 0 && d.now().Sub(d.start) > d.limit
}

func (d *deadline) remaining() time.Duration {
	if d == nil || d.limit <= 0 {
		return 0
	}
	left := d.limit - d.now().Sub(d.start)
	if left < 0 {
		return 0
	}
	return left.Round(time.Second)
}

// defaultCacheDir keeps fetched pages out of the repository and out of the database.
func defaultCacheDir() string {
	if fromEnv := os.Getenv("EUCD_CACHE_DIR"); fromEnv != "" {
		return fromEnv
	}
	if base, err := os.UserCacheDir(); err == nil {
		return filepath.Join(base, "eucd", "pages")
	}
	return filepath.Join(".eucd-cache", "pages")
}

func newEnrichCmd(opts *globals) *cobra.Command {
	var (
		country       string
		limit         int
		dryRun        bool
		deadlineFor   time.Duration
		pagesPer      int
		cacheDir      string
		noCache       bool
		requestsPer   float64
		concurrency   int
		recheck       bool
		onlyCompanyID string
	)
	cmd := &cobra.Command{
		Use:   "enrich",
		Short: "Visit companies' own sites and record the contacts they publish",
		Long: "For each company whose website is known and whose contacts are not, enrich " +
			"fetches a few pages of that company's own site and records the functional " +
			"addresses published on them.\n\n" +
			"Pages are tried in the order that matters: a careers or jobs page, a contact " +
			"page, an imprint or legal notice, and the homepage last — which is also what " +
			"links to the others, so room is always kept for it. Nothing is guessed: an " +
			"address is recorded because a page showed it, with the page named, or not at " +
			"all. An address naming an individual is dropped rather than stored.\n\n" +
			"A site that refuses automated access — robots.txt, a login wall, a hard 403 — " +
			"is recorded as blocked and left alone. --deadline stops the run cleanly before " +
			"a CI job's time limit; the next run continues with the companies waiting " +
			"longest.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if pagesPer < 1 {
				return eucderrors.New(eucderrors.KindUsage, "--pages-per-company must be at least 1")
			}
			db, err := openStore(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer db.Close()

			filter := store.CompanyFilter{
				Country:            country,
				WithWebsite:        true,
				WithoutContact:     !recheck,
				OldestCheckedFirst: true,
				Limit:              limit,
			}
			if !recheck {
				// A site already visited is not visited again by default. Re-reading a
				// company that refused us, or that simply publishes nothing, is a request
				// to somebody else's server that buys nothing.
				filter.CrawlStatuses = []model.CrawlStatus{model.CrawlNotAttempted}
			}
			var companies []model.Company
			if onlyCompanyID != "" {
				// Naming one company overrides every filter, including whether it has been
				// visited already: asking for it is the decision.
				one, loadErr := db.GetCompany(cmd.Context(), onlyCompanyID)
				if loadErr != nil {
					return eucderrors.New(eucderrors.KindUsage,
						"no company with id %q; `eucd companies list` shows what is known",
						onlyCompanyID)
				}
				companies = []model.Company{one}
			} else if companies, err = db.ListCompanies(cmd.Context(), filter); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if len(companies) == 0 {
				fmt.Fprintln(out, "No company is waiting for enrichment: every company either "+
					"has a contact, has already been visited, or has no known website.")
				return nil
			}

			effectiveCache := cacheDir
			if noCache {
				effectiveCache = ""
			}
			fetcher, err := fetch.New(fetch.Options{
				UserAgent: fetch.UserAgent(eucd.Version),
				Limits:    model.CrawlLimits{RequestsPerMin: requestsPer},
				CacheDir:  effectiveCache,
				Logger:    newLogger(opts, cmd.ErrOrStderr()),
			})
			if err != nil {
				return eucderrors.Wrap(eucderrors.KindGeneral, err, "build the fetcher")
			}

			clock := newDeadline(deadlineFor)
			summary := enrichSummary{}

			// Workers visit; they never write. Every store write happens here, in the
			// collector, which keeps the single-writer database free of contention, keeps
			// one company's output block together, and leaves one place where a failure is
			// attributed.
			var writeErr error
			for outcome := range visitAll(cmd.Context(), fetcher, companies, pagesPer,
				concurrency, clock) {
				if outcome.visit.abandoned {
					summary.abandoned++
					continue
				}
				summary.visited++
				reportVisit(out, outcome.company, outcome.visit, dryRun)
				summary.add(outcome.visit)

				if dryRun || writeErr != nil {
					continue
				}
				recorded, err := db.RecordEnrichment(cmd.Context(), outcome.company.ID,
					store.Enrichment{
						CareersURL:  outcome.visit.careersURL,
						Contacts:    outcome.visit.contacts,
						CrawlStatus: outcome.visit.status,
						CrawlError:  outcome.visit.statusDetail,
						CheckedAt:   outcome.visit.checkedAt,
					})
				if err != nil {
					// Keep draining rather than returning: the workers are still running,
					// and abandoning them here would leak goroutines and lose the visits
					// already paid for.
					writeErr = eucderrors.Wrap(eucderrors.KindGeneral, err,
						"record enrichment for %q", outcome.company.Name)
					continue
				}
				summary.contactsRecorded += recorded.ContactsRecorded
				summary.contactsRejected += recorded.ContactsRejected
			}
			if writeErr != nil {
				return writeErr
			}
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			if clock.passed() {
				summary.stoppedEarly = true
				fmt.Fprintf(out, "\nStopped after %s to finish inside the run's time budget; "+
					"%d company/companies were not visited. Run enrich again to continue "+
					"with them.\n", deadlineFor, len(companies)-summary.visited)
			}

			summary.print(out, dryRun, clock)
			return nil
		},
	}
	cmd.Flags().StringVar(&country, "country", "", "only companies in this country code")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum companies to visit (0 for no limit)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"fetch and report what would be recorded, writing nothing to the database")
	cmd.Flags().DurationVar(&deadlineFor, "deadline", 0,
		"stop cleanly after this long, e.g. 45m (0 for no limit)")
	cmd.Flags().IntVar(&pagesPer, "pages-per-company", 6,
		"most pages to fetch from one company's site, homepage included")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", defaultCacheDir(),
		"where fetched pages are cached, so a re-run does not refetch (env EUCD_CACHE_DIR)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "do not read or write the page cache")
	cmd.Flags().Float64Var(&requestsPer, "requests-per-minute", fetch.DefaultRequestsPerMin,
		"per-domain request rate")
	cmd.Flags().IntVar(&concurrency, "concurrency", 4,
		"how many companies to visit at once; the per-domain rate limit is unaffected")
	cmd.Flags().BoolVar(&recheck, "recheck", false,
		"also revisit companies that already have a contact or have already been visited")
	cmd.Flags().StringVar(&onlyCompanyID, "company", "", "only this company id")
	return cmd
}

// visit is what one company's site yielded.
type visit struct {
	contacts     []model.Contact
	careersURL   string
	status       model.CrawlStatus
	statusDetail string
	checkedAt    time.Time
	pages        []string
	blocked      []string
	refused      []string
	missing      int
	dropped      int
	offBrand     int
	// abandoned is set when the run was cancelled while this company was in flight. Such a
	// visit is discarded rather than recorded: writing a crawl_error for it would mark a
	// perfectly good site as failed, and every deadline-truncated run would poison the tail
	// of the work queue with failures that never happened.
	abandoned bool
}

// visitCompany fetches a bounded number of pages of one company's own site.
//
// The page order is the policy in [extract.CandidateURLs]: the pages most likely to carry a
// recruitment address first, the homepage last and always reserved, because it is what
// links to the rest. A careers page discovered on a page we fetched is promoted to the
// front of the queue — a link is better evidence than a guessed path.
func visitCompany(
	ctx context.Context, fetcher *fetch.Fetcher, company model.Company, maxPages int,
) visit {
	result := visit{status: model.CrawlNotAttempted, checkedAt: time.Now().UTC()}

	candidates := extract.CandidateURLs(company.Website)
	if len(candidates) == 0 {
		result.status = model.CrawlRefused
		result.statusDetail = fmt.Sprintf("the recorded website %q is not an http(s) url",
			company.Website)
		return result
	}
	// One page of the budget is held back for a link the company itself published. Without
	// it, a site whose careers page is spelled in a way no guessed path covers would be
	// read right up to the homepage that names it and then stopped — having found the link
	// and spent the last request. The homepage keeps its own slot for the same reason: it
	// is what the link comes from.
	queue := reserveHomepage(candidates, maxPages-linkBudget(maxPages))

	seen := map[string]bool{}
	fetched := 0
	for len(queue) > 0 && fetched < maxPages {
		if err := ctx.Err(); err != nil {
			break
		}
		target := queue[0]
		queue = queue[1:]
		if seen[target] {
			continue
		}
		seen[target] = true

		page, err := fetcher.Get(ctx, target)
		fetched++
		if err != nil {
			if result.classify(target, err) {
				break
			}
			continue
		}
		result.pages = append(result.pages, page.URL)
		if result.status != model.CrawlBlocked {
			result.status = model.CrawlOK
		}

		found := extract.FromPage(page, company.Website)
		result.dropped += found.DroppedPersonal
		result.offBrand += found.DroppedOffBrand
		result.contacts = appendContacts(result.contacts, found.Contacts)
		if result.careersURL == "" && len(found.CareersURLs) > 0 {
			result.careersURL = found.CareersURLs[0]
		}
		// Promote discovered careers pages: a link the company published beats a path we
		// guessed.
		for i := len(found.CareersURLs) - 1; i >= 0; i-- {
			if candidate := found.CareersURLs[i]; !seen[candidate] {
				queue = append([]string{candidate}, queue...)
			}
		}
		if hasRecruitmentContact(result.contacts) {
			// What the run was looking for. Reading further pages of somebody's site after
			// that is a request that buys nothing.
			break
		}
	}

	if len(result.contacts) == 0 && result.status == model.CrawlOK {
		result.statusDetail = fmt.Sprintf(
			"read %d page(s); the company publishes no functional contact address", len(result.pages))
	}
	if result.status == model.CrawlNotAttempted {
		// Every candidate failed without any of them being a refusal or a block.
		result.status = model.CrawlError
		if result.statusDetail == "" {
			result.statusDetail = fmt.Sprintf("none of the %d page(s) tried could be read", fetched)
		}
	}
	return result
}

// classify records why one page could not be read, and reports whether to stop visiting
// this company.
//
// The outcomes are different things and are kept apart: a site that refuses us, a URL our
// own rules refused, a page that is simply not there, and a site that did not answer. Only
// the third is worth trying another page after — the others are about the site as a whole,
// and every remaining candidate is on the same site, so carrying on would mean asking a
// server that has already said no, four more times.
func (v *visit) classify(target string, err error) (stop bool) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Our own clock, not the site's doing. Recording this as an error would blame a
		// company for a deadline we set, and the next run would see a failure that never
		// happened. The caller discards an abandoned visit entirely.
		v.abandoned = true
		return true
	case eucderrors.IsBlocked(err):
		reason, _ := eucderrors.BlockedReason(err)
		v.blocked = append(v.blocked, reason)
		v.status = model.CrawlBlocked
		v.statusDetail = reason
		return true
	case errors.Is(err, fetch.ErrUnsafeURL):
		v.refused = append(v.refused, err.Error())
		if v.status != model.CrawlBlocked && v.status != model.CrawlOK {
			v.status = model.CrawlRefused
			v.statusDetail = err.Error()
		}
		return true
	default:
		var status *fetch.StatusError
		if errors.As(err, &status) {
			// The site answered, this page is simply not there. A missing /impressum is
			// ordinary and says nothing about the next candidate.
			v.missing++
			return false
		}
		// No HTTP response at all: a name that does not resolve, a refused connection, a
		// timeout. No other page of the same site will fare better.
		if v.status == model.CrawlNotAttempted {
			v.status = model.CrawlError
			v.statusDetail = fmt.Sprintf("%s: %v", target, err)
		}
		return true
	}
}

// linkBudget is how many of a company's page budget is kept for pages it links to rather
// than paths we guessed. One is enough to reach the careers page a homepage names; a budget
// of one page has nothing to spare.
func linkBudget(maxPages int) int {
	if maxPages < 2 {
		return 0
	}
	return 1
}

// reserveHomepage trims the candidate list to the page budget while keeping the homepage,
// which [extract.CandidateURLs] puts last. Without this, a low --pages-per-company would
// spend every slot on guessed paths and never read the page that links to the right one.
func reserveHomepage(candidates []string, maxPages int) []string {
	if len(candidates) == 0 || maxPages <= 0 {
		return nil
	}
	homepage := candidates[len(candidates)-1]
	rest := candidates[:len(candidates)-1]
	if maxPages == 1 || len(rest) == 0 {
		return []string{homepage}
	}
	if len(rest) > maxPages-1 {
		rest = rest[:maxPages-1]
	}
	return append(append(make([]string, 0, maxPages), rest...), homepage)
}

func appendContacts(existing, found []model.Contact) []model.Contact {
	known := make(map[string]bool, len(existing))
	for _, contact := range existing {
		known[contact.Address] = true
	}
	for _, contact := range found {
		if !known[contact.Address] {
			known[contact.Address] = true
			existing = append(existing, contact)
		}
	}
	return existing
}

func hasRecruitmentContact(contacts []model.Contact) bool {
	for _, contact := range contacts {
		if contact.Kind == model.ContactRecruitment {
			return true
		}
	}
	return false
}

func reportVisit(out io.Writer, company model.Company, result visit, dryRun bool) {
	verb := "recorded"
	if dryRun {
		verb = "WOULD RECORD"
	}
	fmt.Fprintf(out, "\n%s  %s\n", company.Name, company.Website)
	for _, reason := range result.blocked {
		fmt.Fprintf(out, "  BLOCKED %s\n", reason)
	}
	for _, reason := range result.refused {
		fmt.Fprintf(out, "  REFUSED %s\n", reason)
	}
	for _, contact := range result.contacts {
		fmt.Fprintf(out, "  %s %s (%s) from %s\n", verb, contact.Address, contact.Kind,
			contact.FoundOnURL)
	}
	if result.careersURL != "" {
		fmt.Fprintf(out, "  careers page %s\n", result.careersURL)
	}
	if result.dropped > 0 {
		// Counted, never recorded: the engine does not store an individual's address.
		fmt.Fprintf(out, "  %d address(es) naming an individual were dropped\n", result.dropped)
	}
	if result.offBrand > 0 {
		fmt.Fprintf(out, "  %d address(es) belonging to another company were dropped\n",
			result.offBrand)
	}
	if len(result.contacts) == 0 && len(result.blocked) == 0 && len(result.refused) == 0 {
		if result.status == model.CrawlError {
			// Whatever went wrong is recorded against the company, so it has to be visible
			// here too rather than looking like a site that publishes nothing.
			fmt.Fprintf(out, "  COULD NOT READ %s\n", result.statusDetail)
			return
		}
		fmt.Fprintf(out, "  no published contact on %d page(s) read", len(result.pages))
		if result.missing > 0 {
			fmt.Fprintf(out, ", %d page(s) absent", result.missing)
		}
		fmt.Fprintln(out)
	}
}

type enrichSummary struct {
	visited          int
	withContact      int
	blocked          int
	refused          int
	errored          int
	careers          int
	dropped          int
	offBrand         int
	contactsRecorded int
	contactsRejected int
	abandoned        int
	stoppedEarly     bool
}

func (s *enrichSummary) add(result visit) {
	if len(result.contacts) > 0 {
		s.withContact++
	}
	if result.careersURL != "" {
		s.careers++
	}
	s.dropped += result.dropped
	s.offBrand += result.offBrand
	switch result.status {
	case model.CrawlBlocked:
		s.blocked++
	case model.CrawlRefused:
		s.refused++
	case model.CrawlError:
		s.errored++
	}
}

func (s *enrichSummary) print(out io.Writer, dryRun bool, clock *deadline) {
	fmt.Fprintf(out, "\n%d company/companies visited: %d with a published contact, "+
		"%d careers page(s) found\n", s.visited, s.withContact, s.careers)
	if s.blocked > 0 || s.refused > 0 || s.errored > 0 {
		fmt.Fprintf(out, "%d blocked, %d refused before any request, %d could not be read\n",
			s.blocked, s.refused, s.errored)
	}
	if s.dropped > 0 {
		fmt.Fprintf(out, "%d address(es) naming an individual were found and dropped\n", s.dropped)
	}
	if s.offBrand > 0 {
		fmt.Fprintf(out, "%d address(es) belonging to another company were found and dropped\n",
			s.offBrand)
	}
	if s.abandoned > 0 {
		// Said out loud because it is the one number that looks like a failure and is not:
		// these companies are untouched and will be picked up first next time.
		fmt.Fprintf(out, "%d company/companies were in flight when the deadline passed; "+
			"they were left untouched for the next run\n", s.abandoned)
	}
	if dryRun {
		fmt.Fprintln(out, "dry run: nothing was written")
		return
	}
	fmt.Fprintf(out, "%d contact(s) recorded", s.contactsRecorded)
	if s.contactsRejected > 0 {
		fmt.Fprintf(out, ", %d refused by the store", s.contactsRejected)
	}
	fmt.Fprintln(out)
	if !s.stoppedEarly && clock.limit > 0 {
		fmt.Fprintf(out, "%s of the time budget was left\n", clock.remaining())
	}
}

// outcome pairs a company with what visiting its site yielded.
type outcome struct {
	company model.Company
	visit   visit
}

// visitAll visits companies with a bounded number in flight and returns a channel of
// outcomes, closed when every worker has finished.
//
// Work is partitioned by registrable domain, and one worker owns a whole domain. The
// fetcher's rate limit is per domain, so two workers holding two companies on the same
// domain would queue behind each other while each occupied a slot — politeness would be
// preserved and throughput would quietly collapse to one. Partitioning keeps the unit of
// parallelism and the unit of rate limiting the same thing.
func visitAll(
	ctx context.Context, fetcher *fetch.Fetcher, companies []model.Company,
	pagesPerCompany, concurrency int, clock *deadline,
) <-chan outcome {
	if concurrency < 1 {
		concurrency = 1
	}
	groups := groupByDomain(companies)
	if concurrency > len(groups) {
		concurrency = len(groups)
	}

	// Cancelled when the deadline passes, so a request already in flight is abandoned
	// rather than held onto. Without this the run overruns its deadline by however long the
	// slowest in-flight fetch takes, and the steps after it — exporting and saving what was
	// found — may never run at all.
	runCtx, cancel := context.WithCancel(ctx)

	jobs := make(chan []model.Company)
	outcomes := make(chan outcome)

	go func() {
		defer close(jobs)
		for _, group := range groups {
			if clock.passed() || runCtx.Err() != nil {
				return
			}
			select {
			case jobs <- group:
			case <-runCtx.Done():
				return
			}
		}
	}()

	var workers sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for group := range jobs {
				for _, company := range group {
					// The deadline is checked between companies, never inside one, so a
					// site is never left half-read.
					if clock.passed() {
						cancel()
						return
					}
					if runCtx.Err() != nil {
						return
					}
					result := visitCompany(runCtx, fetcher, company, pagesPerCompany)
					if runCtx.Err() != nil && ctx.Err() == nil {
						// Cut short by our own deadline rather than by the operator. The
						// company stays as it was, to be picked up by the next run.
						result = visit{abandoned: true}
					}
					select {
					case outcomes <- outcome{company: company, visit: result}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}

	go func() {
		workers.Wait()
		cancel()
		close(outcomes)
	}()
	return outcomes
}

// groupByDomain gathers each registrable domain's companies together, keeping the order
// they arrived in so that a run capped by a deadline always starts from the same end of the
// queue. A company with no usable domain is its own group.
func groupByDomain(companies []model.Company) [][]model.Company {
	order := make([]string, 0, len(companies))
	byDomain := make(map[string][]model.Company, len(companies))
	for _, company := range companies {
		key := normalize.CanonicalDomain(company.Website)
		if key == "" {
			key = "company:" + company.ID
		}
		if _, seen := byDomain[key]; !seen {
			order = append(order, key)
		}
		byDomain[key] = append(byDomain[key], company)
	}
	groups := make([][]model.Company, 0, len(order))
	for _, key := range order {
		groups = append(groups, byDomain[key])
	}
	return groups
}
