package directory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	eucderrors "github.com/iobyte-labs/eu-company-discovery/pkg/errors"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// stubFetcher serves recorded pages from testdata. An adapter only ever sees a
// [Fetcher], so a stub is the honest way to test one: no network, no sockets, and the
// adapter under test cannot tell the difference.
type stubFetcher struct {
	pages   map[string]string // URL -> file under testdata, or inline HTML when prefixed "<"
	errs    map[string]error
	fetched []string
}

func (s *stubFetcher) Get(_ context.Context, rawURL string) (*Page, error) {
	s.fetched = append(s.fetched, rawURL)
	if err, ok := s.errs[rawURL]; ok {
		return nil, err
	}
	body, ok := s.pages[rawURL]
	if !ok {
		return nil, &notFoundError{rawURL}
	}
	if !strings.HasPrefix(body, "<") {
		raw, err := os.ReadFile(filepath.Join("testdata", body))
		if err != nil {
			return nil, err
		}
		body = string(raw)
	}
	return &Page{
		URL:          rawURL,
		RequestedURL: rawURL,
		Status:       200,
		ContentType:  "text/html",
		Body:         []byte(body),
		FetchedAt:    time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
	}, nil
}

type notFoundError struct{ url string }

func (e *notFoundError) Error() string { return e.url + " returned HTTP 404" }

func pharmaAssociation(settings map[string]any) model.Association {
	return model.Association{
		Slug:         "fr_pharma_directory",
		Name:         "French pharmaceutical industry directory (example)",
		Country:      "fr",
		Sectors:      []string{"pharma"},
		DirectoryURL: "https://assoc.example.org/annuaire/adherents",
		Adapter:      HTMLDirectoryName,
		AccessMethod: model.AccessHTML,
		Settings:     settings,
	}
}

// committedSettings are the settings in config/associations/fr_pharma_directory.yaml. That
// file documents the intended shape of this adapter's configuration, so the adapter has to
// keep working with exactly what it says.
func committedSettings() map[string]any {
	return map[string]any{
		"row_selector": "ul.member-list > li.member",
		"fields": map[string]any{
			"name": "h3.member-name",
			"website": map[string]any{
				"selector":  "a.member-site",
				"attribute": "href",
			},
			"region":      "span.member-region",
			"description": "p.member-summary",
		},
		"pagination": map[string]any{
			"mode":          "next_link",
			"next_selector": "a[rel=next]",
		},
		"fail_on_empty_page": true,
	}
}

func TestHTMLDirectoryFollowsNextLinks(t *testing.T) {
	fetcher := &stubFetcher{pages: map[string]string{
		"https://assoc.example.org/annuaire/adherents":  "members/page1.html",
		"https://assoc.example.org/annuaire/page2.html": "members/page2.html",
	}}
	result, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(committedSettings()),
		Fetcher:     fetcher,
		Limits:      model.CrawlLimits{MaxPages: 25},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if len(fetcher.fetched) != 2 {
		t.Errorf("fetched %v, want both listing pages exactly once", fetcher.fetched)
	}
	// Three usable rows on page one (one row has no name), two on page two.
	wantNames := []string{
		"Exemple Pharma SAS",
		"Exemple Biotech SA",
		"Exemple Dispositifs Médicaux SARL",
		"Exemple Façonnier SAS",
		"Exemple Analyses SA",
	}
	if len(result.Records) != len(wantNames) {
		t.Fatalf("got %d records, want %d: %+v", len(result.Records), len(wantNames), names(result))
	}
	for i, want := range wantNames {
		if result.Records[i].RawName != want {
			t.Errorf("record %d name = %q, want %q", i, result.Records[i].RawName, want)
		}
	}
	if result.Truncated {
		t.Error("Truncated is set, but the directory ended on its own")
	}

	first := result.Records[0]
	if first.Website != "https://exemple-pharma.example.org" {
		t.Errorf("website = %q", first.Website)
	}
	if first.Region != "Île-de-France" {
		t.Errorf("region = %q, want the accented text as published", first.Region)
	}
	if first.SourceURL != "https://assoc.example.org/annuaire/adherents" {
		t.Errorf("source = %q, want the page it was read from", first.SourceURL)
	}
	if first.RetrievedAt.IsZero() {
		t.Error("RetrievedAt is zero, so the record has no provenance in time")
	}
	if len(first.Sectors) != 1 || first.Sectors[0] != "pharma" {
		t.Errorf("sectors = %v, want the association's", first.Sectors)
	}

	// A relative link is resolved against the page it was read from.
	if got := result.Records[1].Website; got != "https://assoc.example.org/redirect?to=exemple-biotech.example.net" {
		t.Errorf("relative website = %q, want it resolved against the listing page", got)
	}
	// A row that matched but had no name, and a website that is not http(s), are reported
	// rather than silently dropped.
	if len(result.Warnings) != 2 {
		t.Errorf("warnings = %v, want one for the nameless row and one for the javascript: link",
			result.Warnings)
	}
	// A record is allowed to be incomplete, and says so.
	if !strings.Contains(result.Records[2].Incomplete, "website") {
		t.Errorf("Incomplete = %q, want it to name the missing website",
			result.Records[2].Incomplete)
	}
}

func names(result Result) []string {
	out := make([]string, 0, len(result.Records))
	for _, record := range result.Records {
		out = append(out, record.RawName)
	}
	return out
}

func TestHTMLDirectoryFailsWhenTheSelectorMatchesNothing(t *testing.T) {
	// The failure this setting exists for: the site was redesigned, the selector is stale,
	// and a run that reported "0 companies, success" would hide it for weeks.
	fetcher := &stubFetcher{pages: map[string]string{
		"https://assoc.example.org/annuaire/adherents": "members/redesigned.html",
	}}
	_, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(committedSettings()),
		Fetcher:     fetcher,
	})
	if err == nil {
		t.Fatal("a listing page with no matching rows reported success")
	}
	if !strings.Contains(err.Error(), "row_selector") {
		t.Errorf("err = %v, want it to name the selector that stopped matching", err)
	}
}

func TestHTMLDirectoryEmptyPageEndsPaginationWhenAllowed(t *testing.T) {
	settings := committedSettings()
	settings["fail_on_empty_page"] = false
	fetcher := &stubFetcher{pages: map[string]string{
		"https://assoc.example.org/annuaire/adherents": "members/redesigned.html",
	}}
	result, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(settings),
		Fetcher:     fetcher,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Records) != 0 {
		t.Errorf("records = %v, want none", names(result))
	}
	// Opting out of the failure must not opt out of being told.
	if len(result.Warnings) == 0 {
		t.Error("an empty listing page produced no warning at all")
	}
}

func TestHTMLDirectoryWalksAPageParameter(t *testing.T) {
	settings := committedSettings()
	settings["pagination"] = map[string]any{"mode": "page_param", "param": "page", "max": 3}
	fetcher := &stubFetcher{pages: map[string]string{
		"https://assoc.example.org/annuaire/adherents":        "members/page1.html",
		"https://assoc.example.org/annuaire/adherents?page=2": "members/page2.html",
		"https://assoc.example.org/annuaire/adherents?page=3": "members/page2.html",
	}}
	result, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(settings),
		Fetcher:     fetcher,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(fetcher.fetched) != 3 {
		t.Errorf("fetched %v, want three pages: max is 3", fetcher.fetched)
	}
	if len(result.Records) != 7 {
		t.Errorf("got %d records, want 7 (3 + 2 + 2)", len(result.Records))
	}
	if result.Truncated {
		t.Error("Truncated is set, but every configured page was read")
	}
}

func TestHTMLDirectoryReportsTruncationAtTheirPageLimit(t *testing.T) {
	fetcher := &stubFetcher{pages: map[string]string{
		"https://assoc.example.org/annuaire/adherents":  "members/page1.html",
		"https://assoc.example.org/annuaire/page2.html": "members/page2.html",
	}}
	result, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(committedSettings()),
		Fetcher:     fetcher,
		Limits:      model.CrawlLimits{MaxPages: 1},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !result.Truncated {
		t.Error("a run stopped by max_pages did not report itself as partial")
	}
	if len(fetcher.fetched) != 1 {
		t.Errorf("fetched %v, want one page", fetcher.fetched)
	}
}

func TestHTMLDirectoryReportsTruncationAtTheRecordLimit(t *testing.T) {
	fetcher := &stubFetcher{pages: map[string]string{
		"https://assoc.example.org/annuaire/adherents":  "members/page1.html",
		"https://assoc.example.org/annuaire/page2.html": "members/page2.html",
	}}
	result, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(committedSettings()),
		Fetcher:     fetcher,
		Limits:      model.CrawlLimits{MaxRecords: 2},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Records) != 2 {
		t.Errorf("got %d records, want 2", len(result.Records))
	}
	if !result.Truncated {
		t.Error("a run capped by max_records did not report itself as partial")
	}
}

func TestHTMLDirectoryReportsABlockedSourceRatherThanFailing(t *testing.T) {
	// A refusal is a result. The records read before it are kept, and the refusal is
	// reported for a human to decide what to do about — never worked around.
	fetcher := &stubFetcher{
		pages: map[string]string{
			"https://assoc.example.org/annuaire/adherents": "members/page1.html",
		},
		errs: map[string]error{
			"https://assoc.example.org/annuaire/page2.html": eucderrors.NewBlocked(
				"robots.txt on assoc.example.org disallows \"/annuaire/page2.html\""),
		},
	}
	result, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(committedSettings()),
		Fetcher:     fetcher,
	})
	if err != nil {
		t.Fatalf("Discover returned an error for a blocked page: %v", err)
	}
	if len(result.Blocked) != 1 {
		t.Fatalf("Blocked = %v, want one entry", result.Blocked)
	}
	if !strings.Contains(result.Blocked[0].Reason, "robots.txt") {
		t.Errorf("reason = %q, want the robots rule", result.Blocked[0].Reason)
	}
	if result.Blocked[0].URL != "https://assoc.example.org/annuaire/page2.html" {
		t.Errorf("blocked URL = %q", result.Blocked[0].URL)
	}
	if len(result.Records) != 3 {
		t.Errorf("got %d records, want the 3 read before the refusal", len(result.Records))
	}
}

func TestHTMLDirectoryFailsWhenTheFirstPageCannotBeRead(t *testing.T) {
	// Nothing could be read at all: the one case an adapter reports as an error rather
	// than as a partial result.
	fetcher := &stubFetcher{}
	_, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(committedSettings()),
		Fetcher:     fetcher,
	})
	if err == nil {
		t.Fatal("a directory whose first page could not be read reported success")
	}
}

func TestHTMLDirectoryWillNotFollowANextLinkOffSite(t *testing.T) {
	fetcher := &stubFetcher{pages: map[string]string{
		"https://assoc.example.org/annuaire/adherents": "members/offsite_next.html",
	}}
	result, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(committedSettings()),
		Fetcher:     fetcher,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(fetcher.fetched) != 1 {
		t.Errorf("fetched %v, want only the directory's own page", fetcher.fetched)
	}
	if len(result.Warnings) == 0 ||
		!strings.Contains(strings.Join(result.Warnings, " "), "different site") {
		t.Errorf("warnings = %v, want one saying the next link left the site", result.Warnings)
	}
}

func TestHTMLDirectoryStopsWhenPaginationLoops(t *testing.T) {
	loop := `<html><body><ul class="member-list"><li class="member">
	    <h3 class="member-name">Exemple Boucle SAS</h3></li></ul>
	    <a rel="next" href="/annuaire/adherents">suivant</a></body></html>`
	fetcher := &stubFetcher{pages: map[string]string{
		"https://assoc.example.org/annuaire/adherents": loop,
	}}
	result, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(committedSettings()),
		Fetcher:     fetcher,
		Limits:      model.CrawlLimits{MaxPages: 25},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(fetcher.fetched) != 1 {
		t.Errorf("fetched %v, want the page read once despite the link pointing back",
			fetcher.fetched)
	}
	if len(result.Records) != 1 {
		t.Errorf("got %d records, want 1", len(result.Records))
	}
}

func TestHTMLDirectoryKeepsUnrecognisedFieldsAsExtra(t *testing.T) {
	settings := committedSettings()
	fields := settings["fields"].(map[string]any)
	fields["membership"] = "span.member-class"
	fetcher := &stubFetcher{pages: map[string]string{
		"https://assoc.example.org/annuaire/adherents": "members/page1.html",
	}}
	result, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(settings),
		Fetcher:     fetcher,
		Limits:      model.CrawlLimits{MaxPages: 1},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := result.Records[0].Extra["membership"]; got != "Membre actif" {
		t.Errorf("Extra[membership] = %q, want the published value", got)
	}
}

func TestHTMLDirectoryRejectsUnusableSettings(t *testing.T) {
	cases := map[string]struct {
		settings map[string]any
		want     string
	}{
		"no row selector": {
			settings: map[string]any{"fields": map[string]any{"name": "h3"}},
			want:     "row_selector",
		},
		"no fields": {
			settings: map[string]any{"row_selector": "li"},
			want:     "fields",
		},
		"no name field": {
			settings: map[string]any{
				"row_selector": "li",
				"fields":       map[string]any{"region": "span"},
			},
			want: "`name:` selector",
		},
		"invalid selector": {
			settings: map[string]any{
				"row_selector": "li[",
				"fields":       map[string]any{"name": "h3"},
			},
			want: "not a CSS selector",
		},
		"unknown pagination mode": {
			settings: map[string]any{
				"row_selector": "li",
				"fields":       map[string]any{"name": "h3"},
				"pagination":   map[string]any{"mode": "scroll"},
			},
			want: "next_link or page_param",
		},
		"page_param without max": {
			settings: map[string]any{
				"row_selector": "li",
				"fields":       map[string]any{"name": "h3"},
				"pagination":   map[string]any{"mode": "page_param", "param": "p"},
			},
			want: "pagination.max is required",
		},
	}
	for name, testCase := range cases {
		_, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
			Association: pharmaAssociation(testCase.settings),
			Fetcher:     &stubFetcher{},
		})
		if err == nil {
			t.Errorf("%s: settings were accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, testCase.want)
		}
	}
}

func TestHTMLDirectoryRefusesToRunWithoutAFetcher(t *testing.T) {
	// A manual source is handed no fetcher, and an adapter must not pretend it can work
	// without one: an adapter cannot escalate past the declared access method.
	_, err := HTMLDirectoryAdapter{}.Discover(context.Background(), Request{
		Association: pharmaAssociation(committedSettings()),
	})
	if err == nil {
		t.Fatal("the adapter ran with no fetcher")
	}
	if !strings.Contains(err.Error(), "fetcher") {
		t.Errorf("err = %v, want it to say a fetcher is needed", err)
	}
}

func TestHTMLDirectoryIsRegistered(t *testing.T) {
	adapter, err := Get(HTMLDirectoryName)
	if err != nil {
		t.Fatalf("Get(%q): %v", HTMLDirectoryName, err)
	}
	if adapter.Name() != HTMLDirectoryName {
		t.Errorf("Name() = %q", adapter.Name())
	}
}
