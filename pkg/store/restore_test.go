package store

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/export"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

var fixedTime = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// populated builds a database with the awkward cases in it, not just the easy ones: a
// company with no website (so its id comes from its name, not its domain), one with several
// contacts and sources, one carrying crawl state from an enrichment pass, and one with
// nothing but a name.
func populated(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	store := newTestStore(t)

	full := company("Exemple Pharma SAS", "fr", "https://exemple-pharma.example.org")
	full.Region = "Île-de-France"
	full.Description = "Fabrication de formes sèches."
	full.Keywords = []string{"sterile", "qc"}
	full.Sectors = []string{"pharma"}
	full.Languages = []string{"fr", "en"}
	full.CareersURL = "https://exemple-pharma.example.org/carrieres"
	full.Sources = []model.Source{
		{AssociationSlug: "fr_pharma", SourceURL: "https://assoc.example.org/members", RawName: "Exemple Pharma SAS", RetrievedAt: fixedTime},
		{AssociationSlug: "fr_other", SourceURL: "https://other.example.net/list", RawName: "Exemple Pharma", RetrievedAt: fixedTime},
	}
	full.Contacts = []model.Contact{
		publishedContact("recrutement@exemple-pharma.example.org", model.ContactRecruitment, "https://exemple-pharma.example.org/contact"),
		publishedContact("contact@exemple-pharma.example.org", model.ContactGeneral, "https://exemple-pharma.example.org/contact"),
	}
	mustUpsert(t, store, full)

	// No website: its identity, and therefore its id, is its name within its country.
	nameOnly := company("Exemple Sans Site SARL", "fr", "")
	nameOnly.Description = "Usinage de précision."
	mustUpsert(t, store, nameOnly)

	enriched := company("Beispiel Pharma GmbH", "de", "https://beispiel-pharma.example.net")
	mustUpsert(t, store, enriched)
	loaded, err := store.ListCompanies(ctx, CompanyFilter{Country: "de"})
	if err != nil || len(loaded) != 1 {
		t.Fatalf("list: %v (%d rows)", err, len(loaded))
	}
	if _, err := store.RecordEnrichment(ctx, loaded[0].ID, Enrichment{
		CareersURL:  "https://beispiel-pharma.example.net/karriere",
		Contacts:    []model.Contact{publishedContact("bewerbung@beispiel-pharma.example.net", model.ContactRecruitment, "https://beispiel-pharma.example.net/impressum")},
		CrawlStatus: model.CrawlOK,
		CheckedAt:   fixedTime,
	}); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	// A site that refused us. Its crawl state has to survive a restore, or the next run
	// would treat it as unvisited and knock on the same door again.
	blocked := company("Exemple Fermé SA", "fr", "https://exemple-ferme.example.com")
	mustUpsert(t, store, blocked)
	all, err := store.ListCompanies(ctx, CompanyFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, c := range all {
		if c.Name == "Exemple Fermé SA" {
			if _, err := store.RecordEnrichment(ctx, c.ID, Enrichment{
				CrawlStatus: model.CrawlBlocked,
				CrawlError:  "robots.txt on exemple-ferme.example.com disallows \"/\"",
				CheckedAt:   fixedTime,
			}); err != nil {
				t.Fatalf("record blocked: %v", err)
			}
		}
	}
	return store
}

// dump exports every company as the engine would write it to disk, with the envelope's
// volatile fields pinned so two dumps are comparable byte for byte.
func dump(t *testing.T, store *Store) string {
	t.Helper()
	companies, err := store.ListCompanies(context.Background(), CompanyFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var buffer bytes.Buffer
	if err := export.Write(&buffer, export.Envelope{
		GeneratedAt: fixedTime, Generator: "test", Companies: companies,
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	return buffer.String()
}

// The guarantee the scheduled pipeline rests on: the committed JSON is enough to rebuild
// the database, so the database itself never has to be committed. If this fails, a run that
// loses its cache re-crawls every company it has ever seen.
func TestRestoreRoundTripIsLossless(t *testing.T) {
	original := dump(t, populated(t))
	// The fixture has to contain the things that are hard to carry, or this test passes by
	// being empty.
	for _, needed := range []string{
		`"crawl_status": "blocked"`,  // crawl state survives
		`"crawl_status": "ok"`,       // and so does a successful visit
		"recrutement@exemple-pharma", // contacts, with their provenance
		`"last_checked_at"`,          // the field that stops a re-crawl
		"Exemple Sans Site SARL",     // a company keyed on its name, not a domain
		"Île-de-France",              // non-ASCII text
	} {
		if !strings.Contains(original, needed) {
			t.Fatalf("the fixture does not contain %s, so this test proves nothing", needed)
		}
	}

	rebuilt := newTestStore(t)
	envelope, err := export.Read(strings.NewReader(original))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	result, err := rebuilt.RestoreCompanies(context.Background(), envelope.Companies)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if result.Companies != 4 {
		t.Errorf("restored %d companies, want 4", result.Companies)
	}

	if got := dump(t, rebuilt); got != original {
		t.Errorf("a round trip changed the dataset.\n--- before\n%s\n--- after\n%s",
			original, got)
	}
}

func TestRestorePreservesCrawlStateAndIdentity(t *testing.T) {
	// Spelled out separately from the byte comparison, because these two are the fields the
	// pipeline's cost and correctness actually depend on.
	original := dump(t, populated(t))
	envelope, err := export.Read(strings.NewReader(original))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rebuilt := newTestStore(t)
	if _, err := rebuilt.RestoreCompanies(context.Background(), envelope.Companies); err != nil {
		t.Fatalf("restore: %v", err)
	}

	for _, want := range envelope.Companies {
		got, err := rebuilt.GetCompany(context.Background(), want.ID)
		if err != nil {
			t.Fatalf("company %s (%q) is not in the rebuilt database: %v", want.ID, want.Name, err)
		}
		if got.CrawlStatus != want.CrawlStatus {
			t.Errorf("%q: crawl_status %q, want %q", want.Name, got.CrawlStatus, want.CrawlStatus)
		}
		if (got.LastCheckedAt == nil) != (want.LastCheckedAt == nil) {
			t.Errorf("%q: last_checked_at %v, want %v", want.Name, got.LastCheckedAt, want.LastCheckedAt)
		}
		if !got.FirstSeenAt.Equal(want.FirstSeenAt) {
			t.Errorf("%q: first_seen_at %s, want %s", want.Name, got.FirstSeenAt, want.FirstSeenAt)
		}
		if len(got.Contacts) != len(want.Contacts) {
			t.Errorf("%q: %d contacts, want %d", want.Name, len(got.Contacts), len(want.Contacts))
		}
	}
}

// After a restore, re-running discovery against the same sources must create nothing. This
// is the acceptance criterion the CI job already enforces for a fresh database, and it is
// the one that decides whether a restored run costs minutes or hours.
func TestDiscoveryAfterRestoreCreatesNothing(t *testing.T) {
	ctx := context.Background()
	original := dump(t, populated(t))
	envelope, err := export.Read(strings.NewReader(original))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rebuilt := newTestStore(t)
	if _, err := rebuilt.RestoreCompanies(ctx, envelope.Companies); err != nil {
		t.Fatalf("restore: %v", err)
	}

	for _, c := range envelope.Companies {
		// What a directory row looks like the second time around: a name, a country, a
		// website, and nothing else.
		result, err := rebuilt.UpsertCompany(ctx, model.Company{
			Name: c.Name, Country: c.Country, Website: c.Website,
		})
		if err != nil {
			t.Fatalf("re-discover %q: %v", c.Name, err)
		}
		if result.Created {
			t.Errorf("re-discovering %q after a restore created a second company", c.Name)
		}
		if result.CompanyID != c.ID {
			t.Errorf("%q resolved to %s after a restore, want its original id %s",
				c.Name, result.CompanyID, c.ID)
		}
	}
}

func TestRestoreIsIdempotent(t *testing.T) {
	ctx := context.Background()
	original := dump(t, populated(t))
	envelope, err := export.Read(strings.NewReader(original))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rebuilt := newTestStore(t)
	for i := 0; i < 3; i++ {
		if _, err := rebuilt.RestoreCompanies(ctx, envelope.Companies); err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
	}
	if got := dump(t, rebuilt); got != original {
		t.Error("restoring the same dataset three times did not leave the same database")
	}
}

func TestRestoreIsOrderIndependent(t *testing.T) {
	ctx := context.Background()
	original := dump(t, populated(t))
	envelope, err := export.Read(strings.NewReader(original))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	shuffled := append([]model.Company(nil), envelope.Companies...)
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	rebuilt := newTestStore(t)
	if _, err := rebuilt.RestoreCompanies(ctx, shuffled); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// If the order of the file could change who owns an identity, a restore would not be a
	// restore.
	if got := dump(t, rebuilt); got != original {
		t.Error("restoring the same companies in reverse order produced a different database")
	}
}

func TestRestoreRefusesWhatItCannotRestoreFaithfully(t *testing.T) {
	base := func() model.Company {
		return model.Company{
			ID: "aaaaaaaaaaaaaaaa", Name: "Exemple Un SAS", Country: "fr",
			Website: "https://exemple-un.example.org", CanonicalDomain: "exemple-un.example.org",
			FirstSeenAt: fixedTime, Quality: model.QualityLow, CrawlStatus: model.CrawlNotAttempted,
		}
	}
	other := func() model.Company {
		c := base()
		c.ID, c.Name = "bbbbbbbbbbbbbbbb", "Exemple Deux SA"
		c.Website, c.CanonicalDomain = "https://exemple-deux.example.net", "exemple-deux.example.net"
		return c
	}

	cases := map[string]struct {
		companies []model.Company
		want      string
	}{
		"two companies on one domain": {
			// The engine deliberately refuses to merge these. A restore that resolved both
			// to one owner would collapse exactly the records a human was meant to review.
			companies: func() []model.Company {
				a, b := base(), other()
				b.CanonicalDomain = a.CanonicalDomain
				return []model.Company{a, b}
			}(),
			want: "both claim the domain",
		},
		"duplicate company id": {
			companies: func() []model.Company {
				a, b := base(), other()
				b.ID = a.ID
				return []model.Company{a, b}
			}(),
			want: "appears twice",
		},
		"no company id": {
			companies: func() []model.Company { a := base(); a.ID = ""; return []model.Company{a} }(),
			want:      "has no company_id",
		},
		"no first_seen_at": {
			// Silently substituting "now" would turn a company first seen two years ago
			// into one first seen today, and nothing downstream could tell.
			companies: func() []model.Company {
				a := base()
				a.FirstSeenAt = time.Time{}
				return []model.Company{a}
			}(),
			want: "has no first_seen_at",
		},
		"published contact naming no page": {
			companies: func() []model.Company {
				a := base()
				a.Contacts = []model.Contact{{Address: "jobs@exemple-un.example.org", Verification: model.VerificationPublished}}
				return []model.Company{a}
			}(),
			want: "names no page",
		},
	}

	for name, testCase := range cases {
		store := newTestStore(t)
		_, err := store.RestoreCompanies(context.Background(), testCase.companies)
		if err == nil {
			t.Errorf("%s: the dataset was restored anyway", name)
			continue
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, testCase.want)
		}
		// A refusal must write nothing at all.
		count, err := store.CountCompanies(context.Background(), CompanyFilter{})
		if err != nil {
			t.Fatalf("%s: count: %v", name, err)
		}
		if count != 0 {
			t.Errorf("%s: %d companies were written despite the refusal", name, count)
		}
	}
}
