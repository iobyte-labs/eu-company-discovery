// Package extract reads published contact details and careers links off a fetched page.
//
// Two rules decide everything here, and both are promises the project makes rather than
// implementation choices:
//
// Only what the page published. No address is ever constructed — no
// firstname.lastname@domain, no trying jobs@ to see whether it bounces. If a page does not
// show an address, the company has no contact as far as this engine is concerned. Every
// contact therefore carries the URL it was read from, and [model.Contact.Validate] refuses
// one that does not.
//
// Never an individual. [normalize.LooksPersonal] recognises marie.dupont@ and j.dupont@,
// and those are dropped here. The store refuses them again on the way in — two layers,
// because this is a promise, not a step someone might remember.
//
// Undoing obfuscation ("info (at) acme [dot] fr") is reading what the page says, so it is
// done. Guessing is not.
package extract

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"

	"github.com/iobyte-labs/eu-company-discovery/pkg/directory"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// Result is what one page yielded.
type Result struct {
	// Contacts are the published functional addresses found on the page, best first:
	// recruitment, then HR, then a general inbox.
	Contacts []model.Contact
	// CareersURLs are links on the page that look like a careers or jobs page, on the
	// company's own site, in the order they appeared.
	CareersURLs []string
	// DroppedPersonal counts addresses that were found and discarded for naming an
	// individual. Counted, never recorded: a page with ten named employees and no
	// functional mailbox should look different from a page with no addresses at all.
	DroppedPersonal int
	// DroppedOffBrand counts addresses that belonged to somebody else — a vendor named in
	// a privacy notice, an applicant-tracking system, a partner.
	DroppedOffBrand int
}

// candidatePath is one path worth trying on a company's own site, in the order they are
// worth trying.
//
// The order is not arbitrary. A careers page is what we actually want, and a company that
// publishes one has invited applications. A contact page is next. An imprint or legal
// notice is third and is the reliable one in the EU: Germany and Austria require it by law,
// France requires mentions légales, and it almost always carries a real address — but it is
// a legal page rather than an invitation, so it comes after the two pages that are. The
// homepage is last, because a footer address is the weakest of the four.
// The list is kept short deliberately. Every entry is a request to somebody else's server,
// most of them 404s, and the rarer spellings are not worth guessing: a page that exists is
// almost always linked from the homepage, and [Result.CareersURLs] finds it there. Guessing
// covers the common cases cheaply; linking covers the rest honestly.
var candidatePaths = []string{
	// Careers and jobs.
	"/careers", "/jobs", "/carrieres", "/emploi", "/recrutement", "/karriere", "/vacatures",
	// Contact.
	"/contact", "/kontakt", "/contatti", "/contacto",
	// Imprint and legal notice. Required by law in several member states, which is why
	// they so reliably carry a real address.
	"/impressum", "/mentions-legales",
	// The homepage, last: a footer address is the weakest of the four, but the homepage is
	// also what links to the pages above, so a caller is expected to keep room for it.
	"/",
}

// CandidateURLs returns the pages worth visiting on a company's own site, in order.
//
// A caller stops as soon as it has what it needs, so the ordering is the policy: the
// pages most likely to carry a recruitment address come first, and the homepage is a
// fallback. Every URL returned is on the company's own registrable domain — the engine
// follows a company's site, it does not crawl outwards from it.
func CandidateURLs(website string) []string {
	root := normalize.URL(website)
	if root == "" {
		return nil
	}
	base, err := url.Parse(root)
	if err != nil {
		return nil
	}
	seen := make(map[string]bool, len(candidatePaths))
	out := make([]string, 0, len(candidatePaths))
	for _, path := range candidatePaths {
		candidate := *base
		candidate.Path = path
		candidate.RawQuery = ""
		candidate.Fragment = ""
		normalized := normalize.URL(candidate.String())
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out
}

// emailPattern matches an address in already-deobfuscated text. It is deliberately a little
// loose: every match is handed to [normalize.Email], which is the authority on what an
// address is and what it normalises to.
var emailPattern = regexp.MustCompile(
	`[A-Za-z0-9._%+\-]+@[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?` +
		`(?:\.[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?)+`)

// obfuscations are the spellings a page uses to keep an address away from scrapers while
// keeping it readable to a person.
//
// normalize.Email undoes these for one address. The same substitution is needed *before*
// scanning a page, because an obfuscated address does not look like an address to any
// pattern — "info (at) acme [dot] fr" has no "@" in it at all. The final value still comes
// from normalize.Email, so there is one authority on what an address normalises to.
var obfuscations = regexp.MustCompile(
	`(?i)\s*[\(\[\{]\s*(at|arobase|chez|dot|point|punkt)\s*[\)\]\}]\s*`)

func deobfuscate(text string) string {
	return obfuscations.ReplaceAllStringFunc(text, func(match string) string {
		switch strings.ToLower(strings.Trim(strings.TrimSpace(match), "()[]{} \t")) {
		case "at", "arobase", "chez":
			return "@"
		default:
			return "."
		}
	})
}

// careersHints are the words a link to a careers page uses, in its URL or its text.
var careersHints = []string{
	"career", "careers", "job", "jobs", "joboffer", "vacancy", "vacancies",
	"carriere", "carrieres", "emploi", "emplois", "recrutement", "recrute",
	"nous-rejoindre", "rejoignez", "karriere", "stellenangebote", "stellen", "bewerbung",
	"jobboerse", "lavora-con-noi", "lavoro", "trabaja", "empleo", "vacature", "vacatures",
	"werken-bij", "hiring", "we-are-hiring", "join-us", "join-our-team", "work-with-us",
}

// FromPage reads one fetched page.
//
// companyWebsite bounds which links are kept: a careers link is only useful if it is on the
// company's own site. Pass "" to bound them to the page's own site instead.
func FromPage(page *directory.Page, companyWebsite string) Result {
	var result Result
	if page == nil || len(page.Body) == 0 {
		return result
	}
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(page.Body))
	if err != nil {
		return result
	}
	site := companyWebsite
	if site == "" {
		site = page.URL
	}

	addresses := make([]string, 0, 8)
	add := func(candidate string) {
		if normalized := normalize.Email(candidate); normalized != "" {
			addresses = append(addresses, normalized)
		}
	}

	// A mailto: link is the strongest form: the page is not merely displaying an address,
	// it is offering it as a way to write.
	document.Find(`a[href]`).Each(func(_ int, anchor *goquery.Selection) {
		href, _ := anchor.Attr("href")
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(href)), "mailto:") {
			return
		}
		add(mailtoAddress(href))
	})

	// Then the visible text. Script and style content is removed first: an analytics or
	// error-reporting snippet carries addresses that belong to a vendor, not to this
	// company, and they are not published contact details by any reading.
	document.Find("script, style, noscript, template").Remove()
	text := deobfuscate(visibleText(document))
	for _, match := range emailPattern.FindAllString(text, 200) {
		add(match)
	}
	// Some sites put the address in an attribute instead of in text — a data-mail attribute
	// read by a script, or a title on a contact icon.
	for _, attribute := range []string{"data-mail", "data-email", "data-mailto", "title"} {
		document.Find("[" + attribute + "]").Each(func(_ int, node *goquery.Selection) {
			value, _ := node.Attr(attribute)
			add(deobfuscate(value))
		})
	}

	result.Contacts, result.DroppedPersonal, result.DroppedOffBrand =
		contactsFrom(addresses, page, site)
	result.CareersURLs = careersLinks(document, page, site)
	return result
}

// joinedInline are the elements a page uses to style part of a word. Text either side of
// one belongs to the same word, so no separator is inserted around them. Everything else —
// a link, a cell, a list item, a paragraph — separates.
var joinedInline = map[string]bool{
	"b": true, "i": true, "em": true, "strong": true, "span": true, "u": true,
	"small": true, "sup": true, "sub": true, "mark": true, "abbr": true, "code": true,
	"font": true, "wbr": true, "ins": true, "del": true, "s": true, "q": true,
}

// Text returns a fetched page's readable text, with script and style content removed and
// element boundaries preserved as spaces.
//
// Exported because judging what a company does needs the same text that finding its
// address does, and reading it twice in two ways would mean two sets of bugs.
func Text(page *directory.Page) string {
	if page == nil || len(page.Body) == 0 {
		return ""
	}
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(page.Body))
	if err != nil {
		return ""
	}
	document.Find("script, style, noscript, template").Remove()
	return normalize.CollapseWhitespace(visibleText(document))
}

// visibleText returns a document's text with element boundaries preserved as spaces.
//
// goquery's Text() concatenates text nodes with nothing between them, so a row of links
// reading "180" / "muenchen@acme.de" / "mehr" comes out as "180muenchen@acme.demehr" — and
// an address pattern run over that yields muenchen@acme.demehr, an address nobody
// published. This engine's whole claim is that an address was read off a page, so an
// address manufactured by concatenation is the worst thing it can produce.
//
// The inline elements above stay joined, because a page that writes
// "<span>in</span>fo@acme.de" means one address, and splitting there would lose a real one.
// Between a miss and an invention, this prefers the miss.
func visibleText(document *goquery.Document) string {
	var out strings.Builder
	for _, node := range document.Nodes {
		out.WriteString(nodeText(node))
	}
	return out.String()
}

// nodeText walks one node, preserving element boundaries as spaces. Shared by visibleText
// and visibleNodeText so that both produce exactly the same spacing — the boundary rule
// above is subtle enough that a second implementation would eventually disagree with it.
func nodeText(root *html.Node) string {
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			out.WriteString(n.Data)
			return
		case html.ElementNode:
			if n.Data == "br" {
				out.WriteByte(' ')
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if n.Type == html.ElementNode && !joinedInline[n.Data] {
			out.WriteByte(' ')
		}
	}
	walk(root)
	return out.String()
}

// mailtoAddress reads the address out of a mailto: link, dropping any ?subject= and
// anything after the first recipient.
func mailtoAddress(href string) string {
	raw := strings.TrimSpace(href)
	raw = raw[len("mailto:"):]
	if question := strings.IndexByte(raw, '?'); question >= 0 {
		raw = raw[:question]
	}
	if comma := strings.IndexByte(raw, ','); comma >= 0 {
		raw = raw[:comma]
	}
	// mailto: URLs are percent-encoded, so "info%40acme.fr" is an ordinary spelling.
	if decoded, err := url.PathUnescape(raw); err == nil {
		raw = decoded
	}
	return raw
}

// contactsFrom turns addresses into contacts, dropping every one that names a person or
// belongs to somebody else.
func contactsFrom(
	addresses []string, page *directory.Page, site string,
) (contacts []model.Contact, personal, offBrand int) {
	seen := map[string]bool{}
	for _, address := range addresses {
		if address == "" || seen[address] {
			continue
		}
		seen[address] = true
		// A company's own pages carry other companies' addresses: the data-protection
		// officer named in a privacy notice, the applicant-tracking system behind a
		// careers page, an agency in a footer. Recording those as this company's contact
		// attributes an address to a company that never published it — the dataset would
		// say a German manufacturer can be reached at a software vendor's inbox.
		if !sameBrand(address, site, page.URL) {
			offBrand++
			continue
		}
		// The first of the two layers. The store refuses these again; see
		// docs/knowledge-concepts.md §4.
		if normalize.LooksPersonal(address) {
			personal++
			continue
		}
		contacts = append(contacts, model.Contact{
			Address:      address,
			Kind:         model.ContactKind(normalize.ClassifyLocalPart(address)),
			Verification: model.VerificationPublished,
			// The page actually fetched, after redirects — not the URL we asked for. A
			// contact's evidence has to name the document it was read from.
			FoundOnURL: page.URL,
			FoundAt:    page.FetchedAt,
		})
	}
	sortContacts(contacts)
	return contacts, personal, offBrand
}

// genericBrandWords appear in so many company names and domains that sharing one is not
// evidence of anything.
var genericBrandWords = map[string]bool{
	"pharma": true, "pharmaceutical": true, "pharmaceuticals": true, "group": true,
	"gmbh": true, "health": true, "healthcare": true, "care": true, "med": true,
	"medical": true, "medizin": true, "deutschland": true, "germany": true, "france": true,
	"international": true, "holding": true, "consulting": true, "services": true,
	"service": true, "solutions": true, "labor": true, "lab": true, "labs": true,
	"company": true, "europe": true, "global": true, "online": true, "shop": true,
}

// sameBrand reports whether an address plausibly belongs to the company whose page it was
// read from.
//
// Not "the same registrable domain": a company routinely answers mail at a sibling of the
// domain its website sits on — the same brand under a different TLD, or with "-group" or
// "-pharma" appended. What is being tested is whether the address's domain shares a
// distinctive word with the company's own domain or with the page it was read from. A
// sibling of the brand does; the data-protection address of the analytics vendor whose
// banner sits in the footer does not, which is the case this exists to catch.
func sameBrand(address, site, pageURL string) bool {
	// The registrable domain on both sides, so a host and a domain are never compared
	// against each other: mail.diapharm.de and diapharm.de are the same company.
	domain := normalize.CanonicalDomain(normalize.EmailDomain(address))
	if domain == "" {
		return false
	}
	for _, known := range []string{site, pageURL} {
		knownDomain := normalize.CanonicalDomain(known)
		if knownDomain == "" {
			continue
		}
		if knownDomain == domain {
			return true
		}
		if shareBrandWord(brandWords(domain), brandWords(knownDomain)) {
			return true
		}
	}
	return false
}

// brandWords reduces a domain to the distinctive words in its registrable name.
func brandWords(domain string) []string {
	name := domain
	if dot := strings.Index(name, "."); dot > 0 {
		name = name[:dot]
	}
	var words []string
	for _, word := range strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_'
	}) {
		if len(word) > 2 && !genericBrandWords[word] {
			words = append(words, word)
		}
	}
	// The whole label counts too, so "haenseler" matches "haenseler-pharma".
	if len(name) > 2 && !genericBrandWords[name] {
		words = append(words, name)
	}
	return words
}

func shareBrandWord(a, b []string) bool {
	for _, left := range a {
		for _, right := range b {
			if left == right {
				return true
			}
			// One brand inside another: "hwi" in "hwi-group", "medac" in "medacpharma".
			if len(left) > 3 && len(right) > 3 &&
				(strings.Contains(left, right) || strings.Contains(right, left)) {
				return true
			}
		}
	}
	return false
}

var kindOrder = map[model.ContactKind]int{
	model.ContactRecruitment: 0,
	model.ContactHR:          1,
	model.ContactGeneral:     2,
	model.ContactOther:       3,
}

// sortContacts puts the most appropriate address first. Stable within a kind, so the order
// a page listed two recruitment addresses in is preserved.
func sortContacts(contacts []model.Contact) {
	for i := 1; i < len(contacts); i++ {
		for j := i; j > 0 && kindOrder[contacts[j].Kind] < kindOrder[contacts[j-1].Kind]; j-- {
			contacts[j], contacts[j-1] = contacts[j-1], contacts[j]
		}
	}
}

// careersLinks finds links to a careers or jobs page on the company's own site.
func careersLinks(document *goquery.Document, page *directory.Page, site string) []string {
	seen := map[string]bool{}
	var out []string
	document.Find("a[href]").Each(func(_ int, anchor *goquery.Selection) {
		href, _ := anchor.Attr("href")
		href = strings.TrimSpace(href)
		if href == "" || strings.HasPrefix(strings.ToLower(href), "mailto:") {
			return
		}
		resolved := resolve(page.URL, href)
		normalized := normalize.URL(resolved)
		if normalized == "" || seen[normalized] {
			return
		}
		// Staying on the company's own registrable domain is what keeps this from becoming
		// a web crawl. A link to a job board is somebody else's site, however relevant.
		if !normalize.SameSite(normalized, site) {
			return
		}
		haystack := strings.ToLower(normalize.StripAccents(
			pathAndQuery(normalized) + " " + normalize.CollapseWhitespace(anchor.Text())))
		if !looksLikeCareers(haystack) {
			return
		}
		seen[normalized] = true
		out = append(out, normalized)
	})
	return out
}

func looksLikeCareers(haystack string) bool {
	for _, hint := range careersHints {
		if strings.Contains(haystack, hint) {
			return true
		}
	}
	return false
}

func pathAndQuery(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if parsed.RawQuery == "" {
		return parsed.EscapedPath()
	}
	return parsed.EscapedPath() + "?" + parsed.RawQuery
}

func resolve(pageURL, href string) string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return href
	}
	reference, err := url.Parse(href)
	if err != nil {
		return href
	}
	return base.ResolveReference(reference).String()
}

// Describe returns what a company says it does, taken from one of its own pages.
//
// This is not Text. Text is everything visible, which on a homepage is mostly the
// navigation menu — "Skip navigation Start Analytics Contact News Downloads Imprint" is
// what a company's chrome says, not what the company does. Ranking matches a profile's
// skill terms against this text, so chrome would both crowd out the real content and
// produce matches on menu labels.
//
// Three sources, in descending order of how deliberately the company wrote them:
//
//  1. The meta description, which is a company's own one-sentence summary of itself,
//     written for search results and therefore written on purpose.
//  2. The main content region, if the page marks one.
//  3. The body with chrome removed, as a last resort.
//
// Returns "" when the page says nothing useful, which is a better answer than returning a
// menu: an empty description scores nothing, while a menu scores on the wrong words.
func Describe(page *directory.Page, limit int) string {
	if page == nil || len(page.Body) == 0 {
		return ""
	}
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(page.Body))
	if err != nil {
		return ""
	}

	// A meta description is short, so it is combined with the body text rather than used
	// alone: it says what the company is, and the body says what it works on.
	var parts []string
	if summary := metaDescription(document); summary != "" {
		parts = append(parts, summary)
	}

	document.Find("script, style, noscript, template").Remove()
	// Chrome, in the order a page is likely to mark it. Removing these is what separates
	// what a company says from how a visitor moves around its site.
	document.Find("nav, header, footer, aside, form, " +
		"[role=navigation], [role=banner], [role=contentinfo], [role=search], " +
		".nav, .navbar, .navigation, .menu, .header, .footer, .sidebar, " +
		"#nav, #navbar, #navigation, #menu, #header, #footer, #sidebar, " +
		".cookie, .cookies, .cookie-banner, .breadcrumb, .breadcrumbs, .skip-link").Remove()

	// Prefer a marked main region; fall back to the body.
	region := document.Find("main, [role=main], article").First()
	if region.Length() == 0 {
		region = document.Find("body")
	}
	if body := normalize.CollapseWhitespace(visibleNodeText(region)); body != "" {
		parts = append(parts, body)
	}

	text := normalize.CollapseWhitespace(strings.Join(parts, " "))
	if limit > 0 && len(text) > limit {
		// Cut on a word boundary, so the stored text does not end mid-word.
		if cut := strings.LastIndex(text[:limit], " "); cut > limit/2 {
			text = text[:cut]
		} else {
			text = text[:limit]
		}
	}
	return text
}

// metaDescription reads the page's own summary of itself, preferring the standard meta tag
// over the Open Graph one when both are present.
func metaDescription(document *goquery.Document) string {
	for _, selector := range []string{
		`meta[name="description"]`,
		`meta[name="Description"]`,
		`meta[property="og:description"]`,
	} {
		if content, ok := document.Find(selector).First().Attr("content"); ok {
			if trimmed := normalize.CollapseWhitespace(content); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// visibleNodeText is visibleText for a selection rather than a whole document.
func visibleNodeText(selection *goquery.Selection) string {
	var out strings.Builder
	for _, node := range selection.Nodes {
		out.WriteString(nodeText(node))
		out.WriteString(" ")
	}
	return out.String()
}
