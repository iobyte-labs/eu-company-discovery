package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	// A file in the test's temp dir rather than ":memory:", so each test is fully isolated
	// (a shared-cache in-memory database is shared across the whole process).
	store, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(ctx, EngineMigrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func company(name, country, website string) model.Company {
	return model.Company{Name: name, Country: country, Website: website}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	// Already migrated by the helper; a second and third call must be no-ops.
	for i := 0; i < 2; i++ {
		if err := store.Migrate(ctx, EngineMigrations()); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	}
	applied, err := store.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("applied: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("expected 1 applied migration, got %d: %v", len(applied), applied)
	}
}

// The acceptance criterion: repeated discovery runs must not create duplicate companies.
func TestUpsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	input := company("Renault SAS", "fr", "https://www.renault.fr")
	input.Sources = []model.Source{{
		AssociationSlug: "fr_example", SourceURL: "https://example.fr/members?page=1",
		RawName: "Renault SAS", RetrievedAt: time.Now().UTC(),
	}}
	input.Contacts = []model.Contact{{
		Address: "recrutement@acme.fr", Kind: model.ContactRecruitment,
		Verification: model.VerificationPublished, FoundOnURL: "https://www.renault.fr/contact",
	}}

	first, err := store.UpsertCompany(ctx, input)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if !first.Created {
		t.Error("the first upsert should have created the company")
	}

	for i := 0; i < 3; i++ {
		again, err := store.UpsertCompany(ctx, input)
		if err != nil {
			t.Fatalf("repeat upsert %d: %v", i, err)
		}
		if again.Created {
			t.Errorf("repeat upsert %d created a duplicate company", i)
		}
		if again.CompanyID != first.CompanyID {
			t.Errorf("repeat upsert %d resolved to %s, want %s", i, again.CompanyID, first.CompanyID)
		}
	}

	counts, err := store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts["companies"] != 1 {
		t.Errorf("companies = %d, want 1", counts["companies"])
	}
	if counts["contacts"] != 1 {
		t.Errorf("contacts = %d, want 1 (one address must not become four)", counts["contacts"])
	}
	if counts["company_sources"] != 1 {
		t.Errorf("company_sources = %d, want 1", counts["company_sources"])
	}
}

// A company first seen by name only, then seen again with its website, must stay one
// company and gain a domain identity rather than being duplicated.
func TestLearningWebsiteLaterJoinsTheSameCompany(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	byName, err := store.UpsertCompany(ctx, company("Renault SAS", "fr", ""))
	if err != nil {
		t.Fatalf("name-only upsert: %v", err)
	}
	if !byName.Created {
		t.Fatal("expected the name-only record to be created")
	}

	withSite, err := store.UpsertCompany(ctx, company("Renault SAS", "fr", "https://www.renault.fr"))
	if err != nil {
		t.Fatalf("upsert with website: %v", err)
	}
	if withSite.Created {
		t.Error("learning the website created a second company")
	}
	if withSite.CompanyID != byName.CompanyID {
		t.Errorf("company id changed from %s to %s", byName.CompanyID, withSite.CompanyID)
	}
	if withSite.IdentitiesAdded != 1 {
		t.Errorf("IdentitiesAdded = %d, want 1 (the domain)", withSite.IdentitiesAdded)
	}

	// And the domain alone now resolves to it, even under a different spelling of the name.
	byDomain, err := store.UpsertCompany(ctx, company("RENAULT S.A.S.", "fr", "http://renault.fr/en"))
	if err != nil {
		t.Fatalf("upsert by domain: %v", err)
	}
	if byDomain.CompanyID != byName.CompanyID {
		t.Error("the same domain did not resolve to the same company")
	}
}

// Deduplication must not merge companies that merely resemble each other.
func TestSimilarNamesStayDistinct(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	sa, err := store.UpsertCompany(ctx, company("Renault SA", "fr", ""))
	if err != nil {
		t.Fatalf("upsert SA: %v", err)
	}
	sas, err := store.UpsertCompany(ctx, company("Renault SAS", "fr", ""))
	if err != nil {
		t.Fatalf("upsert SAS: %v", err)
	}
	if sa.CompanyID == sas.CompanyID {
		t.Error("Renault SA and Renault SAS were merged")
	}
}

// The same name in two countries is two companies.
func TestSameNameDifferentCountryStaysDistinct(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	fr, _ := store.UpsertCompany(ctx, company("Acme", "fr", ""))
	de, _ := store.UpsertCompany(ctx, company("Acme", "de", ""))
	if fr.CompanyID == de.CompanyID {
		t.Error("Acme (fr) and Acme (de) were merged")
	}
}

// When a domain already belongs to one company and the name to another, nothing is merged
// and the collision is reported.
func TestIdentityConflictIsReportedNotMerged(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	holding, _ := store.UpsertCompany(ctx, company("Groupe Acme", "fr", "https://acme.fr"))
	subsidiary, _ := store.UpsertCompany(ctx, company("Acme Industries", "fr", ""))

	// Now the subsidiary turns up listed under the group's website.
	result, err := store.UpsertCompany(ctx, company("Acme Industries", "fr", "https://acme.fr"))
	if err != nil {
		t.Fatalf("conflicting upsert: %v", err)
	}
	if result.Conflict == "" {
		t.Error("a domain/name collision was not reported")
	}
	if result.CompanyID != holding.CompanyID {
		t.Errorf("expected the domain to win, got %s want %s", result.CompanyID, holding.CompanyID)
	}

	counts, _ := store.Counts(ctx)
	if counts["companies"] != 2 {
		t.Errorf("companies = %d, want 2 — the collision must not merge records", counts["companies"])
	}
	if _, err := store.GetCompany(ctx, subsidiary.CompanyID); err != nil {
		t.Errorf("the subsidiary record was lost: %v", err)
	}
}

// A later, poorer listing must not erase what an earlier, richer one established.
func TestMergeNeverDowngrades(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	rich := company("Acme SAS", "fr", "https://acme.fr")
	rich.CareersURL = "https://acme.fr/careers"
	rich.Description = "A long and informative description of what Acme actually does."
	rich.Region = "idf"
	rich.Sectors = []string{"automotive"}
	rich.Quality = model.QualityHigh
	created, err := store.UpsertCompany(ctx, rich)
	if err != nil {
		t.Fatalf("rich upsert: %v", err)
	}

	// A bare directory row: the name and nothing else.
	poor := company("Acme SAS", "fr", "")
	poor.Sectors = []string{"metalworking"}
	if _, err := store.UpsertCompany(ctx, poor); err != nil {
		t.Fatalf("poor upsert: %v", err)
	}

	after, err := store.GetCompany(ctx, created.CompanyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Website != "https://acme.fr" {
		t.Errorf("website was lost: %q", after.Website)
	}
	if after.CareersURL != "https://acme.fr/careers" {
		t.Errorf("careers url was lost: %q", after.CareersURL)
	}
	if after.Description == "" {
		t.Error("description was lost")
	}
	if after.Region != "idf" {
		t.Errorf("region was lost: %q", after.Region)
	}
	if after.Quality != model.QualityHigh {
		t.Errorf("quality was downgraded to %q", after.Quality)
	}
	// Sectors from both listings are kept.
	if len(after.Sectors) != 2 {
		t.Errorf("sectors = %v, want both automotive and metalworking", after.Sectors)
	}
}

// An individual's address must never be stored, even if a caller passes one in.
func TestPersonalContactsAreRefused(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	input := company("Acme SAS", "fr", "https://acme.fr")
	input.Contacts = []model.Contact{
		{Address: "marie.dupont@acme.fr", Verification: model.VerificationPublished,
			FoundOnURL: "https://acme.fr/team"},
		{Address: "recrutement@acme.fr", Verification: model.VerificationPublished,
			FoundOnURL: "https://acme.fr/jobs"},
	}
	if _, err := store.UpsertCompany(ctx, input); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	counts, _ := store.Counts(ctx)
	if counts["contacts"] != 1 {
		t.Fatalf("contacts = %d, want 1 — the personal address must be dropped", counts["contacts"])
	}
	stored, _ := store.ListCompanies(ctx, CompanyFilter{})
	if len(stored) != 1 || len(stored[0].Contacts) != 1 {
		t.Fatal("expected exactly one company with one contact")
	}
	if stored[0].Contacts[0].Address != "recrutement@acme.fr" {
		t.Errorf("wrong contact kept: %q", stored[0].Contacts[0].Address)
	}
}

// A published sighting must not be downgraded by a later unverified one.
func TestPublishedContactIsNotDowngraded(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	input := company("Acme SAS", "fr", "https://acme.fr")
	input.Contacts = []model.Contact{{
		Address: "jobs@acme.fr", Kind: model.ContactRecruitment,
		Verification: model.VerificationPublished, FoundOnURL: "https://acme.fr/jobs",
	}}
	if _, err := store.UpsertCompany(ctx, input); err != nil {
		t.Fatalf("published upsert: %v", err)
	}

	input.Contacts = []model.Contact{{
		Address: "jobs@acme.fr", Kind: model.ContactRecruitment,
		Verification: model.VerificationUnverified,
	}}
	if _, err := store.UpsertCompany(ctx, input); err != nil {
		t.Fatalf("unverified upsert: %v", err)
	}

	stored, _ := store.ListCompanies(ctx, CompanyFilter{})
	got := stored[0].Contacts[0]
	if got.Verification != model.VerificationPublished {
		t.Errorf("verification downgraded to %q", got.Verification)
	}
	if got.FoundOnURL != "https://acme.fr/jobs" {
		t.Errorf("the page that published it was lost: %q", got.FoundOnURL)
	}
}

// A contact claiming to be published must name the page, or it is not admissible.
func TestPublishedContactWithoutPageIsRejected(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	input := company("Acme SAS", "fr", "https://acme.fr")
	input.Contacts = []model.Contact{{
		Address: "jobs@acme.fr", Verification: model.VerificationPublished,
	}}
	if _, err := store.UpsertCompany(ctx, input); err == nil {
		t.Fatal("a published contact with no source page was accepted")
	}
}

func TestUpsertRequiresNameAndCountry(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if _, err := store.UpsertCompany(ctx, company("", "fr", "")); err == nil {
		t.Error("a company with no name was accepted")
	}
	if _, err := store.UpsertCompany(ctx, company("Acme", "", "")); err == nil {
		t.Error("a company with no country was accepted")
	}
}

func TestListCompaniesFilters(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	withContact := company("Avec Contact", "fr", "https://avec.fr")
	withContact.Sectors = []string{"automotive"}
	withContact.Contacts = []model.Contact{{
		Address: "jobs@acme.fr", Verification: model.VerificationPublished,
		FoundOnURL: "https://avec.fr/jobs",
	}}
	mustUpsert(t, store, withContact)

	bare := company("Sans Contact", "fr", "https://sans.fr")
	bare.Sectors = []string{"aerospace"}
	mustUpsert(t, store, bare)

	german := company("Deutsch AG", "de", "https://deutsch.de")
	mustUpsert(t, store, german)

	cases := []struct {
		name   string
		filter CompanyFilter
		want   int
	}{
		{"all", CompanyFilter{}, 3},
		{"by country", CompanyFilter{Country: "fr"}, 2},
		{"by country uppercase", CompanyFilter{Country: "FR"}, 2},
		{"by sector", CompanyFilter{Sector: "automotive"}, 1},
		{"with contact", CompanyFilter{WithContact: true}, 1},
		{"country and contact", CompanyFilter{Country: "fr", WithContact: true}, 1},
		{"unknown sector", CompanyFilter{Sector: "nonexistent"}, 0},
		{"limit", CompanyFilter{Limit: 2}, 2},
	}
	for _, tc := range cases {
		got, err := store.ListCompanies(ctx, tc.filter)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(got) != tc.want {
			t.Errorf("%s: got %d companies, want %d", tc.name, len(got), tc.want)
		}
	}
}

// A sector filter must not match on a prefix: "auto" is not "automotive".
func TestSectorFilterMatchesWholeValue(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	c := company("Acme", "fr", "https://acme.fr")
	c.Sectors = []string{"automotive"}
	mustUpsert(t, store, c)

	got, err := store.ListCompanies(ctx, CompanyFilter{Sector: "auto"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("sector %q matched %q", "auto", "automotive")
	}
}

func TestBackupProducesAReadableDatabase(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	mustUpsert(t, store, company("Acme SAS", "fr", "https://acme.fr"))

	destination := filepath.Join(t.TempDir(), "backup.db")
	if err := store.Backup(ctx, destination); err != nil {
		t.Fatalf("backup: %v", err)
	}

	restored, err := Open(ctx, destination)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer restored.Close()
	counts, err := restored.Counts(ctx)
	if err != nil {
		t.Fatalf("counts on backup: %v", err)
	}
	if counts["companies"] != 1 {
		t.Errorf("backup holds %d companies, want 1", counts["companies"])
	}
}

func TestParseMigrationName(t *testing.T) {
	version, name, err := parseMigrationName("003_add_contacts.sql")
	if err != nil || version != 3 || name != "add_contacts" {
		t.Errorf("got (%d, %q, %v)", version, name, err)
	}
	for _, bad := range []string{"initial.sql", "abc_x.sql", "000_zero.sql", "-1_neg.sql"} {
		if _, _, err := parseMigrationName(bad); err == nil {
			t.Errorf("parseMigrationName(%q) should have failed", bad)
		}
	}
}

func mustUpsert(t *testing.T, store *Store, c model.Company) model.Company {
	t.Helper()
	result, err := store.UpsertCompany(context.Background(), c)
	if err != nil {
		t.Fatalf("upsert %q: %v", c.Name, err)
	}
	c.ID = result.CompanyID
	return c
}

// Three companies on subdomains of one domain must not merge silently. The domain join is
// still made (the domain is the strongest signal we have), but the mismatch is reported.
func TestSharedDomainJoinIsReported(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if _, err := store.UpsertCompany(ctx,
		company("Exemple Emboutissage SAS", "fr", "https://stamping.shared.example.org")); err != nil {
		t.Fatalf("first: %v", err)
	}
	result, err := store.UpsertCompany(ctx,
		company("Totalement Different SARL", "fr", "https://injection.shared.example.org"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if result.Conflict == "" {
		t.Error("a domain join between unrelated names was not reported")
	}
	if !strings.Contains(result.Conflict, "no word in common") {
		t.Errorf("the warning should say why it fired, got: %s", result.Conflict)
	}
}

// The same company under a different spelling must join silently, or the warning is noise.
func TestSameCompanyRejoinIsSilent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if _, err := store.UpsertCompany(ctx,
		company("Renault SAS", "fr", "https://www.renault.fr")); err != nil {
		t.Fatalf("first: %v", err)
	}
	result, err := store.UpsertCompany(ctx,
		company("RENAULT S.A.S.", "fr", "https://renault.fr/en"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if result.Conflict != "" {
		t.Errorf("rejoining the same company warned unnecessarily: %s", result.Conflict)
	}
}

// A legal form or a country word is not evidence that two names are related.
func TestLegalFormsAreNotEvidence(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if _, err := store.UpsertCompany(ctx,
		company("Alpha SAS", "fr", "https://a.shared2.example.org")); err != nil {
		t.Fatalf("first: %v", err)
	}
	result, err := store.UpsertCompany(ctx,
		company("Beta SAS", "fr", "https://b.shared2.example.org"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if result.Conflict == "" {
		t.Error("sharing only the legal form \"SAS\" should not silence the warning")
	}
}
