package store

import (
	"context"
	"testing"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

func publishedContact(address string, kind model.ContactKind, url string) model.Contact {
	return model.Contact{
		Address:      address,
		Kind:         kind,
		Verification: model.VerificationPublished,
		FoundOnURL:   url,
		FoundAt:      time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
	}
}

func TestRecordEnrichmentStoresContactsAndCrawlState(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	created, err := store.UpsertCompany(ctx,
		company("Exemple Pharma SAS", "fr", "https://exemple-pharma.example.org"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	result, err := store.RecordEnrichment(ctx, created.CompanyID, Enrichment{
		CareersURL: "https://exemple-pharma.example.org/carrieres",
		Contacts: []model.Contact{
			publishedContact("recrutement@exemple-pharma.example.org", model.ContactRecruitment,
				"https://exemple-pharma.example.org/contact"),
		},
		CrawlStatus: model.CrawlOK,
		CheckedAt:   time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RecordEnrichment: %v", err)
	}
	if result.ContactsRecorded != 1 {
		t.Errorf("ContactsRecorded = %d, want 1", result.ContactsRecorded)
	}
	if !result.CareersURLSet {
		t.Error("CareersURLSet is false, but this run learned the careers page")
	}

	loaded, err := store.GetCompany(ctx, created.CompanyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded.CrawlStatus != model.CrawlOK {
		t.Errorf("crawl status = %q, want ok", loaded.CrawlStatus)
	}
	if loaded.LastCheckedAt == nil {
		t.Error("last_checked_at was not set, so the company still looks unvisited")
	}
	if loaded.CareersURL != "https://exemple-pharma.example.org/carrieres" {
		t.Errorf("careers url = %q", loaded.CareersURL)
	}
	// A company whose site published an application address is as well evidenced as this
	// engine can make it.
	if loaded.Quality != model.QualityHigh {
		t.Errorf("quality = %q, want high", loaded.Quality)
	}
	best, ok := loaded.BestContact()
	if !ok || best.Address != "recrutement@exemple-pharma.example.org" {
		t.Errorf("best contact = %+v", best)
	}
}

func TestRecordEnrichmentRefusesAnIndividualsAddress(t *testing.T) {
	// The second of the two layers. Extraction drops these; this refuses them again, so no
	// future caller can get it wrong by accident.
	ctx := context.Background()
	store := newTestStore(t)
	created, err := store.UpsertCompany(ctx,
		company("Exemple Pharma SAS", "fr", "https://exemple-pharma.example.org"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	result, err := store.RecordEnrichment(ctx, created.CompanyID, Enrichment{
		Contacts: []model.Contact{
			publishedContact("marie.dupont@exemple-pharma.example.org", model.ContactOther,
				"https://exemple-pharma.example.org/contact"),
			// Published, but naming no page: a claim with no evidence.
			{Address: "rh@exemple-pharma.example.org", Verification: model.VerificationPublished},
		},
		CrawlStatus: model.CrawlOK,
	})
	if err != nil {
		t.Fatalf("RecordEnrichment: %v", err)
	}
	if result.ContactsRecorded != 0 {
		t.Errorf("ContactsRecorded = %d, want 0", result.ContactsRecorded)
	}
	if result.ContactsRejected != 2 {
		t.Errorf("ContactsRejected = %d, want 2", result.ContactsRejected)
	}

	loaded, err := store.GetCompany(ctx, created.CompanyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(loaded.Contacts) != 0 {
		t.Errorf("contacts = %+v, want none stored", loaded.Contacts)
	}
}

func TestRecordEnrichmentIsIdempotentAndNeverDowngrades(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	created, err := store.UpsertCompany(ctx,
		company("Exemple Pharma SAS", "fr", "https://exemple-pharma.example.org"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	enrichment := Enrichment{
		CareersURL: "https://exemple-pharma.example.org/carrieres",
		Contacts: []model.Contact{
			publishedContact("recrutement@exemple-pharma.example.org", model.ContactRecruitment,
				"https://exemple-pharma.example.org/contact"),
		},
		CrawlStatus: model.CrawlOK,
	}
	for i := 0; i < 3; i++ {
		if _, err := store.RecordEnrichment(ctx, created.CompanyID, enrichment); err != nil {
			t.Fatalf("RecordEnrichment %d: %v", i, err)
		}
	}
	loaded, err := store.GetCompany(ctx, created.CompanyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Several contacts would mean several applications downstream. One company, one
	// contact, however many times it is seen.
	if len(loaded.Contacts) != 1 {
		t.Errorf("contacts = %+v, want exactly one", loaded.Contacts)
	}

	// A later visit that finds nothing must not undo what an earlier one established.
	if _, err := store.RecordEnrichment(ctx, created.CompanyID, Enrichment{
		CrawlStatus: model.CrawlBlocked,
		CrawlError:  "robots.txt disallows /contact",
	}); err != nil {
		t.Fatalf("second RecordEnrichment: %v", err)
	}
	loaded, err = store.GetCompany(ctx, created.CompanyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded.Quality != model.QualityHigh {
		t.Errorf("quality = %q, want it kept at high", loaded.Quality)
	}
	if loaded.CareersURL == "" {
		t.Error("the careers page found earlier was lost")
	}
	if len(loaded.Contacts) != 1 {
		t.Errorf("contacts = %+v, want the published one kept", loaded.Contacts)
	}
	// Crawl state is the exception: it describes the latest attempt, so a refusal replaces
	// the earlier success and stays visible.
	if loaded.CrawlStatus != model.CrawlBlocked {
		t.Errorf("crawl status = %q, want blocked", loaded.CrawlStatus)
	}
	if loaded.CrawlError == "" {
		t.Error("a blocked company records no reason, so nobody can act on it")
	}
}

func TestListCompaniesSelectsWhatEnrichmentShouldVisit(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	waiting, err := store.UpsertCompany(ctx,
		company("Exemple Attente SAS", "fr", "https://exemple-attente.example.org"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	noSite, err := store.UpsertCompany(ctx, company("Exemple Sans Site SARL", "fr", ""))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	done, err := store.UpsertCompany(ctx,
		company("Exemple Fini SA", "fr", "https://exemple-fini.example.net"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := store.RecordEnrichment(ctx, done.CompanyID, Enrichment{
		Contacts: []model.Contact{
			publishedContact("rh@exemple-fini.example.net", model.ContactHR,
				"https://exemple-fini.example.net/contact"),
		},
		CrawlStatus: model.CrawlOK,
	}); err != nil {
		t.Fatalf("RecordEnrichment: %v", err)
	}

	companies, err := store.ListCompanies(ctx, CompanyFilter{
		WithWebsite:        true,
		WithoutContact:     true,
		CrawlStatuses:      []model.CrawlStatus{model.CrawlNotAttempted},
		OldestCheckedFirst: true,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(companies) != 1 {
		t.Fatalf("got %d companies, want 1: %+v", len(companies), companies)
	}
	if companies[0].ID != waiting.CompanyID {
		t.Errorf("selected %q, want the company with a website and no contact", companies[0].Name)
	}
	if companies[0].ID == noSite.CompanyID {
		t.Error("a company with no website was selected; there is nothing to visit")
	}
}
