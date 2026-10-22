package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/directory"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

var fetchedAt = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// page loads a recorded fixture. Every fixture uses example.org domains: this is a public
// repository and holds no real contact data.
func page(t *testing.T, name, url string) *directory.Page {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return &directory.Page{
		URL:          url,
		RequestedURL: url,
		Status:       200,
		ContentType:  "text/html; charset=utf-8",
		Body:         body,
		FetchedAt:    fetchedAt,
	}
}

func addresses(contacts []model.Contact) []string {
	out := make([]string, 0, len(contacts))
	for _, contact := range contacts {
		out = append(out, contact.Address)
	}
	return out
}

func TestFromPageReadsPublishedFunctionalAddresses(t *testing.T) {
	result := FromPage(
		page(t, "site/contact.html", "https://exemple-pharma.example.org/contact"),
		"https://exemple-pharma.example.org")

	want := []string{
		"recrutement@exemple-pharma.example.org", // mailto:, percent-encoded
		"contact@exemple-pharma.example.org",     // in the visible text
		"qualite@exemple-pharma.example.org",     // written "(at)" and "[dot]"
	}
	got := addresses(result.Contacts)
	if len(got) != len(want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
	// Best first: a recruitment address beats a general inbox.
	for i, address := range want {
		if got[i] != address {
			t.Errorf("address %d = %q, want %q", i, got[i], address)
		}
	}

	first := result.Contacts[0]
	if first.Kind != model.ContactRecruitment {
		t.Errorf("kind = %q, want recruitment", first.Kind)
	}
	if first.Verification != model.VerificationPublished {
		t.Errorf("verification = %q, want published", first.Verification)
	}
	// The claim and its evidence are inseparable: a published contact names its page.
	if first.FoundOnURL != "https://exemple-pharma.example.org/contact" {
		t.Errorf("FoundOnURL = %q, want the page actually fetched", first.FoundOnURL)
	}
	if !first.FoundAt.Equal(fetchedAt) {
		t.Errorf("FoundAt = %s, want the fetch time", first.FoundAt)
	}
	if err := first.Validate(); err != nil {
		t.Errorf("the contact the extractor produced does not validate: %v", err)
	}

	if result.Contacts[1].Kind != model.ContactGeneral {
		t.Errorf("contact@ classified as %q, want general", result.Contacts[1].Kind)
	}
}

func TestFromPageDropsIndividualsAddresses(t *testing.T) {
	// The costly mistake is keeping one, so this errs towards dropping. The page carries
	// marie.dupont@ in text and j.martin@ as a mailto:; neither may be recorded.
	result := FromPage(
		page(t, "site/contact.html", "https://exemple-pharma.example.org/contact"),
		"https://exemple-pharma.example.org")

	for _, contact := range result.Contacts {
		local := strings.SplitN(contact.Address, "@", 2)[0]
		if local == "marie.dupont" || local == "j.martin" {
			t.Errorf("an individual's address was recorded: %q", contact.Address)
		}
	}
	if result.DroppedPersonal != 2 {
		t.Errorf("DroppedPersonal = %d, want 2: a page of named employees and no functional "+
			"mailbox should not look like a page with no addresses", result.DroppedPersonal)
	}
}

func TestFromPageIgnoresAddressesInScripts(t *testing.T) {
	// An analytics or error-reporting snippet carries a vendor's address. It is not a
	// contact this company published by any reading.
	result := FromPage(
		page(t, "site/contact.html", "https://exemple-pharma.example.org/contact"),
		"https://exemple-pharma.example.org")

	for _, contact := range result.Contacts {
		if strings.Contains(contact.Address, "vendor.example.org") {
			t.Errorf("an address from a <script> was recorded: %q", contact.Address)
		}
	}
}

func TestFromPageReadsAnAddressOutOfAnAttribute(t *testing.T) {
	result := FromPage(
		page(t, "site/impressum.html", "https://beispiel-pharma.example.org/impressum"),
		"https://beispiel-pharma.example.org")

	got := addresses(result.Contacts)
	want := []string{
		"bewerbung@beispiel-pharma.example.org", // recruitment first
		"info@beispiel-pharma.example.org",      // from data-mail, HTML-escaped
	}
	if len(got) != len(want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("address %d = %q, want %q", i, got[i], want[i])
		}
	}
	if result.DroppedPersonal != 1 {
		t.Errorf("DroppedPersonal = %d, want 1 (the Geschäftsführer)", result.DroppedPersonal)
	}
}

func TestFromPageFindsCareersLinksOnTheCompanysOwnSite(t *testing.T) {
	result := FromPage(
		page(t, "site/home.html", "https://voorbeeld-farma.example.org/"),
		"https://voorbeeld-farma.example.org")

	want := []string{
		"https://voorbeeld-farma.example.org/vacatures/overzicht",
		"https://voorbeeld-farma.example.org/werken-bij-ons",
	}
	if len(result.CareersURLs) != len(want) {
		t.Fatalf("CareersURLs = %v, want %v", result.CareersURLs, want)
	}
	for i := range want {
		if result.CareersURLs[i] != want[i] {
			t.Errorf("careers link %d = %q, want %q", i, result.CareersURLs[i], want[i])
		}
	}
	if len(result.Contacts) != 0 {
		t.Errorf("contacts = %v, want none: the homepage publishes no address",
			addresses(result.Contacts))
	}
}

func TestFromPageWillNotFollowLinksOffTheCompanysSite(t *testing.T) {
	// The contact page links to a job board. However relevant, it is somebody else's site.
	result := FromPage(
		page(t, "site/contact.html", "https://exemple-pharma.example.org/contact"),
		"https://exemple-pharma.example.org")

	for _, link := range result.CareersURLs {
		if !strings.Contains(link, "exemple-pharma.example.org") {
			t.Errorf("a careers link off the company's own site was kept: %q", link)
		}
	}
	if len(result.CareersURLs) != 1 || !strings.HasSuffix(result.CareersURLs[0], "/carrieres") {
		t.Errorf("CareersURLs = %v, want only the company's own /carrieres", result.CareersURLs)
	}
}

func TestCandidateURLsAreOrderedWithTheHomepageLast(t *testing.T) {
	urls := CandidateURLs("https://acme.example.org/fr/accueil")
	if len(urls) < 5 {
		t.Fatalf("CandidateURLs = %v, want several", urls)
	}
	// Careers before contact before the legal notice before the homepage: the order is the
	// policy, and it is what a caller with a small page budget relies on.
	positions := map[string]int{}
	for index, candidate := range urls {
		positions[candidate] = index
	}
	careers := positions["https://acme.example.org/careers"]
	contact := positions["https://acme.example.org/contact"]
	legal := positions["https://acme.example.org/mentions-legales"]
	// normalize.URL drops a bare trailing slash, so the homepage is the bare origin.
	home := positions["https://acme.example.org"]
	if !(careers < contact && contact < legal && legal < home) {
		t.Errorf("order is careers=%d contact=%d legal=%d home=%d, want them in that order",
			careers, contact, legal, home)
	}
	if home != len(urls)-1 {
		t.Errorf("the homepage is at %d of %d, want last", home, len(urls))
	}
	// Every candidate is on the company's own site, and the path we were given is dropped
	// rather than used as a prefix.
	for _, candidate := range urls {
		if !strings.HasPrefix(candidate, "https://acme.example.org") {
			t.Errorf("candidate %q leaves the company's site", candidate)
		}
		if strings.Contains(candidate, "/fr/accueil") {
			t.Errorf("candidate %q was built on the recorded page's path", candidate)
		}
	}
}

func TestCandidateURLsRefusesAnUnusableWebsite(t *testing.T) {
	for _, website := range []string{"", "not a url", "mailto:info@acme.example.org", "ftp://acme.example.org"} {
		if urls := CandidateURLs(website); len(urls) != 0 {
			t.Errorf("CandidateURLs(%q) = %v, want none", website, urls)
		}
	}
}

func TestFromPageHandlesAnEmptyOrBrokenPage(t *testing.T) {
	// A page that is not HTML, or not there at all, yields nothing rather than panicking.
	if result := FromPage(nil, "https://acme.example.org"); len(result.Contacts) != 0 {
		t.Error("a nil page produced contacts")
	}
	empty := &directory.Page{URL: "https://acme.example.org/", FetchedAt: fetchedAt}
	if result := FromPage(empty, ""); len(result.Contacts) != 0 {
		t.Error("an empty page produced contacts")
	}
	binary := &directory.Page{
		URL:       "https://acme.example.org/x.pdf",
		Body:      []byte{0x25, 0x50, 0x44, 0x46, 0x00, 0x01, 0x02},
		FetchedAt: fetchedAt,
	}
	if result := FromPage(binary, ""); len(result.Contacts) != 0 {
		t.Error("a binary body produced contacts")
	}
}

func TestDeobfuscateUndoesWhatAPageActuallySays(t *testing.T) {
	cases := map[string]string{
		"info (at) acme [dot] fr":      "info@acme.fr",
		"rh [at] acme (point) fr":      "rh@acme.fr",
		"jobs {at} acme {punkt} de":    "jobs@acme.de",
		"contact (arobase) acme.fr":    "contact@acme.fr",
		"already@acme.fr":              "already@acme.fr",
		"two words then info@acme.fr.": "info@acme.fr",
	}
	for input, want := range cases {
		got := emailPattern.FindString(deobfuscate(input))
		if got != want {
			t.Errorf("deobfuscate(%q) yielded %q, want %q", input, got, want)
		}
	}
}
