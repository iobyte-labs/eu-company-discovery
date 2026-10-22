package cli

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// recorder serves a fixed set of paths and remembers what was asked for, so a test can
// assert not only what was found but how many requests somebody else's server received.
type recorder struct {
	pages map[string]string

	mu    sync.Mutex
	asked []string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	r.asked = append(r.asked, request.URL.Path)
	r.mu.Unlock()

	body, ok := r.pages[request.URL.Path]
	if !ok {
		http.NotFound(w, request)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, body)
}

func (r *recorder) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

func TestVisitCompanyStopsAsSoonAsItHasARecruitmentAddress(t *testing.T) {
	site := &recorder{pages: map[string]string{
		"/careers": `<html><body><h1>Careers</h1>
		  <p>Applications: <a href="mailto:jobs@acme.example.org">jobs@acme.example.org</a></p>
		  <p>Anna Schmidt: <a href="mailto:anna.schmidt@acme.example.org">anna</a></p>
		  </body></html>`,
		"/contact": `<html><body>contact@acme.example.org</body></html>`,
	}}
	fetcher, base := testSite(t, "acme.example.org", site)

	company := model.Company{Name: "Acme Example SAS", Country: "fr", Website: base}
	result := visitCompany(context.Background(), fetcher, company, 6)

	if result.status != model.CrawlOK {
		t.Errorf("status = %q, want ok", result.status)
	}
	if len(result.contacts) != 1 {
		t.Fatalf("contacts = %+v, want one", result.contacts)
	}
	contact := result.contacts[0]
	if contact.Address != "jobs@acme.example.org" {
		t.Errorf("address = %q", contact.Address)
	}
	if contact.Kind != model.ContactRecruitment {
		t.Errorf("kind = %q, want recruitment", contact.Kind)
	}
	if contact.Verification != model.VerificationPublished {
		t.Errorf("verification = %q, want published", contact.Verification)
	}
	if !strings.HasSuffix(contact.FoundOnURL, "/careers") {
		t.Errorf("FoundOnURL = %q, want the page it was read from", contact.FoundOnURL)
	}
	// An individual's address was on the same page. It is counted and dropped, never
	// stored.
	if result.dropped != 1 {
		t.Errorf("dropped = %d, want 1", result.dropped)
	}
	// /careers answered first, so nothing else was asked for: robots.txt and /careers.
	if got := site.requests(); len(got) != 2 {
		t.Errorf("requests = %v, want robots.txt and /careers only", got)
	}
}

func TestVisitCompanyFollowsACareersLinkFromTheHomepage(t *testing.T) {
	// "/stellenangebote/uebersicht" is a spelling no guessed path covers. The homepage is
	// how it is found, which is why the homepage always keeps a slot in the page budget.
	site := &recorder{pages: map[string]string{
		"/": `<html><body>
		  <a href="/stellenangebote/uebersicht">Stellenangebote</a>
		  <a href="https://jobboard.example.net/beispiel">Auch auf einer Jobbörse</a>
		  </body></html>`,
		"/stellenangebote/uebersicht": `<html><body>
		  <a href="mailto:bewerbung@beispiel.example.org">bewerbung@beispiel.example.org</a>
		  </body></html>`,
	}}
	fetcher, base := testSite(t, "beispiel.example.org", site)

	company := model.Company{Name: "Beispiel GmbH", Country: "de", Website: base}
	// Two slots: one guess (which misses) and the homepage. The careers page the homepage
	// links to is then read with the third.
	result := visitCompany(context.Background(), fetcher, company, 3)

	if result.status != model.CrawlOK {
		t.Errorf("status = %q, want ok", result.status)
	}
	if len(result.contacts) != 1 || result.contacts[0].Address != "bewerbung@beispiel.example.org" {
		t.Fatalf("contacts = %+v, want the address on the linked careers page", result.contacts)
	}
	if !strings.HasSuffix(result.careersURL, "/stellenangebote/uebersicht") {
		t.Errorf("careersURL = %q, want the page the company linked to", result.careersURL)
	}
	asked := strings.Join(site.requests(), " ")
	if !strings.Contains(asked, "/stellenangebote/uebersicht") {
		t.Errorf("requests = %v, want the linked careers page among them", site.requests())
	}
	// A link to a job board is somebody else's site, however relevant.
	if strings.Contains(result.careersURL, "jobboard.example.net") {
		t.Errorf("careersURL = %q, want the company's own page", result.careersURL)
	}
}

func TestVisitCompanyReadsTheHomepageWhenEveryGuessMisses(t *testing.T) {
	site := &recorder{pages: map[string]string{
		"/": `<html><body>
		  <p>Schrijf ons: <a href="mailto:info@voorbeeld.example.org">info</a></p>
		  </body></html>`,
	}}
	fetcher, base := testSite(t, "voorbeeld.example.org", site)

	company := model.Company{Name: "Voorbeeld BV", Country: "nl", Website: base}
	// Three slots: one guess, the homepage, and one kept for anything it links to.
	result := visitCompany(context.Background(), fetcher, company, 3)

	if result.status != model.CrawlOK {
		t.Errorf("status = %q, want ok", result.status)
	}
	if len(result.contacts) != 1 || result.contacts[0].Address != "info@voorbeeld.example.org" {
		t.Fatalf("contacts = %+v, want the homepage's general inbox", result.contacts)
	}
	// A general inbox is a fallback that wants a human decision, not an invitation, so it
	// has to be classified as one.
	if result.contacts[0].Kind != model.ContactGeneral {
		t.Errorf("kind = %q, want general", result.contacts[0].Kind)
	}
	if result.missing == 0 {
		t.Error("missing = 0, want the absent guessed page counted")
	}
	if got := site.requests(); len(got) > 3 {
		t.Errorf("requests = %v, want at most robots.txt, one guess and the homepage", got)
	}
	if result.careersURL != "" {
		t.Errorf("careersURL = %q, want none: this site links to no careers page",
			result.careersURL)
	}
}

func TestVisitCompanyRecordsThatASitePublishesNothing(t *testing.T) {
	site := &recorder{pages: map[string]string{
		"/": `<html><body><p>Postbus 1, Utrecht</p></body></html>`,
	}}
	fetcher, base := testSite(t, "stil.example.org", site)

	company := model.Company{Name: "Stil BV", Country: "nl", Website: base}
	result := visitCompany(context.Background(), fetcher, company, 2)

	if result.status != model.CrawlOK {
		t.Errorf("status = %q, want ok: the site was read, it simply publishes no address",
			result.status)
	}
	if len(result.contacts) != 0 {
		t.Errorf("contacts = %+v, want none", result.contacts)
	}
	if result.statusDetail == "" {
		t.Error("nothing was recorded about why this company has no contact")
	}
}

func TestVisitCompanyRecordsARefusalByRobots(t *testing.T) {
	site := &recorder{pages: map[string]string{
		"/robots.txt": "User-agent: *\nDisallow: /\n",
		"/":           `<html><a href="mailto:jobs@acme.example.org">jobs</a></html>`,
	}}
	fetcher, base := testSite(t, "acme.example.org", site)

	company := model.Company{Name: "Acme Example SAS", Country: "fr", Website: base}
	result := visitCompany(context.Background(), fetcher, company, 3)

	if result.status != model.CrawlBlocked {
		t.Errorf("status = %q, want blocked", result.status)
	}
	if len(result.contacts) != 0 {
		t.Errorf("contacts = %+v, want none: the site asked us not to read it", result.contacts)
	}
	if !strings.Contains(result.statusDetail, "robots.txt") {
		t.Errorf("statusDetail = %q, want the robots rule recorded", result.statusDetail)
	}
	if len(result.blocked) == 0 {
		t.Error("the refusal was not reported")
	}
	// Asked once for robots.txt, and then nothing else.
	if got := site.requests(); len(got) != 1 || got[0] != "/robots.txt" {
		t.Errorf("requests = %v, want only robots.txt", got)
	}
}

func TestVisitCompanyRecordsARedirectToASignInPageAsBlocked(t *testing.T) {
	site := &recorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site.mu.Lock()
		site.asked = append(site.asked, r.URL.Path)
		site.mu.Unlock()
		switch r.URL.Path {
		case "/robots.txt":
			http.NotFound(w, r)
		case "/login":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><form>sign in</form></html>")
		default:
			http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusFound)
		}
	})
	fetcher, base := testSite(t, "gated.example.org", handler)

	company := model.Company{Name: "Gated SAS", Country: "fr", Website: base}
	result := visitCompany(context.Background(), fetcher, company, 2)

	if result.status != model.CrawlBlocked {
		t.Errorf("status = %q, want blocked: the content is behind a login", result.status)
	}
	if len(result.contacts) != 0 {
		t.Errorf("contacts = %+v, want none", result.contacts)
	}
}
